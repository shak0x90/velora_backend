-- +goose Up
-- +goose StatementBegin

-- Messaging, built to the chat-system design: durable first, live second.
--
-- The ordering rule behind every table here is that PostgreSQL holds the
-- truth and the socket is only a shortcut. Anything a client could miss
-- because a connection dropped must be recoverable by reading rows, so every
-- fact that changes a thread is written as an event, not merely pushed.

-- A conversation is one row for the pair, ordered the way matches are. Two
-- people have one thread between them however it began, so the pair is the
-- identity and the ordering constraint makes that enforceable.
create table conversations (
    id     uuid primary key,
    user_a uuid not null references users(id) on delete cascade,
    user_b uuid not null references users(id) on delete cascade,

    -- How this thread earned the right to exist, which decides who may write.
    -- match:     an active mutual match, open from the start.
    -- request:   a social introduction that has to be accepted.
    -- anonymous: a server-authorised pairing where identities start hidden.
    origin text not null default 'match'
           check (origin in ('match', 'request', 'anonymous')),

    -- pending:  the opener has sent their one intro; the other side has not
    --           agreed to talk.
    -- open:     both sides may write.
    -- declined: refused. The row stays, so the refusal is remembered even if
    --           the two later match.
    -- closed:   a block or unmatch ended it. Distinct from declined: this one
    --           was a conversation before it stopped being allowed.
    state  text not null default 'open'
           check (state in ('pending', 'open', 'declined', 'closed')),

    -- Who opened a request. Null for a match, which nobody had to ask for.
    requested_by uuid references users(id) on delete cascade,

    -- Identities are hidden until both sides consent. Set once, in the same
    -- transaction as the second consent and the identity.revealed event.
    revealed_at timestamptz,

    -- The high-water mark for this thread's messages. Allocated under the
    -- conversation's row lock so two simultaneous sends cannot take the same
    -- number, which a sequence generator could not promise.
    last_seq bigint not null default 0,

    created_at      timestamptz not null default now(),
    -- Denormalised so the inbox sorts without touching the messages table.
    last_message_at timestamptz,

    unique (user_a, user_b),
    constraint conversations_ordered check (user_a < user_b),
    -- A pending thread nobody opened could never be answered.
    constraint conversations_request_has_asker
        check (state <> 'pending' or requested_by is not null),
    -- Only an anonymous thread can be revealed.
    constraint conversations_reveal_is_anonymous
        check (revealed_at is null or origin = 'anonymous')
);

create index conversations_a_idx on conversations (user_a, last_message_at desc nulls last);
create index conversations_b_idx on conversations (user_b, last_message_at desc nulls last);

create table messages (
    id              uuid primary key,
    conversation_id uuid not null references conversations(id) on delete cascade,

    -- Position within this thread. Gap-free and strictly increasing, which is
    -- what lets a client that missed messages ask for "everything after 16"
    -- and know it received all of them. A timestamp could not: two messages
    -- can share one, and a clock can step backwards.
    seq             bigint not null,

    sender_id       uuid not null references users(id) on delete cascade,
    body            text not null,

    -- The same four kinds both clients declare. Only text can be sent today;
    -- the rest exist so adding them is not a migration.
    kind            text not null default 'text'
                    check (kind in ('text', 'image', 'voice', 'gif')),

    -- The sender's own id for this message, generated before the request.
    --
    -- This is what makes a retry safe. If the response to a send is lost, the
    -- client retries with the same value and gets the stored message back
    -- rather than writing a second one. Without it, every dropped response on
    -- a weak connection is a duplicate on both people's screens — and a weak
    -- connection is exactly when someone taps send again.
    client_message_id text not null,

    created_at timestamptz not null default now(),
    edited_at  timestamptz,
    -- Soft delete: the row survives so the sequence stays gap-free and so
    -- other devices can be told the message went away.
    deleted_at timestamptz,

    -- Positions are unique within a thread, not globally.
    unique (conversation_id, seq),
    -- One message per client id per sender per thread. Scoped to the sender so
    -- one person's id choice can never collide with another's.
    unique (conversation_id, sender_id, client_message_id)
);

-- History is read forward from a position, so the index leads with the thread
-- and then the sequence.
create index messages_thread_idx on messages (conversation_id, seq);

-- Per-side receipts, as positions rather than per-message rows.
--
-- With two participants, two numbers answer "delivered?" and "read?" for every
-- message in the thread at once, and cost the same whether it holds ten
-- messages or ten thousand. A row per message per reader would grow without
-- bound to store a fact already implied by "they have caught up to here".
--
-- Membership is user_a and user_b on the conversation; this is state, written
-- when a thread is created so that a missing row means a data problem rather
-- than a default to guess at.
create table conversation_participants (
    conversation_id    uuid not null references conversations(id) on delete cascade,
    user_id            uuid not null references users(id) on delete cascade,

    -- Their device acknowledged receiving up to here. A server-side socket
    -- write is not proof of this; only the client saying so is.
    last_delivered_seq bigint not null default 0,
    -- They had it on screen up to here.
    last_read_seq      bigint not null default 0,

    -- Muting silences push without leaving the thread.
    muted    boolean not null default false,
    archived boolean not null default false,

    primary key (conversation_id, user_id)
);

-- The inbox asks "which of my threads have unread messages", so the lookup
-- runs from the person to their threads.
create index conversation_participants_user_idx
    on conversation_participants (user_id);

-- Consent to drop the mask on an anonymous thread. One row per person; the
-- identities unlock only when both exist.
--
-- A table rather than two booleans because the rule is "both agreed", and a
-- row that must be inserted is harder to set by accident than a boolean that
-- defaults to false.
create table reveal_consents (
    conversation_id uuid not null references conversations(id) on delete cascade,
    user_id         uuid not null references users(id) on delete cascade,
    created_at      timestamptz not null default now(),
    primary key (conversation_id, user_id)
);

-- Everything that changed, per person, in the order they must apply it.
--
-- This is what makes a dropped connection survivable. A client keeps its
-- cursor, reconnects, and asks for everything after it; the socket becomes an
-- optimisation rather than the only delivery path. Receipts and permission
-- changes live here too, because "they read it" and "this thread is closed"
-- are as easy to miss as a message and as confusing to be wrong about.
create table user_events (
    user_id uuid not null references users(id) on delete cascade,

    -- Per-user position, allocated under that user's row lock rather than from
    -- a shared sequence. A sequence hands out 1841 and 1842 to two
    -- transactions, and if 1842 commits first a client can read past 1841 and
    -- lose it permanently. The lock makes commit order and cursor order agree.
    seq bigint not null,

    kind text not null
         check (kind in (
             'conversation.new',      -- a thread appeared, or was requested
             'conversation.access',   -- accepted, declined, closed, reopened
             'message.new',
             'message.edited',
             'message.deleted',
             'receipt.delivered',
             'receipt.read',
             'identity.revealed'
         )),

    -- The thread this concerns, when it concerns one.
    conversation_id uuid references conversations(id) on delete cascade,
    -- The event's detail, shaped by kind. Deliberately not a wide table of
    -- mostly-null columns: these payloads differ completely from each other.
    payload jsonb not null default '{}'::jsonb,

    created_at timestamptz not null default now(),

    primary key (user_id, seq)
);

-- Pruning reads by age across all users, which the primary key cannot serve.
create index user_events_created_idx on user_events (created_at);

-- The counter behind user_events.seq. On users rather than its own table
-- because it is one number per account with no other attributes, and because
-- locking the row we already have avoids a second lock in the send path.
alter table users add column event_seq bigint not null default 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
alter table users drop column if exists event_seq;
drop table if exists user_events;
drop table if exists reveal_consents;
drop table if exists conversation_participants;
drop table if exists messages;
drop table if exists conversations;
-- +goose StatementEnd

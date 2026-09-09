-- +goose Up
-- +goose StatementBegin
-- Keep the existing messaging draft intact; separate social and dating consent.
alter table conversations drop constraint conversations_user_a_user_b_key;
alter table conversations add unique (user_a, user_b, origin);
alter table messages add column reply_to_id uuid references messages(id);
alter table conversation_participants add column participant_id uuid not null default gen_random_uuid();
alter table conversation_participants add unique (participant_id);
create table chat_tickets (
 token_hash bytea primary key, user_id uuid not null references users(id) on delete cascade,
 expires_at timestamptz not null, access_expires_at timestamptz not null
);
create index chat_tickets_expiry on chat_tickets(expires_at);
create table chat_queue (
 user_id uuid primary key references users(id) on delete cascade,
 waiting_since timestamptz not null default now(), expires_at timestamptz not null
);
create table chat_push_subscriptions (
 id uuid primary key, user_id uuid not null references users(id) on delete cascade,
 endpoint text not null unique, p256dh text not null, auth text not null,
 created_at timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose Down
-- The transaction fails safely if distinct consent contexts now share a pair.
-- Export/resolve those conversations before attempting to revert this schema.
alter table conversations drop constraint conversations_user_a_user_b_origin_key;
alter table conversations add constraint conversations_user_a_user_b_key unique (user_a, user_b);
drop table chat_push_subscriptions;
drop table chat_queue;
drop table chat_tickets;
alter table conversation_participants drop column participant_id;
alter table messages drop column reply_to_id;

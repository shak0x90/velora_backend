-- +goose Up
-- +goose StatementBegin

-- A block is one-directional as a record and symmetric as a rule: the row says
-- who did the blocking, but every query treats a block in either direction as
-- a wall. Storing it one way keeps "who blocked whom" answerable, which
-- moderation needs; enforcing it both ways is what makes it mean anything.
create table blocks (
    user_id    uuid not null references users(id) on delete cascade,
    blocked_id uuid not null references users(id) on delete cascade,
    -- Free text, never shown to the blocked person.
    reason     text not null default '',
    created_at timestamptz not null default now(),
    primary key (user_id, blocked_id),
    constraint blocks_not_self check (user_id <> blocked_id)
);

-- The reverse lookup matters as much as the forward one: every feed query asks
-- "has this candidate blocked me" as well as "have I blocked them".
create index blocks_blocked_idx on blocks (blocked_id);

create table reports (
    id          uuid primary key,
    reporter_id uuid not null references users(id) on delete cascade,
    subject_id  uuid not null references users(id) on delete cascade,
    reason      text not null
                check (reason in (
                    'harassment', 'spam', 'fake_profile', 'inappropriate_photos',
                    'underage', 'offline_behaviour', 'other'
                )),
    -- What the reporter typed, and where they were when they reported.
    detail      text not null default '',
    context     text not null default 'profile'
                check (context in ('profile', 'photo', 'message', 'prompt')),

    status      text not null default 'open'
                check (status in ('open', 'reviewing', 'actioned', 'dismissed')),
    reviewed_by uuid references users(id) on delete set null,
    reviewed_at timestamptz,
    -- What the moderator decided and why, for whoever looks next.
    resolution  text not null default '',
    created_at  timestamptz not null default now(),

    constraint reports_not_self check (reporter_id <> subject_id)
);

-- The queue is read by status, oldest first, so open reports surface in the
-- order they arrived rather than whenever someone scrolls far enough.
create index reports_queue_idx on reports (status, created_at);
create index reports_subject_idx on reports (subject_id, created_at desc);

-- One live report per person per subject. Reporting someone again updates the
-- open row instead of flooding the queue with duplicates of one grievance.
create unique index reports_one_open_per_pair
    on reports (reporter_id, subject_id)
    where status in ('open', 'reviewing');

-- Moderation is a property of the account rather than its own table: there
-- will be a handful of these and they need no other attributes yet.
alter table users add column is_moderator boolean not null default false;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
alter table users drop column if exists is_moderator;
drop table if exists reports;
drop table if exists blocks;
-- +goose StatementEnd

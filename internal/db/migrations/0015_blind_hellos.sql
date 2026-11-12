-- +goose Up

-- A blind hello: the first message of a blind chat, left by someone who found
-- nobody looking. The next compatible person who starts a blind chat (within
-- 24 hours) is paired with its author and sees it as the opening line, so two
-- people no longer have to be searching in the same 30 seconds. One per person.
create table blind_hellos (
    user_id           uuid primary key references users (id) on delete cascade,
    body              text not null check (length(body) between 1 and 500),
    client_message_id uuid not null,
    created_at        timestamptz not null default now(),
    expires_at        timestamptz not null
);
create index blind_hellos_created_idx on blind_hellos (created_at);

-- "Someone is looking for a blind chat" pushes, kept to rate-limit them: a
-- person gets at most one every few hours. Pruned after a day.
create table blind_alerts (
    recipient uuid not null references users (id) on delete cascade,
    sent_at   timestamptz not null default now()
);
create index blind_alerts_recipient_idx on blind_alerts (recipient, sent_at desc);

-- +goose Down
drop table if exists blind_alerts;
drop table if exists blind_hellos;

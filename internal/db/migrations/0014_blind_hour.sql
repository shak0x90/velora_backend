-- +goose Up

-- Blind hour: a daily window (21:00–22:00 in each person's time zone) when
-- anonymous chat is promoted, so enough people are looking at once for the
-- queue to pair them. One switch for the reminder push, default on.
alter table user_settings add column notify_blind_hour boolean not null default true;

-- One reminder per person per local day. The worker inserts here before
-- enqueueing the push, so a restart or a second worker cannot send it twice.
create table blind_hour_reminders (
    user_id uuid not null references users (id) on delete cascade,
    day     date not null,
    primary key (user_id, day)
);

-- +goose Down
drop table if exists blind_hour_reminders;
alter table user_settings drop column if exists notify_blind_hour;

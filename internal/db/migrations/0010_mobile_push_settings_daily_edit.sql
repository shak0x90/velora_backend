-- +goose Up

-- Native push tokens (FCM for Android, APNs or FCM for iOS). A token belongs to
-- one install, so it is unique on its own: when a phone signs into a different
-- account, re-registering moves the row instead of leaving the old account
-- receiving the new account's pushes.
create table devices (
    id           uuid primary key,
    user_id      uuid not null references users (id) on delete cascade,
    platform     text not null check (platform in ('android', 'ios')),
    token        text not null unique check (length(token) between 1 and 4096),
    app_version  text not null default '' check (length(app_version) <= 64),
    created_at   timestamptz not null default now(),
    last_seen_at timestamptz not null default now()
);
create index devices_user_idx on devices (user_id);

-- Notification preferences. One row per user, written on first PATCH; a user
-- with no row gets the defaults in code, so this table never needs a backfill.
-- Quiet hours are local wall-clock times ('HH:MM') in tz, both or neither.
create table user_settings (
    user_id           uuid primary key references users (id) on delete cascade,
    notify_matches    boolean not null default true,
    notify_messages   boolean not null default true,
    notify_daily_edit boolean not null default true,
    notify_weekly     boolean not null default true,
    quiet_start       text check (quiet_start ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    quiet_end         text check (quiet_end ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    tz                text not null default 'UTC' check (length(tz) between 1 and 64),
    updated_at        timestamptz not null default now(),
    constraint user_settings_quiet_pair check ((quiet_start is null) = (quiet_end is null))
);

-- The Daily Edit: the batch handed to a user for one edit day, frozen when
-- first asked for, so reopening the app does not reshuffle it. Positions keep
-- the original ranking order; rank_score is the score it was ranked with.
create table daily_edits (
    user_id    uuid not null references users (id) on delete cascade,
    edit_date  date not null,
    profile_id uuid not null references users (id) on delete cascade,
    position   smallint not null,
    rank_score double precision not null default 0,
    created_at timestamptz not null default now(),
    primary key (user_id, edit_date, profile_id)
);
create index daily_edits_profile_idx on daily_edits (profile_id);

-- +goose Down
drop table if exists daily_edits;
drop table if exists user_settings;
drop table if exists devices;

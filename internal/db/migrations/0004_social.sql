-- +goose Up
-- +goose StatementBegin

-- A pass is a decision not to see someone again. Keyed on the pair rather than
-- a surrogate id: passing twice is the same fact, not two of them.
create table passes (
    user_id    uuid not null references users(id) on delete cascade,
    target_id  uuid not null references users(id) on delete cascade,
    created_at timestamptz not null default now(),
    primary key (user_id, target_id),
    constraint passes_not_self check (user_id <> target_id)
);

-- One like per direction per pair. Re-liking updates the existing row, so a
-- double tap cannot produce two entries on someone's likes screen.
create table likes (
    id           uuid primary key,
    from_user_id uuid not null references users(id) on delete cascade,
    to_user_id   uuid not null references users(id) on delete cascade,
    kind         text not null default 'profile'
                 check (kind in ('profile', 'photo', 'prompt', 'interest')),
    label        text not null default '',
    -- Premium "priority" likes surface at the top of the recipient's list.
    priority     boolean not null default false,
    created_at   timestamptz not null default now(),
    unique (from_user_id, to_user_id),
    constraint likes_not_self check (from_user_id <> to_user_id)
);

create index likes_to_idx   on likes (to_user_id, created_at desc);
create index likes_from_idx on likes (from_user_id, created_at desc);

-- A match is one row for the pair, not one per side. The ordering constraint
-- is what makes that enforceable: without it, (a,b) and (b,a) would both
-- insert and the same two people would match twice.
create table matches (
    id         uuid primary key,
    user_a     uuid not null references users(id) on delete cascade,
    user_b     uuid not null references users(id) on delete cascade,
    created_at timestamptz not null default now(),
    unique (user_a, user_b),
    constraint matches_ordered check (user_a < user_b)
);

create index matches_a_idx on matches (user_a, created_at desc);
create index matches_b_idx on matches (user_b, created_at desc);

create table notifications (
    id         uuid primary key,
    user_id    uuid not null references users(id) on delete cascade,
    kind       text not null
               check (kind in ('like', 'match', 'message', 'matchmaker', 'profile')),
    title      text not null,
    body       text not null default '',
    -- The person the notification is about, when there is one. Null for
    -- account-level notices, so it cannot be declared not null.
    profile_id uuid references users(id) on delete cascade,
    read_at    timestamptz,
    created_at timestamptz not null default now()
);

create index notifications_user_idx on notifications (user_id, created_at desc);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop table if exists notifications;
drop table if exists matches;
drop table if exists likes;
drop table if exists passes;
-- +goose StatementEnd

-- +goose Up
-- +goose StatementBegin

-- Geo extensions for the discovery candidate query. earthdistance needs cube.
create extension if not exists cube;
create extension if not exists earthdistance;

create table users (
    id         uuid primary key,
    created_at timestamptz not null default now(),
    status     text        not null default 'active'
               check (status in ('active', 'suspended', 'deleted')),
    is_premium boolean     not null default false
);

-- One user, many ways to sign in. The primary key is (method, identifier)
-- rather than a surrogate id so the same phone or email cannot be claimed
-- twice — the uniqueness *is* the constraint we care about.
create table auth_identities (
    user_id     uuid not null references users(id) on delete cascade,
    method      text not null check (method in ('phone', 'email', 'google', 'apple')),
    identifier  text not null,  -- E.164 phone, lowercased email, or provider subject
    verified_at timestamptz,
    created_at  timestamptz not null default now(),
    primary key (method, identifier)
);

create index auth_identities_user_idx on auth_identities (user_id);

-- Rotating refresh tokens. Tokens are stored hashed; family_id ties a rotation
-- chain together so presenting an already-used token can revoke the whole
-- family, which is the standard theft signal.
create table refresh_tokens (
    id         uuid primary key,
    user_id    uuid  not null references users(id) on delete cascade,
    family_id  uuid  not null,
    token_hash bytea not null unique,
    expires_at timestamptz not null,
    used_at    timestamptz,
    revoked_at timestamptz,
    created_at timestamptz not null default now()
);

create index refresh_tokens_user_idx   on refresh_tokens (user_id);
create index refresh_tokens_family_idx on refresh_tokens (family_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop table if exists refresh_tokens;
drop table if exists auth_identities;
drop table if exists users;
-- +goose StatementEnd

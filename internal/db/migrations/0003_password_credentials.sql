-- +goose Up
-- +goose StatementBegin

-- Password credentials live on the identity row rather than in a separate
-- table: an email identity either has a password or it doesn't, and the two
-- are always read together at login.
--
-- Nullable on purpose. A Google identity has no password, and an email
-- identity created as a side effect of Google sign-in has none either — that
-- row exists only so a later email login resolves to the same user.
alter table auth_identities
    add column password_hash bytea,
    add column password_set_at timestamptz;

-- Only email identities may carry a password. Enforced here so no code path
-- can attach one to a Google or Apple identity by mistake.
alter table auth_identities
    add constraint auth_identities_password_email_only
    check (password_hash is null or method = 'email');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
alter table auth_identities drop constraint if exists auth_identities_password_email_only;
alter table auth_identities drop column if exists password_set_at;
alter table auth_identities drop column if exists password_hash;
-- +goose StatementEnd

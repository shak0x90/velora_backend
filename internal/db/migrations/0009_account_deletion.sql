-- +goose Up
-- Self-service deletion keeps the users row as a tombstone (status 'deleted')
-- rather than removing it. Conversations, reports and receipts reference the
-- id with cascading foreign keys, so a hard delete would silently erase the
-- other person's side of a thread and every report filed about the account.
-- The tombstone carries no personal data: id, timestamps and flags only.
alter table users add column deleted_at timestamptz;
-- Any row an operator marked deleted by hand before this existed.
update users set deleted_at = now() where status = 'deleted';
alter table users add constraint users_deleted_has_time
    check ((status = 'deleted') = (deleted_at is not null));

-- +goose Down
alter table users drop constraint if exists users_deleted_has_time;
alter table users drop column if exists deleted_at;

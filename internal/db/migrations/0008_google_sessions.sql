-- +goose Up
alter table refresh_tokens add column auth_method text not null default 'email'
    check (auth_method in ('email', 'google'));

-- Cutover retires tickets issued by the password-authenticated API.
delete from chat_tickets;

-- +goose Down
alter table refresh_tokens drop column auth_method;

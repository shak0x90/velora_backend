-- +goose Up

-- Every profile gets a unique, public username: the one thing a friend can
-- type to find a specific person, since first names are shared by thousands.
-- Stored lowercase, so uniqueness is case-insensitive by construction.
alter table profiles add column username text;

-- suggest_username turns a first name into a free username: lowercase ASCII
-- letters and digits from the name (at most 15), padded to the 3-character
-- minimum, then the first of base, base2, base3, ... nobody holds. Used by the
-- backfill below and by onboarding when the client does not choose one, so
-- there is exactly one definition of a generated username.
-- +goose StatementBegin
create function suggest_username(first_name text) returns text
language plpgsql as $$
declare
    base text := left(regexp_replace(lower(coalesce(first_name, '')), '[^a-z0-9]', '', 'g'), 15);
    candidate text;
    n int := 1;
begin
    if length(base) < 3 then
        base := base || 'user';
    end if;
    candidate := base;
    while exists (select 1 from profiles where username = candidate) loop
        n := n + 1;
        candidate := base || n;
    end loop;
    return candidate;
end;
$$;
-- +goose StatementEnd

-- Backfill oldest first, so the first Shakil keeps "shakil".
-- +goose StatementBegin
do $$
declare
    r record;
begin
    for r in select user_id, first_name from profiles where username is null order by created_at, user_id loop
        update profiles set username = suggest_username(r.first_name) where user_id = r.user_id;
    end loop;
end;
$$;
-- +goose StatementEnd

-- Any insert without a username gets one, so seed scripts, fixtures and
-- hand-written SQL keep working and the NOT NULL below can never be the
-- thing that breaks an onboarding.
-- +goose StatementBegin
create function profiles_default_username() returns trigger
language plpgsql as $$
begin
    if new.username is null then
        new.username := suggest_username(new.first_name);
    end if;
    return new;
end;
$$;
-- +goose StatementEnd
create trigger profiles_default_username before insert on profiles
    for each row execute function profiles_default_username();

alter table profiles alter column username set not null;
alter table profiles add constraint profiles_username_format
    check (username ~ '^[a-z0-9][a-z0-9_.]{2,19}$');
create unique index profiles_username_key on profiles (username);

-- +goose Down
drop trigger if exists profiles_default_username on profiles;
drop function if exists profiles_default_username();
drop index if exists profiles_username_key;
alter table profiles drop constraint if exists profiles_username_format;
alter table profiles drop column if exists username;
drop function if exists suggest_username(text);

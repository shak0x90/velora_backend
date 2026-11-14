-- +goose Up

-- Blind hellos become a board: matching people browse them and reply, and
-- each reply opens its own blind chat with the author, up to five. Before,
-- a hello sat unseen until the next searcher happened to be paired with it.
alter table blind_hellos add column id uuid not null default gen_random_uuid();
alter table blind_hellos add constraint blind_hellos_id_key unique (id);
alter table blind_hellos add column reply_count int not null default 0;

-- Who replied to which hello, so nobody replies twice and a hello closes at
-- five chats. Goes with the hello, either person, or the chat.
create table blind_hello_replies (
    hello_id        uuid not null references blind_hellos (id) on delete cascade,
    user_id         uuid not null references users (id) on delete cascade,
    conversation_id uuid not null references conversations (id) on delete cascade,
    created_at      timestamptz not null default now(),
    primary key (hello_id, user_id)
);

-- +goose Down
drop table if exists blind_hello_replies;
alter table blind_hellos drop column if exists reply_count;
alter table blind_hellos drop constraint if exists blind_hellos_id_key;
alter table blind_hellos drop column if exists id;

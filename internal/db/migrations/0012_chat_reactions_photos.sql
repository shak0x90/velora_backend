-- +goose Up

-- One reaction per person per message, from a small fixed set. Choosing a
-- different emoji replaces the old one rather than stacking, which is how the
-- clients present it ("you reacted ❤️").
create table message_reactions (
    message_id uuid not null references messages (id) on delete cascade,
    user_id    uuid not null references users (id) on delete cascade,
    emoji      text not null check (emoji in ('❤️', '😂', '😮', '😢', '😡', '👍')),
    created_at timestamptz not null default now(),
    primary key (message_id, user_id)
);

-- Photos sent in chat. Uploaded first (message_id null), then attached by the
-- send that names them, exactly once. The files live outside the public
-- /media directory and are only ever served through an authenticated route
-- that checks the viewer is in the conversation.
create table message_attachments (
    id              uuid primary key,
    conversation_id uuid not null references conversations (id) on delete cascade,
    uploader_id     uuid not null references users (id) on delete cascade,
    message_id      uuid unique references messages (id) on delete cascade,
    object_key      text not null,
    width           int not null check (width > 0),
    height          int not null check (height > 0),
    created_at      timestamptz not null default now()
);
-- The prune job sweeps uploads that were never sent.
create index message_attachments_unsent_idx on message_attachments (created_at) where message_id is null;
create index message_attachments_uploader_idx on message_attachments (uploader_id);

-- +goose Down
drop table if exists message_attachments;
drop table if exists message_reactions;

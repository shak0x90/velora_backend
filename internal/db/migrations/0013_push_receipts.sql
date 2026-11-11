-- +goose Up

-- One row per push handed to FCM or APNs, for phones whose app confirms what
-- it receives (POST /push/ack with the pushId sent in the push data). A push
-- the provider accepted but the phone never confirmed was blocked on the
-- phone, typically by a battery manager (Realme, OPPO, Xiaomi, vivo...). The
-- app is told how many it missed when it next registers, so only people whose
-- pushes are actually being dropped are asked to change settings.
-- Rows older than 14 days are pruned by the worker.
create table push_receipts (
    id          uuid primary key,
    device_id   uuid not null references devices (id) on delete cascade,
    sent_at     timestamptz not null default now(),
    received_at timestamptz
);
create index push_receipts_device_idx on push_receipts (device_id, sent_at desc);

-- +goose Down
drop table if exists push_receipts;

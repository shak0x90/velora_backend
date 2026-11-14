-- +goose Up

-- Reporting a hello on the board. The reporter stops seeing it at once, and
-- three different reporters take it off the board for everyone until a
-- moderator looks (the report itself lands in the usual reports queue).
create table blind_hello_reports (
    hello_id   uuid not null references blind_hellos (id) on delete cascade,
    user_id    uuid not null references users (id) on delete cascade,
    created_at timestamptz not null default now(),
    primary key (hello_id, user_id)
);
alter table blind_hellos add column report_count int not null default 0;

-- "N new hellos match you" is sent at most once per person per local day.
create table hello_digests (
    user_id uuid not null references users (id) on delete cascade,
    day     date not null,
    primary key (user_id, day)
);

-- +goose Down
drop table if exists hello_digests;
alter table blind_hellos drop column if exists report_count;
drop table if exists blind_hello_reports;

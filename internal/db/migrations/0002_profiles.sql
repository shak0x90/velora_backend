-- +goose Up
-- +goose StatementBegin

-- One profile per user. Lifestyle fields are flat columns rather than JSON
-- because the discovery filters query them directly; a JSON blob would make
-- every filter a sequential scan.
create table profiles (
    user_id             uuid primary key references users(id) on delete cascade,
    first_name          text not null,
    birth_date          date not null,
    gender              text not null,
    pronouns            text not null default '',
    city                text not null default '',
    neighborhood        text not null default '',
    lat                 double precision,
    lng                 double precision,
    occupation          text not null default '',
    education           text not null default '',
    bio                 text not null default '',
    interests           text[] not null default '{}',
    relationship_intent text not null default 'notSure',
    communication_style text not null default 'thoughtful',
    personality_traits  text[] not null default '{}',

    -- Lifestyle
    exercise       text not null default '',
    drinking       text not null default '',
    smoking        text not null default '',
    diet           text not null default '',
    sleep_schedule text not null default '',
    pets           text not null default '',
    children       text not null default '',
    religion       text not null default '',
    languages      text[] not null default '{}',
    height_cm      int,

    -- Dating preferences
    interested_in   text[] not null default '{}',
    min_age         int not null default 21,
    max_age         int not null default 40,
    max_distance_km int not null default 40,
    intents         text[] not null default '{}',

    -- State
    photo_verified boolean not null default false,
    phone_verified boolean not null default false,
    hidden         boolean not null default false,
    incognito      boolean not null default false,
    last_active_at timestamptz not null default now(),
    created_at     timestamptz not null default now(),
    updated_at     timestamptz not null default now(),

    -- Enforced at the schema level, not just in the app: an under-18 row
    -- should be impossible regardless of which code path inserts it.
    constraint profiles_adult_only check (birth_date <= current_date - interval '18 years')
);

create index profiles_geo_idx    on profiles using gist (ll_to_earth(lat, lng));
create index profiles_active_idx on profiles (last_active_at desc) where hidden = false;

create table profile_photos (
    id         uuid primary key,
    user_id    uuid not null references users(id) on delete cascade,
    position   smallint not null,
    object_key text not null,
    status     text not null default 'pending'
               check (status in ('pending', 'ready', 'rejected')),
    created_at timestamptz not null default now(),
    unique (user_id, position)
);

create table profile_prompts (
    id       uuid primary key,
    user_id  uuid not null references users(id) on delete cascade,
    position smallint not null,
    question text not null,
    answer   text not null,
    unique (user_id, position)
);

create table personality_answers (
    user_id     uuid not null references users(id) on delete cascade,
    question_id text not null,
    question    text not null,
    answer      text not null,
    primary key (user_id, question_id)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop table if exists personality_answers;
drop table if exists profile_prompts;
drop table if exists profile_photos;
drop table if exists profiles;
-- +goose StatementEnd

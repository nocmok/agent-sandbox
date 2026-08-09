create table sandbox
(
    id              uuid primary key default gen_random_uuid(),
    image           text not null,
    workspace       text not null,
    locked_until    timestamptz,
    created_at      timestamptz not null default now(),
    deleted_at      timestamptz,
    idempotency_key uuid not null
);

create unique index sandbox_idempotency_key_uidx
    on sandbox (idempotency_key) where deleted_at is null;

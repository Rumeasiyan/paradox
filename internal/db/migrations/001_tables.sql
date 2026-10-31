-- PARADOX's own tables (PRD Annex A). River's tables are separate.
-- Limits from the PRD are CHECK constraints, so a bug cannot store what
-- the PRD forbids.

CREATE TABLE system (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name          text NOT NULL UNIQUE,
    wake_mode     text NOT NULL CHECK (wake_mode IN ('sablier', 'none')),
    address       text,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    removed_at    timestamptz
);

CREATE TABLE job (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    system_id   bigint NOT NULL REFERENCES system (id),
    name        text NOT NULL,
    repeat      text,
    timeout_s   integer NOT NULL CHECK (timeout_s > 0),
    retries     integer NOT NULL CHECK (retries BETWEEN 0 AND 100),
    next_due_at timestamptz,
    last_due_at timestamptz,
    UNIQUE (system_id, name)
);

CREATE TABLE run (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id       bigint NOT NULL REFERENCES job (id),
    source       text NOT NULL CHECK (source IN ('schedule', 'queue')),
    data         jsonb,
    due_at       timestamptz NOT NULL,
    state        text NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled', 'skipped', 'missed')),
    max_attempts integer NOT NULL CHECK (max_attempts >= 1),
    attempts     integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND max_attempts),
    started_at   timestamptz,
    finished_at  timestamptz,
    last_error   text CHECK (octet_length(last_error) <= 2048),
    reported_at  timestamptz
);
CREATE INDEX run_job_due ON run (job_id, due_at);
CREATE INDEX run_open ON run (state) WHERE state IN ('queued', 'running');
CREATE INDEX run_finished ON run (finished_at);

CREATE TABLE attempt (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id      uuid NOT NULL REFERENCES run (id) ON DELETE CASCADE,
    number      integer NOT NULL CHECK (number >= 1),
    started_at  timestamptz NOT NULL,
    ended_at    timestamptz,
    outcome     text CHECK (outcome IN ('success', 'error', 'timeout', 'wake_timeout')),
    status_code integer,
    error       text CHECK (octet_length(error) <= 2048),
    UNIQUE (run_id, number)
);

CREATE TABLE queue_request (
    system_id       bigint NOT NULL REFERENCES system (id),
    request_id      text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    run_id          uuid NOT NULL UNIQUE REFERENCES run (id) ON DELETE CASCADE,
    received_at     timestamptz NOT NULL DEFAULT now(),
    key_fingerprint text NOT NULL,
    PRIMARY KEY (system_id, request_id)
);
CREATE INDEX queue_request_received ON queue_request (received_at);

CREATE TABLE run_action (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id       uuid NOT NULL REFERENCES run (id) ON DELETE CASCADE,
    by_system_id bigint REFERENCES system (id),
    action       text NOT NULL CHECK (action IN ('retry', 'cancel')),
    acted_at     timestamptz NOT NULL DEFAULT now()
);

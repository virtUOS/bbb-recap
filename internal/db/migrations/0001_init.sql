-- 0001: initial schema (docs/reference/architecture.md §6.1, metadata-interface.md §2).
-- Enum CHECK constraints use the exact values of docs/reference/README.md "States and enums".

-- Registrations from the front end (metadata-interface.md §2). After delivery, cleanup deletes the owner identity
-- and keeps the row as the result record until result_expires_at (§2.7).
CREATE TABLE registrations (
    registration_id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    record_id         text COLLATE "C" NOT NULL UNIQUE, -- exact, case-sensitive join key
    frontend_id       text NOT NULL,
    meeting_id        text NOT NULL,
    owner_sub         text,                             -- NULL in a tombstone and a result record
    owner_email       text,                             -- NULL in a tombstone and a result record
    status            text NOT NULL CHECK (status IN ('registered', 'cancelled')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz,
    cancelled_at      timestamptz,
    result_expires_at timestamptz                       -- delivered_at + RECAP_RESULT_TTL, set by cleanup
);

-- Meeting packages received from the exporter. file_count and total_bytes stay as job metadata after cleanup has
-- deleted the package_files rows, whose paths hold BBB user IDs (privacy.md §5.2).
CREATE TABLE packages (
    package_id       text PRIMARY KEY CHECK (package_id ~ '^[0-9a-f]{64}$'),
    job_id           uuid NOT NULL UNIQUE, -- allocated at the first manifest PUT
    bbb_server_id    text NOT NULL,
    record_id        text COLLATE "C" NOT NULL,
    meeting_id       text NOT NULL,
    state            text NOT NULL CHECK (state IN ('receiving', 'complete', 'committed')),
    total_bytes      bigint NOT NULL CHECK (total_bytes >= 0),
    created_at       timestamptz NOT NULL DEFAULT now(),
    last_activity_at timestamptz NOT NULL DEFAULT now(),
    committed_at     timestamptz,
    file_count       integer CHECK (file_count >= 0),
    UNIQUE (bbb_server_id, record_id)
);

CREATE TABLE package_files (
    package_id  text NOT NULL REFERENCES packages (package_id) ON DELETE CASCADE,
    path        text NOT NULL,
    role        text NOT NULL CHECK (role IN ('events', 'audio-microphone', 'notes', 'presentation')),
    size        bigint NOT NULL CHECK (size >= 0),
    sha256      text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    verified_at timestamptz,
    PRIMARY KEY (package_id, path)
);

-- Jobs (architecture.md §5). cancel_requested_at stops a job; purge_requested_at marks an admin purge or the old job
-- of a rerun, which runs cleanup only and ends cleaned/purged (§5.4).
CREATE TABLE jobs (
    job_id              uuid PRIMARY KEY, -- UUIDv7
    package_id          text NOT NULL REFERENCES packages (package_id),
    rerun_of            uuid REFERENCES jobs (job_id) ON DELETE SET NULL,
    state               text NOT NULL CHECK (state IN ('received', 'running', 'held', 'failed', 'delivered', 'cleaned')),
    held_reason         text CHECK (held_reason IN ('invalid_metadata', 'unknown_type', 'unknown_sink',
                            'no_registration', 'owner_unresolved', 'delivery_uncertain')),
    failure_reason      text CHECK (failure_reason IN ('unsupported_audio_bridge', 'events_mismatch',
                            'manifest_metadata_mismatch', 'retries_exhausted', 'all_tracks_failed', 'config_error',
                            'rejected_by_provider', 'rejected_by_sink', 'internal_error')),
    outcome             text CHECK (outcome IN ('delivered', 'cancelled', 'purged')),
    current_step        text CHECK (current_step IN ('validate', 'normalize', 'transcribe', 'assemble', 'summarize',
                            'render', 'deliver', 'cleanup')),
    detail              text, -- a code only, never personal data (architecture.md §4)
    meeting_type        text,
    sink_profile        text,
    registration_id     bigint REFERENCES registrations (registration_id) ON DELETE SET NULL,
    config_hash         text NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    delivered_at        timestamptz,
    cleaned_at          timestamptz,
    cancel_requested_at timestamptz,
    purge_requested_at  timestamptz
);

CREATE TABLE steps (
    job_id           uuid NOT NULL REFERENCES jobs (job_id) ON DELETE CASCADE,
    name             text NOT NULL CHECK (name IN ('validate', 'normalize', 'transcribe', 'assemble', 'summarize',
                         'render', 'deliver', 'cleanup')),
    state            text NOT NULL CHECK (state IN ('pending', 'claimed', 'succeeded', 'retrying', 'failed')),
    attempt          integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    run_after        timestamptz NOT NULL DEFAULT now(),
    owner            text,
    lease_expires_at timestamptz,
    last_error_class text,
    last_error_code  text,
    output           jsonb, -- blob keys and small non-personal facts
    PRIMARY KEY (job_id, name)
);

-- Per-track progress of transcribe (architecture.md §6.4). A failed track
-- becomes a gap with one of the README gap reasons.
CREATE TABLE step_items (
    job_id     uuid NOT NULL REFERENCES jobs (job_id) ON DELETE CASCADE,
    item       text NOT NULL, -- track reference trk-<n>
    state      text NOT NULL CHECK (state IN ('pending', 'succeeded', 'gap')),
    attempt    integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    gap_reason text CHECK (gap_reason IN ('asr_failed', 'asr_rejected_input', 'asr_invalid_response',
                   'track_unreadable')),
    output     jsonb,
    PRIMARY KEY (job_id, item)
);

CREATE TABLE deliveries (
    job_id       uuid PRIMARY KEY REFERENCES jobs (job_id) ON DELETE CASCADE,
    sink_profile text NOT NULL,
    state        text NOT NULL CHECK (state IN ('sending', 'sent', 'confirmed_by_admin')),
    document_id  text,
    sent_at      timestamptz
);

CREATE TABLE audit (
    audit_id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at         timestamptz NOT NULL DEFAULT now(),
    actor_kind text NOT NULL CHECK (actor_kind IN ('system', 'admin', 'client')),
    actor_id   text,
    action     text NOT NULL CHECK (action IN ('retry', 'release', 'rerun', 'purge', 'erase', 'ingest_commit',
                   'job_state')),
    job_id     uuid, -- no foreign key: audit rows outlive job rows
    detail     text  -- codes only
);

-- Claim query (architecture.md §6.2): runnable steps, and active jobs oldest first.
CREATE INDEX steps_runnable ON steps (run_after) WHERE state IN ('pending', 'retrying');
CREATE INDEX jobs_active ON jobs (created_at) WHERE state IN ('received', 'running');
-- Reaper: expired leases.
CREATE INDEX steps_lease ON steps (lease_expires_at) WHERE state = 'claimed';
-- Lookups: jobs of a package (rerun, ingest ack), jobs of a registration (held release), audit by job.
CREATE INDEX jobs_package_id ON jobs (package_id);
CREATE INDEX jobs_registration_id ON jobs (registration_id);
CREATE INDEX audit_job_id ON audit (job_id);
-- Cleanup loop: result records past their TTL.
CREATE INDEX registrations_result_expires_at ON registrations (result_expires_at) WHERE result_expires_at IS NOT NULL;

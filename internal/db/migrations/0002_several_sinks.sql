-- 0002: several sinks per job, webhook and Matrix sinks (ADR-016; architecture.md §6.1, sinks.md §1.3, §1.8,
-- metadata-interface.md §2.3).

-- One delivery record per sink of a job: the key becomes (job_id, sink_profile). target is the Matrix room ID an
-- event went to (sinks.md §1.3). skipped_by_admin is the release of a failed sink (sinks.md §1.8).
ALTER TABLE deliveries DROP CONSTRAINT deliveries_pkey;
ALTER TABLE deliveries ADD PRIMARY KEY (job_id, sink_profile);
ALTER TABLE deliveries ADD COLUMN target text;
ALTER TABLE deliveries DROP CONSTRAINT deliveries_state_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_state_check
    CHECK (state IN ('sending', 'sent', 'confirmed_by_admin', 'skipped_by_admin'));

-- jobs.sink_profile now holds the job's sink list in delivery order, comma-separated (validate). matrix_room is the
-- job's recap-matrix-room value when the list names a matrix profile (metadata-interface.md §3).
ALTER TABLE jobs ADD COLUMN matrix_room text;

-- The front end's link to the summary (registration request 1.1.0, metadata-interface.md §2.3).
ALTER TABLE registrations ADD COLUMN result_url text;

-- step_items holds the per-track progress of transcribe and the per-sink progress of deliver (architecture.md
-- §6.1). step tells them apart, so a sink profile named like a track reference cannot collide with one; the rows
-- that exist are transcribe's. A deliver item is a sink profile name; its row exists only while the sink is failed
-- (sinks.md §1.8), with the failure reason and, where the adapter gives one, an allow-listed code in detail.
ALTER TABLE step_items ADD COLUMN step text NOT NULL DEFAULT 'transcribe' CHECK (step IN ('transcribe', 'deliver'));
ALTER TABLE step_items DROP CONSTRAINT step_items_pkey;
ALTER TABLE step_items ADD PRIMARY KEY (job_id, step, item);
ALTER TABLE step_items DROP CONSTRAINT step_items_state_check;
ALTER TABLE step_items ADD CONSTRAINT step_items_state_check CHECK (state IN ('pending', 'succeeded', 'gap', 'failed'));
ALTER TABLE step_items ADD COLUMN failure_reason text
    CHECK (failure_reason IN ('config_error', 'rejected_by_sink', 'retries_exhausted'));
ALTER TABLE step_items ADD COLUMN detail text; -- a code only, never personal data

-- 0003: the describe step between assemble and summarize (ADR-017; architecture.md §5.2, §6.1).

ALTER TABLE jobs DROP CONSTRAINT jobs_current_step_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_current_step_check
    CHECK (current_step IN ('validate', 'normalize', 'transcribe', 'assemble', 'describe', 'summarize', 'render',
        'deliver', 'cleanup'));
ALTER TABLE steps DROP CONSTRAINT steps_name_check;
ALTER TABLE steps ADD CONSTRAINT steps_name_check
    CHECK (name IN ('validate', 'normalize', 'transcribe', 'assemble', 'describe', 'summarize', 'render', 'deliver',
        'cleanup'));

-- step_items holds the per-image progress of describe (item img-<64 hex>) next to the per-track progress of
-- transcribe (item trk-<n>); each step reads only its own items.

-- One describe row for every existing job, whatever its state: pending for a job that has not passed assemble, so it
-- runs the step like any other (a no-op unless its type switches slide images on); succeeded for a job at or past
-- summarize, and for every delivered or cleaned job. summarize treats a job without a descriptions file as one
-- without descriptions.
INSERT INTO steps (job_id, name, state)
SELECT job_id, 'describe',
       CASE WHEN state IN ('delivered', 'cleaned') OR current_step IN ('summarize', 'render', 'deliver', 'cleanup')
            THEN 'succeeded' ELSE 'pending' END
FROM jobs;

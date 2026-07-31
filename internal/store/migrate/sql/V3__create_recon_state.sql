-- Bookkeeping for the dataset-wide reconcile pass.
--
-- recon.Classify cannot judge a row in isolation: whether a payment is matched
-- depends on whether ANY ticket references it, and a late-arriving order flips a
-- previously-unmatched payment to matched. So the pass runs over everything, and
-- this table is what stops it running on every tick.
--
-- Single row, enforced by the primary key and CHECK rather than by convention.
CREATE TABLE recon_state (
    id                    boolean PRIMARY KEY DEFAULT true CHECK (id),

    -- When the last pass finished.
    last_run_at           timestamptz,

    -- The input watermark observed BEFORE that pass — max(updated_at) across
    -- reconcilable rows. The pass is skipped while this is unchanged, which is
    -- the steady state and costs one indexed aggregate per tick.
    last_input_updated_at timestamptz,

    last_run_id           uuid,

    -- Rows whose verdict actually moved. Zero means the pass was a no-op, which
    -- is the normal outcome once the data has settled.
    verdicts_changed      bigint NOT NULL DEFAULT 0
);

INSERT INTO recon_state (id) VALUES (true);

-- The watermark query: max(updated_at) over reconcilable rows.
CREATE INDEX events_updated_at_idx ON events (updated_at)
    WHERE resource_type <> 'other';

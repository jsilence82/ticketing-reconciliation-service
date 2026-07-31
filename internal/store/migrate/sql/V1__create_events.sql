-- The single persisted table. There is no projection/derived-table layer:
-- the canonical frame is assembled at read time (see internal/model.Assemble).

CREATE TABLE events (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- seq is the INGEST ORDINAL, and it is load-bearing rather than cosmetic.
    -- The reconciliation engine is order-sensitive: money.SumPandas is pairwise
    -- and money.SumPythonBuiltin is Neumaier-compensated, so the order values
    -- are summed in changes the result at ULP level, and tools/parity/compare.py
    -- compares matched_txn_ids as an ORDERED list.
    --
    -- (created_at, id) cannot recover the provider's array order: 1,026 of 2,562
    -- real tickets share a created_at, and 387 of 462 PayPal rows share a date
    -- because that field is only YYYY-MM-DD. Only an insertion ordinal works,
    -- which is why the reconcile read is ORDER BY seq.
    seq                     bigint GENERATED ALWAYS AS IDENTITY,

    source                  text NOT NULL
                            CHECK (source IN ('paypal','tickettailor')),

    -- The table holds four Ticket Tailor resource types plus PayPal
    -- transactions, not just orders. Without this column you cannot express
    -- "all issued tickets", and the unique constraint below would be relying on
    -- Ticket Tailor's id prefixes (or_/it_/ev_/es_) never colliding.
    resource_type           text NOT NULL
                            CHECK (resource_type IN ('order','issued_ticket','event',
                                                     'event_series','paypal_transaction',
                                                     'other')),

    -- The resource's OWN id, never the webhook envelope's notification id.
    -- Backfilled rows have no notification, so keying on that would stop the two
    -- ingestion paths recognising the same real-world resource.
    resource_id             text NOT NULL CHECK (resource_id <> ''),

    webhook_notification_id text,

    origin                  text NOT NULL CHECK (origin IN ('webhook','backfill')),
    topic                   text NOT NULL DEFAULT '',

    status                  text NOT NULL
                            CHECK (status IN ('received','processing','processed',
                                              'failed','dead_lettered','ignored')),

    payload                 jsonb NOT NULL,

    -- REQUIRED, not the optional extra CLAUDE.md describes. Ticket Tailor orders
    -- carry no updated_at (verified: 0 of 1,532), so a refund posted against an
    -- old order changes refund_amount while created_at — and therefore
    -- occurred_at — stays put. With both writes at origin='backfill' the version
    -- guard ties and the update would be discarded, defeating the full-resync
    -- policy. The hash is the only discriminator left. See the upsert in
    -- internal/store.
    payload_hash            bytea NOT NULL,

    -- The PROVIDER's timestamp. Never receive time: ordering on receive time is
    -- exactly what lets a late delivery overwrite fresher state.
    occurred_at             timestamptz NOT NULL,

    -- The service's product: the per-resource verdict.
    recon_status            text NOT NULL DEFAULT 'not_applicable'
                            CHECK (recon_status IN ('matched','unmatched','transferred',
                                                    'pending','not_applicable')),
    recon_counterpart_id    text NOT NULL DEFAULT '',
    -- Which reconcile pass last wrote the verdict; finds rows a pass missed.
    recon_run_id            uuid,

    retry_count             int NOT NULL DEFAULT 0,
    -- Backoff is unimplementable with retry_count alone: without a "not before"
    -- the worker re-claims a failing row on every poll tick and burns its whole
    -- retry budget in seconds.
    next_attempt_at         timestamptz NOT NULL DEFAULT now(),
    last_error              text NOT NULL DEFAULT '',

    -- Null for backfilled rows: a REST response has no signature to verify.
    signature_verified_at   timestamptz,

    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),

    UNIQUE (source, resource_id)
);

-- Keyset pagination for GET /events. OFFSET is not used: a consumer summing a
-- date range must see every row exactly once.
CREATE INDEX events_created_at_id_idx ON events (created_at, id);

-- Filtered list queries. The filter column leads so that
-- "WHERE source=$1 AND (created_at,id) > ($2,$3) ORDER BY created_at,id"
-- is a single range scan rather than a filter-then-sort.
CREATE INDEX events_source_ck_idx ON events (source, created_at, id);
CREATE INDEX events_status_ck_idx ON events (status, created_at, id);

-- Partial: over 90% of rows are not_applicable, and the query that matters is
-- ?recon_status=unmatched — the one satisfying "unmatched must be surfaced".
CREATE INDEX events_recon_ck_idx ON events (recon_status, created_at, id)
    WHERE recon_status <> 'not_applicable';

-- ?since= filters on provider time, not row-creation time.
CREATE INDEX events_occurred_at_idx ON events (occurred_at);

-- The worker claim.
CREATE INDEX events_claim_idx ON events (next_attempt_at, seq)
    WHERE status IN ('received','failed');

-- The reconcile read, which must come back in ingest order.
CREATE INDEX events_read_idx ON events (source, resource_type, seq);

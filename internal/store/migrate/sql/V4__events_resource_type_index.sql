-- Composite index for GET /events?resource_type=..., matching the same
-- (filter_col, created_at, id) shape as events_source_ck_idx and
-- events_status_ck_idx, so a resource_type filter combined with the
-- endpoint's keyset ordering can be served from one index instead of a
-- full scan.
CREATE INDEX events_resource_type_ck_idx ON events (resource_type, created_at, id);

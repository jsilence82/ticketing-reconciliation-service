-- A database-level canary for buyer PII.
--
-- internal/ingest sanitizes with an allowlist, and internal/store rescans at the
-- write boundary. This is the third layer, and the only one that holds no matter
-- what code does the writing — including a future migration, a manual INSERT, or
-- a restore from a mis-sanitized dump.
--
-- Limitation, stated plainly: `?|` tests TOP-LEVEL keys only. It cannot see an
-- email nested inside buyer_details. That nesting is covered by the Go scanner;
-- this constraint covers the case that matters most at top level, which is
-- exactly where `email` sits on a raw Ticket Tailor issued_ticket.
ALTER TABLE events
  ADD CONSTRAINT events_payload_no_pii
  CHECK (NOT (payload ?| ARRAY[
      'email','buyer_details','first_name','last_name','full_name',
      'phone','address','payer_info','custom_questions',
      'barcode','barcode_url','qr_code_url','group_ticket_barcode'
  ]));

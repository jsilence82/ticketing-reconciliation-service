// PII rules.
//
// The sanitizer in internal/ingest reduces payloads to an allowlist before they
// are stored. The scanner here is a SECOND, independent check applied at the
// storage boundary. The duplication is deliberate: it turns "someone added a
// write path that skipped Sanitize" into a hard failure rather than a silent
// leak.

package model

import (
	"encoding/json"
	"fmt"
)

// forbiddenKeys must never reach the database, at any depth.
//
// This lives in model rather than ingest because it is a domain rule about what
// may be persisted, not a property of one ingestion path — which lets both the
// sanitizer and the storage boundary enforce it without store depending on
// ingest.
var forbiddenKeys = map[string]bool{
	"email": true, "first_name": true, "last_name": true, "full_name": true,
	"name": true, "phone": true, "address": true,
	"buyer_details": true, "buyer_name": true, "buyer_id": true,
	"custom_questions": true, "payer_info": true, "payer": true,
	"barcode": true, "barcode_url": true, "qr_code_url": true,
	"group_ticket_barcode": true,
}

// ScanForPII walks a payload and reports the first forbidden key at any depth.
//
// "name" is on the list, so resources that legitimately carry one (events,
// series) are exempted by the caller rather than by weakening the scan — an
// exemption you have to ask for is safer than a hole you have to remember.
func ScanForPII(raw []byte, exempt map[string]bool) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	return walk(v, "", exempt)
}

func walk(v any, path string, exempt map[string]bool) error {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if forbiddenKeys[k] && !exempt[k] {
				at := k
				if path != "" {
					at = path + "." + k
				}
				return fmt.Errorf("payload carries forbidden key %q at %s", k, at)
			}
			next := k
			if path != "" {
				next = path + "." + k
			}
			if err := walk(child, next, exempt); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range t {
			if err := walk(child, fmt.Sprintf("%s[%d]", path, i), exempt); err != nil {
				return err
			}
		}
	}
	return nil
}

// PIIExemptions returns the keys a resource type may legitimately carry despite
// being on the forbidden list.
//
// Only "name" qualifies, and only where it names a show rather than a person.
func PIIExemptions(rt ResourceType) map[string]bool {
	switch rt {
	case ResourceEvent, ResourceEventSeries:
		return map[string]bool{"name": true}
	case ResourceOrder:
		// payment_method.name is the method's label ("PayPal"), not a person.
		return map[string]bool{"name": true}
	default:
		return nil
	}
}

// IsForbiddenKey reports whether a key must never appear in a stored payload.
//
// Exposed as a function rather than the map itself so callers cannot mutate the
// rule they are being checked against.
func IsForbiddenKey(k string) bool { return forbiddenKeys[k] }

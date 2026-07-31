package store

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Driver errors are classified HERE, not by callers.
//
// depguard confines pgx to this package, which means no handler can inspect a
// SQLSTATE code directly — and that is the point. An HTTP layer that knows
// "42501" is an HTTP layer that has grown a second opinion about the database.

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// ErrInvalidID is returned when an identifier is not a well-formed uuid. It is
// a client error, not a server fault.
var ErrInvalidID = errors.New("store: invalid id")

// ErrInvalidCursor is returned for a malformed pagination cursor.
var ErrInvalidCursor = errors.New("store: invalid cursor")

// Postgres SQLSTATE codes this package translates.
const (
	sqlStateInsufficientPrivilege     = "42501"
	sqlStateReadOnlySQLTransaction    = "25006"
	sqlStateInvalidTextRepresentation = "22P02"
)

// isPermissionDenied reports whether the database refused a write.
//
// Two codes qualify. 42501 is a role lacking INSERT. 25006 is a session or
// transaction set read-only, which is a legitimate alternative way to enforce
// the same restriction and one a hosted Postgres may use — accepting it avoids
// failing startup on a correctly-configured deployment.
func isPermissionDenied(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == sqlStateInsufficientPrivilege ||
		pgErr.Code == sqlStateReadOnlySQLTransaction
}

func isInvalidText(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlStateInvalidTextRepresentation {
		return true
	}
	// pgx can also reject the value before it reaches the server.
	return strings.Contains(err.Error(), "invalid UUID")
}

// translate maps a driver error onto this package's sentinels, so callers
// switch on meaning rather than on SQLSTATE.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case isInvalidText(err):
		return ErrInvalidID
	default:
		return err
	}
}

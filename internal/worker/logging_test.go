package worker_test

import (
	"io"
	"log/slog"
)

// discardLogger keeps test output readable; the worker logs each pass.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Package api serves the read-only query API.
//
// The service is the source of truth for reconciled event data, not a report
// generator. There is deliberately no /reconciliation/summary or
// /reconciliation/categories: Totals and Statistics are computed on demand by
// whoever needs them, from the raw events served here. See CLAUDE.md,
// Persistence & read model.
//
// # Read-only is structural, not conventional
//
// Guardrail 3 says no endpoint may mutate state. Rather than trusting that no
// handler ever writes, the server takes a store whose database role has SELECT
// only, and PROVES it at startup by attempting a write and requiring it to
// fail. A configuration that would let the API write is a boot failure, not a
// latent risk.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jsilence82/ticketing-reconciliation-service/internal/ingest"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/model"
	"github.com/jsilence82/ticketing-reconciliation-service/internal/store"
)

// Server holds the read-only handlers.
type Server struct {
	store     *store.Store
	consumers Consumers
	log       *slog.Logger
}

// New builds a server. The store MUST be backed by a read-only role; call
// VerifyReadOnly before serving.
func New(st *store.Store, consumers Consumers, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{store: st, consumers: consumers, log: log}
}

// Routes returns the mux. net/http with Go 1.22 method-and-path patterns; no
// framework, per CLAUDE.md.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Unauthenticated: the reverse proxy and uptime checks need it, and it
	// exposes counts rather than any buyer or payment data.
	mux.HandleFunc("GET /healthz", s.handleHealth)

	mux.HandleFunc("GET /events", s.authenticate(s.handleListEvents))
	mux.HandleFunc("GET /events/{id}", s.authenticate(s.handleGetEvent))

	return mux
}

// VerifyReadOnly proves the API cannot write, by trying to and requiring
// failure.
//
// This is the enforcement guardrail 3 asks for. Documentation saying "use a
// SELECT-only role" is not enforcement; a boot-time check that fails loudly when
// the role is over-privileged is. The write is attempted inside a transaction
// that is always rolled back, so even a misconfigured role leaves nothing
// behind.
func (s *Server) VerifyReadOnly(ctx context.Context) error {
	writable, err := s.store.ProbeWritable(ctx)
	if err != nil {
		return err
	}
	if writable {
		return errors.New(
			"the API's database role can INSERT into events. Guardrail 3 requires " +
				"a SELECT-only role — set API_DATABASE_URL to a read-only user " +
				"(see deploy/readonly-role.sql). Refusing to serve")
	}

	s.log.Info("read-only verified: the API role cannot write to events")
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.Health(r.Context())
	if err != nil {
		s.log.Error("health", "err", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	unresolved, err := s.unresolvedCounts(r.Context())
	if err != nil {
		s.log.Error("health: unresolved references", "err", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	writeJSON(w, http.StatusOK, healthResponse{
		Status:     "ok",
		Events:     counts.ByStatus,
		Recon:      counts.ByReconStatus,
		Unresolved: unresolved,
	})
}

// unresolvedCounts tallies tickets whose order or event has not been seen yet.
//
// CLAUDE.md, Persistence & read model: a missing parent reads as empty/zero at
// assemble time rather than erroring, which is correct for the matching rule
// but means an orphan silently changes a night's numbers unless something
// surfaces it — this is that something. Reuses the same LoadResources +
// DecodeResources path the reconcile pass uses, rather than a bespoke query,
// so the two can never disagree about what "unresolved" means.
func (s *Server) unresolvedCounts(ctx context.Context) (unresolvedDTO, error) {
	rows, err := s.store.LoadResources(ctx)
	if err != nil {
		return unresolvedDTO{}, fmt.Errorf("load resources: %w", err)
	}
	inputs, err := ingest.DecodeResources(rows)
	if err != nil {
		return unresolvedDTO{}, fmt.Errorf("decode resources: %w", err)
	}

	missingOrders, missingEvents := model.UnresolvedParents(inputs.Orders, inputs.Tickets, inputs.Events)
	return unresolvedDTO{MissingOrders: missingOrders, MissingEvents: missingEvents}, nil
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := store.ListFilter{
		Source:      q.Get("source"),
		Status:      q.Get("status"),
		ReconStatus: q.Get("recon_status"),
		Cursor:      q.Get("cursor"),
	}

	if v := q.Get("since"); v != "" {
		t, err := parseTime(v)
		if err != nil {
			writeError(w, http.StatusBadRequest,
				"since must be RFC3339 or YYYY-MM-DD")
			return
		}
		filter.Since = t
	}

	if v := q.Get("limit"); v != "" {
		n, err := parsePositiveInt(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		filter.Limit = n
	}

	page, err := s.store.ListEvents(r.Context(), filter)
	if err != nil {
		// A bad cursor is the caller's fault, not ours.
		if errors.Is(err, store.ErrInvalidCursor) {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		s.log.Error("list events", "err", err)
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}

	out := listResponse{Events: make([]eventDTO, 0, len(page.Events)), NextCursor: page.Next}
	for _, e := range page.Events {
		out.Events = append(out.Events, toDTO(e))
	}

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	rec, err := s.store.GetEvent(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such event")
			return
		}
		// An unparseable uuid is a client error, not a server fault.
		if errors.Is(err, store.ErrInvalidID) {
			writeError(w, http.StatusBadRequest, "id must be a uuid")
			return
		}
		s.log.Error("get event", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}

	writeJSON(w, http.StatusOK, toDTO(rec))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so there is nothing useful left to do
		// but avoid pretending it succeeded.
		_ = err
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// parseTime accepts both a full timestamp and a bare date, because a treasurer
// asking for "sales since 2026-01-01" should not have to write an RFC3339
// string.
func parseTime(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", v)
}

// parsePositiveInt rejects a limit that is absent, non-numeric or non-positive.
func parsePositiveInt(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, errors.New("must be positive")
	}
	return n, nil
}

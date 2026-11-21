package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"softpos/pkg/identity"
	"softpos/pkg/store"
)

// SAT 69-B list ingestion (MVP-20): the server loads the SAT CSV (worker in
// cmd/server, backoffice trigger, cmd/operatorctl load-69b), swaps it in
// atomically and re-checks approved merchants. See docs/KYB.md.

// SAT69BFetcher downloads the SAT file.
type SAT69BFetcher func(ctx context.Context) ([]byte, error)

// SetSAT69BSource configures the download used by the worker and the
// backoffice trigger (source is the URL, recorded with each load) and the
// shrink protection in percent (<= 0: identity.DefaultMax69BShrinkPct).
func (s *Server) SetSAT69BSource(source string, fetch SAT69BFetcher, maxShrinkPct int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sat69bSource, s.sat69bFetch, s.sat69bShrinkPct = source, fetch, maxShrinkPct
}

// Blacklist returns the SAT 69-B service.
func (s *Server) Blacklist() *identity.BlacklistService { return s.blacklist }

func (s *Server) sat69bStore() (identity.List69BStore, bool) {
	if s.blacklist == nil {
		return nil, false
	}
	st, ok := s.blacklist.Repository().(identity.List69BStore)
	return st, ok
}

func (s *Server) sat69bConfig() (string, SAT69BFetcher, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sat69bSource, s.sat69bFetch, s.sat69bShrinkPct
}

var errSAT69BNotConfigured = errors.New("SAT 69-B store not configured")

// errSAT69BFetch wraps download failures (502 in the backoffice).
type errSAT69BFetch struct{ err error }

func (e errSAT69BFetch) Error() string { return e.err.Error() }
func (e errSAT69BFetch) Unwrap() error { return e.err }

// LoadSAT69B loads raw as the SAT 69-B list (actor: system:69b-worker,
// operator id, operatorctl:<user>) and re-checks approved merchants when the
// list is current.
func (s *Server) LoadSAT69B(ctx context.Context, raw []byte, source, actor string, override bool) (identity.List69BResult, store.Retroscreen69BResult, error) {
	st, ok := s.sat69bStore()
	if !ok {
		return identity.List69BResult{}, store.Retroscreen69BResult{}, errSAT69BNotConfigured
	}
	_, _, pct := s.sat69bConfig()
	s.sat69bMu.Lock()
	defer s.sat69bMu.Unlock()
	syncer := &identity.List69BSyncer{Store: st, MaxShrinkPct: pct}
	res, err := syncer.Load(ctx, raw, source, actor, override)
	if err != nil {
		if errors.Is(err, identity.ErrList69BShrank) {
			slog.Error("ALERT SAT 69-B list shrank beyond the threshold; previous list kept (operator override required)",
				"rows", res.Load.RowCount, "previous", res.Load.PreviousCount, "source", source)
		}
		return res, store.Retroscreen69BResult{}, err
	}
	repo, _ := s.kybStore()
	retro, rerr := store.Retroscreen69B(ctx, repo, s.auditLog(), s.blacklist.Lookup, "system:69b")
	if rerr != nil {
		slog.Error("SAT 69-B retroscreen failed", "error", rerr)
	}
	return res, retro, nil
}

// SyncSAT69B downloads the configured source and loads it.
func (s *Server) SyncSAT69B(ctx context.Context, actor string, override bool) (identity.List69BResult, store.Retroscreen69BResult, error) {
	source, fetch, _ := s.sat69bConfig()
	if fetch == nil {
		return identity.List69BResult{}, store.Retroscreen69BResult{}, errSAT69BNotConfigured
	}
	raw, err := fetch(ctx)
	if err != nil {
		if st, ok := s.sat69bStore(); ok {
			meta, _, _ := st.List69BMeta(ctx)
			_ = st.RecordLoadFailure(ctx, identity.List69BLoad{Status: identity.Load69BFailed, Source: source,
				PreviousCount: meta.RowCount, Detail: err.Error(), Actor: actor, Override: override, CreatedAt: nowUTC()})
		}
		return identity.List69BResult{}, store.Retroscreen69BResult{}, errSAT69BFetch{err}
	}
	return s.LoadSAT69B(ctx, raw, source, actor, override)
}

// BOSAT69BLoad is one load attempt in the backoffice.
type BOSAT69BLoad struct {
	ID             int64  `json:"id"`
	Status         string `json:"status"`
	Version        string `json:"list_version"`
	Source         string `json:"source"`
	RowCount       int    `json:"row_count"`
	PreviousCount  int    `json:"previous_count"`
	MalformedCount int    `json:"malformed_count"`
	Override       bool   `json:"override_shrink"`
	Detail         string `json:"detail,omitempty"`
	Actor          string `json:"actor"`
	CreatedAt      string `json:"created_at"`
}

func boLoad(l identity.List69BLoad) BOSAT69BLoad {
	return BOSAT69BLoad{ID: l.ID, Status: l.Status, Version: l.Version, Source: l.Source, RowCount: l.RowCount,
		PreviousCount: l.PreviousCount, MalformedCount: l.MalformedCount, Override: l.Override, Detail: l.Detail,
		Actor: l.Actor, CreatedAt: l.CreatedAt.UTC().Format(time.RFC3339)}
}

// BOSAT69BStatus is GET /api/v1/backoffice/sat69b.
type BOSAT69BStatus struct {
	Loaded           bool           `json:"loaded"`
	Stale            bool           `json:"stale"`
	ListVersion      string         `json:"list_version,omitempty"`
	Source           string         `json:"source,omitempty"`
	Published        string         `json:"published,omitempty"`
	RowCount         int            `json:"row_count"`
	MalformedCount   int            `json:"malformed_count"`
	FetchedAt        string         `json:"fetched_at,omitempty"`
	VerifiedAt       string         `json:"verified_at,omitempty"`
	MaxAgeHours      int64          `json:"max_age_hours"`
	SourceConfigured bool           `json:"source_configured"`
	ConfiguredSource string         `json:"configured_source,omitempty"`
	Loads            []BOSAT69BLoad `json:"loads"`
}

func (s *Server) handleBOSAT69BStatus(w http.ResponseWriter, r *http.Request, _ string) {
	st, ok := s.sat69bStore()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "SAT69B_NOT_CONFIGURED")
		return
	}
	ctx := r.Context()
	meta, found, err := st.List69BMeta(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "SAT69B_UNAVAILABLE")
		return
	}
	loads, err := st.ListLoads(ctx, 20)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "SAT69B_UNAVAILABLE")
		return
	}
	source, fetch, _ := s.sat69bConfig()
	maxAge := s.blacklist.MaxAge()
	out := BOSAT69BStatus{Loaded: found, MaxAgeHours: int64(maxAge / time.Hour), SourceConfigured: fetch != nil,
		ConfiguredSource: source, Loads: make([]BOSAT69BLoad, 0, len(loads))}
	if found {
		out.Stale = nowUTC().Sub(meta.VerifiedAt) > maxAge
		out.ListVersion, out.Source, out.Published = meta.Version, meta.Source, meta.Published
		out.RowCount, out.MalformedCount = meta.RowCount, meta.MalformedCount
		out.FetchedAt, out.VerifiedAt = meta.FetchedAt.Format(time.RFC3339), meta.VerifiedAt.Format(time.RFC3339)
	}
	for _, l := range loads {
		out.Loads = append(out.Loads, boLoad(l))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleBOSAT69BSync(w http.ResponseWriter, r *http.Request, op string) {
	var body struct {
		OverrideShrink bool   `json:"override_shrink"`
		Reason         string `json:"reason"`
	}
	if !decodeBOBody(w, r, &body) {
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if body.OverrideShrink && (body.Reason == "" || len(body.Reason) > 1000) {
		writeError(w, http.StatusBadRequest, "REASON_REQUIRED")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	res, retro, err := s.SyncSAT69B(ctx, op, body.OverrideShrink)
	detail := identity.FormatLoad(res.Load)
	if err != nil {
		detail = err.Error()
	}
	if body.Reason != "" {
		detail += " | reason: " + body.Reason
	}
	_ = s.audit(r.Context(), op, "sat69b.sync", "", res.Load.Version, nil, map[string]any{
		"status": res.Load.Status, "rows": res.Load.RowCount, "previous": res.Load.PreviousCount,
		"override_shrink": body.OverrideShrink, "blocked_merchants": retro.Blocked}, detail)
	var fetchErr errSAT69BFetch
	switch {
	case errors.Is(err, errSAT69BNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "SAT69B_SOURCE_NOT_CONFIGURED")
		return
	case errors.As(err, &fetchErr):
		writeError(w, http.StatusBadGateway, "SAT69B_FETCH_FAILED")
		return
	case errors.Is(err, identity.ErrList69BShrank):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "LIST_SHRANK", "load": boLoad(res.Load)})
		return
	case errors.Is(err, identity.ErrList69BNoHeader), errors.Is(err, identity.ErrList69BEmpty):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "SAT69B_INVALID_FILE", "load": boLoad(res.Load)})
		return
	case err != nil:
		slog.Error("SAT 69-B sync failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "SAT69B_UNAVAILABLE")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"load": boLoad(res.Load), "malformed": res.Malformed,
		"published": res.Published, "retroscreen": map[string]any{"checked": retro.Checked, "blocked": retro.Blocked, "unknown": retro.Unknown}})
}

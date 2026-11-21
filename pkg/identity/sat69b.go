package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// SAT Article 69-B list ingestion (MVP-20). The SAT publishes the complete
// list as one CSV (Listado_Completo_69-B.csv): Latin-1 / Windows-1252, a few
// free-text banner rows, then a header row ("No,RFC,Nombre del
// Contribuyente,Situación del contribuyente,…") and one row per taxpayer.
// A load replaces the whole list atomically (List69BStore.ReplaceList).

var (
	// ErrList69BEmpty: the file had no valid row (wrong file, SAT outage page…).
	ErrList69BEmpty = errors.New("SAT 69-B file has no valid rows")
	// ErrList69BNoHeader: no "RFC" / "Situación" header row was found.
	ErrList69BNoHeader = errors.New("SAT 69-B file has no RFC/Situación header row")
	// ErrList69BShrank: the new list is smaller than the loaded one by more
	// than the allowed percentage (needs an explicit operator override).
	ErrList69BShrank = errors.New("SAT 69-B list shrank beyond the allowed threshold")
	// ErrList69BChanged: too many RFCs present in both lists changed their
	// situation (same size, different content: wrong file, column shift…);
	// needs an explicit operator override.
	ErrList69BChanged = errors.New("SAT 69-B list situations changed beyond the allowed threshold")
	// ErrBlacklistStale: the loaded list was not verified within the max age.
	ErrBlacklistStale = errors.New("SAT 69-B list is stale")
)

// List69BRow is one parsed row of the SAT file.
type List69BRow struct {
	RFC    string
	Name   string
	Status BlacklistStatus
	Oficio string // first "500-…" oficio number of the row, if any
}

// List69BParse is the result of parsing a SAT 69-B file.
type List69BParse struct {
	Rows       []List69BRow // de-duplicated by RFC (most severe situation wins)
	Malformed  int          // data rows skipped (bad RFC, unknown status, short row, CSV error)
	Duplicates int          // RFCs repeated in the file
	// MalformedLines holds up to 20 line numbers of skipped rows (for logs).
	MalformedLines []int
	// Published is the "Información actualizada al …" text, when present.
	Published string
	SHA256    string // of the raw file bytes
}

var (
	rfc69BRe       = regexp.MustCompile(`^[A-ZÑ&]{3,4}[0-9]{6}[A-Z0-9]{3}$`)
	publishedRe    = regexp.MustCompile(`(?i)actualizad[ao]s? al ([0-9]{1,2} de [a-záéíóúñ]+ de [0-9]{4})`)
	maxMalformedLn = 20
)

// severity69B ranks situations for duplicated RFCs.
var severity69B = map[BlacklistStatus]int{
	BlacklistDesvirtuado: 1, BlacklistSentenciaFavorable: 2, BlacklistPresunto: 3, BlacklistDefinitivo: 4,
}

// decode69B returns the file as UTF-8: UTF-8 input (with or without BOM) is
// kept, anything else is decoded as Windows-1252 (a superset of the Latin-1
// the SAT uses).
func decode69B(raw []byte) string {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if utf8.Valid(raw) {
		return string(raw)
	}
	if out, err := charmap.Windows1252.NewDecoder().Bytes(raw); err == nil {
		return string(out)
	}
	out, _ := charmap.ISO8859_1.NewDecoder().Bytes(raw) // every byte is valid Latin-1
	return string(out)
}

func normalizeHeader(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	return strings.NewReplacer("Á", "A", "É", "E", "Í", "I", "Ó", "O", "Ú", "U").Replace(s)
}

// Parse69BCSV parses the SAT 69-B CSV. Rows before the header are ignored;
// data rows that cannot be used are counted (and logged), never fatal. It
// fails only when there is no header or no valid row at all.
func Parse69BCSV(raw []byte) (List69BParse, error) {
	sum := sha256.Sum256(raw)
	res := List69BParse{SHA256: hex.EncodeToString(sum[:])}
	text := decode69B(raw)
	if m := publishedRe.FindStringSubmatch(text[:min(len(text), 4096)]); m != nil {
		res.Published = strings.ToLower(m[1])
	}

	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.ReuseRecord = true

	rfcCol, statusCol, nameCol := -1, -1, -1
	seen := map[string]int{} // RFC -> index in res.Rows
	malformed := func(line int) {
		res.Malformed++
		if len(res.MalformedLines) < maxMalformedLn {
			res.MalformedLines = append(res.MalformedLines, line)
		}
	}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line, _ := r.FieldPos(0)
		if err != nil {
			if rfcCol >= 0 {
				malformed(line)
			}
			continue
		}
		if rfcCol < 0 {
			for i, f := range rec {
				switch h := normalizeHeader(f); {
				case h == "RFC":
					rfcCol = i
				case strings.HasPrefix(h, "SITUACION"):
					statusCol = i
				case strings.HasPrefix(h, "NOMBRE"):
					nameCol = i
				}
			}
			if rfcCol < 0 || statusCol < 0 {
				rfcCol, statusCol, nameCol = -1, -1, -1
			}
			continue
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue // blank line
		}
		if len(rec) <= max(rfcCol, statusCol) {
			malformed(line)
			continue
		}
		rfc := strings.ToUpper(strings.TrimSpace(rec[rfcCol]))
		status := normalizeStatus(rec[statusCol])
		if !rfc69BRe.MatchString(rfc) || status == "" {
			malformed(line)
			continue
		}
		row := List69BRow{RFC: rfc, Status: status}
		if nameCol >= 0 && nameCol < len(rec) {
			row.Name = truncateRunes(strings.TrimSpace(rec[nameCol]), 255)
		}
		for _, f := range rec {
			if f = strings.TrimSpace(f); strings.HasPrefix(f, "500-") {
				row.Oficio = truncateRunes(strings.Fields(f)[0], 128)
				break
			}
		}
		if i, dup := seen[rfc]; dup {
			// An RFC listed twice (re-listed under another oficio): keep the
			// most severe situation, so a duplicate never clears a listing.
			res.Duplicates++
			if severity69B[row.Status] >= severity69B[res.Rows[i].Status] {
				res.Rows[i] = row
			}
			continue
		}
		seen[rfc] = len(res.Rows)
		res.Rows = append(res.Rows, row)
	}
	if rfcCol < 0 {
		return res, ErrList69BNoHeader
	}
	if len(res.Rows) == 0 {
		return res, ErrList69BEmpty
	}
	if res.Malformed > 0 {
		slog.Warn("SAT 69-B rows skipped", "malformed", res.Malformed, "lines", res.MalformedLines)
	}
	return res, nil
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// List69BMeta describes the loaded list (sat_blacklist_meta).
type List69BMeta struct {
	Version        string // SHA-256 of the loaded file
	Source         string // URL or file name
	Published      string // "Información actualizada al …"
	RowCount       int
	MalformedCount int
	FetchedAt      time.Time // when this version was loaded
	VerifiedAt     time.Time // last time the source was checked and matched (staleness clock)
	LoadedBy       string
}

// List69BLoad is one load attempt (sat_blacklist_loads history).
type List69BLoad struct {
	ID             int64
	Status         string // LOADED | UNCHANGED | REJECTED_SHRINK | FAILED
	Version        string
	Source         string
	RowCount       int
	PreviousCount  int
	MalformedCount int
	Detail         string
	Actor          string
	Override       bool // shrink protection lifted
	// OverrideChanges: change protection lifted.
	OverrideChanges bool
	CreatedAt       time.Time
}

// Load statuses.
const (
	Load69BLoaded         = "LOADED"
	Load69BUnchanged      = "UNCHANGED"
	Load69BRejectedShrink = "REJECTED_SHRINK"
	// Load69BRejectedChanges: refused by the change protection (MVP-27).
	Load69BRejectedChanges = "REJECTED_CHANGES"
	Load69BFailed          = "FAILED"
)

// ReplaceGuard is the shrink and change protection evaluated inside the
// store's lock.
type ReplaceGuard struct {
	MaxShrinkPct int  // refuse when the new count < old count * (100-pct)/100
	Override     bool // operator override of the shrink check
	// MaxChangePct refuses a load in which more than pct% of the RFCs present
	// in both the loaded and the new list changed their situation (<= 0 or
	// >= 100 disables the check).
	MaxChangePct    int
	OverrideChanges bool // operator override of the change check
}

// AllowsChanges reports whether changed of the common RFCs (present in both
// lists) may change their situation in one load.
func (g ReplaceGuard) AllowsChanges(common, changed int) bool {
	if g.OverrideChanges || common <= 0 || g.MaxChangePct <= 0 || g.MaxChangePct >= 100 {
		return true
	}
	return int64(changed)*100 <= int64(common)*int64(g.MaxChangePct)
}

// ChangedSituations counts the RFCs present in both old and rows and how
// many of them have a different situation.
func ChangedSituations(old map[string]BlacklistStatus, rows []List69BRow) (common, changed int) {
	for _, r := range rows {
		if prev, ok := old[r.RFC]; ok {
			common++
			if prev != r.Status {
				changed++
			}
		}
	}
	return common, changed
}

// Allows reports whether replacing a list of oldCount rows with newCount is allowed.
func (g ReplaceGuard) Allows(oldCount, newCount int) bool {
	if g.Override || oldCount <= 0 || g.MaxShrinkPct >= 100 {
		return true
	}
	pct := max(g.MaxShrinkPct, 0)
	return int64(newCount)*100 >= int64(oldCount)*int64(100-pct)
}

// List69BStore persists the list with atomic replacement. Readers never see
// a partially loaded list.
type List69BStore interface {
	// ReplaceList swaps the whole list for rows and records meta and a
	// LOADED history row in one transaction. It returns ErrList69BShrank
	// (recording a REJECTED_SHRINK row) or ErrList69BChanged (recording a
	// REJECTED_CHANGES row) when guard refuses the swap.
	ReplaceList(ctx context.Context, rows []List69BRow, meta List69BMeta, guard ReplaceGuard, actor string) (List69BLoad, error)
	// List69BMeta returns the loaded list's metadata (found=false: never loaded).
	List69BMeta(ctx context.Context) (List69BMeta, bool, error)
	// MarkVerified records that the source still serves version (UNCHANGED row).
	MarkVerified(ctx context.Context, version string, at time.Time, actor string) error
	// RecordLoadFailure records a FAILED attempt.
	RecordLoadFailure(ctx context.Context, load List69BLoad) error
	// ListLoads returns the most recent attempts, newest first.
	ListLoads(ctx context.Context, limit int) ([]List69BLoad, error)
}

// List69BSyncer loads SAT files into a List69BStore.
type List69BSyncer struct {
	Store        List69BStore
	MaxShrinkPct int // default 10 (see DefaultMax69BShrinkPct)
	// MaxChangePct is the change protection (0: DefaultMax69BChangePct;
	// negative disables it).
	MaxChangePct int
	Now          func() time.Time
}

// DefaultMax69BShrinkPct is the default shrink protection.
const DefaultMax69BShrinkPct = 10

// DefaultMax69BChangePct is the default change protection: at most 10% of
// the RFCs present in both lists may change situation in one load.
const DefaultMax69BChangePct = 10

// Load69BOptions are the operator overrides of a load (audited by the caller).
type Load69BOptions struct {
	OverrideShrink  bool // lift the shrink protection
	OverrideChanges bool // lift the change protection
}

// Any reports whether some protection is lifted.
func (o Load69BOptions) Any() bool { return o.OverrideShrink || o.OverrideChanges }

// List69BResult reports a load.
type List69BResult struct {
	Load      List69BLoad
	Malformed int
	Published string
}

func (s *List69BSyncer) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Load parses raw and, unless it is the version already loaded (then only
// the verification time moves), replaces the list. override lifts the shrink
// protection (operator decision, audited by the caller).
func (s *List69BSyncer) Load(ctx context.Context, raw []byte, source, actor string, override bool) (List69BResult, error) {
	return s.LoadWithOptions(ctx, raw, source, actor, Load69BOptions{OverrideShrink: override})
}

// LoadWithOptions is Load with independent overrides of the shrink and the
// change protection.
func (s *List69BSyncer) LoadWithOptions(ctx context.Context, raw []byte, source, actor string, opts Load69BOptions) (List69BResult, error) {
	override := opts.Any()
	if s == nil || s.Store == nil {
		return List69BResult{}, errors.New("69-B store not configured")
	}
	now := s.now()
	sum := sha256.Sum256(raw)
	version := hex.EncodeToString(sum[:])
	meta, found, err := s.Store.List69BMeta(ctx)
	if err != nil {
		return List69BResult{}, err
	}
	if found && meta.Version == version && !override {
		if err := s.Store.MarkVerified(ctx, version, now, actor); err != nil {
			return List69BResult{}, err
		}
		return List69BResult{Load: List69BLoad{Status: Load69BUnchanged, Version: version, Source: source,
			RowCount: meta.RowCount, PreviousCount: meta.RowCount, Actor: actor, CreatedAt: now}}, nil
	}
	parsed, perr := Parse69BCSV(raw)
	if perr != nil {
		fail := List69BLoad{Status: Load69BFailed, Version: version, Source: source, PreviousCount: meta.RowCount,
			MalformedCount: parsed.Malformed, Detail: perr.Error(), Actor: actor, Override: opts.OverrideShrink,
			OverrideChanges: opts.OverrideChanges, CreatedAt: now}
		if err := s.Store.RecordLoadFailure(ctx, fail); err != nil {
			slog.Error("SAT 69-B failure not recorded", "error", err)
		}
		return List69BResult{Load: fail, Malformed: parsed.Malformed}, perr
	}
	pct := s.MaxShrinkPct
	if pct == 0 {
		pct = DefaultMax69BShrinkPct
	}
	newMeta := List69BMeta{Version: version, Source: source, Published: parsed.Published, RowCount: len(parsed.Rows),
		MalformedCount: parsed.Malformed, FetchedAt: now, VerifiedAt: now, LoadedBy: actor}
	changePct := s.MaxChangePct
	if changePct == 0 {
		changePct = DefaultMax69BChangePct
	}
	load, err := s.Store.ReplaceList(ctx, parsed.Rows, newMeta, ReplaceGuard{MaxShrinkPct: pct, Override: opts.OverrideShrink,
		MaxChangePct: changePct, OverrideChanges: opts.OverrideChanges}, actor)
	res := List69BResult{Load: load, Malformed: parsed.Malformed, Published: parsed.Published}
	if err != nil {
		return res, err
	}
	slog.Info("SAT 69-B list loaded", "rows", load.RowCount, "previous", load.PreviousCount, "malformed", parsed.Malformed,
		"duplicates", parsed.Duplicates, "published", parsed.Published, "version", version[:12], "actor", actor, "override_shrink", opts.OverrideShrink, "override_changes", opts.OverrideChanges)
	return res, nil
}

// FormatLoad is a one-line description of a load (audit detail / CLI output).
func FormatLoad(l List69BLoad) string {
	return fmt.Sprintf("%s rows=%d previous=%d malformed=%d version=%.12s %s", l.Status, l.RowCount, l.PreviousCount, l.MalformedCount, l.Version, l.Detail)
}

// MaxList69BBytes bounds a downloaded SAT file (the real one is ~5 MB).
const MaxList69BBytes = 64 << 20

// HTTP69BFetcher returns a fetcher that downloads the SAT 69-B CSV from url.
func HTTP69BFetcher(url string, client *http.Client) func(ctx context.Context) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	return func(ctx context.Context) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "kerno-69b-sync/1")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch SAT 69-B list: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch SAT 69-B list: HTTP %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, MaxList69BBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read SAT 69-B list: %w", err)
		}
		if len(body) > MaxList69BBytes {
			return nil, fmt.Errorf("SAT 69-B list larger than %d bytes", MaxList69BBytes)
		}
		return body, nil
	}
}

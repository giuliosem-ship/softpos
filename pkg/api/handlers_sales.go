package api

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"softpos/pkg/store"
)

// Sales listing (MVP-19): card payments, SPEI abonos and CoDi confirmations
// of a merchant, newest first, with an opaque (created_at, id) cursor.

const (
	defaultSalesPage = 50
	maxSalesPage     = 100
)

// SaleItem is one entry of GET /api/v1/merchants/{id}/sales.
type SaleItem struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"` // card | spei | codi
	Status        string `json:"status"`
	AmountCents   int64  `json:"amount_cents"`
	CreatedAt     string `json:"created_at"`
	CardPaymentID string `json:"card_payment_id,omitempty"`
}

// SalesPage is the response of the sales listing.
type SalesPage struct {
	Items      []SaleItem `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

// encodeSalesCursor / decodeSalesCursor: base64url("<unix nanos>|<id>").
func encodeSalesCursor(r store.SaleRecord) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(r.CreatedAt.UnixNano(), 10) + "|" + r.ID))
}

func decodeSalesCursor(s string) (*store.SalesCursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" || len(id) > 200 {
		return nil, false
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || n <= 0 {
		return nil, false
	}
	return &store.SalesCursor{CreatedAt: time.Unix(0, n).UTC(), ID: id}, true
}

// parseSalesPage reads limit (1..max) and before from the query string.
func parseSalesPage(w http.ResponseWriter, r *http.Request, max int) (int, *store.SalesCursor, bool) {
	limit := defaultSalesPage
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > max {
			writeError(w, http.StatusBadRequest, "INVALID_LIMIT")
			return 0, nil, false
		}
		limit = n
	}
	var cursor *store.SalesCursor
	if v := r.URL.Query().Get("before"); v != "" {
		c, ok := decodeSalesCursor(v)
		if !ok {
			writeError(w, http.StatusBadRequest, "INVALID_CURSOR")
			return 0, nil, false
		}
		cursor = c
	}
	return limit, cursor, true
}

func (s *Server) salesRepo() (store.SalesRepository, bool) {
	repo, ok := s.moneyStore().(store.SalesRepository)
	return repo, ok
}

// listSalesPage runs q with one extra row to know whether a next page exists.
func (s *Server) listSalesPage(r *http.Request, q store.SalesQuery) ([]store.SaleRecord, *string, error) {
	repo, ok := s.salesRepo()
	if !ok {
		return nil, nil, errSalesUnavailable
	}
	limit := q.Limit
	q.Limit = limit + 1
	recs, err := repo.ListSales(r.Context(), q)
	if err != nil {
		return nil, nil, err
	}
	var next *string
	if len(recs) > limit {
		recs = recs[:limit]
		c := encodeSalesCursor(recs[len(recs)-1])
		next = &c
	}
	return recs, next, nil
}

type salesError string

func (e salesError) Error() string { return string(e) }

const errSalesUnavailable = salesError("sales repository not configured")

func saleItem(rec store.SaleRecord) SaleItem {
	return SaleItem{ID: rec.ID, Kind: rec.Kind, Status: rec.Status, AmountCents: rec.AmountCents,
		CreatedAt: rec.CreatedAt.UTC().Format(time.RFC3339Nano), CardPaymentID: rec.CardPaymentID}
}

// HandleListSales: GET /api/v1/merchants/{id}/sales?limit=50&before=<cursor>
// (any member of the tenant; another merchant's id -> 403).
func (s *Server) HandleListSales(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	limit, cursor, ok := parseSalesPage(w, r, maxSalesPage)
	if !ok {
		return
	}
	recs, next, err := s.listSalesPage(r, store.SalesQuery{MerchantID: tenant, Before: cursor, Limit: limit})
	if err != nil {
		slog.Error("sales listing failed", "merchant_id", tenant, "error", err)
		writeError(w, http.StatusServiceUnavailable, "SALES_UNAVAILABLE")
		return
	}
	page := SalesPage{Items: make([]SaleItem, 0, len(recs)), NextCursor: next}
	for _, rec := range recs {
		page.Items = append(page.Items, saleItem(rec))
	}
	writeJSON(w, http.StatusOK, page)
}

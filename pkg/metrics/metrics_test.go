package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRegistry_TextExposition(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("kerno_test_total", "Test counter.", "route", "status")
	g := r.NewGauge("kerno_test_gauge", "Test gauge.")
	h := r.NewHistogram("kerno_test_seconds", "Test histogram.", []float64{0.1, 1}, "route")
	c.Inc("/a/{id}", "200")
	c.Add(2, "/a/{id}", "200")
	c.Inc(`we"ird`, "500")
	c.Add(-5, "/a/{id}", "200") // ignored
	g.Set(7.5)
	h.Observe(0.05, "/a")
	h.Observe(0.5, "/a")
	h.Observe(3, "/a")
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# TYPE kerno_test_total counter\n",
		`kerno_test_total{route="/a/{id}",status="200"} 3` + "\n",
		`kerno_test_total{route="we\"ird",status="500"} 1` + "\n",
		"# TYPE kerno_test_gauge gauge\nkerno_test_gauge 7.5\n",
		`kerno_test_seconds_bucket{route="/a",le="0.1"} 1` + "\n",
		`kerno_test_seconds_bucket{route="/a",le="1"} 2` + "\n",
		`kerno_test_seconds_bucket{route="/a",le="+Inf"} 3` + "\n",
		`kerno_test_seconds_sum{route="/a"} 3.55` + "\n",
		`kerno_test_seconds_count{route="/a"} 3` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if c.Value("/a/{id}", "200") != 3 || h.Count("/a") != 3 || g.Value() != 7.5 {
		t.Fatal("values")
	}
	// Same name and labels: same family.
	if r.NewCounter("kerno_test_total", "x", "route", "status").Value("/a/{id}", "200") != 3 {
		t.Fatal("re-registration must return the existing family")
	}
}

func TestRegistry_CardinalityCapAndConcurrency(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("kerno_cap_total", "cap", "k")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				c.Inc(strings.Repeat("x", i) + string(rune('a'+j%26)) + string(rune(j)))
			}
		}(i)
	}
	wg.Wait()
	c.f.mu.Lock()
	n := len(c.f.series)
	c.f.mu.Unlock()
	if n > maxSeries {
		t.Fatalf("series %d > cap %d", n, maxSeries)
	}
}

func TestHandler_Token(t *testing.T) {
	r := NewRegistry()
	r.NewGauge("kerno_up", "up").Set(1)
	h := Handler(r, "s3cret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer s3cret-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "kerno_up 1") || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("with token: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	Handler(r, "").ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}

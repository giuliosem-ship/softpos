package api

import (
	"net/http"
	"os"
	"path/filepath"
)

// backofficeCSP locks the backoffice UI to its own static files: no inline
// script or style, no third-party origins, no framing.
const backofficeCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob: data:; " +
	"connect-src 'self'; frame-src blob:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// SetBackofficeDir sets the directory of the static backoffice UI
// (default web/backoffice next to the POS directory).
func (s *Server) SetBackofficeDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backofficeDir = dir
}

func (s *Server) backofficeUIDir() string {
	s.mu.RLock()
	dir, pos := s.backofficeDir, s.staticDir
	s.mu.RUnlock()
	if dir != "" {
		return dir
	}
	candidates := []string{"web/backoffice", "../../web/backoffice"}
	if pos != "" {
		candidates = append([]string{filepath.Join(filepath.Dir(filepath.Clean(pos)), "backoffice")}, candidates...)
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return "web/backoffice"
}

// serveBackofficeUI: GET /backoffice/* (public static files with strict
// security headers; every API call the UI makes needs a platform-operator
// token).
func (s *Server) serveBackofficeUI(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", backofficeCSP)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	http.StripPrefix("/backoffice/", http.FileServer(http.Dir(s.backofficeUIDir()))).ServeHTTP(w, r)
}

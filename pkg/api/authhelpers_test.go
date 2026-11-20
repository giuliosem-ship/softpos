package api

import (
	"net/http"
	"testing"

	"softpos/pkg/auth"
)

// asTenant signs req with an owner access token scoped to merchantID.
func asTenant(t *testing.T, s *Server, merchantID string, req *http.Request) *http.Request {
	t.Helper()
	return asRole(t, s, merchantID, auth.RoleOwner, req)
}

func asRole(t *testing.T, s *Server, merchantID string, role auth.Role, req *http.Request) *http.Request {
	t.Helper()
	tok, err := s.AuthService().MintAccessToken("usr_test_"+merchantID, merchantID, role)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	return withBearer(req, tok)
}

func withBearer(req *http.Request, token string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

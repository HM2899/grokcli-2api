package admin

import (
	"net/http"
	"strings"
)

const AdminCookie = "g2a_admin"

type SessionVerifier interface {
	VerifyAdminSession(token string) bool
}

func RequireSession(r *http.Request, verifier SessionVerifier) (string, bool) {
	token := ExtractSession(r)
	if token == "" || verifier == nil || !verifier.VerifyAdminSession(token) {
		return "", false
	}
	return token, true
}

func ExtractSession(r *http.Request) string {
	if r == nil {
		return ""
	}
	if token := strings.TrimSpace(r.Header.Get("X-Admin-Token")); token != "" {
		return token
	}
	if authorization := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		return strings.TrimSpace(authorization[7:])
	}
	cookie, err := r.Cookie(AdminCookie)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cookie.Value)
}

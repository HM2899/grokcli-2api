package auth

import (
	"net/http/httptest"
	"testing"

	"github.com/hm2899/grokcli-2api/internal/config"
)

func TestTokenFromRequestPrecedence(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer preferred")
	req.Header.Set("x-api-key", "fallback")
	if got := tokenFromRequest(req); got != "preferred" {
		t.Fatalf("token = %q", got)
	}
}

func TestLegacyKeyVerification(t *testing.T) {
	verifier := NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil)
	rec, err := verifier.Verify(t.Context(), "secret")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.ID != "env" || rec.Prefix != "secret" {
		t.Fatalf("unexpected record %#v", rec)
	}
	if rec, err := verifier.Verify(t.Context(), "wrong"); err != nil || rec != nil {
		t.Fatalf("expected wrong key miss, rec=%#v err=%v", rec, err)
	}
}

func TestAuthRequiredModes(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want bool
	}{
		{"true", true},
		{"1", true},
		{"off", false},
		{"false", false},
	} {
		verifier := NewAPIKeyVerifier(config.Config{RequireAPIKey: tc.mode}, nil)
		got, err := verifier.AuthRequired(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("mode %s got %v want %v", tc.mode, got, tc.want)
		}
	}
}

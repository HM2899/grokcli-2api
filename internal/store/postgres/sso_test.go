package postgres

import (
	"testing"

	"github.com/hm2899/grokcli-2api/internal/accounts"
)

func TestHasSSONestedAndCookieHeader(t *testing.T) {
	cases := []map[string]any{
		{"sso": "abc123"},
		{"sso_cookie": "sso=xyz"},
		{"session_cookies": map[string]any{"sso": "nested"}},
		{"cookies": map[string]any{"sso-rw": "rwval"}},
		{"cookie": "a=1; sso=fromheader; b=2"},
		{"set_cookie": "sso=setcookieval; Path=/"},
	}
	for i, c := range cases {
		if !hasSSO(c) {
			t.Fatalf("case %d should have sso: %#v get=%q", i, c, accounts.GetSSOValue(c))
		}
	}
	if hasSSO(map[string]any{"email": "a@b.c"}) {
		t.Fatal("no sso should be false")
	}
}

func TestBuildAccountListWhereHasSSOUsesNestedPaths(t *testing.T) {
	trueVal := true
	where, _ := buildAccountListWhere("", "", &trueVal)
	if where == "" || !containsFold(where, "session_cookies") || !containsFold(where, "sso=%") {
		t.Fatalf("where missing nested sso paths: %s", where)
	}
}

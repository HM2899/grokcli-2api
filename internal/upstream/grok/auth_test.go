package grok

import "testing"

func TestAccountFromCredentials(t *testing.T) {
	acct := AccountFromCredentials(Credentials{Token: " tok ", UserID: "user", Email: "e@example.com"})
	if acct.ID != "user" || acct.Token != "tok" {
		t.Fatalf("unexpected account %#v", acct)
	}
}

func TestHeadersForCredentials(t *testing.T) {
	headers := HeadersForCredentials(Credentials{Token: "tok"}, "grok-4.5", Client{})
	if headers["Authorization"] != "Bearer tok" {
		t.Fatalf("unexpected authorization %q", headers["Authorization"])
	}
	if headers["X-XAI-Token-Auth"] != "xai-grok-cli" || headers["x-grok-model-override"] != "grok-4.5" {
		t.Fatalf("unexpected headers %#v", headers)
	}
}

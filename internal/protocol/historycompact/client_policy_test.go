package historycompact

import (
	"testing"
	"time"
)

func TestIsOpenAINativeClient(t *testing.T) {
	if IsOpenAINativeClient("") {
		t.Fatal("empty UA must be conservative")
	}
	if IsOpenAINativeClient("claude-cli/1.0") {
		t.Fatal("claude-cli is not native")
	}
	if IsOpenAINativeClient("anthropic-sdk") {
		t.Fatal("anthropic is not native")
	}
	if !IsOpenAINativeClient("codex-cli/0.1") {
		t.Fatal("codex should be native")
	}
	if !IsOpenAINativeClient("openai-python/1.0") {
		t.Fatal("openai-python should be native")
	}
}

func TestResolveOutboundMaxTools(t *testing.T) {
	if got := ResolveOutboundMaxTools("openai", "anything", 1, 0, 0); got != 0 {
		t.Fatalf("chat max=%d", got)
	}
	if got := ResolveOutboundMaxTools("openai_responses", "codex", 1, 0, 0); got != 0 {
		t.Fatalf("native responses max=%d", got)
	}
	if got := ResolveOutboundMaxTools("openai_responses", "claude-cli", 1, 0, 0); got != 1 {
		t.Fatalf("claude responses max=%d", got)
	}
	if got := ResolveOutboundMaxTools("anthropic", "", 1, 0, 0); got != 1 {
		t.Fatalf("anthropic max=%d", got)
	}
}

func TestResolveOutboundToolGap(t *testing.T) {
	claude := 80 * time.Millisecond
	native := time.Duration(0)
	if got := ResolveOutboundToolGap("openai", "x", claude, native); got != native {
		t.Fatalf("chat gap=%v", got)
	}
	if got := ResolveOutboundToolGap("openai_responses", "codex", claude, native); got != native {
		t.Fatalf("native gap=%v", got)
	}
	if got := ResolveOutboundToolGap("anthropic", "claude-cli", claude, native); got != claude {
		t.Fatalf("claude gap=%v", got)
	}
}

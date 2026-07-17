package toolcall

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReadinessParity(t *testing.T) {
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"Update", `{"file_path":"/x","old_string":"a","new_string":""}`, true},
		{"Edit", `{"file_path":"/x","old_string":"a","new_string":""}`, true},
		{"Update", `{"file_path":"/x","old_string":"a"}`, false},
		{"Read", `{"file_path":""}`, false},
		{"Read", `{"file_path":"/x"}`, true},
		{"TaskUpdate", `{"taskId":"1","status":"completed"}`, true},
		{"mcp__x__Update", `{"file_path":"/x"}`, false},
	}
	for _, tc := range cases {
		if got := CompleteJSON(tc.args, tc.name); got != tc.want {
			t.Errorf("%s CompleteJSON=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestCanonicalName(t *testing.T) {
	if got := CanonicalName("Update", []string{"Edit", "Read"}); got != "Edit" {
		t.Fatalf("got %q", got)
	}
	if got := CanonicalName("TaskUpdate", []string{"Edit"}); got != "TaskUpdate" {
		t.Fatalf("protected tool changed to %q", got)
	}
}

func TestLaterConflictingAliasWins(t *testing.T) {
	raw := `{"file_path":"/wrong","path":"/right","oldString":"a","newString":""}`
	got := NormalizeJSON(raw, "Update")
	want := `{"file_path":"/right","old_string":"a","new_string":""}`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if !CompleteJSON(got, "Update") {
		t.Fatalf("normalized update is incomplete: %s", got)
	}
}

func TestMergeCompleteRewriteWins(t *testing.T) {
	current := `{"path":"/wrong"}`
	incoming := `{"file_path":"/right","old_string":"a","new_string":""}`
	got := Merge(current, incoming, "Update")
	if !CompleteJSON(got, "Update") {
		t.Fatalf("incomplete: %s", got)
	}
	if !strings.Contains(got, `"/right"`) {
		t.Fatalf("path not corrected: %s", got)
	}
}

func TestMergeBothCompleteLaterPathWins(t *testing.T) {
	// Critical Claude Code → sub2api: both sides complete; later path alias must win.
	current := `{"file_path":"/wrong","old_string":"a","new_string":"b"}`
	incoming := `{"path":"/correct","old_string":"a","new_string":"c"}`
	got := Merge(current, incoming, "Update")
	if !CompleteJSON(got, "Update") {
		t.Fatalf("incomplete: %s", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse %s: %v", got, err)
	}
	if parsed["file_path"] != "/correct" {
		t.Fatalf("file_path=%v want /correct in %s", parsed["file_path"], got)
	}
	if parsed["new_string"] != "c" {
		t.Fatalf("new_string=%v want c in %s", parsed["new_string"], got)
	}
}

func TestMergeIncompleteLaterDoesNotClobberComplete(t *testing.T) {
	current := `{"file_path":"/right","old_string":"a","new_string":"b"}`
	incoming := `{"path":"/wrong"}`
	got := Merge(current, incoming, "Update")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse %s: %v", got, err)
	}
	if parsed["file_path"] != "/right" {
		t.Fatalf("file_path=%v want /right in %s", parsed["file_path"], got)
	}
	if parsed["old_string"] != "a" || parsed["new_string"] != "b" {
		t.Fatalf("edit body clobbered: %s", got)
	}
}

func TestSanitizeDoubledJSONPicksRicher(t *testing.T) {
	raw := `{"file_path":"/a"}{"file_path":"/b","old_string":"x","new_string":""}`
	got := SanitizeJSON(raw, "Update")
	if !CompleteJSON(got, "Update") {
		t.Fatalf("incomplete after sanitize: %s", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse %s: %v", got, err)
	}
	if parsed["file_path"] != "/b" {
		t.Fatalf("file_path=%v want /b in %s", parsed["file_path"], got)
	}
	if parsed["new_string"] != "" {
		t.Fatalf("new_string should be empty delete: %s", got)
	}
}

func TestSanitizeIdenticalDoubledKeepsFirst(t *testing.T) {
	raw := `{"file_path":"/x"}{"file_path":"/x"}`
	got := SanitizeJSON(raw, "Read")
	if got != `{"file_path":"/x"}` {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizeSingleValidUnchanged(t *testing.T) {
	raw := `{"file_path":"/x","old_string":"a","new_string":"b"}`
	if got := SanitizeJSON(raw, "Update"); got != raw {
		t.Fatalf("single valid JSON must stay unchanged: %q", got)
	}
}

func TestMergeEmptyNewStringOverwrite(t *testing.T) {
	current := `{"file_path":"/x","old_string":"a","new_string":"keep"}`
	incoming := `{"file_path":"/x","old_string":"a","new_string":""}`
	got := Merge(current, incoming, "Edit")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse %s: %v", got, err)
	}
	if parsed["new_string"] != "" {
		t.Fatalf("empty new_string must overwrite: %s", got)
	}
	if !CompleteJSON(got, "Edit") {
		t.Fatalf("empty new_string must remain complete: %s", got)
	}
}

func TestMergeDoubledIncomingChunk(t *testing.T) {
	current := `{"file_path":"/stale"}`
	incoming := `{"file_path":"/stale"}{"path":"/correct","old_string":"a","new_string":"b"}`
	got := Merge(current, incoming, "Update")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse %s: %v", got, err)
	}
	if parsed["file_path"] != "/correct" {
		t.Fatalf("file_path=%v want /correct in %s", parsed["file_path"], got)
	}
}

func FuzzNormalizeNeverPanics(f *testing.F) {
	for _, seed := range []string{"", `{`, `{}`, `{"path":"/x"}`, "\xff", `[]`, `{"a":1}{"a":2}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		_ = SanitizeJSON(raw, "Update")
		_ = NormalizeJSON(raw, "Update")
		_ = CompleteJSON(raw, "Update")
		_ = Merge(raw, raw, "Update")
	})
}

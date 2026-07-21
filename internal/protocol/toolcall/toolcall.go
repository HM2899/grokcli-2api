package toolcall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var nonName = regexp.MustCompile(`[^a-z0-9_]+`)

var required = map[string][]string{
	"read":          {"file_path"},
	"write":         {"file_path", "content"},
	"edit":          {"file_path", "old_string", "new_string"},
	"update":        {"file_path", "old_string", "new_string"},
	"strreplace":    {"file_path", "old_string", "new_string"},
	"str_replace":   {"file_path", "old_string", "new_string"},
	"stringreplace": {"file_path", "old_string", "new_string"},
	"replace":       {"file_path", "old_string", "new_string"},
	"multiedit":     {"file_path", "edits"},
	"notebookedit":  {"notebook_path", "new_source"},
	"bash":          {"command"},
	"shell":         {"command"},
	"localshell":    {"command"},
	"local_shell":   {"command"},
	// Hermes agent terminal tool (OpenAI-style "command", not Codex "cmd").
	"terminal":      {"command"},
	"exec":          {"command"},
	"execcommand":   {"command"},
	"exec_command":  {"command"},
	"run":           {"command"},
	"runcommand":    {"command"},
	"run_command":   {"command"},
	"shellcommand":  {"command"},
	"shell_command": {"command"},
	"applypatch":    {"input"},
	"apply_patch":   {"input"},
	"grep":          {"pattern"},
	"glob":          {"pattern"},
	"webfetch":      {"url"},
	"websearch":     {"query"},
	"web_search":    {"query"},
}

var emptyStringOK = map[string]bool{
	"new_string": true,
	"new_source": true,
	"content":    true,
}

var aliases = map[string]string{
	"path": "file_path", "filepath": "file_path", "file": "file_path", "filename": "file_path",
	"target_file": "file_path", "targetfile": "file_path", "targetpath": "file_path", "target_path": "file_path", "file_name": "file_path",
	"oldstring": "old_string", "oldstr": "old_string", "oldtext": "old_string", "old": "old_string", "old_text": "old_string", "original": "old_string", "original_text": "old_string",
	"from": "old_string", "from_string": "old_string", "find": "old_string", "find_string": "old_string", "match": "old_string", "match_string": "old_string",
	"newstring": "new_string", "newstr": "new_string", "newtext": "new_string", "new": "new_string", "new_text": "new_string", "replacement": "new_string", "replace_with": "new_string",
	"to": "new_string", "to_string": "new_string", "with": "new_string", "with_string": "new_string", "replacement_text": "new_string",
	"contents": "content", "filecontent": "content", "file_content": "content", "filecontents": "content",
	"notebookpath": "notebook_path", "notebook": "notebook_path",
	"cmd": "command", "shell_command": "command", "argv": "command", "args": "command", "cmdline": "command", "command_line": "command", "script": "command",
	"bash": "command", "line": "command",
	"patch": "input", "diff": "input", "patch_text": "input", "patch_input": "input",
	"q": "query", "search": "query", "search_query": "query",
	"uri": "url", "href": "url", "regex": "pattern", "glob_pattern": "pattern",
}

// Edit/Update-only aliases. Keep global "search"→query for Grep; map search/replace
// only when the tool is an edit rewrite (Grok often emits search/replace for Update).
var editOnlyAliases = map[string]string{
	"search": "old_string", "search_string": "old_string", "searchtext": "old_string", "search_text": "old_string",
	"searchfor": "old_string", "search_for": "old_string", "findtext": "old_string", "find_text": "old_string",
	"replace": "new_string", "replacestring": "new_string", "replace_string": "new_string", "replacetext": "new_string", "replace_text": "new_string",
	"replacewith": "new_string", "replace_with": "new_string", "replacementtext": "new_string",
	"content": "new_string", "contents": "new_string",
	"text": "new_string", "body": "new_string", "value": "new_string",
	"old": "old_string", "original": "old_string", "src": "old_string", "source": "old_string",
	"oldcode": "old_string", "old_code": "old_string", "oldsnippet": "old_string",
	"newcode": "new_string", "new_code": "new_string", "newsnippet": "new_string",
	"before": "old_string", "after": "new_string",
	"target": "file_path", "targetfile": "file_path", "target_file": "file_path",
}

// weakEditAliasSources never overwrite an already-set value that came from a real
// schema key (or a stronger alias). Grok often appends content/text chatter after a
// valid new_string; taking it as later-wins produced wrong Edit text / invalid params.
var weakEditAliasSources = map[string]bool{
	"content": true, "contents": true, "text": true, "body": true, "value": true,
	"explanation": true, "reason": true, "comment": true, "message": true, "note": true,
}

func isWeakEditAliasSource(rawKey string) bool {
	folded := nonName.ReplaceAllString(strings.ToLower(strings.TrimSpace(rawKey)), "")
	alnum := strings.ReplaceAll(folded, "_", "")
	return weakEditAliasSources[folded] || weakEditAliasSources[alnum]
}

var editAliases = map[string]bool{
	"update": true, "strreplace": true, "str_replace": true, "stringreplace": true,
	"string_replace": true, "fileedit": true, "file_edit": true, "replace": true,
	"strreplaceeditor": true, "str_replace_editor": true,
	"strreplacebasededittool": true, "str_replace_based_edit_tool": true,
}

var protectedNames = map[string]bool{
	"taskupdate": true, "taskcreate": true, "taskget": true, "tasklist": true,
	"taskoutput": true, "taskstop": true, "todowrite": true, "todoread": true,
}

func nameKey(name string) string {
	return nonName.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "")
}

func CanonicalName(name string, allowed []string) string {
	raw := strings.TrimSpace(name)
	key := nameKey(raw)
	if raw == "" || key == "" || protectedNames[key] {
		return raw
	}
	byKey := make(map[string]string, len(allowed))
	for _, item := range allowed {
		item = strings.TrimSpace(item)
		if k := nameKey(item); k != "" {
			if _, exists := byKey[k]; !exists {
				byKey[k] = item
			}
		}
	}
	if exact, ok := byKey[key]; ok {
		return exact
	}
	if !editAliases[key] {
		return raw
	}
	if edit, ok := byKey["edit"]; ok {
		return edit
	}
	for _, alternative := range []string{"update", "strreplace", "str_replace", "stringreplace"} {
		if advertised, ok := byKey[alternative]; ok {
			return advertised
		}
	}
	return "Edit"
}

func requiredKeys(name string) []string {
	key := nameKey(name)
	if editAliases[key] {
		key = "edit"
	}
	if keys, ok := required[key]; ok {
		return keys
	}
	for short, keys := range required {
		if strings.HasSuffix(key, "_"+short) || strings.HasSuffix(key, "__"+short) {
			return keys
		}
	}
	return nil
}

func canonicalArgKey(key string) string {
	raw := strings.TrimSpace(key)
	folded := nonName.ReplaceAllString(strings.ToLower(raw), "")
	alnum := strings.ReplaceAll(folded, "_", "")
	if alias, ok := aliases[folded]; ok {
		return alias
	}
	if alias, ok := aliases[alnum]; ok {
		return alias
	}
	return raw
}

func canonicalArgKeyForTool(key, toolName string) string {
	raw := strings.TrimSpace(key)
	folded := nonName.ReplaceAllString(strings.ToLower(raw), "")
	alnum := strings.ReplaceAll(folded, "_", "")
	// Edit-family tools: allow search/replace/content → old/new_string.
	if isEditTool(toolName) {
		if alias, ok := editOnlyAliases[folded]; ok {
			return alias
		}
		if alias, ok := editOnlyAliases[alnum]; ok {
			return alias
		}
	}
	// Grep/Glob (Claude Code): schema uses path + pattern, NOT file_path.
	// Global aliases map path→file_path for Read/Edit, which makes Grep reject with
	// "An unexpected parameter file_path was provided".
	if isGrepLikeTool(toolName) {
		switch folded {
		case "path", "filepath", "file_path", "file", "filename", "file_name",
			"target_file", "targetfile", "targetpath", "target_path", "target":
			return "path"
		case "search", "query", "q", "search_query", "searchquery":
			// required["grep"] is pattern; Grok often emits search/query instead.
			return "pattern"
		}
		if alnum == "filepath" || alnum == "filename" || alnum == "targetfile" || alnum == "targetpath" {
			return "path"
		}
		if alnum == "searchquery" {
			return "pattern"
		}
	}
	return canonicalArgKey(key)
}

// isGrepLikeTool is true for Claude Code Grep / Glob and common aliases.
// These tools accept path (directory/file scope), not file_path.
func isGrepLikeTool(name string) bool {
	key := nameKey(name)
	switch key {
	case "grep", "glob", "searchfiles", "search_files", "rg", "ripgrep":
		return true
	}
	if strings.HasSuffix(key, "grep") || strings.HasSuffix(key, "glob") {
		return true
	}
	return false
}

func isEditTool(name string) bool {
	key := nameKey(name)
	if key == "edit" || editAliases[key] {
		return true
	}
	if key == "update" || key == "strreplace" || key == "replace" {
		return true
	}
	if protectedNames[key] {
		return false
	}
	if strings.Contains(key, "strreplace") || strings.Contains(key, "stringreplace") {
		return true
	}
	if strings.HasSuffix(key, "edit") || strings.HasSuffix(key, "update") {
		if key == "evaluate" || strings.Contains(key, "evaluat") {
			return false
		}
		return true
	}
	return false
}

type chosenValue struct {
	value     any
	canonical bool
}

// NormalizeObject renames common alternate tool-arg keys to Claude Code schema
// names. Later conflicting non-empty values win (authoritative rewrite).
// Prefer NormalizeObjectForTool when the tool name is known (Edit/Update aliases).
func NormalizeObject(input map[string]any) map[string]any {
	return NormalizeObjectForTool(input, "")
}

// NormalizeObjectForTool is like NormalizeObject but applies tool-specific aliases
// (e.g. Update/Edit: search/replace → old_string/new_string). Missing new_string
// is left absent; call CoerceCompleteJSON / fillEditNewStringDefault at force-finish.
func NormalizeObjectForTool(input map[string]any, toolName string) map[string]any {
	chosen := make(map[string]chosenValue, len(input))
	// Preserve insertion order of first-seen canonical keys for stable encode.
	order := make([]string, 0, len(input))
	for raw, value := range input {
		// Unwrap accidental JSON-string values: {"file_path":""/x""} / nested JSON.
		value = unwrapJSONStringValue(value)
		canonical := canonicalArgKeyForTool(raw, toolName)
		current, exists := chosen[canonical]
		if !exists {
			order = append(order, canonical)
			chosen[canonical] = chosenValue{value: value, canonical: raw == canonical}
			continue
		}
		oldEmpty, newEmpty := empty(current.value), empty(value)
		if oldEmpty && !newEmpty {
			chosen[canonical] = chosenValue{value: value, canonical: raw == canonical}
			continue
		}
		if newEmpty {
			continue
		}
		if equal(current.value, value) {
			if raw == canonical && !current.canonical {
				chosen[canonical] = chosenValue{value: value, canonical: true}
			}
			continue
		}
		// Conflict policy (must match NormalizeJSON ordered decode):
		// - weak chatter (content/text/body/explanation) never overwrites non-empty values
		// - prefer real schema keys over non-path aliases (new_string beats replace/content)
		// - path aliases still later-win (path flip-form: file_path then path → path)
		incomingCanonical := raw == canonical
		if isWeakEditAliasSource(raw) && !empty(current.value) {
			continue
		}
		if current.canonical && !incomingCanonical && !isPathArgKey(raw) {
			continue
		}
		if !current.canonical && incomingCanonical {
			chosen[canonical] = chosenValue{value: value, canonical: true}
			continue
		}
		// Different non-empty values: later wins (path flip-form, rewrite deltas).
		chosen[canonical] = chosenValue{value: value, canonical: incomingCanonical}
	}
	out := make(map[string]any, len(chosen))
	for _, key := range order {
		if value, ok := chosen[key]; ok {
			out[key] = value.value
		}
	}
	// Map iteration above may miss keys if order only has first-seen; fill rest.
	for key, value := range chosen {
		if _, ok := out[key]; !ok {
			out[key] = value.value
		}
	}
	return applyEditDefaults(out, toolName)
}

// editClientKeys are the only fields Claude Code Edit accepts. Extra Grok
// chatter (explanation/mode/content/query) triggers "Invalid tool parameters".
var editClientKeys = map[string]bool{
	"file_path":   true,
	"old_string":  true,
	"new_string":  true,
	"replace_all": true,
}

// densifyEditObject keeps only Claude Code Edit schema fields.
func densifyEditObject(obj map[string]any) map[string]any {
	if obj == nil {
		return nil
	}
	out := make(map[string]any, 4)
	for _, key := range []string{"file_path", "old_string", "new_string"} {
		if v, ok := obj[key]; ok {
			out[key] = v
		}
	}
	if v, ok := obj["replace_all"]; ok && v != nil {
		switch b := v.(type) {
		case bool:
			out["replace_all"] = b
		case string:
			switch strings.TrimSpace(strings.ToLower(b)) {
			case "true", "1", "yes":
				out["replace_all"] = true
			case "false", "0", "no":
				out["replace_all"] = false
			}
		case float64:
			out["replace_all"] = b != 0
		case json.Number:
			if n, err := b.Int64(); err == nil {
				out["replace_all"] = n != 0
			}
		}
	}
	return out
}

// unwrapPathValue flattens nested path objects Grok sometimes emits:
// {"file_path":{"path":"/x"}} → "/x".
func unwrapPathValue(v any) any {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		for _, k := range []string{"file_path", "path", "target_file", "targetfile", "filepath", "file"} {
			if s, ok := t[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		// Nested once more (rare).
		for _, k := range []string{"file_path", "path", "target_file"} {
			if nested, ok := t[k].(map[string]any); ok {
				if s := unwrapPathValue(nested); s != nil {
					if ss, ok := s.(string); ok && strings.TrimSpace(ss) != "" {
						return strings.TrimSpace(ss)
					}
				}
			}
		}
	}
	return v
}

// fillEditNewStringDefault sets missing new_string to "" when path+old are present.
// Used at force-finish (CoerceCompleteJSON) so stream Merge does not invent empty
// new_string mid-turn when replace has not arrived yet.
func fillEditNewStringDefault(obj map[string]any, toolName string) map[string]any {
	if obj == nil || !isEditTool(toolName) {
		return obj
	}
	fp, _ := obj["file_path"].(string)
	old, hasOld := obj["old_string"]
	if strings.TrimSpace(fp) == "" || !hasOld || old == nil {
		return obj
	}
	if _, hasNew := obj["new_string"]; !hasNew {
		obj["new_string"] = ""
	}
	return obj
}

// applyEditDefaults coerces path/old/new types for Edit/Update. Does NOT invent
// missing new_string during mid-stream merges (see fillEditNewStringDefault).
func applyEditDefaults(obj map[string]any, toolName string) map[string]any {
	if obj == nil {
		return obj
	}
	if isEditTool(toolName) {
		// Recover keys poisoned when args were normalized before the tool name
		// was known: global alias maps search→query (Grep), so a nameless Merge
		// of Update search/replace becomes query/replace and never maps to
		// old_string. Promote leftovers once we know this is an edit tool.
		recoverEditPoisonedKeys(obj)

		if v, ok := obj["file_path"]; ok && v != nil {
			v = unwrapPathValue(v)
			obj["file_path"] = v
			if _, isStr := v.(string); !isStr {
				s := strings.TrimSpace(stringify(v))
				if s != "" && s != "null" && s != "{}" && s != "[]" {
					obj["file_path"] = s
				}
			}
		}
		for _, k := range []string{"old_string", "new_string"} {
			v, ok := obj[k]
			if !ok || v == nil {
				continue
			}
			if _, isStr := v.(string); isStr {
				continue
			}
			switch v.(type) {
			case map[string]any, []any:
				// leave complex (rare nested edits) — densify will still type-check
			default:
				obj[k] = stringify(v)
			}
		}
		// Strip Grok chatter / leftover aliases so Claude Code never sees
		// explanation/mode/content/query and rejects with "Invalid tool parameters".
		dense := densifyEditObject(obj)
		// Mutate in place so callers sharing the map see densified keys.
		for k := range obj {
			if !editClientKeys[k] {
				delete(obj, k)
			}
		}
		for k, v := range dense {
			obj[k] = v
		}
		// Intentionally do NOT invent missing new_string here.
		// Mid-stream Merge/Normalize must wait for an explicit new_string/replace
		// (or force-finish via CoerceCompleteJSON → fillEditNewStringDefault).
		// Premature "" defaults caused Claude Code to accept a delete-match Edit
		// before Grok finished streaming the real replacement, then drop the
		// late args (tool already stopped) — intermittent Update failures.
	}
	obj = applyGrepDefaults(obj, toolName)
	return applyShellDefaults(obj, toolName)
}

// applyGrepDefaults rewrites Read/Edit-style path keys onto Claude Code Grep/Glob
// schema (path + pattern) and drops file_path so clients stop rejecting with
// "An unexpected parameter file_path was provided".
func applyGrepDefaults(obj map[string]any, toolName string) map[string]any {
	if obj == nil || !isGrepLikeTool(toolName) {
		return obj
	}
	// file_path / leftovers → path
	if v, ok := obj["path"]; !ok || empty(v) {
		for _, alt := range []string{"file_path", "filepath", "file", "target_file", "targetfile"} {
			if av, exists := obj[alt]; exists && !empty(av) {
				obj["path"] = unwrapPathValue(av)
				break
			}
		}
	} else {
		obj["path"] = unwrapPathValue(v)
	}
	for _, alt := range []string{"file_path", "filepath", "file", "target_file", "targetfile"} {
		delete(obj, alt)
	}
	// search/query → pattern (required field for Grep)
	if v, ok := obj["pattern"]; !ok || empty(v) {
		for _, alt := range []string{"query", "search", "q", "search_query", "regex"} {
			if av, exists := obj[alt]; exists && !empty(av) {
				obj["pattern"] = av
				break
			}
		}
	}
	// Drop query/search when pattern is set so clients never see dual keys.
	if _, has := obj["pattern"]; has && !empty(obj["pattern"]) {
		for _, alt := range []string{"query", "search", "q", "search_query"} {
			delete(obj, alt)
		}
	}
	return obj
}

// recoverEditPoisonedKeys remaps leftover Grep/generic aliases onto Edit schema
// when the tool is known to be Edit/Update. Safe only for isEditTool names.
func recoverEditPoisonedKeys(obj map[string]any) {
	if obj == nil {
		return
	}
	// old_string: search was often rewritten to query under empty tool name.
	if v, ok := obj["old_string"]; !ok || empty(v) {
		for _, alt := range []string{
			"query", "search", "search_string", "search_text", "searchfor", "search_for",
			"find", "find_string", "find_text", "match", "match_string",
		} {
			if av, exists := obj[alt]; exists && !empty(av) {
				obj["old_string"] = av
				delete(obj, alt)
				break
			}
		}
	}
	// Drop residual search aliases so clients never see query+old_string together.
	for _, alt := range []string{
		"query", "search", "search_string", "search_text", "searchfor", "search_for",
		"find", "find_string", "find_text", "match", "match_string",
	} {
		if _, has := obj["old_string"]; has {
			delete(obj, alt)
		}
	}
	// new_string: promote aliases only when missing. Prefer replace* over content
	// (content is a common Grok chatter key that must not clobber real new_string).
	if _, hasNew := obj["new_string"]; !hasNew {
		for _, alt := range []string{
			"replace", "replacement", "replace_with", "replacewith", "replace_string",
			"contents", "text", "body", "value", "to", "to_string", "with",
			"content", // last: weaker fallback
		} {
			if av, exists := obj[alt]; exists && av != nil {
				obj["new_string"] = av
				delete(obj, alt)
				break
			}
		}
	}
	// Always drop leftover aliases — even when new_string already present —
	// so densify is not the only line of defense against Invalid tool parameters.
	for _, alt := range []string{
		"replace", "replacement", "replace_with", "replacewith", "replace_string",
		"content", "contents", "text", "body", "value", "to", "to_string", "with",
		"explanation", "mode", "reason", "comment", "notes", "description",
	} {
		delete(obj, alt)
	}
}

func isShellTool(name string) bool {
	key := nameKey(name)
	// Namespaced forms: "default_api.exec_command", "functions.Shell", "mcp__shell".
	if i := strings.LastIndex(key, "."); i >= 0 && i+1 < len(key) {
		key = key[i+1:]
	}
	// Strip common prefixes that survive nameKey (mcp, tool, fn).
	for _, prefix := range []string{"mcp", "tool", "fn", "function"} {
		if strings.HasPrefix(key, prefix) && len(key) > len(prefix) {
			// only strip when remainder still looks shell-like
			rest := key[len(prefix):]
			if isShellToolKey(rest) {
				return true
			}
		}
	}
	return isShellToolKey(key)
}

func isShellToolKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	switch key {
	case "bash", "shell", "localshell", "exec", "run", "sh", "zsh", "powershell",
		// Hermes agent (Nous Research): tool name "terminal", schema param "command".
		"terminal",
		"execcommand", "runcommand", "shellcommand", "localshellcommand":
		return true
	default:
		// Codex / agent shells often use compound names: exec_command, run_command,
		// shell_command, local_shell, *_shell, shell_*, exec_*.
		if strings.HasSuffix(key, "shell") || strings.HasSuffix(key, "command") {
			if strings.Contains(key, "shell") || strings.Contains(key, "exec") ||
				strings.Contains(key, "bash") || strings.Contains(key, "run") ||
				strings.HasPrefix(key, "cmd") {
				return true
			}
		}
		if strings.HasPrefix(key, "shell") || strings.HasPrefix(key, "exec") ||
			strings.HasPrefix(key, "bash") || strings.HasPrefix(key, "run") ||
			strings.HasPrefix(key, "terminal") {
			return true
		}
		return false
	}
}

// IsShellTool reports whether name is a shell-family tool (bash/shell/exec/...).
func IsShellTool(name string) bool { return isShellTool(name) }

// NameKey returns the normalized alnum-only tool name key used for alias maps.
func NameKey(name string) string { return nameKey(name) }

// DefaultShellArgKey returns the client-facing shell parameter name when the
// request did not advertise a schema (or the tool was missing from ShellArgKeyMap).
//
// Codex local tools validate "cmd". Hermes agent (Nous Research) uses tool
// name "terminal" with required parameter "command" — projecting to cmd makes
// Hermes drop the payload (args.get("command") is empty).
func DefaultShellArgKey(name string) string {
	switch nameKey(name) {
	case "terminal":
		return "command"
	default:
		return "cmd"
	}
}

func isApplyPatchTool(name string) bool {
	key := nameKey(name)
	if key == "" {
		return false
	}
	switch key {
	case "applypatch", "apply_patch", "applydiff", "apply_diff", "applygitpatch", "apply_git_patch":
		return true
	}
	// Namespaced / compound: *.apply_patch, apply_patch_v2, codex_apply_patch, ...
	if strings.Contains(key, "applypatch") || strings.Contains(key, "apply_patch") {
		return true
	}
	if strings.Contains(key, "applydiff") || strings.Contains(key, "apply_diff") {
		return true
	}
	if strings.HasSuffix(key, "patch") && (strings.Contains(key, "apply") || strings.Contains(key, "git")) {
		return true
	}
	return false
}

// applyShellDefaults flattens nested command argv and drops empty junk that
// Grok sometimes emits for Codex shell tools: {"command":[[""]]}.
// Also promotes common wrong parameter names (cmd/argv/args/...) onto "command"
// so Codex stops retrying "参数名写错了".
func applyShellDefaults(obj map[string]any, toolName string) map[string]any {
	if obj == nil {
		return obj
	}
	if isShellTool(toolName) {
		// Prefer existing command; otherwise promote known aliases that may have
		// survived earlier renames (or been double-keyed by the model).
		if _, ok := obj["command"]; !ok {
			for _, alt := range []string{"cmd", "argv", "args", "shell_command", "cmdline", "command_line", "script", "bash", "line"} {
				if v, exists := obj[alt]; exists && !empty(v) {
					obj["command"] = v
					delete(obj, alt)
					break
				}
			}
		}
		// Drop leftover alias keys so clients never see both cmd and command.
		for _, alt := range []string{"cmd", "argv", "args", "shell_command", "cmdline", "command_line", "script", "bash", "line"} {
			if _, ok := obj[alt]; ok {
				// If command empty and alt has value, already handled above; always strip alias.
				if empty(obj["command"]) && !empty(obj[alt]) {
					obj["command"] = obj[alt]
				}
				delete(obj, alt)
			}
		}
		if cmd, ok := obj["command"]; ok {
			obj["command"] = normalizeShellCommand(cmd)
			// After normalize, nil means incomplete — delete so CompleteJSON fails cleanly.
			if obj["command"] == nil {
				delete(obj, "command")
			}
		}
	}
	if isApplyPatchTool(toolName) {
		aliases := []string{"patch", "diff", "patch_text", "patch_input", "content", "patch_content", "text", "body", "data", "code", "code_edit", "edit"}
		if _, ok := obj["input"]; !ok {
			for _, alt := range aliases {
				if v, exists := obj[alt]; exists && !empty(v) {
					obj["input"] = v
					delete(obj, alt)
					break
				}
			}
		}
		// Drop leftover aliases so clients never see both input and patch/diff.
		for _, alt := range aliases {
			if _, ok := obj[alt]; !ok {
				continue
			}
			if empty(obj["input"]) && !empty(obj[alt]) {
				obj["input"] = obj[alt]
			}
			delete(obj, alt)
		}
		if v, ok := obj["input"]; ok {
			if s, ok := v.(string); ok {
				obj["input"] = s
			} else if v != nil {
				if encoded, err := compactJSON(v); err == nil {
					obj["input"] = encoded
				} else {
					obj["input"] = stringify(v)
				}
			}
		}
	}
	return obj
}

// emptyArrayJunkPrefix matches a leading empty JSON-array artifact that Grok
// sometimes glues onto the real shell command for Codex, e.g.
//
//	"[    ]Get-Content ..."  →  "Get-Content ..."
//	"[]Write-Output 'ok'"    →  "Write-Output 'ok'"
//
// Four-space form is common in live Codex sessions (2026-07-20). PowerShell
// then parses "[    ]Write-Output" as a type-cast and fails with
// "Missing type name after '['".
var emptyArrayJunkPrefix = regexp.MustCompile(`^\s*\[\s*\]\s*`)

// normalizeShellCommand always returns a non-empty string (or nil if incomplete).
//
// Codex exec_command / shell local schema requires:
//
//	cmd: string   (NOT an argv array)
//
// Grok often emits {"command":["ls","-la"]} or nested [["echo","hi"]]. Flatten
// and join into a single shell string so clients stop rejecting with
// "command 要字符串，不要 argv 数组".
func normalizeShellCommand(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		s := stripShellCommandJunk(strings.TrimSpace(v))
		if s == "" {
			return nil
		}
		// Unwrap accidental JSON-encoded argv: "[\"ls\",\"-la\"]"
		// Also reject pure empty-array strings ("[]", "[    ]") which used to
		// fall through and become a "valid" command that later glued onto real text.
		if (strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]")) ||
			(strings.HasPrefix(s, `"[`) && strings.Contains(s, `]`)) {
			var arr []any
			if json.Unmarshal([]byte(s), &arr) == nil {
				if flat := flattenCommandParts(arr); len(flat) > 0 {
					return joinShellArgv(flat, DialectPOSIX)
				}
				// Parsed as empty array — not a usable command.
				return nil
			}
			var unquoted string
			if json.Unmarshal([]byte(s), &unquoted) == nil {
				unquoted = stripShellCommandJunk(strings.TrimSpace(unquoted))
				if unquoted != "" && unquoted[0] == '[' {
					if json.Unmarshal([]byte(unquoted), &arr) == nil {
						if flat := flattenCommandParts(arr); len(flat) > 0 {
							return joinShellArgv(flat, DialectPOSIX)
						}
						return nil
					}
				}
				if unquoted != "" {
					return unquoted
				}
			}
		}
		// Leading empty-array junk + real command: "[    ]Get-Content ..."
		if cleaned := emptyArrayJunkPrefix.ReplaceAllString(s, ""); cleaned != s {
			cleaned = strings.TrimSpace(cleaned)
			if cleaned == "" {
				return nil
			}
			// Recurse so nested argv-string cases still flatten.
			return normalizeShellCommand(cleaned)
		}
		return s
	case []any:
		flat := flattenCommandParts(v)
		if len(flat) == 0 {
			return nil
		}
		return joinShellArgv(flat, DialectPOSIX)
	case []string:
		tmp := make([]any, len(v))
		for i, s := range v {
			tmp[i] = s
		}
		return normalizeShellCommand(tmp)
	default:
		// Numbers / bools / objects: stringify; objects that are JSON arrays go through flatten.
		if encoded, err := compactJSON(v); err == nil {
			if strings.HasPrefix(encoded, "[") {
				var arr []any
				if json.Unmarshal([]byte(encoded), &arr) == nil {
					flat := flattenCommandParts(arr)
					if len(flat) == 0 {
						return nil
					}
					return joinShellArgv(flat, DialectPOSIX)
				}
			}
		}
		s := stripShellCommandJunk(strings.TrimSpace(stringify(v)))
		if s == "" || s == "null" || s == "[]" || s == "{}" {
			return nil
		}
		return s
	}
}

// stripShellCommandJunk drops pure empty-array / null-looking shells that must
// never be treated as a complete Codex cmd.
func stripShellCommandJunk(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" || s == "[]" || s == "{}" {
		return ""
	}
	// Pure whitespace-inside-brackets empty array: "[    ]", "[ ]", "[\t\n]"
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return ""
		}
	}
	return s
}

// shellDialect is retained only for argv join/quote helpers.
// We no longer rewrite PowerShell vs bash command text at the proxy boundary:
// that was cache-hostile (instruction inject) and often damaged real Codex cmds.
// Outbound path only remaps cmd↔command and preserves workdir/timeout extras.
type shellDialect int

const (
	DialectPOSIX shellDialect = iota
)

// shellDialectName returns a stable log label.
func shellDialectName(d shellDialect) string {
	return "passthrough"
}

// joinShellArgv joins argv parts into one shell command string.
// dialect is ignored (passthrough); kept for call-site compatibility.
func joinShellArgv(parts []string, dialect shellDialect) string {
	if len(parts) == 0 {
		return ""
	}
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		cleaned = append(cleaned, p)
	}
	if len(cleaned) == 0 {
		return ""
	}
	if len(cleaned) == 1 {
		// Full command string already: do not re-quote.
		return cleaned[0]
	}
	// Multi complete statements (spaces inside each part, not bare flags).
	// Includes PowerShell here-strings (@' ... '@) that Grok often splits across
	// argv slots — "; " keeps them as sequential statements, not re-quoted tokens.
	// "; " is also valid bash (sequential), so safe for DialectPOSIX.
	if allLookLikeShellStatements(cleaned) {
		return strings.Join(cleaned, "; ")
	}
	// Classic argv: quote only meta-bearing tokens.
	out := make([]string, 0, len(cleaned))
	for _, p := range cleaned {
		out = append(out, shellQuoteToken(p, dialect))
	}
	return strings.Join(out, " ")
}

// allLookLikeShellStatements is true when every token already looks like a full
// command line (contains whitespace and is not a bare -flag). Used to decide
// "; " join vs argv join for multi-part shell args from Grok.
//
// Also true when any part is a multi-line here-string body / has newlines, even
// if a sibling part is a short PowerShell opener like "@'" — Grok splits
// Set-Content here-strings across argv slots; space-joining + re-quote breaks PS.
func allLookLikeShellStatements(parts []string) bool {
	if len(parts) < 2 {
		return false
	}
	hasNewline := false
	multiWord := 0
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return false
		}
		// Bare flags / short tokens that are clearly argv pieces.
		if strings.HasPrefix(p, "-") && !strings.ContainsAny(p, " \t\n") {
			return false
		}
		if strings.ContainsAny(p, "\n\r") {
			hasNewline = true
		}
		if strings.ContainsAny(p, " \t\n") {
			multiWord++
		}
	}
	// Classic: every part multi-word → statements.
	if multiWord == len(parts) {
		return true
	}
	// Here-string / multi-line split across argv: prefer "; " if majority
	// multi-word and no bare -flags (already filtered).
	if hasNewline && multiWord >= len(parts)-1 {
		return true
	}
	// PowerShell cmdlet pattern Verb-Noun in every multi-ish part.
	cmdletish := 0
	for _, p := range parts {
		tok := strings.Fields(strings.TrimSpace(p))
		if len(tok) == 0 {
			continue
		}
		head := tok[0]
		if strings.Contains(head, "-") && head[0] >= 'A' && head[0] <= 'Z' {
			cmdletish++
		}
	}
	return cmdletish >= 2 && multiWord >= len(parts)-1
}

func shellQuoteToken(s string, dialect shellDialect) string {
	// Safe bare token: alnum + common path/flag chars.
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '/' || c == '\\' || c == '=' || c == ':' || c == '@' || c == '+' || c == '%' {
			continue
		}
		safe = false
		break
	}
	if safe {
		return s
	}
	_ = dialect
	// Prefer double-quote with "" escape. Bash-style '\'' around tokens like
	// 'rg -n' makes PowerShell treat the first token as a string literal (not a
	// command) and then ParserError on the next quoted fragment.
	if !strings.Contains(s, "\"") {
		return "\"" + s + "\""
	}
	return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
}

func flattenCommandParts(parts []any) []string {
	out := make([]string, 0, len(parts))
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case nil:
			return
		case string:
			s := cleanShellToken(strings.TrimSpace(t))
			if s != "" {
				out = append(out, s)
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		case []string:
			for _, item := range t {
				walk(item)
			}
		default:
			s := cleanShellToken(strings.TrimSpace(stringify(t)))
			if s != "" && s != "null" {
				out = append(out, s)
			}
		}
	}
	for _, p := range parts {
		walk(p)
	}
	return out
}

// cleanShellToken drops empty-array junk prefixes from a single argv token
// (e.g. "[    ]Write-Output 'ok'" → "Write-Output 'ok'") and always strips
// bash dialect artifacts that explode under Codex PowerShell.
func cleanShellToken(s string) string {
	// POSIX-safe: only strip empty-array junk. Bash quote-glue MUST stay intact
	// for Linux CLI clients; PowerShell sanitization runs only at client projection
	// at projection time (passthrough).
	s = stripShellCommandJunk(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if cleaned := emptyArrayJunkPrefix.ReplaceAllString(s, ""); cleaned != s {
		return strings.TrimSpace(cleaned)
	}
	return s
}

// (PowerShell dialect rewrites removed — shell cmds pass through unchanged.)

func shellCommandNonEmpty(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(flattenCommandParts(v)) > 0
	case []string:
		for _, s := range v {
			if strings.TrimSpace(s) != "" {
				return true
			}
		}
		return false
	default:
		return strings.TrimSpace(stringify(v)) != ""
	}
}

// unwrapJSONStringValue peels one layer of accidental JSON-encoded strings
// (Grok sometimes double-encodes path/old/new as ""..."" or full JSON objects).
func unwrapJSONStringValue(value any) any {
	s, ok := value.(string)
	if !ok {
		return value
	}
	text := strings.TrimSpace(s)
	if text == "" {
		return value
	}
	// Double-quoted JSON string: ""/path"" or ""old text""
	if len(text) >= 2 && text[0] == '"' {
		var decoded string
		if err := json.Unmarshal([]byte(text), &decoded); err == nil {
			// Only unwrap when it changes something (avoid turning bare words into empty).
			if decoded != text {
				return decoded
			}
		}
	}
	// Nested JSON object/array encoded as string — leave for callers that expect strings;
	// only unwrap when key is not a path/text field is handled above.
	return value
}

// NormalizeJSON alias-normalizes a single JSON object string. Multi-object
// blobs are recovered via SanitizeJSON first. Object member order is preserved
// so later conflicting aliases (path vs file_path) win.
func NormalizeJSON(raw string, toolName string) string {
	cleaned := SanitizeJSON(raw, toolName)
	text := strings.TrimSpace(cleaned)
	if text == "" || (text[0] != '{' && text[0] != '[') {
		return cleaned
	}
	if text[0] != '{' {
		return cleaned
	}
	// Prefer ordered pair decode so "later wins" matches Python dict order.
	pairs, err := decodeObjectPairs(text)
	if err != nil {
		return cleaned
	}
	chosen := make(map[string]chosenValue, len(pairs))
	order := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		val := unwrapJSONStringValue(pair.value)
		canonical := canonicalArgKeyForTool(pair.key, toolName)
		current, exists := chosen[canonical]
		if !exists {
			order = append(order, canonical)
			chosen[canonical] = chosenValue{value: val, canonical: pair.key == canonical}
			continue
		}
		oldEmpty, newEmpty := empty(current.value), empty(val)
		if oldEmpty && !newEmpty {
			chosen[canonical] = chosenValue{value: val, canonical: pair.key == canonical}
			continue
		}
		if newEmpty {
			continue
		}
		if equal(current.value, val) {
			if pair.key == canonical && !current.canonical {
				chosen[canonical] = chosenValue{value: val, canonical: true}
			}
			continue
		}
		// Conflict policy (ordered decode, matches NormalizeObjectForTool):
		// - weak chatter never overwrites non-empty values
		// - prefer real schema keys over non-path aliases (new_string beats replace/content)
		// - path aliases still later-win (file_path then path → path value)
		incomingCanonical := pair.key == canonical
		if isWeakEditAliasSource(pair.key) && !empty(current.value) {
			continue
		}
		if current.canonical && !incomingCanonical && !isPathArgKey(pair.key) {
			continue
		}
		if !current.canonical && incomingCanonical {
			chosen[canonical] = chosenValue{value: val, canonical: true}
			continue
		}
		// Later conflicting same-class aliases represent a later authoritative rewrite.
		chosen[canonical] = chosenValue{value: val, canonical: incomingCanonical}
	}
	// Materialize map so edit defaults (missing new_string) can apply.
	tmp := make(map[string]any, len(chosen))
	for _, key := range order {
		if value, ok := chosen[key]; ok {
			tmp[key] = value.value
		}
	}
	for key, value := range chosen {
		if _, ok := tmp[key]; !ok {
			tmp[key] = value.value
		}
	}
	tmp = applyEditDefaults(tmp, toolName)
	// Re-encode with order. applyShellDefaults (via applyEditDefaults) may:
	// - promote bash/line/cmd → command
	// - drop shell alias keys
	// - flatten nested command argv / delete empty command
	// so we must sync order+chosen from tmp rather than only patching edit keys.
	shellAliasKeys := map[string]bool{
		"cmd": true, "argv": true, "args": true, "shell_command": true,
		"cmdline": true, "command_line": true, "script": true, "bash": true, "line": true,
	}
	newOrder := make([]string, 0, len(order)+4)
	seen := map[string]bool{}
	for _, key := range order {
		if shellAliasKeys[key] {
			if _, keep := tmp[key]; !keep {
				delete(chosen, key)
				continue
			}
		}
		if v, ok := tmp[key]; ok {
			newOrder = append(newOrder, key)
			seen[key] = true
			chosen[key] = chosenValue{value: v, canonical: true}
		} else {
			delete(chosen, key)
		}
	}
	if isShellTool(toolName) {
		if v, ok := tmp["command"]; ok && !seen["command"] {
			newOrder = append([]string{"command"}, newOrder...)
			seen["command"] = true
			chosen["command"] = chosenValue{value: v, canonical: true}
		}
	}
	for _, key := range []string{"file_path", "old_string", "new_string", "command", "input"} {
		if seen[key] {
			continue
		}
		if v, ok := tmp[key]; ok {
			newOrder = append(newOrder, key)
			seen[key] = true
			chosen[key] = chosenValue{value: v, canonical: true}
		}
	}
	for key, v := range tmp {
		if seen[key] {
			continue
		}
		newOrder = append(newOrder, key)
		seen[key] = true
		chosen[key] = chosenValue{value: v, canonical: true}
	}
	return encodeObjectOrdered(newOrder, chosen)
}

type objectPair struct {
	key   string
	value any
}

func decodeObjectPairs(text string) ([]objectPair, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, err
	}
	pairs := make([]objectPair, 0)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("JSON object key is not a string")
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		pairs = append(pairs, objectPair{key: key, value: value})
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return pairs, nil
}

// marshalNoHTML encodes a single JSON value without HTML escaping so shell
// args keep raw & < > (json.Marshal rewrites them as \u0026 / \u003c / \u003e).
func marshalNoHTML(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	// Encode appends a trailing newline.
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	return out, nil
}

func encodeObjectOrdered(order []string, chosen map[string]chosenValue) string {
	var output bytes.Buffer
	output.WriteByte('{')
	for index, key := range order {
		if index > 0 {
			output.WriteByte(',')
		}
		encodedKey, err := marshalNoHTML(key)
		if err != nil {
			return ""
		}
		encodedValue, err := marshalNoHTML(chosen[key].value)
		if err != nil {
			return ""
		}
		output.Write(encodedKey)
		output.WriteByte(':')
		output.Write(encodedValue)
	}
	output.WriteByte('}')
	return output.String()
}

// SanitizeJSON recovers doubled JSON blobs in one chunk and picks the richest
// / latest-complete rewrite. Single valid JSON is returned unchanged so true
// OpenAI delta suffixes keep prefix continuity.
//
// Mirrors Python sanitize_tool_arguments_json.
func SanitizeJSON(raw string, toolName string) string {
	if raw == "" {
		return ""
	}
	// Already a single valid JSON value — keep original text.
	if json.Valid([]byte(raw)) {
		return raw
	}
	stripped := strings.TrimSpace(raw)
	if stripped != "" && stripped != raw && json.Valid([]byte(stripped)) {
		return stripped
	}

	src := stripped
	if src == "" {
		src = raw
	}
	values, ends := decodeJSONValues(src)
	if len(values) < 2 {
		return raw
	}

	first := values[0]
	firstText := strings.TrimSpace(src[:ends[0]])
	allEqual := true
	for _, v := range values[1:] {
		if !equal(v, first) {
			allEqual = false
			break
		}
	}
	if allEqual {
		return firstText
	}

	type candidate struct {
		score valueScore
		text  string
	}
	candidates := make([]candidate, 0, len(values)+1)
	if merged := mergeArgDicts(values, toolName); merged != nil {
		if text, err := compactJSON(merged); err == nil {
			candidates = append(candidates, candidate{score: scoreValue(merged), text: text})
		}
	}
	for i, v := range values {
		var text string
		if i == 0 {
			text = firstText
		} else {
			start := ends[i-1]
			for start < ends[i] && isSpace(src[start]) {
				start++
			}
			text = strings.TrimSpace(src[start:ends[i]])
			if text == "" {
				if encoded, err := compactJSON(v); err == nil {
					text = encoded
				}
			}
		}
		if text == "" {
			continue
		}
		candidates = append(candidates, candidate{score: scoreValue(v), text: text})
	}
	if len(candidates) == 0 {
		return firstText
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.score.less(best.score) {
			continue
		}
		// Higher or equal score with later preference when equal? Python sorts
		// reverse so first max wins among equals after sort — we pick strictly
		// greater, keep first on tie.
		if c.score.greater(best.score) {
			best = c
		}
	}
	return best.text
}

func decodeJSONValues(src string) ([]any, []int) {
	decoder := json.NewDecoder(strings.NewReader(src))
	decoder.UseNumber()
	values := make([]any, 0, 2)
	ends := make([]int, 0, 2)
	// Track byte offset via decoder.InputOffset after each value.
	for {
		// Skip leading whitespace by attempting decode; stop on error.
		var value any
		if err := decoder.Decode(&value); err != nil {
			break
		}
		values = append(values, value)
		ends = append(ends, int(decoder.InputOffset()))
		// Cap recovery scans: a pathological buffer of thousands of repeated
		// JSON blobs (runaway upstream rewrite loop) would otherwise be decoded
		// in full on every mid-stream Merge / EffectiveJSON / CompleteJSON call,
		// turning one glitchy tool call into a multi-second CPU stall.
		if len(values) >= 32 {
			break
		}
	}
	return values, ends
}

type valueScore struct {
	kind     int
	keys     int
	present  int
	nonEmpty int
	nbytes   int
}

func (a valueScore) greater(b valueScore) bool {
	if a.kind != b.kind {
		return a.kind > b.kind
	}
	if a.keys != b.keys {
		return a.keys > b.keys
	}
	if a.present != b.present {
		return a.present > b.present
	}
	aRank := a.nonEmpty*1_000_000 + a.nbytes
	bRank := b.nonEmpty*1_000_000 + b.nbytes
	return aRank > bRank
}

func (a valueScore) less(b valueScore) bool {
	return b.greater(a)
}

func scoreValue(value any) valueScore {
	switch v := value.(type) {
	case map[string]any:
		present, nonEmpty := 0, 0
		for _, item := range v {
			if item == nil {
				continue
			}
			present++
			switch t := item.(type) {
			case string:
				if strings.TrimSpace(t) == "" {
					continue
				}
			case []any:
				if len(t) == 0 {
					continue
				}
			case map[string]any:
				if len(t) == 0 {
					continue
				}
			}
			nonEmpty++
		}
		nbytes := 0
		if encoded, err := compactJSON(v); err == nil {
			nbytes = len(encoded)
		} else {
			nbytes = len(stringify(v))
		}
		return valueScore{kind: 3, keys: len(v), present: present, nonEmpty: nonEmpty, nbytes: nbytes}
	case []any:
		nbytes := 0
		if encoded, err := compactJSON(v); err == nil {
			nbytes = len(encoded)
		} else {
			nbytes = len(stringify(v))
		}
		return valueScore{kind: 2, keys: len(v), nbytes: nbytes}
	case nil:
		return valueScore{}
	default:
		text := stringify(v)
		present := 0
		if strings.TrimSpace(text) != "" {
			present = 1
		}
		return valueScore{kind: 1, keys: present, nbytes: len(text)}
	}
}

// mergeArgDicts mirrors Python _merge_tool_arg_dicts: prefer latest complete
// object as base, then fold non-conflicting extras.
func mergeArgDicts(values []any, toolName string) map[string]any {
	dicts := make([]map[string]any, 0, len(values))
	for _, v := range values {
		if d, ok := v.(map[string]any); ok {
			dicts = append(dicts, d)
		}
	}
	if len(dicts) == 0 {
		return nil
	}

	objComplete := func(d map[string]any) bool {
		text, err := compactJSON(NormalizeObjectForTool(d, toolName))
		if err != nil {
			return false
		}
		return CompleteJSON(text, toolName)
	}

	baseIdx := 0
	lastComplete := -1
	for i, d := range dicts {
		if objComplete(d) {
			lastComplete = i
		}
	}
	if lastComplete > 0 {
		baseIdx = lastComplete
	}

	if baseIdx > 0 {
		base := cloneMap(dicts[baseIdx])
		baseCanons := map[string]bool{}
		for k := range base {
			baseCanons[canonicalArgKey(k)] = true
		}
		for i, d := range dicts {
			if i == baseIdx {
				continue
			}
			for k, v := range d {
				canon := canonicalArgKey(k)
				if i < baseIdx && baseCanons[canon] {
					continue
				}
				if _, exists := base[k]; !exists {
					base[k] = v
					baseCanons[canon] = true
					continue
				}
				old := base[k]
				if empty(old) {
					base[k] = v
				} else if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
					if i > baseIdx {
						base[k] = v
					}
				} else if oldMap, ok1 := old.(map[string]any); ok1 {
					if newMap, ok2 := v.(map[string]any); ok2 {
						tmp := cloneMap(oldMap)
						for nk, nv := range newMap {
							tmp[nk] = nv
						}
						base[k] = tmp
					} else if i > baseIdx && !empty(v) {
						base[k] = v
					}
				} else if oldList, ok1 := old.([]any); ok1 {
					if newList, ok2 := v.([]any); ok2 && len(newList) >= len(oldList) {
						base[k] = v
					} else if i > baseIdx && !empty(v) {
						base[k] = v
					}
				} else if i > baseIdx && !empty(v) {
					base[k] = v
				}
			}
		}
		return NormalizeObjectForTool(base, toolName)
	}

	// No later complete base: sequential merge with path-strip on later path.
	merged := map[string]any{}
	for _, d := range dicts {
		if len(merged) > 0 && dictHasPathArg(d) {
			laterPathNonEmpty := false
			for k, v := range d {
				if isPathArgKey(k) && !empty(v) {
					laterPathNonEmpty = true
					break
				}
			}
			if laterPathNonEmpty {
				merged = stripPathArgs(merged)
			}
		}
		for k, v := range d {
			if _, exists := merged[k]; !exists {
				merged[k] = v
				continue
			}
			old := merged[k]
			if empty(old) {
				merged[k] = v
			} else if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
				// Later explicit empty string (delete match).
				merged[k] = v
			} else if empty(v) {
				continue
			} else if oldMap, ok1 := old.(map[string]any); ok1 {
				if newMap, ok2 := v.(map[string]any); ok2 {
					tmp := cloneMap(oldMap)
					for nk, nv := range newMap {
						tmp[nk] = nv
					}
					merged[k] = tmp
				} else {
					merged[k] = v
				}
			} else if oldList, ok1 := old.([]any); ok1 {
				if newList, ok2 := v.([]any); ok2 && len(newList) >= len(oldList) {
					merged[k] = v
				} else {
					merged[k] = v
				}
			} else if !empty(v) {
				merged[k] = v
			}
		}
	}
	return NormalizeObjectForTool(merged, toolName)
}

func isPathArgKey(key string) bool {
	return canonicalArgKey(key) == "file_path"
}

func dictHasPathArg(d map[string]any) bool {
	for k := range d {
		if isPathArgKey(k) {
			return true
		}
	}
	return false
}

func stripPathArgs(d map[string]any) map[string]any {
	out := make(map[string]any, len(d))
	for k, v := range d {
		if !isPathArgKey(k) {
			out[k] = v
		}
	}
	return out
}

// Merge merges tool argument stream pieces (delta or cumulative re-send).
// Mirrors Python merge_tool_argument_delta.
func Merge(current, incoming, toolName string) string {
	curRaw := ""
	if current != "" {
		curRaw = SanitizeJSON(current, toolName)
	}
	pieceRaw := ""
	if incoming != "" {
		pieceRaw = SanitizeJSON(incoming, toolName)
	}
	cur := ""
	if curRaw != "" {
		cur = NormalizeJSON(curRaw, toolName)
	}
	piece := ""
	if pieceRaw != "" {
		piece = NormalizeJSON(pieceRaw, toolName)
	}
	if piece == "" && pieceRaw == "" {
		return cur
	}
	// Whitespace-only pieces still matter mid-string: NormalizeJSON/SanitizeJSON
	// trim them to "", but dropping " " inside `"echo hello"` corrupts the
	// command. Only a truly empty incoming chunk is a no-op; when there is a
	// non-empty buffer, append the whitespace fragment to the raw form so the
	// next delta still lands on the correct byte boundary.
	if strings.TrimSpace(incoming) == "" {
		if curRaw != "" {
			return curRaw + incoming
		}
		return cur + incoming
	}
	if cur == "" && curRaw == "" {
		if piece != "" {
			return piece
		}
		return pieceRaw
	}
	if piece != "" && cur != "" && piece == cur {
		return cur
	}
	if piece != "" && cur != "" && strings.HasPrefix(piece, cur) {
		return piece
	}
	if cur != "" && piece != "" && strings.HasPrefix(cur, piece) {
		return cur
	}

	// Strict completeness for merge decisions: mid-stream fragments must not
	// be "repaired" into looking complete or the merge keeps a truncated
	// buffer and drops the live remainder of the tool call.
	curComplete := CompleteJSONStrict(cur, toolName)
	pieceComplete := CompleteJSONStrict(piece, toolName)
	curAny := isCompleteJSONText(cur)
	pieceAny := isCompleteJSONText(piece)

	bothDicts := false
	var a0, b0 any
	if errA := json.Unmarshal([]byte(firstNonEmpty(curRaw, cur, "null")), &a0); errA == nil {
		if errB := json.Unmarshal([]byte(firstNonEmpty(pieceRaw, piece, "null")), &b0); errB == nil {
			_, aIs := a0.(map[string]any)
			_, bIs := b0.(map[string]any)
			bothDicts = aIs && bIs
		}
	}

	if pieceComplete && !curComplete && !bothDicts {
		return piece
	}
	if curComplete && !pieceComplete && !bothDicts {
		return cur
	}
	_ = curAny
	_ = pieceAny

	// Structural merge on raw (pre-alias) objects so path/file_path coexist
	// until NormalizeObject applies preference.
	aText := firstNonEmpty(curRaw, cur)
	bText := firstNonEmpty(pieceRaw, piece)
	var a, b any
	if err := json.Unmarshal([]byte(aText), &a); err != nil {
		// Incomplete fragments: append raw, do not re-normalize yet (lossy).
		appended := aText + bText
		if n := NormalizeJSON(appended, toolName); n != "" && looksCompleteJSONValue(n) {
			return n
		}
		return appended
	}
	if err := json.Unmarshal([]byte(bText), &b); err != nil {
		appended := aText + bText
		if n := NormalizeJSON(appended, toolName); n != "" && looksCompleteJSONValue(n) {
			return n
		}
		return appended
	}
	if equal(a, b) {
		if cur != "" {
			return cur
		}
		return NormalizeJSON(curRaw, toolName)
	}

	aMap, aOK := a.(map[string]any)
	bMap, bOK := b.(map[string]any)
	if aOK && bOK {
		aNorm := NormalizeObjectForTool(aMap, toolName)
		bNorm := NormalizeObjectForTool(bMap, toolName)
		aCompleteObj := false
		bCompleteObj := false
		if text, err := compactJSON(aNorm); err == nil {
			aCompleteObj = CompleteJSONStrict(text, toolName)
		}
		if text, err := compactJSON(bNorm); err == nil {
			bCompleteObj = CompleteJSONStrict(text, toolName)
		}

		var merged map[string]any
		switch {
		case bCompleteObj && !aCompleteObj:
			// Later complete rewrite is authoritative.
			merged = cloneMap(bMap)
			bCanons := map[string]bool{}
			for k := range bMap {
				bCanons[canonicalArgKey(k)] = true
			}
			for k, v := range aMap {
				canon := canonicalArgKey(k)
				if bCanons[canon] {
					continue
				}
				if _, exists := merged[k]; !exists {
					merged[k] = v
				}
			}
		case aCompleteObj && !bCompleteObj:
			// Later fragment incomplete — keep complete early payload.
			merged = cloneMap(aMap)
			aCanons := map[string]bool{}
			for k := range aMap {
				aCanons[canonicalArgKey(k)] = true
			}
			for k, v := range bMap {
				canon := canonicalArgKey(k)
				if aCanons[canon] {
					continue
				}
				if _, exists := merged[k]; !exists {
					merged[k] = v
				}
			}
		case aCompleteObj && bCompleteObj:
			// BOTH complete: later rewrite is authoritative.
			merged = cloneMap(bMap)
			bCanons := map[string]bool{}
			for k := range bMap {
				bCanons[canonicalArgKey(k)] = true
			}
			for k, v := range aMap {
				canon := canonicalArgKey(k)
				if bCanons[canon] {
					continue
				}
				if _, exists := merged[k]; !exists {
					merged[k] = v
				}
			}
		default:
			// Both incomplete: later same-key wins; strip early path when later supplies path.
			merged = cloneMap(aMap)
			if dictHasPathArg(bMap) {
				merged = stripPathArgs(merged)
			}
			for k, v := range bMap {
				if _, exists := merged[k]; !exists {
					merged[k] = v
					continue
				}
				old := merged[k]
				if empty(old) {
					merged[k] = v
				} else if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
					merged[k] = v
				} else if oldMap, ok1 := old.(map[string]any); ok1 {
					if newMap, ok2 := v.(map[string]any); ok2 {
						tmp := cloneMap(oldMap)
						for nk, nv := range newMap {
							tmp[nk] = nv
						}
						merged[k] = tmp
					} else {
						merged[k] = v
					}
				} else if oldList, ok1 := old.([]any); ok1 {
					if newList, ok2 := v.([]any); ok2 && len(newList) >= len(oldList) {
						merged[k] = v
					} else {
						merged[k] = v
					}
				} else {
					merged[k] = v
				}
			}
		}
		mergedText, err := compactJSON(merged)
		if err != nil {
			if len(bMap) >= len(aMap) {
				return NormalizeJSON(firstNonEmpty(pieceRaw, piece), toolName)
			}
			return NormalizeJSON(firstNonEmpty(curRaw, cur), toolName)
		}
		return NormalizeJSON(mergedText, toolName)
	}

	aList, aListOK := a.([]any)
	bList, bListOK := b.([]any)
	if aListOK && bListOK {
		if len(bList) >= len(aList) {
			return NormalizeJSON(firstNonEmpty(pieceRaw, piece), toolName)
		}
		return NormalizeJSON(firstNonEmpty(curRaw, cur), toolName)
	}
	if (aOK || aListOK) && !(bOK || bListOK) {
		if cur != "" {
			return cur
		}
		return NormalizeJSON(curRaw, toolName)
	}
	if (bOK || bListOK) && !(aOK || aListOK) {
		if piece != "" {
			return piece
		}
		return NormalizeJSON(pieceRaw, toolName)
	}

	// Both incomplete / non-JSON-ish: append raw fragments so streaming
	// deltas accumulate byte-for-byte. NormalizeJSON of a partial payload is
	// lossy (it cannot decode an unterminated object and returns ""), so
	// appending normalized text would DROP earlier fragments — the next
	// delta then restarts from a shorter buffer and the tool call never
	// completes (intermittent Claude Code tool failures on char-level
	// upstream chunking). Callers pass the result back into Merge as
	// `current`, where SanitizeJSON+NormalizeJSON re-derive the normalized
	// form once the buffer finally parses.
	return firstNonEmpty(curRaw, cur) + firstNonEmpty(pieceRaw, piece)
}

func EffectiveJSON(raw string, toolName string) string {
	// 1) Normal path.
	text := strings.TrimSpace(NormalizeJSON(raw, toolName))
	if text != "" && looksCompleteJSONValue(text) {
		// Do NOT invent missing new_string here — that belongs only to
		// CoerceCompleteJSON (stream Finish / non-stream force-finish).
		// Mid-stream EffectiveJSON must leave path+old incomplete so the
		// real replace can still arrive before Claude Code accepts the tool.
		return text
	}
	// 2) Recover first complete JSON object when stream left trailing junk
	// (common intermittent Grok glitch: valid object + garbage suffix).
	if recovered := firstCompleteJSONFragment(raw); recovered != "" {
		if norm := strings.TrimSpace(NormalizeJSON(recovered, toolName)); norm != "" {
			return norm
		}
		return strings.TrimSpace(recovered)
	}
	// 3) Soft-repair truncated objects at stream end (missing closing braces/quotes).
	if repaired := repairTruncatedJSONObject(raw); repaired != "" {
		if norm := strings.TrimSpace(NormalizeJSON(repaired, toolName)); norm != "" {
			return norm
		}
		return repaired
	}
	if text != "" {
		return text
	}
	if len(requiredKeys(toolName)) > 0 {
		return ""
	}
	return "{}"
}

// CoerceCompleteJSON is a force-finish helper: EffectiveJSON + last-chance
// required-field fills so intermittent partial tool args still emit rather
// than vanishing mid-turn (Claude/Codex "Tool use interrupted" / tool disappeared).
//
// At stream end we MUST be aggressive: dropping a half tool after the client
// already saw content_block_start / function_call in_progress is worse than
// emitting a best-effort complete payload (empty new_string / recovered cmd).
func CoerceCompleteJSON(raw string, toolName string) string {
	// Force-finish path: recover JSON first, then invent missing new_string only here.
	text := strings.TrimSpace(NormalizeJSON(raw, toolName))
	if text == "" || !looksCompleteJSONValue(text) {
		if recovered := firstCompleteJSONFragment(raw); recovered != "" {
			if norm := strings.TrimSpace(NormalizeJSON(recovered, toolName)); norm != "" {
				text = norm
			} else {
				text = strings.TrimSpace(recovered)
			}
		} else if repaired := repairTruncatedJSONObject(raw); repaired != "" {
			if norm := strings.TrimSpace(NormalizeJSON(repaired, toolName)); norm != "" {
				text = norm
			} else {
				text = repaired
			}
		} else if text == "" {
			text = EffectiveJSON(raw, toolName)
		}
	}
	// Second pass: if still invalid JSON, try repair on both raw and text.
	if text == "" || !looksCompleteJSONValue(text) {
		for _, candidate := range []string{text, raw, EffectiveJSON(raw, toolName)} {
			if candidate == "" {
				continue
			}
			if looksCompleteJSONValue(candidate) {
				if norm := strings.TrimSpace(NormalizeJSON(candidate, toolName)); norm != "" {
					text = norm
					break
				}
				text = strings.TrimSpace(candidate)
				break
			}
			if repaired := repairTruncatedJSONObject(candidate); repaired != "" {
				if norm := strings.TrimSpace(NormalizeJSON(repaired, toolName)); norm != "" {
					text = norm
				} else {
					text = repaired
				}
				break
			}
		}
	}
	// Force-finish: fill default new_string for delete-match Update/Edit.
	if obj := parseObjectLoose(text); obj != nil && isEditTool(toolName) {
		obj = fillEditNewStringDefault(NormalizeObjectForTool(obj, toolName), toolName)
		if encoded, err := compactJSON(obj); err == nil {
			text = encoded
		}
	}
	if CompleteJSON(text, toolName) {
		return text
	}
	// Last chance: parse whatever object we can and apply edit/shell defaults again.
	if repaired := repairTruncatedJSONObject(text); repaired != "" {
		if norm := strings.TrimSpace(NormalizeJSON(repaired, toolName)); CompleteJSON(norm, toolName) {
			return norm
		}
		if CompleteJSON(repaired, toolName) {
			return repaired
		}
		text = repaired
	}
	// Shell: if we have any non-empty command-like field after normalize, force shape.
	if isShellTool(toolName) {
		if obj := parseObjectLoose(text); obj != nil {
			obj = applyShellDefaults(obj, toolName)
			if shellCommandNonEmpty(obj["command"]) || shellCommandNonEmpty(obj["cmd"]) {
				if _, ok := obj["command"]; !ok {
					if v, ok2 := obj["cmd"]; ok2 {
						obj["command"] = v
						delete(obj, "cmd")
					}
				}
				// Flatten argv → string for Codex.
				if cmd := normalizeShellCommand(obj["command"]); cmd != nil {
					obj["command"] = cmd
				}
				if encoded, err := compactJSON(obj); err == nil && CompleteJSON(encoded, toolName) {
					return encoded
				}
				// Soft salvage: keep any non-empty string command even if CompleteJSON picky.
				if s, ok := obj["command"].(string); ok && strings.TrimSpace(s) != "" {
					if encoded, err := compactJSON(map[string]any{"command": strings.TrimSpace(s)}); err == nil {
						return encoded
					}
				}
			}
		}
		// Extremely partial: try to extract "cmd"/"command":"..." from raw text.
		if salvaged := salvageShellCommandJSON(raw); salvaged != "" {
			return salvaged
		}
	}
	// apply_patch: promote patch/diff aliases and force-finish when input present.
	if isApplyPatchTool(toolName) {
		if obj := parseObjectLoose(text); obj != nil {
			obj = applyShellDefaults(obj, toolName)
			if v, ok := obj["input"]; ok && !empty(v) {
				if encoded, err := compactJSON(obj); err == nil && CompleteJSON(encoded, toolName) {
					return encoded
				}
			}
		}
		if salvaged := salvageApplyPatchJSON(raw); salvaged != "" {
			return salvaged
		}
	}
	// Read/Grep/Write: salvage partial objects that only have the primary key.
	if salvaged := salvageRequiredFieldJSON(raw, toolName); salvaged != "" {
		return salvaged
	}
	// Edit: if we at least have file_path+old_string, force empty new_string.
	if isEditTool(toolName) {
		if obj := parseObjectLoose(text); obj != nil {
			obj = fillEditNewStringDefault(NormalizeObjectForTool(obj, toolName), toolName)
			if encoded, err := compactJSON(obj); err == nil && CompleteJSON(encoded, toolName) {
				return encoded
			}
		}
	}
	return text
}

// salvageShellCommandJSON pulls a cmd/command string out of a broken JSON fragment.
func salvageShellCommandJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Prefer complete fragment first.
	if frag := firstCompleteJSONFragment(s); frag != "" {
		if obj := parseObjectLoose(frag); obj != nil {
			obj = applyShellDefaults(obj, "shell")
			if cmd := normalizeShellCommand(obj["command"]); cmd != nil {
				if encoded, err := compactJSON(map[string]any{"command": cmd}); err == nil {
					return encoded
				}
			}
			if shellCommandNonEmpty(obj["cmd"]) {
				if encoded, err := compactJSON(map[string]any{"command": obj["cmd"]}); err == nil {
					return encoded
				}
			}
		}
	}
	// Extract "cmd"|"command" : "..." from partial JSON.
	for _, key := range []string{`"cmd"`, `"command"`, `"shell_command"`, `"cmdline"`} {
		i := strings.Index(s, key)
		if i < 0 {
			continue
		}
		rest := s[i+len(key):]
		rest = strings.TrimLeft(rest, " \t\r\n:")
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" || rest[0] != '"' {
			continue
		}
		if val, ok := extractJSONString(rest); ok && strings.TrimSpace(val) != "" {
			if encoded, err := compactJSON(map[string]any{"command": strings.TrimSpace(val)}); err == nil {
				return encoded
			}
		}
	}
	return ""
}

func salvageApplyPatchJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if frag := firstCompleteJSONFragment(s); frag != "" {
		if obj := parseObjectLoose(frag); obj != nil {
			obj = applyShellDefaults(obj, "apply_patch")
			if v, ok := obj["input"]; ok && !empty(v) {
				if encoded, err := compactJSON(map[string]any{"input": v}); err == nil {
					return encoded
				}
			}
		}
	}
	for _, key := range []string{`"input"`, `"patch"`, `"diff"`, `"patch_text"`} {
		i := strings.Index(s, key)
		if i < 0 {
			continue
		}
		rest := strings.TrimLeft(s[i+len(key):], " \t\r\n:")
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" || rest[0] != '"' {
			continue
		}
		if val, ok := extractJSONString(rest); ok && strings.TrimSpace(val) != "" {
			if encoded, err := compactJSON(map[string]any{"input": val}); err == nil {
				return encoded
			}
		}
	}
	return ""
}

// salvageRequiredFieldJSON recovers minimal {required: value} objects for Read/Grep/etc.
func salvageRequiredFieldJSON(raw, toolName string) string {
	keys := requiredKeys(toolName)
	if len(keys) == 0 {
		return ""
	}
	// Prefer full object salvage first.
	if frag := firstCompleteJSONFragment(raw); frag != "" {
		if obj := parseObjectLoose(frag); obj != nil {
			obj = NormalizeObjectForTool(obj, toolName)
			if isEditTool(toolName) {
				obj = fillEditNewStringDefault(obj, toolName)
			}
			if encoded, err := compactJSON(obj); err == nil && CompleteJSON(encoded, toolName) {
				return encoded
			}
		}
	}
	if repaired := repairTruncatedJSONObject(raw); repaired != "" {
		if norm := strings.TrimSpace(NormalizeJSON(repaired, toolName)); CompleteJSON(norm, toolName) {
			return norm
		}
	}
	// Single-field tools (Read/Grep/WebFetch): extract primary key.
	primary := keys[0]
	searchKeys := []string{`"` + primary + `"`}
	switch primary {
	case "file_path":
		searchKeys = append(searchKeys, `"path"`, `"file"`, `"target_file"`, `"filepath"`)
	case "pattern":
		searchKeys = append(searchKeys, `"regex"`, `"glob"`, `"query"`)
	case "query":
		searchKeys = append(searchKeys, `"q"`, `"search"`, `"search_query"`)
	case "url":
		searchKeys = append(searchKeys, `"uri"`, `"href"`)
	case "content":
		searchKeys = append(searchKeys, `"contents"`, `"file_content"`, `"text"`)
	}
	s := strings.TrimSpace(raw)
	for _, key := range searchKeys {
		i := strings.Index(s, key)
		if i < 0 {
			continue
		}
		rest := strings.TrimLeft(s[i+len(key):], " \t\r\n:")
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" || rest[0] != '"' {
			continue
		}
		val, ok := extractJSONString(rest)
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" {
			continue
		}
		obj := map[string]any{primary: val}
		if isEditTool(toolName) {
			obj = fillEditNewStringDefault(NormalizeObjectForTool(obj, toolName), toolName)
		}
		if encoded, err := compactJSON(obj); err == nil && CompleteJSON(encoded, toolName) {
			return encoded
		}
		// Read only needs file_path.
		if primary == "file_path" && len(keys) == 1 {
			if encoded, err := compactJSON(map[string]any{"file_path": val}); err == nil {
				return encoded
			}
		}
	}
	return ""
}

// extractJSONString parses a JSON string starting at rest[0]=='"'.
func extractJSONString(rest string) (string, bool) {
	if rest == "" || rest[0] != '"' {
		return "", false
	}
	var out strings.Builder
	esc := false
	for j := 1; j < len(rest); j++ {
		ch := rest[j]
		if esc {
			out.WriteByte(ch)
			esc = false
			continue
		}
		if ch == '\\' {
			esc = true
			out.WriteByte(ch)
			continue
		}
		if ch == '"' {
			val := out.String()
			var unq string
			if json.Unmarshal([]byte(`"`+val+`"`), &unq) == nil {
				return unq, true
			}
			return val, true
		}
		out.WriteByte(ch)
	}
	return "", false
}

func CompleteJSONStrict(raw, toolName string) bool {
	text := strings.TrimSpace(NormalizeJSON(raw, toolName))
	if text == "" || (text[0] != '{' && text[0] != '[') {
		return false
	}
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return false
	}
	switch typed := value.(type) {
	case []any:
		return len(typed) > 0 || len(requiredKeys(toolName)) == 0
	case map[string]any:
		if norm := NormalizeObjectForTool(typed, toolName); norm != nil {
			typed = norm
		}
		keys := requiredKeys(toolName)
		if len(typed) == 0 {
			return len(keys) == 0
		}
		for _, key := range keys {
			item, ok := typed[key]
			if !ok || item == nil {
				if key == "command" && isShellTool(toolName) {
					if shellCommandNonEmpty(typed["cmd"]) {
						continue
					}
				}
				return false
			}
			if key == "command" && isShellTool(toolName) {
				if !shellCommandNonEmpty(item) && !shellCommandNonEmpty(typed["cmd"]) {
					return false
				}
				continue
			}
			switch v := item.(type) {
			case string:
				if strings.TrimSpace(v) == "" && !emptyStringOK[key] {
					return false
				}
			case []any:
				if len(v) == 0 {
					return false
				}
			case map[string]any:
				if len(v) == 0 {
					return false
				}
			}
		}
		return true
	default:
		return false
	}
}

func CompleteJSON(raw string, toolName string) bool {
	// Prefer EffectiveJSON recovery so trailing junk / mild truncation does not
	// intermittently fail completeness after a successful stream merge.
	text := strings.TrimSpace(EffectiveJSON(raw, toolName))
	if text == "" || (text[0] != '{' && text[0] != '[') {
		return false
	}
	var value any
	// Unmarshal first value only — do NOT reject on trailing whitespace that
	// Decoder.More() would flag (intermittent failures after model padding).
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		// One more recovery attempt for slightly broken JSON.
		if frag := firstCompleteJSONFragment(text); frag != "" {
			if err2 := json.Unmarshal([]byte(frag), &value); err2 != nil {
				return false
			}
			text = frag
		} else {
			return false
		}
	}
	switch typed := value.(type) {
	case []any:
		return len(typed) > 0 || len(requiredKeys(toolName)) == 0
	case map[string]any:
		// Re-normalize so alias keys (cmd/path/search) count toward required fields.
		if norm := NormalizeObjectForTool(typed, toolName); norm != nil {
			typed = norm
		}
		// Missing new_string is incomplete for Edit/Update until CoerceCompleteJSON
		// fills "" at force-finish. This prevents mid-stream delete-match emits.
		keys := requiredKeys(toolName)
		if len(typed) == 0 {
			return len(keys) == 0
		}
		for _, key := range keys {
			item, ok := typed[key]
			if !ok || item == nil {
				// Shell: accept cmd when command missing (pre-projection).
				if key == "command" && isShellTool(toolName) {
					if shellCommandNonEmpty(typed["cmd"]) {
						continue
					}
				}
				return false
			}
			// Codex shell: command may be string or argv array; reject nested empties.
			if key == "command" && isShellTool(toolName) {
				if !shellCommandNonEmpty(item) && !shellCommandNonEmpty(typed["cmd"]) {
					return false
				}
				continue
			}
			if key == "input" && isApplyPatchTool(toolName) {
				switch v := item.(type) {
				case string:
					if strings.TrimSpace(v) == "" {
						return false
					}
				default:
					if empty(item) {
						return false
					}
				}
				continue
			}
			switch v := item.(type) {
			case string:
				if strings.TrimSpace(v) == "" && !emptyStringOK[key] {
					return false
				}
			case []any:
				if len(v) == 0 {
					return false
				}
				// Arrays of only empty strings are incomplete for required fields.
				if key == "command" && !shellCommandNonEmpty(v) {
					return false
				}
			case map[string]any:
				if len(v) == 0 {
					return false
				}
			}
		}
		return true
	default:
		return false
	}
}

// firstCompleteJSONFragment returns the first complete JSON value in src when
// the stream left trailing garbage after a valid object/array.
func firstCompleteJSONFragment(src string) string {
	src = strings.TrimSpace(src)
	if src == "" {
		return ""
	}
	// Unwrap a single JSON string layer that itself holds an object.
	if len(src) >= 2 && src[0] == '"' {
		var unquoted string
		if json.Unmarshal([]byte(src), &unquoted) == nil {
			unquoted = strings.TrimSpace(unquoted)
			if unquoted != "" && (unquoted[0] == '{' || unquoted[0] == '[') {
				src = unquoted
			}
		}
	}
	if json.Valid([]byte(src)) {
		return src
	}
	values, ends := decodeJSONValues(src)
	if len(values) == 0 || len(ends) == 0 {
		return ""
	}
	// Prefer last complete object (authoritative rewrite), else first.
	for i := len(values) - 1; i >= 0; i-- {
		if d, ok := values[i].(map[string]any); ok && len(d) > 0 {
			if encoded, err := compactJSON(d); err == nil && json.Valid([]byte(encoded)) {
				return encoded
			}
		}
	}
	start := 0
	if len(ends) > 0 {
		// Text for first value.
		frag := strings.TrimSpace(src[:ends[0]])
		if json.Valid([]byte(frag)) {
			return frag
		}
	}
	_ = start
	if encoded, err := compactJSON(values[0]); err == nil {
		return encoded
	}
	return ""
}

// repairTruncatedJSONObject attempts to close a truncated {...} payload that
// Grok sometimes ends mid-stream without a final brace. Conservative: only
// when braces are unbalanced and a single root object is present.
func repairTruncatedJSONObject(src string) string {
	src = strings.TrimSpace(src)
	if src == "" || src[0] != '{' {
		return ""
	}
	if json.Valid([]byte(src)) {
		return src
	}
	// Strip trailing commas before close attempts.
	trimmed := strings.TrimRight(src, " \t\r\n")
	for strings.HasSuffix(trimmed, ",") {
		trimmed = strings.TrimSpace(trimmed[:len(trimmed)-1])
	}
	// Close open strings if odd number of unescaped quotes after last key-ish pattern.
	if repaired := closeOpenJSONString(trimmed); repaired != "" {
		trimmed = repaired
	}
	open, closeN := 0, 0
	inStr := false
	esc := false
	for i := 0; i < len(trimmed); i++ {
		ch := trimmed[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			if ch == '\\' {
				esc = true
				continue
			}
			if ch == '"' {
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			open++
		case '}':
			closeN++
		}
	}
	// Allow deeper truncation (long nested args). Only give up when braces
	// are already balanced but still invalid, or massively unbalanced (>12).
	if open <= closeN || open-closeN > 12 {
		// Already balanced-but-invalid, or hopelessly truncated.
		if !inStr {
			return ""
		}
		// Close dangling string then braces.
		trimmed += `"`
		open, closeN = 0, 0
		inStr, esc = false, false
		for i := 0; i < len(trimmed); i++ {
			ch := trimmed[i]
			if inStr {
				if esc {
					esc = false
					continue
				}
				if ch == '\\' {
					esc = true
					continue
				}
				if ch == '"' {
					inStr = false
				}
				continue
			}
			switch ch {
			case '"':
				inStr = true
			case '{':
				open++
			case '}':
				closeN++
			}
		}
	}
	for closeN < open {
		trimmed += "}"
		closeN++
	}
	if !json.Valid([]byte(trimmed)) {
		return ""
	}
	return trimmed
}

func closeOpenJSONString(src string) string {
	inStr := false
	esc := false
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if !inStr {
			if ch == '"' {
				inStr = true
			}
			continue
		}
		if esc {
			esc = false
			continue
		}
		if ch == '\\' {
			esc = true
			continue
		}
		if ch == '"' {
			inStr = false
		}
	}
	if !inStr {
		return ""
	}
	return src + `"`
}

func looksCompleteJSONValue(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	return json.Valid([]byte(text))
}

func parseObjectLoose(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) == nil {
		return obj
	}
	if frag := firstCompleteJSONFragment(raw); frag != "" {
		if json.Unmarshal([]byte(frag), &obj) == nil {
			return obj
		}
	}
	if repaired := repairTruncatedJSONObject(raw); repaired != "" {
		if json.Unmarshal([]byte(repaired), &obj) == nil {
			return obj
		}
	}
	return nil
}

func isCompleteJSONText(s string) bool {
	text := strings.TrimSpace(s)
	if text == "" {
		return false
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	return !decoder.More()
}

func empty(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	default:
		return false
	}
}

func equal(left, right any) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for k, v := range input {
		out[k] = v
	}
	return out
}

// compactJSON encodes values with stable map key order so edit/shell tool
// arguments keep a deterministic client-facing shape (file_path before
// old_string/new_string). encoding/json map iteration order is not stable
// for string equality checks used by clients and tests.
func compactJSON(value any) (string, error) {
	return marshalStable(value)
}

func marshalStable(value any) (string, error) {
	switch v := value.(type) {
	case map[string]any:
		return encodeMapStable(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			s, err := marshalStable(item)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	default:
		encoded, err := marshalNoHTML(value)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	}
}

func encodeMapStable(m map[string]any) (string, error) {
	if m == nil {
		return "null", nil
	}
	// Preferred key order for Claude/Codex tool schemas.
	prefer := []string{
		"file_path", "old_string", "new_string", "content",
		"command", "cmd", "input", "pattern", "query", "url",
		"path", "notebook_path", "taskId", "status",
	}
	keys := make([]string, 0, len(m))
	seen := make(map[string]bool, len(m))
	for _, k := range prefer {
		if _, ok := m[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	rest := make([]string, 0, len(m))
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		ek, err := marshalNoHTML(k)
		if err != nil {
			return "", err
		}
		ev, err := marshalStable(m[k])
		if err != nil {
			return "", err
		}
		b.Write(ek)
		b.WriteByte(':')
		b.WriteString(ev)
	}
	b.WriteByte('}')
	return b.String(), nil
}

func stringify(value any) string {
	encoded, err := marshalNoHTML(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// PreferredShellArgKey inspects a tool parameters schema and returns the
// parameter name the *client* expects for the shell command payload.
// Codex historically uses "cmd"; OpenAI/Claude-style tools use "command".
func PreferredShellArgKey(parameters any) string {
	params, _ := parameters.(map[string]any)
	if params == nil {
		return "command"
	}
	props, _ := params["properties"].(map[string]any)
	req, _ := params["required"].([]any)
	has := func(name string) bool {
		if props != nil {
			if _, ok := props[name]; ok {
				return true
			}
		}
		for _, item := range req {
			if strings.EqualFold(strings.TrimSpace(fmt.Sprint(item)), name) {
				return true
			}
		}
		return false
	}
	// Prefer explicit required/property order used by the client.
	// Codex: required ["cmd"] or properties.cmd without command.
	if has("cmd") && !has("command") {
		return "cmd"
	}
	if has("command") && !has("cmd") {
		return "command"
	}
	// Both present: prefer required list first item that matches.
	for _, item := range req {
		key := strings.ToLower(strings.TrimSpace(fmt.Sprint(item)))
		if key == "cmd" || key == "command" {
			return key
		}
	}
	if has("cmd") {
		return "cmd"
	}
	return "command"
}

// PreferredShellArgKeyFromTool extracts preferred key from a Responses/OpenAI tool object.
func PreferredShellArgKeyFromTool(tool map[string]any) string {
	if tool == nil {
		return "command"
	}
	if fn, ok := tool["function"].(map[string]any); ok {
		return PreferredShellArgKey(firstNonNil(fn["parameters"], fn["input_schema"], tool["parameters"], tool["input_schema"]))
	}
	return PreferredShellArgKey(firstNonNil(tool["parameters"], tool["input_schema"]))
}

// ShellArgKeyMap builds tool-name → preferred shell arg key from a tools array.
func ShellArgKeyMap(tools any) map[string]string {
	out := map[string]string{}
	items, ok := tools.([]any)
	if !ok {
		return out
	}
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := ""
		if fn, ok := tool["function"].(map[string]any); ok {
			name = strings.TrimSpace(fmt.Sprint(fn["name"]))
		}
		if name == "" {
			name = strings.TrimSpace(fmt.Sprint(tool["name"]))
		}
		if name == "" || !isShellTool(name) {
			continue
		}
		out[name] = PreferredShellArgKeyFromTool(tool)
		// also store lower-case + nameKey for lookup tolerance (exec_command / ExecCommand).
		out[strings.ToLower(name)] = out[name]
		if nk := nameKey(name); nk != "" {
			out[nk] = out[name]
		}
	}
	return out
}

// ProjectShellArgsForClient rewrites internal {"command":...} args into the
// parameter name expected by the client schema (often Codex "cmd").
// Non-shell tools / non-objects are returned unchanged.
//
// Default preferred key is "cmd" (Codex schema). Pass preferredKey="command"
// explicitly for pure OpenAI/Claude shell tools that only declare "command".
// Always emits a STRING for the preferred key — never argv arrays — so Codex
// does not reject the tool call and retry with "参数名应是 cmd，不是 command".
func ProjectShellArgsForClient(argsJSON, toolName, preferredKey string) string {
	preferredKey = strings.TrimSpace(preferredKey)
	// When caller already decided preferredKey=cmd (Codex path), always project
	// even if tool name is exotic / namespaced and isShellTool missed it.
	force := preferredKey == "cmd" || preferredKey == "command"
	if !isShellTool(toolName) && !force {
		return argsJSON
	}
	if preferredKey == "" {
		// Codex validates shell tools against local schema field "cmd".
		// Hermes "terminal" (and any other command-style shell) uses DefaultShellArgKey.
		preferredKey = DefaultShellArgKey(toolName)
	}
	// Passthrough dialect: never rewrite command text for PowerShell/bash.
	dialect := DialectPOSIX
	text := strings.TrimSpace(argsJSON)
	if text == "" {
		return argsJSON
	}
	// Unwrap accidental double-encoded JSON strings: "\"{\\\"command\\\":\\\"x\\\"}\""
	for i := 0; i < 2; i++ {
		if len(text) >= 2 && text[0] == '"' {
			var unquoted string
			if json.Unmarshal([]byte(text), &unquoted) == nil && strings.TrimSpace(unquoted) != "" {
				text = strings.TrimSpace(unquoted)
				continue
			}
		}
		break
	}
	// Start from normalized internal form (command key). Use a shell-family name
	// for alias maps when the real tool name is namespaced / unknown.
	normName := toolName
	if !isShellTool(normName) && force {
		normName = "shell"
	}
	normalized := NormalizeJSON(text, normName)
	if strings.TrimSpace(normalized) == "" {
		normalized = text
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(normalized), &obj); err != nil {
		if err2 := json.Unmarshal([]byte(text), &obj); err2 != nil {
			return argsJSON
		}
	}
	// Collect command value from command or any known alias.
	var val any
	for _, k := range []string{"command", "cmd", "argv", "args", "shell_command", "cmdline", "command_line", "script", "bash", "line"} {
		if v, ok := obj[k]; ok && !empty(v) {
			val = v
			break
		}
	}
	if val == nil {
		for _, k := range []string{"command", "cmd"} {
			if v, ok := obj[k]; ok {
				val = v
				break
			}
		}
	}
	if val == nil {
		return normalized
	}
	// Codex / OpenAI shell clients require a STRING for cmd/command — never argv
	// arrays. Grok frequently emits ["ls","-la"] or nested lists; flatten+join
	// at the final client projection boundary.
	// Use client dialect so Linux bash keeps quote-glue while Codex PowerShell gets
	// PS-safe quotes + glue-strip — never bake PS rewrites into internal POSIX normalize.
	var strVal any
	switch v := val.(type) {
	case []any:
		if flat := flattenCommandParts(v); len(flat) > 0 {
			strVal = joinShellArgv(flat, dialect)
		}
	case []string:
		tmp := make([]any, len(v))
		for i, s := range v {
			tmp[i] = s
		}
		if flat := flattenCommandParts(tmp); len(flat) > 0 {
			strVal = joinShellArgv(flat, dialect)
		}
	default:
		strVal = normalizeShellCommand(val)
		// JSON-array string: flatten to a single command string (no dialect rewrite).
		if s, ok := val.(string); ok {
			s = strings.TrimSpace(s)
			if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
				var arr []any
				if json.Unmarshal([]byte(s), &arr) == nil {
					if flat := flattenCommandParts(arr); len(flat) > 0 {
						strVal = joinShellArgv(flat, dialect)
					}
				}
			}
		}
	}
	if strVal == nil {
		// Last resort for exotic non-string/non-array leftovers.
		if s, ok := val.(string); ok {
			strVal = strings.TrimSpace(s)
		} else if encoded, err := compactJSON(val); err == nil && strings.HasPrefix(encoded, "[") {
			var arr []any
			if json.Unmarshal([]byte(encoded), &arr) == nil {
				if flat := flattenCommandParts(arr); len(flat) > 0 {
					strVal = joinShellArgv(flat, dialect)
				}
			}
		} else if val != nil {
			if s := strings.TrimSpace(stringify(val)); s != "" && s != "null" && s != "[]" && s != "{}" {
				strVal = s
			}
		}
	}
	if strVal == nil {
		return normalized
	}
	// Passthrough: do not rewrite command text. Only remap key + keep extras.
	// workdir/timeout/justification must survive — Codex App relies on workdir
	// for project-root anchoring (relative paths).
	sanitizerApplied := false
	// Unwrap accidental nested shells: pwsh -Command '...' / powershell -Command "..."
	// when the host executor is already PowerShell (common Codex Desktop waste).
	if s, ok := strVal.(string); ok {
		if unwrapped, ok := unwrapNestedPowerShellCommand(s); ok {
			strVal = unwrapped
			sanitizerApplied = true
		}
	}
	// Rebuild: preferred key first, keep non-alias extras (workdir, timeout, ...).
	out := map[string]any{preferredKey: strVal}
	for k, v := range obj {
		lk := strings.ToLower(strings.TrimSpace(k))
		switch lk {
		case "command", "cmd", "argv", "args", "shell_command", "cmdline", "command_line", "script", "bash", "line":
			continue
		default:
			out[k] = coerceShellNumericField(lk, v)
		}
	}
	encoded, err := compactJSON(out)
	if err != nil {
		maybeLogShellArgsProjection(toolName, preferredKey, dialect, sanitizerApplied, argsJSON, normalized)
		return normalized
	}
	maybeLogShellArgsProjection(toolName, preferredKey, dialect, sanitizerApplied, argsJSON, encoded)
	return encoded
}


// coerceShellNumericField converts JSON float-looking numbers for Codex shell
// integer fields (yield_time_ms, max_output_tokens, timeout, etc.) to int64.
// Codex rejects float literals such as 10000.0 with "expected u64".
func coerceShellNumericField(key string, value any) any {
	lk := strings.ToLower(strings.TrimSpace(key))
	switch lk {
	case "yield_time_ms", "yield_ms", "timeout_ms", "timeout", "max_output_tokens", "max_tokens":
		// coerce below
	default:
		if !strings.HasSuffix(lk, "_ms") && !strings.HasSuffix(lk, "_tokens") {
			return value
		}
	}
	switch v := value.(type) {
	case float64:
		if v == float64(int64(v)) {
			return int64(v)
		}
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i
		}
		if f, err := v.Float64(); err == nil && f == float64(int64(f)) {
			return int64(f)
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return value
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && f == float64(int64(f)) {
			return int64(f)
		}
	}
	return value
}

// unwrapNestedPowerShellCommand strips a single redundant outer
// pwsh/powershell -Command/-c wrapper when the payload is already a full
// PowerShell script. Avoids "ScriptBlock should only be specified as a value
// of the Command parameter" when host shell is already pwsh.
func unwrapNestedPowerShellCommand(cmd string) (string, bool) {
	s := strings.TrimSpace(cmd)
	if s == "" {
		return cmd, false
	}

	idx, flagLen := findPowerShellCommandFlag(s)
	if idx < 0 {
		return cmd, false
	}

	prefix := strings.TrimSpace(s[:idx])
	if !isPowerShellLauncherPrefix(prefix) {
		return cmd, false
	}

	body := strings.TrimSpace(s[idx+flagLen:])
	if body == "" {
		return cmd, false
	}
	if len(body) >= 2 {
		q := body[0]
		if (q == '\'' || q == '"') && body[len(body)-1] == q {
			inner := body[1 : len(body)-1]
			if q == '\'' {
				inner = strings.ReplaceAll(inner, "''", "'")
			}
			body = inner
		}
	}
	body = strings.TrimSpace(body)
	if body == "" || body == s {
		return cmd, false
	}
	return body, true
}

func findPowerShellCommandFlag(s string) (idx, flagLen int) {
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			continue
		}
		if i > 0 && s[i-1] != ' ' && s[i-1] != '\t' {
			continue
		}
		rest := s[i+1:]
		rl := strings.ToLower(rest)
		if strings.HasPrefix(rl, "command") {
			end := len("command")
			if end < len(rest) && (rest[end] == ' ' || rest[end] == '\t' || rest[end] == '=') {
				flagLen = 1 + end
				if rest[end] == '=' {
					flagLen++
				}
				return i, flagLen
			}
			continue
		}
		// Only bare -c (not -Confirm/-Configuration).
		if strings.HasPrefix(rl, "c") && len(rest) >= 2 {
			n := rest[1]
			if n == ' ' || n == '\t' || n == '=' {
				flagLen = 2
				if n == '=' {
					flagLen = 3
				}
				return i, flagLen
			}
		}
	}
	return -1, 0
}

// isPowerShellLauncherPrefix accepts only a pure pwsh/powershell executable
// invocation with optional common switches (-NoProfile, -NonInteractive, ...).
// It rejects prose such as: Write-Output 'pwsh -Command demo'
func isPowerShellLauncherPrefix(prefix string) bool {
	p := strings.TrimSpace(prefix)
	if p == "" {
		return false
	}
	if strings.ContainsAny(p, "|;") || strings.Contains(p, "\n") {
		return false
	}
	fields := strings.Fields(p)
	if len(fields) == 0 {
		return false
	}
	exe := strings.Trim(fields[0], `"'`)
	base := strings.ToLower(exe)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if base != "pwsh" && base != "pwsh.exe" && base != "powershell" && base != "powershell.exe" {
		return false
	}
	for _, f := range fields[1:] {
		fl := strings.ToLower(strings.Trim(f, `"'`))
		if fl == "" {
			continue
		}
		if !strings.HasPrefix(fl, "-") {
			return false
		}
		// Allow short launcher switches only.
		if len(fl) > 24 || strings.ContainsAny(fl, "(){}") {
			return false
		}
	}
	return true
}

func firstNonNil(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// debugShellArgs is the durable admin setting (WebUI → 系统设置 → Relay).
// Hot-reloaded via ConfigureDebugShellArgs; default off.
var (
	debugShellArgsMu      sync.RWMutex
	debugShellArgsEnabled bool
	shellArgsDebugOnce    sync.Once
	// shellArgsLastSig dedups identical projection log lines from stream reassembly.
	shellArgsDebugMu sync.Mutex
	shellArgsLastSig string
)

// ConfigureDebugShellArgs turns on/off shell cmd projection logging from
// app_settings.debug_shell_args (admin WebUI). Hot without restart.
func ConfigureDebugShellArgs(enabled bool) {
	debugShellArgsMu.Lock()
	prev := debugShellArgsEnabled
	debugShellArgsEnabled = enabled
	debugShellArgsMu.Unlock()
	if enabled && !prev {
		slog.Info("shell args debug enabled", "source", "admin_settings", "key", "debug_shell_args")
	} else if !enabled && prev {
		slog.Info("shell args debug disabled", "source", "admin_settings", "key", "debug_shell_args")
	}
}

// DebugShellArgsEnabled reports whether shell projection debug logging is on.
func DebugShellArgsEnabled() bool {
	debugShellArgsMu.RLock()
	defer debugShellArgsMu.RUnlock()
	return debugShellArgsEnabled
}

func shellArgsDebugEnabled() bool {
	return DebugShellArgsEnabled()
}

func maybeLogShellArgsProjection(toolName, preferredKey string, dialect shellDialect, sanitizerApplied bool, raw, projected string) {
	if !shellArgsDebugEnabled() {
		return
	}
	shellArgsDebugOnce.Do(func() {
		slog.Info("shell args projection logging active")
	})
	rawCmd := extractShellCmdValue(raw)
	outCmd := extractShellCmdValue(projected)
	inputKind := classifyShellCommandInputKind(raw)
	changed := rawCmd != outCmd
	// Dedup stream spam; include input_kind so array residual is visible once.
	shellArgsDebugMu.Lock()
	sig := toolName + "|" + preferredKey + "|" + inputKind + "|" + rawCmd + "|" + outCmd
	same := sig == shellArgsLastSig && !changed
	if !same {
		shellArgsLastSig = sig
	}
	shellArgsDebugMu.Unlock()
	if same && !sanitizerApplied {
		return
	}
	// workdir preserved?
	hasWorkdir := false
	var obj map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(projected)), &obj) == nil {
		if v, ok := obj["workdir"]; ok && !empty(v) {
			hasWorkdir = true
		}
		if v, ok := obj["working_directory"]; ok && !empty(v) {
			hasWorkdir = true
		}
	}
	slog.Info("shell args projection",
		"tool", toolName,
		"preferred_key", preferredKey,
		"dialect", shellDialectName(dialect),
		"input_kind", inputKind,
		"changed", changed,
		"has_workdir", hasWorkdir,
		"raw_cmd", truncateForLog(rawCmd, 240),
		"projected_cmd", truncateForLog(outCmd, 240),
		"raw_len", len(raw),
		"projected_len", len(projected),
	)
}

// classifyShellCommandInputKind reports how the shell command was encoded in
// the raw tool arguments (before projection). Used only for debug logs.
func classifyShellCommandInputKind(argsJSON string) string {
	text := strings.TrimSpace(argsJSON)
	if text == "" {
		return "empty"
	}
	var obj map[string]any
	if json.Unmarshal([]byte(text), &obj) != nil {
		return "invalid"
	}
	for _, k := range []string{"cmd", "command", "argv", "args", "shell_command", "cmdline", "command_line", "script", "bash", "line"} {
		v, ok := obj[k]
		if !ok || empty(v) {
			continue
		}
		switch t := v.(type) {
		case []any, []string:
			return "array"
		case string:
			s := strings.TrimSpace(t)
			if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
				var arr []any
				if json.Unmarshal([]byte(s), &arr) == nil {
					return "array"
				}
			}
			return "string"
		default:
			if encoded, err := compactJSON(t); err == nil && strings.HasPrefix(encoded, "[") {
				return "array"
			}
			return "other"
		}
	}
	return "none"
}

func extractShellCmdValue(argsJSON string) string {
	text := strings.TrimSpace(argsJSON)
	if text == "" {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(text), &obj) != nil {
		return text
	}
	for _, k := range []string{"cmd", "command", "argv", "args", "shell_command", "cmdline"} {
		if v, ok := obj[k]; ok && !empty(v) {
			if s, ok := v.(string); ok {
				return s
			}
			return stringify(v)
		}
	}
	return text
}

func truncateForLog(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Codex shell outbound policy (cache-safe):
// - Auto-detect Codex via historycompact.LooksLikeCodexRequest / tools schema.
// - Project shell args: command↔cmd key remap + argv flatten only.
// - Preserve workdir/timeout and other non-command fields.
// - Do NOT inject PowerShell instructions into the prompt (would bust cache).
// - Do NOT rewrite command text (bash↔PS heuristics removed).
// - Upstream shell schema is string-only; residual arrays are flattened without bash-style single quotes.


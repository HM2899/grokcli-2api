package toolcall

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
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
	"newstring": "new_string", "newstr": "new_string", "newtext": "new_string", "new": "new_string", "new_text": "new_string", "replacement": "new_string", "replace_with": "new_string",
	"contents": "content", "filecontent": "content", "file_content": "content", "filecontents": "content",
	"notebookpath": "notebook_path", "notebook": "notebook_path",
	"cmd": "command", "shell_command": "command",
	"q": "query", "search": "query", "search_query": "query",
	"uri": "url", "href": "url", "regex": "pattern", "glob_pattern": "pattern",
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

type chosenValue struct {
	value     any
	canonical bool
}

// NormalizeObject renames common alternate tool-arg keys to Claude Code schema
// names. Later conflicting non-empty values win (authoritative rewrite).
func NormalizeObject(input map[string]any) map[string]any {
	chosen := make(map[string]chosenValue, len(input))
	// Preserve insertion order of first-seen canonical keys for stable encode.
	order := make([]string, 0, len(input))
	for raw, value := range input {
		canonical := canonicalArgKey(raw)
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
		// Different non-empty values: later wins.
		chosen[canonical] = chosenValue{value: value, canonical: raw == canonical}
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
	return out
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
		canonical := canonicalArgKey(pair.key)
		current, exists := chosen[canonical]
		if !exists {
			order = append(order, canonical)
			chosen[canonical] = chosenValue{value: pair.value, canonical: pair.key == canonical}
			continue
		}
		oldEmpty, newEmpty := empty(current.value), empty(pair.value)
		if oldEmpty && !newEmpty {
			chosen[canonical] = chosenValue{value: pair.value, canonical: pair.key == canonical}
			continue
		}
		if newEmpty {
			continue
		}
		if equal(current.value, pair.value) {
			if pair.key == canonical && !current.canonical {
				chosen[canonical] = chosenValue{value: pair.value, canonical: true}
			}
			continue
		}
		// Later conflicting aliases represent a later authoritative rewrite.
		chosen[canonical] = chosenValue{value: pair.value, canonical: pair.key == canonical}
	}
	return encodeObjectOrdered(order, chosen)
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

func encodeObjectOrdered(order []string, chosen map[string]chosenValue) string {
	var output bytes.Buffer
	output.WriteByte('{')
	for index, key := range order {
		if index > 0 {
			output.WriteByte(',')
		}
		encodedKey, _ := json.Marshal(key)
		encodedValue, err := json.Marshal(chosen[key].value)
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
		text, err := compactJSON(NormalizeObject(d))
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
		return NormalizeObject(base)
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
	return NormalizeObject(merged)
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

	curComplete := CompleteJSON(cur, toolName)
	pieceComplete := CompleteJSON(piece, toolName)
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
		return NormalizeJSON(aText+bText, toolName)
	}
	if err := json.Unmarshal([]byte(bText), &b); err != nil {
		return NormalizeJSON(aText+bText, toolName)
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
		aNorm := NormalizeObject(aMap)
		bNorm := NormalizeObject(bMap)
		aCompleteObj := false
		bCompleteObj := false
		if text, err := compactJSON(aNorm); err == nil {
			aCompleteObj = CompleteJSON(text, toolName)
		}
		if text, err := compactJSON(bNorm); err == nil {
			bCompleteObj = CompleteJSON(text, toolName)
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

	// Both incomplete / non-JSON-ish: only append when it looks like a true delta.
	return NormalizeJSON(firstNonEmpty(curRaw, cur)+firstNonEmpty(pieceRaw, piece), toolName)
}

func EffectiveJSON(raw string, toolName string) string {
	text := strings.TrimSpace(NormalizeJSON(raw, toolName))
	if text != "" {
		return text
	}
	if len(requiredKeys(toolName)) > 0 {
		return ""
	}
	return "{}"
}

func CompleteJSON(raw string, toolName string) bool {
	text := strings.TrimSpace(NormalizeJSON(raw, toolName))
	if text == "" || (text[0] != '{' && text[0] != '[') {
		return false
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	if decoder.More() {
		return false
	}
	switch typed := value.(type) {
	case []any:
		return len(typed) > 0 || len(requiredKeys(toolName)) == 0
	case map[string]any:
		keys := requiredKeys(toolName)
		if len(typed) == 0 {
			return len(keys) == 0
		}
		for _, key := range keys {
			item, ok := typed[key]
			if !ok || item == nil {
				return false
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

func compactJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func stringify(value any) string {
	encoded, err := json.Marshal(value)
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

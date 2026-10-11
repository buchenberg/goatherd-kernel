package trace

// Serialization helpers for the SQLite trace store: canonical-shaped
// JSON for receipts, bodies, and contexts, plus the map and slice
// utilities shared across the store seams.

import (
	"encoding/json"

	"fmt"
	"sort"

	"strings"

	_ "modernc.org/sqlite"
)

// NonNilStrings returns an empty (non-nil) slice for nil input.
//
// The canonical writer emits null for a nil slice and [] for an empty one, and
// Python serialises an empty tuple as []. Without this a Go zero value would
// canonicalise differently from the Python it is supposed to match.
func NonNilStrings(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

// nullableString renders an unset Go string as JSON null, which is what Python's
// Optional fields serialise to. Go has no separate "unset" for a string field.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func receiptToJSON(r AppendReceipt) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func receiptFromJSON(s string) (AppendReceipt, error) {
	var r AppendReceipt
	err := json.Unmarshal([]byte(s), &r)
	return r, err
}

func bodyToJSON(body RecordBody) string {
	b, _ := json.Marshal(body.Payload)
	return string(b)
}

// bodyFromJSON decodes a retained body.
//
// It uses UseNumber because the canonical writer depends on the int/float
// distinction that JSON carries in the token text: `1` and `1.0` are different
// values to Python and canonicalise to "1" and "1.0". With the default decoder
// every number becomes float64, so a retained body that said `1` would
// re-canonicalise as `1.0` and the record could no longer reproduce its own id —
// which is exactly what happened before this was fixed.
func bodyFromJSON(s string) (RecordBody, error) {
	var payload map[string]any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		// Reported rather than swallowed. An empty body would still be returned to
		// the caller looking like a valid record whose payload happened to be
		// empty, and any digest computed from it would be a plausible wrong answer.
		return RecordBody{}, fmt.Errorf("decode retained body: %w", err)
	}
	return RecordBody{Payload: payload}, nil
}

func stringToJSON(ss []string) string {
	b, _ := json.Marshal(ss)
	return string(b)
}

func jsonToStrings(s string) []string {
	var result []string
	json.Unmarshal([]byte(s), &result)
	return result
}

func CopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func deduplicate(ss []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// orderedValues returns a map's values in the given key order.
//
// It exists because every map-backed field of a read result has a defined order
// in the Python reference — dicts are insertion-ordered — while Go's map
// iteration is randomised. Callers pass the insertion order they recorded.
//
// The length check is a guard, not an expected path: if the supplied order does
// not cover the map, this falls back to sorted keys rather than to map iteration,
// so a missed insertion site degrades to a different deterministic order instead
// of to nondeterminism.
func orderedValues[T any](m map[string]T, order []string) []T {
	if len(order) != len(m) {
		order = make([]string, 0, len(m))
		for k := range m {
			order = append(order, k)
		}
		sort.Strings(order)
	}
	s := make([]T, 0, len(m))
	for _, k := range order {
		if v, ok := m[k]; ok {
			s = append(s, v)
		}
	}
	return s
}

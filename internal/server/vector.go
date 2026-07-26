package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// vector.go implements CONTRACT-05 T2: the text canonicalization of vector
// values that librarian performs by hand with database/sql, because librarian
// writes parameterized SQL directly — it does NOT go through compat.Store's
// write/canonicalize path. The stored text MUST be byte-identical to what
// compat's canonicalVectorValue produces, so the SQLite source and the
// exported snapshot (and the Postgres destination) converge.
//
// The canonical form is '[c1,c2,...]': no spaces, no outer whitespace, each
// component normalized through the shortest round-trippable float text. This
// replicates compat/store.go canonicalVectorValue + normalizeFloat EXACTLY:
//
//	normalizeFloat(text) = strconv.FormatFloat(strconv.ParseFloat(text, 64), 'g', -1, 64)
//
// Because JSON numbers arrive as float64 already parsed by encoding/json, the
// writer skips the ParseFloat step and applies the same FormatFloat('g', -1,
// 64) — equivalent, since the float64 IS the parsed value. So '2.0' and '2'
// both converge to '2', '1.5' stays '1.5', and scientific notation round-trips
// through 'g'. The convergence is proven by TestVectorFormatConvergesWithCompat
// (a real compat SQLite round-trip, default suite, no PG).

// formatVectorComponent normalizes a single float64 component to the same
// shortest text compat's normalizeFloat yields. It is the exact algorithm of
// compat/store.go normalizeFloat applied to an already-parsed float.
func formatVectorComponent(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// FormatVector renders a float64 slice as the canonical '[c1,c2,...]' text that
// compat expects in storage. The caller has already validated the dimension
// against the schema, so this only does the textual formatting. It must be the
// exact inverse of ParseVector for the values librarian stores.
func FormatVector(components []float64) string {
	parts := make([]string, len(components))
	for i, c := range components {
		parts[i] = formatVectorComponent(c)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// ParseVector parses a stored canonical '[c1,c2,...]' back into a []float64 for
// JSON serialization (GET returns an array of numbers, never the raw carrier
// text). It accepts the same permissiveness compat's canonicalVectorValue does
// on input (optional surrounding/per-component whitespace) so a value that
// canonicalized to the canonical form — or one a future writer produced with
// stray spaces — still parses. A non-bracketed or non-numeric value is an
// error; the caller surfaces it as a 500 only for genuinely corrupt storage,
// never for a client-supplied value (those are validated before storage).
func ParseVector(text string) ([]float64, error) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return nil, fmt.Errorf("invalid vector %q: expected '[c1,c2,...]'", text)
	}
	inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	if inner == "" {
		return []float64{}, nil
	}
	parts := strings.Split(inner, ",")
	out := make([]float64, len(parts))
	for i, part := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid vector component %q: %w", part, err)
		}
		out[i] = f
	}
	return out, nil
}

// embeddingRefusedMessage explains why a request carrying `embedding` is refused
// on an installation that did not declare the vector capability (CONTRACT-23
// T2).
//
// IT IS REFUSED, NOT IGNORED, and that is the whole of T2. Accepting a field and
// discarding it is the silent degradation this project does not do: the client
// would receive a 201/200 for a write that stored nothing, and would discover it
// only when reading the value back — if it ever read it back. The message names
// the three things the caller needs: what is missing (the column), why (the
// installation was created without the capability), and that it cannot be turned
// on for this installation.
const embeddingRefusedMessage = "this installation does not have the vector capability: its articles table has no embedding column, so an embedding cannot be stored. " +
	"The field is refused rather than ignored, because accepting it and discarding it would report a success that stored nothing. " +
	"The choice is made when the installation is created (LIBRARIAN_VECTOR) and is irreversible; a new installation with the capability enabled is the only way to store embeddings"

// refuseEmbeddingIfDisabled answers 400 with the explanation above when the
// request carries an `embedding` field and this installation has no vector
// capability. It returns true when it wrote the response, i.e. when the caller
// must stop.
//
// It refuses ANY present value, INCLUDING an explicit JSON null. A null means
// "clear the embedding", which is a statement about a column that does not
// exist; answering 200 to it would be exactly the same silent lie as accepting
// an array. An ABSENT field is not a statement about the embedding at all, so it
// is the only shape that passes — which is what keeps every existing client that
// never used the capability working unchanged.
func (h *handlers) refuseEmbeddingIfDisabled(w http.ResponseWriter, raw json.RawMessage) bool {
	if h.caps.Vector() || len(raw) == 0 {
		return false
	}
	writeError(w, http.StatusBadRequest, embeddingRefusedMessage)
	return true
}

// validateEmbedding decodes an embedding JSON value (a json.RawMessage from the
// request body) and returns the canonical carrier text plus a state flag.
//
//   - present=false: the field was ABSENT (raw is empty) → the caller leaves the
//     column untouched (create: NULL default; update: no change). This is the
//     backward-compatible path.
//   - present=true, isNull=true: a literal JSON null → the caller sets the
//     column to NULL (create: same as absent; update: explicit clear).
//   - present=true, isNull=false, canonical non-empty: a valid array of the
//     exact declared dimension, canonicalized to '[c1,c2,...]'.
//
// Validation errors (not an array, wrong dimension, a non-numeric component)
// are returned as plain errors and the handler maps them to 400 — never 500,
// never silent truncation. dimension is the schema's declared vector dimension
// (schema.EmbeddingDimension); it is enforced exactly.
func validateEmbedding(raw json.RawMessage, dimension int) (canonical string, present bool, isNull bool, err error) {
	if len(raw) == 0 {
		// Absent field: leave the column as-is (create default / update no-op).
		return "", false, false, nil
	}
	if string(raw) == "null" {
		// Explicit null: clear the column (create: NULL; update: set NULL).
		return "", true, true, nil
	}
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return "", true, false, fmt.Errorf("embedding must be a JSON array of %d numbers", dimension)
	}
	if arr == nil {
		return "", true, true, nil
	}
	if len(arr) != dimension {
		return "", true, false, fmt.Errorf("embedding dimension mismatch: expected %d, got %d", dimension, len(arr))
	}
	components := make([]float64, len(arr))
	for i, el := range arr {
		f, ok := el.(float64)
		if !ok {
			// encoding/json decodes JSON numbers into float64; anything else
			// (string, bool, null, nested array/object) is a non-numeric
			// component → reject clearly, not silently.
			return "", true, false, fmt.Errorf("embedding component %d is not a number", i)
		}
		components[i] = f
	}
	return FormatVector(components), true, false, nil
}

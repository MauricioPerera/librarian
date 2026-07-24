package schema

import "encoding/json"

// JSON serializes the canonical schema (Build()) to indented JSON. It is the
// generated artifact the compat CLI consumes as its schema_ref: Go's Build()
// is the single source of truth, and this dump is regenerated whenever the
// schema changes — it is never hand-maintained, so the JSON and the Go model
// cannot drift. CONTRACT-04 T1: the dump exists so `compat copy` has an
// explicit schema to migrate (it does not discover one from the source DB),
// and the round-trip JSON→Schema→CompileDDL stays identical to the original.
func JSON() ([]byte, error) {
	return json.MarshalIndent(Build(), "", "  ")
}

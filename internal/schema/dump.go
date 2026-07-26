package schema

import "encoding/json"

// JSONWith serializes the COMPLETE canonical schema — Build() (code) plus the
// tables derived from the persisted dynamic content-type definitions passed in
// — to indented JSON. It is the generated artifact the compat CLI consumes as
// its schema_ref: Go stays the single source of truth for the code half, the
// database is the source of truth for the dynamic half, and this dump is
// regenerated on demand, never hand-maintained.
//
// CONTRACT-13 replaced the previous parameterless JSON() (which serialized
// Build() alone) with this function ON PURPOSE. A parameterless dump would
// still compile and still look correct while silently omitting every dynamic
// content type AND ITS DATA from a `compat copy` export — the single worst
// failure mode in this project, because it destroys the no-cutover
// exportability guarantee that motivates it (DEFINITION.md). Forcing the caller
// to supply the definitions makes the omission impossible to commit by
// accident: there is no overload that "just dumps the code schema".
//
// defs comes from store.LoadContentTypeDefinitions (or nil for a database that
// provably has no dynamic types, i.e. one whose registry table does not exist).
func JSONWith(defs []ContentTypeDefinition) ([]byte, error) {
	return JSONWithFor(Capabilities{}, defs)
}

// JSONWithFor is JSONWith for a declared set of capabilities (CONTRACT-23 T4:
// `--dump-schema` must REFLECT the choice). This matters beyond cosmetics: the
// dump is the schema_ref `compat copy` consumes, so an installation without the
// vector capability must not hand the export a schema declaring a column its
// tables do not have — the destination would be created with a vector(1536)
// column, and on PostgreSQL that reintroduces the pgvector requirement the
// installation exists to avoid.
func JSONWithFor(caps Capabilities, defs []ContentTypeDefinition) ([]byte, error) {
	full, err := BuildWithFor(caps, defs)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(full, "", "  ")
}

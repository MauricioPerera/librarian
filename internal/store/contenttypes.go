package store

// contenttypes.go implements CONTRACT-13 T2/T3 on the persistence side:
//
//   - CanonicalSchema: the composed "code + dynamic" schema, THE definition of
//     what this instance's schema is. Used by EnsureSchema, by
//     `librarian --dump-schema`, and by the compat.InferFeatures audit contract
//     — the three places that previously treated schema.Build() as complete.
//   - LoadContentTypeDefinitions: reads the persisted definitions back.
//   - CreateContentType: persists a definition AND creates its real table in a
//     SINGLE transaction, so the two can never come apart.

import (
	"context"
	"errors"
	"fmt"

	"github.com/MauricioPerera/librarian/internal/dual"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

// ErrDuplicateContentType is returned when a content type with that name
// already exists. The real guarantee is the schema-level UNIQUE(name) on
// content_types (an application-level pre-check can lose a concurrent race);
// this sentinel merely lets the server translate the constraint violation into
// a clean 400 instead of leaking a raw SQL error as a 500 — the exact pattern
// products.sku and terms(taxonomy_id, slug) already use.
var ErrDuplicateContentType = errors.New("content type already exists")

// ErrContentTypeNotFound is returned by FetchContentType when no definition
// with that name exists (→ 404).
var ErrContentTypeNotFound = errors.New("content type not found")

// ErrUnknownReferenceTarget is returned when a creation declares a reference to
// a content type that does not exist (CONTRACT-27 T2 — "the target has to exist
// FIRST; if not, an explicit refusal").
//
// It is a 400, not a 500, and it is checked in Go rather than left to the
// engine: leaving it to the engine would mean the CREATE TABLE failing on a
// foreign key naming a table (`cpt_autores`) the admin never typed, in an error
// that says nothing about the ORDER being the problem.
var ErrUnknownReferenceTarget = errors.New("the target of the reference does not exist")

// registryPresent reports whether the content_types registry table physically
// exists in the database.
//
// CONTRACT-21 T1 — ONE OF THE TWO CATALOG PROBES. It used to be a hand-written
// `SELECT count(*) FROM sqlite_master`, which is not merely non-portable: the
// relation does not exist on PostgreSQL at all, and PostgreSQL's equivalent
// (pg_class joined to pg_namespace, restricted to current_schema()) has no
// SQLite counterpart. That is exactly the shape compat.Store.TableExists exists
// for, and it is used here instead of a probe of our own.
//
// The reason this place avoids compat's InspectSchema is UNCHANGED and still
// valid: InspectSchema prefers the __compat_schema metadata row, which is
// exactly the thing EnsureSchema is about to rewrite, so the metadata cannot
// also be the oracle. TableExists documents that same property as its own
// raison d'être ("it asks the engine, not the cache"), so the probe and the
// reason for the probe now agree by design rather than by comment.
func registryPresent(ctx context.Context, store *compat.Store) (bool, error) {
	present, err := store.TableExists(ctx, schema.ContentTypesTable)
	if err != nil {
		return false, fmt.Errorf("look up %s in the engine catalog: %w", schema.ContentTypesTable, err)
	}
	return present, nil
}

// LoadContentTypeDefinitions reads every persisted dynamic content-type
// definition, with its fields in ordinal order, from a database whose registry
// table is known to exist. Ordering by name then ordinal makes the result
// deterministic.
//
// Every row is re-validated through the T1 gate before being returned. A row
// that fails (only possible if the registry was written outside the API) is a
// HARD ERROR, never a skipped entry: silently dropping a persisted type is what
// would make the dump — and therefore the export — incomplete.
//
// CONTRACT-21 T1 — WHY THE FINAL ORDER IS IMPOSED IN GO. The statement keeps
// `ORDER BY t.name, f.ordinal` as a stable base (it is what groups a type's
// field rows together and puts them in ordinal order — an INTEGER key, which
// both engines order identically), but the order of the TYPES is re-imposed
// here, byte-wise, by dual.SortByKeys. The reason is the collation divergence
// CONTRACT-20 measured: PostgreSQL orders TEXT by the database collation and
// SQLite by bytes, and a type name may contain `_` (`blog_posts` vs `bloga`
// sorts differently under the two rules). This order is not cosmetic — it is the
// order of the tables in the COMPOSED CANONICAL SCHEMA, so it decides the
// contents of `--dump-schema` and the order ApplySchema creates tables in. Two
// instances of librarian with the same content types must produce the same
// canonical schema whichever engine they run on; that is what makes the dumps
// comparable and the export faithful.
func LoadContentTypeDefinitions(ctx context.Context, store *compat.Store) ([]schema.ContentTypeDefinition, error) {
	rows, err := store.DB.QueryContext(ctx,
		`SELECT t.name, COALESCE(f.name, ''), COALESCE(f.field_type, '')
		   FROM `+schema.ContentTypesTable+` t
		   LEFT JOIN `+schema.ContentTypeFieldsTable+` f ON f.content_type_id = t.id
		  ORDER BY t.name, f.ordinal`,
	)
	if err != nil {
		return nil, fmt.Errorf("query content type definitions: %w", err)
	}
	defer rows.Close()

	var (
		out   []schema.ContentTypeDefinition
		index = map[string]int{}
	)
	for rows.Next() {
		var typeName, fieldName, fieldType string
		if err := rows.Scan(&typeName, &fieldName, &fieldType); err != nil {
			return nil, fmt.Errorf("scan content type definition: %w", err)
		}
		i, ok := index[typeName]
		if !ok {
			out = append(out, schema.ContentTypeDefinition{Name: typeName})
			i = len(out) - 1
			index[typeName] = i
		}
		// The LEFT JOIN yields one all-empty field row for a type with no fields.
		if fieldName == "" {
			continue
		}
		out[i].Fields = append(out[i].Fields, schema.FieldDefinition{
			Name: fieldName,
			Type: schema.FieldType(fieldType),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate content type definitions: %w", err)
	}
	// CONTRACT-27: the relation half of every definition, read from the third
	// registry table. It is a SECOND query rather than a third LEFT JOIN on the
	// one above on purpose: joining two independent one-to-many children of the
	// same parent multiplies their rows against each other (fields × references),
	// and de-duplicating that in Go is exactly the kind of implicit
	// reconstruction that loses a row when one of the two sides is empty.
	//
	// On a database whose references table does not exist yet — every
	// installation, on its first boot with this binary, because LoadDefinitions
	// runs INSIDE EnsureSchema before the missing tables are created — this
	// yields an empty map, which is PROVABLY correct: a relation can only live
	// there. See referencesTablePresent.
	references, err := loadReferencesByType(ctx, store)
	if err != nil {
		return nil, err
	}
	// CONTRACT-34: which of these types carries the folded search column. A THIRD
	// query, and a third one for the same reason the references half is a second:
	// joining another independent one-to-many child of content_types onto the
	// statement above would multiply its rows against the field rows. This one is
	// one-to-ONE, so it could have been a LEFT JOIN — it is kept separate anyway so
	// the shape of the read matches the shape of the registry and a future
	// contract's fourth child table has an obvious place to go.
	folded, err := loadFoldedTypeNames(ctx, store)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].References = references[out[i].Name]
		_, out[i].Folded = folded[out[i].Name]
	}
	dual.SortByKeys(out, func(d schema.ContentTypeDefinition) []dual.Key {
		return dual.Ascending(d.Name)
	})
	for _, d := range out {
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("persisted content type definition %q is invalid: %w", d.Name, err)
		}
	}
	return out, nil
}

// LoadDefinitions returns the persisted dynamic definitions, or an empty slice
// when the registry table does not exist yet.
//
// "Registry table absent ⇒ zero dynamic types" is a PROOF, not an assumption: a
// definition can only be persisted in that table, so a database without it
// cannot have one. This is the only case in which an empty result is legitimate
// — every OTHER failure (unreadable database, corrupt row, wrong engine)
// propagates as an error, so no caller can ever be handed a silently incomplete
// picture.
func LoadDefinitions(ctx context.Context, store *compat.Store) ([]schema.ContentTypeDefinition, error) {
	present, err := registryPresent(ctx, store)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	return LoadContentTypeDefinitions(ctx, store)
}

// CanonicalSchema returns THE canonical schema of this database: the code
// tables (schema.Build()) composed with one table per persisted dynamic
// content-type definition. It is the single answer to "what is this instance's
// schema?", and it is what every one of the three former schema.Build() call
// sites now uses.
// CONTRACT-23: on an installation that already exists, the capabilities are NOT
// a matter of opinion — they are read back from the physical tables
// (InstalledCapabilities), which is what makes it impossible for a caller that
// does not know the declaration to compose a schema that contradicts the
// database. A database that has not been created yet has made no choice, so the
// default (every capability enabled) applies; the only caller that reaches this
// function on an empty database is a test, because the startup path composes
// through CanonicalSchemaFor with the declaration it just validated.
func CanonicalSchema(ctx context.Context, store *compat.Store) (compat.Schema, error) {
	caps, installed, err := InstalledCapabilities(ctx, store)
	if err != nil {
		return compat.Schema{}, err
	}
	if !installed {
		caps = schema.Capabilities{}
	}
	return CanonicalSchemaFor(ctx, store, caps)
}

// CanonicalSchemaFor is CanonicalSchema for a DECLARED set of capabilities. It
// is what the startup path uses, because on a database that does not exist yet
// the declaration is the only source there is.
func CanonicalSchemaFor(ctx context.Context, store *compat.Store, caps schema.Capabilities) (compat.Schema, error) {
	defs, err := LoadDefinitions(ctx, store)
	if err != nil {
		return compat.Schema{}, fmt.Errorf("load dynamic content type definitions: %w", err)
	}
	full, err := schema.BuildWithFor(caps, defs)
	if err != nil {
		return compat.Schema{}, fmt.Errorf("compose canonical schema: %w", err)
	}
	return full, nil
}

// FetchContentType returns one persisted definition by name, or
// ErrContentTypeNotFound. The name is NOT validated first on purpose: a lookup
// of a syntactically impossible name simply finds nothing (404), and the value
// is bound as a parameter, never interpolated.
func FetchContentType(ctx context.Context, store *compat.Store, name string) (schema.ContentTypeDefinition, error) {
	defs, err := LoadContentTypeDefinitions(ctx, store)
	if err != nil {
		return schema.ContentTypeDefinition{}, err
	}
	for _, d := range defs {
		if d.Name == name {
			return d, nil
		}
	}
	return schema.ContentTypeDefinition{}, ErrContentTypeNotFound
}

// CreateContentType persists a dynamic content-type definition AND creates its
// real table, ATOMICALLY — all of it or none of it.
//
// WHY ATOMICITY IS THE WHOLE POINT (red-team item of the contract): a
// definition persisted WITHOUT its table is the worst reachable state. Every
// subsequent EnsureSchema would compose a canonical schema containing a table
// that does not exist, the generic CRUD layer would query a missing table, and
// `compat copy` would be handed a schema_ref describing a table it cannot read.
// The mirror state — a table with no definition — is just as bad in the other
// direction: the table would be invisible to the composed schema and therefore
// silently excluded from every export, together with its data.
//
// HOW: everything happens inside ONE database transaction. BOTH engines execute
// DDL transactionally — SQLite by design, PostgreSQL likewise (it is one of the
// few engines that do) — so the CREATE TABLE, the two INSERTs and the
// __compat_schema metadata update commit or roll back together on either one.
//
// CONTRACT-21 T1 — WHAT MAKES THE ROLLBACK IDENTICAL ON BOTH ENGINES, and it is
// not luck. PostgreSQL marks the WHOLE transaction as aborted at the first
// failing statement: every later statement then fails with 25P02 and only
// ROLLBACK is accepted. SQLite does not; a failed statement leaves the
// transaction usable. That divergence is invisible here because this function
// RETURNS at the first error and the `defer tx.Rollback()` is what runs next —
// no statement is ever issued after a failure. The two engines therefore reach
// the same end state by different internal routes. Any future edit that tries to
// "recover" from a mid-transaction error and continue would break atomicity on
// PostgreSQL only, and silently: that is why this is written down rather than
// left as a property of the current control flow.
//
// The catalog probe (missingTables → InspectSchema) and the compilation both
// happen BEFORE the transaction opens. That is deliberate on both engines and
// load-bearing on SQLite, where compat pins the pool to a single connection, so
// a query on the pool while a transaction is open would deadlock. The steps are
// deliberately the SAME machinery EnsureSchema uses — compose, diff against
// what exists (missingTables), compile with compat.CompileDDL, write the FULL
// composed metadata — rather than a loose ApplySchema, so there is exactly one
// notion of "apply a table" in the project. compat.Store.ApplySchema itself is
// not called because it opens its own transaction (and compat pins the SQLite
// pool to a single connection, so a nested transaction would deadlock); its
// body is reproduced here statement-for-statement instead.
//
// Concurrency: two admins racing on the SAME name are decided by the
// schema-level UNIQUE(name) on content_types, which cannot lose the race — the
// loser's whole transaction (table included) rolls back and it gets a 400. The
// caller (the HTTP layer) additionally serializes schema-creating requests with
// a mutex so two DIFFERENT names cannot interleave their diff-and-create steps.
func CreateContentType(ctx context.Context, store *compat.Store, def schema.ContentTypeDefinition) error {
	// 1. The T1 gate, before the name touches anything at all.
	if err := def.Validate(); err != nil {
		return err
	}
	// CONTRACT-34 — EVERY TYPE CREATED FROM NOW ON IS BORN FOLDED, and the flag is
	// FORCED here rather than taken from the caller. Folded is not a declaration:
	// it describes whether the physical table has the system column, and the only
	// code that can make that true is this function (it creates the table) and the
	// rebuild. A caller that sent `"folded": false` in a JSON body would otherwise
	// be asking for a table whose search is case-sensitive for no reason, and a
	// caller that sent `"folded": true` to some other path would be asserting
	// something about a table it is not creating. Overwriting it makes the field
	// what it is: an ANSWER, never a request.
	def.Folded = true

	// 2. Compose the schema this database WILL have, and let compat validate the
	//    whole thing (FK targets, column families, duplicate columns) up front.
	existing, err := LoadDefinitions(ctx, store)
	if err != nil {
		return fmt.Errorf("load existing content type definitions: %w", err)
	}
	for _, d := range existing {
		if d.Name == def.Name {
			// Fast, friendly path. The authoritative check is still the UNIQUE
			// constraint below (this one can lose a race; that one cannot).
			return ErrDuplicateContentType
		}
	}
	// 2b. CONTRACT-27 T2 — THE ORDER THIS CONTRACT IMPOSES. A reference is a real
	//     foreign key, so its target table must already exist; there is no
	//     ALTER TABLE to add the constraint later, and creating both types in one
	//     step is not expressible through this API. The check is explicit, and
	//     it is what turns "you did it in the wrong order" into a sentence rather
	//     than into a driver error about `cpt_autores`.
	//
	//     Only DYNAMIC types are candidates: `existing` comes from the registry,
	//     which holds nothing else. A reference to a CODE type (articles,
	//     products) is therefore rejected here by the same check, which is the
	//     scope DEFINITION-CPT-DINAMICOS.md fixed — relating to a code type still
	//     requires a code type.
	present := make(map[string]struct{}, len(existing))
	for _, d := range existing {
		present[d.Name] = struct{}{}
	}
	for _, r := range def.References {
		if _, ok := present[r.Target]; !ok {
			return fmt.Errorf("%w: the content type %q declares a reference %q to %q, which is not a dynamic content type of this installation. Create %q first (a reference is a real foreign key, so its target table must exist before the referring table is created), or check the name: only dynamic content types can be referenced, not the code-defined ones",
				ErrUnknownReferenceTarget, def.Name, r.Name, r.Target, r.Target)
		}
	}

	// CONTRACT-23: composed for the capabilities this installation was CREATED
	// with, read from the physical table — not for the default. This write ends
	// with writeFullSchemaMetadata, so composing it with the wrong capabilities
	// would record a __compat_schema row describing a column the articles table
	// does not have, on an installation that never asked for one.
	caps, err := installedCapabilitiesOrDefault(ctx, store)
	if err != nil {
		return err
	}
	want, err := schema.BuildWithFor(caps, append(append([]schema.ContentTypeDefinition{}, existing...), def))
	if err != nil {
		return err
	}

	// 3. Diff against what already exists — the same incremental step
	//    EnsureSchema performs. It must resolve to exactly the one new table;
	//    anything else means the database is not in the state we believe it to
	//    be in, and creating tables blindly at that point is unsafe.
	missing, err := missingTables(ctx, store, want)
	if err != nil {
		return err
	}
	// The expected table is the PREFIXED one (CONTRACT-17): def.Name is the
	// public name, def.TableName() is what BuildWith actually put in the schema.
	//
	// CONTRACT-27 asked explicitly whether "exactly one missing table" still holds
	// once the new table carries a foreign key to another DYNAMIC table. It does,
	// and for a checked reason rather than by luck: the target must already exist
	// (step 2b refuses otherwise), so its table is present in the catalog and the
	// diff cannot report it as missing. The composition order changed — the target
	// is now emitted BEFORE the referrer, see schema.sortDefinitionsByDependency —
	// but the diff is a set difference, so the order affects only which order the
	// (single) missing table would be created in.
	if len(missing) != 1 || missing[0].Name != def.TableName() {
		names := make([]string, 0, len(missing))
		for _, t := range missing {
			names = append(names, t.Name)
		}
		return fmt.Errorf("refusing to create content type %q: expected exactly one missing table (%q), found %v", def.Name, def.TableName(), names)
	}

	// 4. Compile the DDL BEFORE opening the transaction, so a compilation
	//    failure (a name compat rejects despite the T1 gate, an unsupported
	//    type) costs nothing and cannot leave anything half-done.
	statements, err := compat.CompileDDL(store.Target, compat.Schema{Tables: missing})
	if err != nil {
		return fmt.Errorf("compile DDL for content type %q: %w", def.Name, err)
	}

	// 5. One transaction: definition rows + CREATE TABLE + full metadata.
	engine := store.Target.Engine
	// The ids are generated HERE rather than read back with RETURNING, the same
	// resolution CONTRACT-19 and CONTRACT-20 gave every other insert of this
	// project: compat deliberately does not implement RETURNING, the columns keep
	// their DEFAULT gen_random_uuid() as a safety net for writes that do not come
	// from the application, and — the part that matters for THIS function — the
	// id of the new type is known BEFORE the write, so the field inserts no longer
	// depend on a value the previous statement had to hand back.
	typeID, err := dual.NewUUID()
	if err != nil {
		return fmt.Errorf("generate id for content type %q: %w", def.Name, err)
	}
	// The registry ids of the reference targets, resolved BEFORE the transaction
	// opens — required on SQLite, where compat pins the pool to a single
	// connection, and correct on both engines. A target that disappears between
	// this read and the transaction is not a hole: the CREATE TABLE would fail on
	// its foreign key and the reference INSERT would fail on target_type_id's own
	// FK, so the whole transaction rolls back and nothing half-exists.
	targetIDs, err := contentTypeIDsByName(ctx, store, def.References)
	if err != nil {
		return err
	}

	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO `+schema.ContentTypesTable+` (id, name, created_at) VALUES (`+
			dual.Bind(engine, 1)+`, `+dual.Bind(engine, 2)+`, `+dual.Bind(engine, 3)+`)`,
		typeID, def.Name, dual.Now(),
	)
	// CONTRACT-21 T1 — CLASSIFY BY DRIVER CODE, NEVER BY MESSAGE TEXT. This used
	// to match the string "UNIQUE constraint failed" plus "content_types.name",
	// which is SQLite's wording; PostgreSQL says "duplicate key value violates
	// unique constraint" and reports SQLSTATE 23505, so the match would NEVER have
	// fired there and a duplicate content-type name would have become a raw 500
	// instead of the clean 400 CONTRACT-13 specified — with nothing failing to
	// compile. It is the same latent defect CONTRACT-20 found in products.sku and
	// terms.slug, in the one place that contract could not reach.
	//
	// compat.Store.IsUniqueViolation cannot say WHICH constraint was violated
	// (documented limitation), and content_types carries two: PRIMARY KEY(id) and
	// UNIQUE(name). Attributing any violation of this INSERT to the name is
	// sound because the id is a v4 UUID this function generated three lines
	// above — the same argument products/terms use for their own tables.
	if store.IsUniqueViolation(err) {
		return ErrDuplicateContentType
	}
	if err != nil {
		return fmt.Errorf("insert content type %q: %w", def.Name, err)
	}
	for i, f := range def.Fields {
		fieldID, err := dual.NewUUID()
		if err != nil {
			return fmt.Errorf("generate id for field %q of content type %q: %w", f.Name, def.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+schema.ContentTypeFieldsTable+` (id, content_type_id, name, field_type, ordinal) VALUES (`+
				dual.Bind(engine, 1)+`, `+dual.Bind(engine, 2)+`, `+dual.Bind(engine, 3)+`, `+
				dual.Bind(engine, 4)+`, `+dual.Bind(engine, 5)+`)`,
			fieldID, typeID, f.Name, string(f.Type), i,
		); err != nil {
			return fmt.Errorf("insert field %q of content type %q: %w", f.Name, def.Name, err)
		}
	}
	// CONTRACT-27: the relation rows, in the SAME transaction as everything else.
	// A reference row without its FK column (or the other way round) is the exact
	// class of split this function's whole design exists to make unreachable.
	if err := insertReferenceRows(ctx, engine, tx, typeID, def, targetIDs); err != nil {
		return err
	}
	// CONTRACT-34: the fold marker, in the SAME transaction as the CREATE TABLE
	// that gives the table its folded column. The two commit together or neither
	// does, which is what makes "the registry says folded ⇔ the table has the
	// column" a guarantee instead of a convention — and the reason no catalog
	// probe is needed to answer the question later.
	if err := insertFoldRow(ctx, engine, tx, typeID); err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create table for content type %q: %w", def.Name, err)
		}
	}
	// The FULL composed schema (code + every dynamic type, including the new
	// one) — never the reduced one — so InspectSchema's canonical-metadata view
	// stays accurate and the next restart neither loses nor re-creates anything.
	if err := writeFullSchemaMetadata(ctx, engine, tx, want); err != nil {
		return fmt.Errorf("record full schema metadata: %w", err)
	}
	return tx.Commit()
}

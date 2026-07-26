package schema

// capabilities.go — CONTRACT-23 T1: the ONE optional capability of the canonical
// schema, and the type that carries the choice.
//
// THE PROBLEM IT CLOSES (docs/PENDIENTES.md, hueco 5). articles.embedding is
// declared vector(1536). The column is NULLABLE — an instance may never store a
// single embedding — but it EXISTS in the schema of every installation, so
// creating the tables on PostgreSQL requires the pgvector extension. An
// installation that only administers content therefore cannot run on a managed
// PostgreSQL that does not offer pgvector: an infrastructure requirement imposed
// by a capability that installation does not use. The optionality was real at
// the level of the DATA and never reached the DEPLOYMENT.
//
// THE ZERO VALUE IS THE STATUS QUO, AND THAT IS THE WHOLE DESIGN OF THIS TYPE.
// The field is spelled in the NEGATIVE (VectorDisabled) so that Capabilities{} —
// what every existing call site, every existing test and every forgotten
// parameter produces — is EXACTLY the schema every deployed installation
// already has. A capability struct whose zero value silently DROPPED a column
// would turn every un-updated call site into a schema divergence, which is the
// failure this contract exists to prevent, not to introduce.
//
// THE CHOICE IS IRREVERSIBLE AFTER THE FIRST BOOT, and that is not a detail of
// the implementation, it is what the design is shaped around. store.EnsureSchema
// creates only MISSING tables and never alters an existing one (a deliberate
// decision that is what makes a restart safe):
//
//   - an installation that booted WITHOUT the capability has an `articles` table
//     with no such column, and enabling it later would NOT add one, because
//     `articles` already exists;
//   - an installation that booted WITH it cannot drop it, for the same reason.
//
// So changing the decision on an already-booted installation must FAIL LOUDLY
// (store.EnsureSchemaFor's coherence guard), never boot anyway: the canonical
// schema would say one thing and the physical table another, and that breaks the
// export, the equivalence check and the writes — silently, and later.
//
// Making it reversible would require rebuilding a CODE table, a capability this
// project does not have and that this contract deliberately does not build.

// Capabilities is the set of optional capabilities an installation declares.
// Its ZERO VALUE means "every capability enabled", i.e. the canonical schema
// exactly as it was before this contract.
type Capabilities struct {
	// VectorDisabled removes articles.embedding — the only vector(N) column in
	// the schema — from the canonical schema, and with it the pgvector
	// requirement on PostgreSQL. It also removes the routine that reads that
	// column and the column from the two article read routines, because a
	// routine declaring a column the table does not have is a broken SELECT.
	VectorDisabled bool
}

// Vector reports whether this installation declares the vector capability.
func (c Capabilities) Vector() bool { return !c.VectorDisabled }

// Describe renders the choice for an operator-facing message.
func (c Capabilities) Describe() string {
	if c.VectorDisabled {
		return "disabled"
	}
	return "enabled"
}

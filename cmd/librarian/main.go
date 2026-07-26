// Command librarian is the single headless backend binary. On startup it opens
// (or creates) the embedded libSQL database, applies the canonical schema
// idempotently, seeds the role/permission catalogs, and only then starts
// serving HTTP. The JWT signing secret is required (LIBRARIAN_JWT_SECRET);
// without it the process refuses to start.
//
// CONTRACT-04 T1 added `librarian --dump-schema [<path>] [--db <path>]`, which
// serializes the canonical schema to JSON and exits. The JSON is the generated
// artifact the compat CLI consumes as its schema_ref when exporting to
// PostgreSQL; it is never hand-maintained.
//
// CONTRACT-13 made that mode read the database. It still needs no JWT secret,
// but the canonical schema is now code PLUS the dynamic content types persisted
// in the database, so a dump that skipped the database would hand `compat copy`
// a schema silently missing those tables and all their rows. It fails loudly
// (non-zero exit) rather than emitting an incomplete schema — see dumpSchema.
//
// CONTRACT-22 adds the SECOND mode that does not serve HTTP:
// `librarian --bootstrap --email <address>`, which makes a clean installation
// administrable (first identity + the administrator role's permissions, in one
// atomic operation, usable exactly once) and exits. The password is read from
// STANDARD INPUT and can never be a command line argument. See bootstrapCommand.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/MauricioPerera/librarian/internal/auth"
	"github.com/MauricioPerera/librarian/internal/config"
	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/server"
	"github.com/MauricioPerera/librarian/internal/store"
	"github.com/MauricioPerera/sqlite-postgres-compat/compat"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("librarian: %v", err)
	}
}

func run() error {
	if dumpPath, ok := dumpSchemaFlag(os.Args[1:]); ok {
		return dumpSchema(os.Args[1:], dumpPath)
	}
	if hasFlag(os.Args[1:], "--bootstrap") {
		return bootstrapCommand(os.Args[1:], os.Stdin, os.Stdout)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// CONTRACT-21 T2: the engine and the DSN come from ONE decision
	// (config.ResolveEngine), and store.Open pairs the connection with the target
	// of that same engine. Nothing downstream chooses an engine again — NewMux
	// takes the store, not a bare pool.
	db, err := store.Open(cfg.Engine, cfg.DSN)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	if err := store.EnsureSchema(ctx, db); err != nil {
		return err
	}
	if err := store.SeedCatalogs(ctx, db); err != nil {
		return err
	}

	mux, err := server.NewMux(server.Deps{Store: db, JWTSecret: cfg.JWTSecret})
	if err != nil {
		return err
	}

	log.Printf("librarian: schema ready on %s (%s), listening on %s", redactDSN(cfg.DSN), cfg.Engine, cfg.Addr)
	srv := &http.Server{Addr: cfg.Addr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// dumpSchema implements the --dump-schema mode.
//
// CONTRACT-13 CHANGED THIS MODE, and the change is deliberate and unavoidable.
// Until CONTRACT-12 the canonical schema was pure code, so the dump was fully
// OFFLINE: no database, no JWT secret. With dynamic content types the canonical
// schema is code PLUS the definitions persisted in the database, so a dump that
// does not read the database is, by definition, an INCOMPLETE schema — and this
// artifact is precisely the schema_ref `compat copy` uses to export, so an
// incomplete one silently leaves the dynamic tables AND ALL THEIR ROWS out of
// the export. That is the single failure mode the whole project exists to
// prevent (DEFINITION.md), so it must be impossible, not merely unlikely.
//
// The resolution, in order of how loudly it fails:
//
//   - It still does NOT require LIBRARIAN_JWT_SECRET (config.Load is still not
//     called): dumping a schema is not serving traffic. Only the database is
//     newly required.
//   - The database is located by --db <path> / --db=<path>, else LIBRARIAN_DB,
//     else the same "librarian.db" default the server uses.
//   - If that file DOES NOT EXIST, the command FAILS with a non-zero exit and
//     an explicit message. It does NOT fall back to the code-only schema and it
//     does NOT let SQLite helpfully create an empty database (which would
//     produce a plausible-looking but incomplete dump from a mistyped path —
//     exactly the silent failure being designed out).
//   - If the database exists but its registry table does not, zero dynamic
//     types is a PROOF (a definition can only live in that table), not a guess,
//     so the code-only schema is emitted and it is complete. This is what keeps
//     the command working against a pre-CONTRACT-13 database.
//   - Any other failure (unreadable file, corrupt definition row) propagates as
//     a non-zero exit. There is no path that prints a partial schema.
//
// CONTRACT-21 T2 makes this mode ENGINE-AWARE, which it has to be for the same
// reason the server is: the dump reads the database, so it needs a connection,
// and a connection needs an engine. The engine comes from the SAME
// config.ResolveEngine the server uses (so `--dump-schema` and the running
// instance can never disagree about what this deployment is), with --db
// overriding only the DSN. The "does the file exist?" guard is SQLite-specific
// by nature — a PostgreSQL DSN names no file, and PostgreSQL does not
// helpfully create a database out of a mistyped name the way SQLite creates a
// file — so it applies to SQLite only; on PostgreSQL the equivalent mistake
// surfaces as a connection error, loudly, which is the property that guard
// exists to preserve.
func dumpSchema(args []string, dumpPath string) error {
	engine, dsn, err := config.ResolveEngine()
	if err != nil {
		return err
	}
	if override := dbPathFlag(args); override != "" {
		dsn = override
		if engine == compat.SQLite && config.LooksLikePostgresDSN(dsn) {
			return fmt.Errorf(
				"--db is a PostgreSQL connection URL but the engine is sqlite: set %s=postgres", config.EngineVar)
		}
	}
	if engine == compat.SQLite {
		if _, err := os.Stat(dsn); err != nil {
			return fmt.Errorf(
				"--dump-schema must read the database to include the dynamic content types (a dump without them would export an incomplete schema): cannot access %q: %w — pass --db <path> or set LIBRARIAN_DB",
				dsn, err)
		}
	}

	db, err := store.Open(engine, dsn)
	if err != nil {
		return fmt.Errorf("open database %q: %w", redactDSN(dsn), err)
	}
	defer db.Close()

	defs, err := store.LoadDefinitions(context.Background(), db)
	if err != nil {
		return fmt.Errorf("read dynamic content type definitions from %q: %w", redactDSN(dsn), err)
	}
	data, err := schema.JSONWith(defs)
	if err != nil {
		return fmt.Errorf("marshal schema: %w", err)
	}
	if dumpPath == "" {
		fmt.Println(string(data))
		return nil
	}
	if err := os.WriteFile(dumpPath, data, 0o644); err != nil {
		return fmt.Errorf("write schema: %w", err)
	}
	return nil
}

// bootstrapCommand implements `librarian --bootstrap --email <address>`, the
// CONTRACT-22 T2 mode: it makes a CLEAN installation administrable and exits.
// It never starts the HTTP server.
//
// WHY A MODE OF THE BINARY AND NOT AN HTTP ROUTE. An HTTP route would have to be
// reachable WITHOUT authentication (there is nobody to authenticate as — that is
// the whole problem), which means the running service would permanently expose
// an unauthenticated administrator-creation endpoint whose only protection is
// the marker row. That is a large, network-reachable surface guarding an
// irreversible action, added to a service whose entire configuration story is
// "fail closed". The binary mode adds ZERO surface to the running service: it
// requires shell access to the host, which anyone who could repair the database
// by hand already has. The contract's own requirement ("no debe agregar
// superficie al servicio en marcha") points the same way.
//
// WHY THE PASSWORD COMES FROM STANDARD INPUT AND CANNOT BE A FLAG. A command
// line argument is the worst available channel for a secret, for two independent
// reasons, and both are ordinary rather than exotic:
//
//   - It is written verbatim into the shell history file, where it stays.
//   - It is visible in the PROCESS LIST to every user of the machine for as long
//     as the process runs: /proc/<pid>/cmdline is world-readable on Linux, and
//     `ps -ef` / `Get-CimInstance Win32_Process` show it. It leaks to users who
//     have no access to the database at all.
//
// An ENVIRONMENT VARIABLE was also rejected, though it is better: it is
// inherited by every child process and is readable from /proc/<pid>/environ, and
// it invites being written into a systemd unit or a compose file, i.e. into
// version control. Standard input is not in the history, not in the process
// list, not in the environment, and not inherited; it is also the channel every
// secret manager can already write to:
//
//	librarian --bootstrap --email admin@example.com < /run/secrets/admin-password
//	pass show librarian/admin | librarian --bootstrap --email admin@example.com
//
// The EMAIL stays a flag on purpose — it is not a secret, and having it on the
// command line keeps the invocation self-documenting in the history.
//
// It applies the schema first (EnsureSchema + SeedCatalogs, the same two calls
// the server makes on startup, in the same order) so a genuinely empty database
// is a valid starting point: the answer to "does the bootstrap create the schema
// or fail?" is that it creates it, exactly as a first boot would.
func bootstrapCommand(args []string, in io.Reader, out io.Writer) error {
	if flag, ok := passwordFlag(args); ok {
		return fmt.Errorf(
			"%s is not accepted: a password passed on the command line is written to the shell history and is visible in the process list to every user of this machine. --bootstrap reads the password from STANDARD INPUT instead — for example: librarian --bootstrap --email admin@example.com < /run/secrets/admin-password",
			flag)
	}
	email := valueFlag(args, "--email")
	if email == "" {
		return errors.New("--bootstrap requires --email <address> (the address of the first administrator); the password is read from standard input, never from a flag")
	}
	password, err := readPassword(in)
	if err != nil {
		return err
	}

	// CONTRACT-21's resolution, SHARED and not duplicated: the same
	// config.ResolveEngine the server and --dump-schema call, with the same --db
	// override and the same contradiction guard. A bootstrap that could disagree
	// with the running instance about which engine this deployment is would write
	// the administrator into a database nobody serves from.
	engine, dsn, err := config.ResolveEngine()
	if err != nil {
		return err
	}
	if override := dbPathFlag(args); override != "" {
		dsn = override
		if engine == compat.SQLite && config.LooksLikePostgresDSN(dsn) {
			return fmt.Errorf(
				"--db is a PostgreSQL connection URL but the engine is sqlite: set %s=postgres", config.EngineVar)
		}
	}

	db, err := store.Open(engine, dsn)
	if err != nil {
		return fmt.Errorf("open database %q: %w", redactDSN(dsn), err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := store.EnsureSchema(ctx, db); err != nil {
		return err
	}
	if err := store.SeedCatalogs(ctx, db); err != nil {
		return err
	}

	result, err := auth.Bootstrap(ctx, db, email, password)
	if err != nil {
		return err
	}

	// Say what was DONE, both halves of it — the identity and the grants. The
	// second line is the one whose absence caused the historical incident, so it
	// is reported explicitly rather than implied.
	fmt.Fprintf(out, "librarian: bootstrap complete on %s (%s)\n", redactDSN(dsn), engine)
	fmt.Fprintf(out, "  identity created: %s (id %s), status active, role %q\n", result.Email, result.UserID, result.Role)
	fmt.Fprintf(out, "  role %q now holds %d permission(s): %s\n", result.Role, len(result.Permissions), strings.Join(result.Permissions, ", "))
	fmt.Fprintf(out, "  roles editor/author/contributor were left with NO permissions on purpose; grant them from the admin UI\n")
	fmt.Fprintf(out, "  this installation is now claimed and can never be bootstrapped again\n")
	return nil
}

// readPassword reads the administrator password from standard input and refuses
// an empty one. Exactly one trailing end-of-line is what a `<file` or a heredoc
// contributes, so trailing CR/LF is stripped; everything else, including inner
// spaces, is part of the password. A password that ENDS in a newline is
// therefore not expressible through this channel, which is a deliberate and
// documented trade for making `printf '%s\n'`, `echo` and a secrets file all
// behave the same.
func readPassword(in io.Reader) (string, error) {
	data, err := io.ReadAll(in)
	if err != nil {
		return "", fmt.Errorf("read the administrator password from standard input: %w", err)
	}
	password := strings.TrimRight(string(data), "\r\n")
	if password == "" {
		return "", errors.New("the administrator password is read from STANDARD INPUT and nothing (or only a newline) was supplied: pipe it in, for example `librarian --bootstrap --email admin@example.com < /run/secrets/admin-password`")
	}
	return password, nil
}

// hasFlag reports whether args contains exactly the token flag. Used for
// --bootstrap, which takes no value of its own.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// valueFlag returns the value of `--name <value>` / `--name=<value>` (empty when
// absent). Same shape as dbPathFlag, which keeps its own name for the historical
// --db spelling.
func valueFlag(args []string, name string) string {
	for i, a := range args {
		switch {
		case a == name:
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
		case strings.HasPrefix(a, name+"="):
			return strings.TrimPrefix(a, name+"=")
		}
	}
	return ""
}

// passwordFlag detects an attempt to pass the password (or any secret spelled
// like one) as an argument, so the refusal explains the reason instead of the
// flag being silently ignored — silently ignoring it would leave the secret in
// the history and the process list AND fail confusingly.
func passwordFlag(args []string) (string, bool) {
	for _, a := range args {
		for _, name := range []string{"--password", "--pass", "--pwd", "--secret"} {
			if a == name || strings.HasPrefix(a, name+"=") {
				return name, true
			}
		}
	}
	return "", false
}

// redactDSN replaces the password of a PostgreSQL URL with `***` so it can be
// logged or embedded in an error.
//
// CONTRACT-21 T2 introduces this because the DSN stopped being a harmless file
// path: a PostgreSQL URL carries the credentials, and the startup line printed
// it verbatim. A password in the service log (or in an error a client might
// see) is a leak that no later contract can take back.
//
// It edits the userinfo section textually rather than through net/url, on
// purpose: url.Parse fails on some perfectly usable DSNs, and a redactor that
// falls back to "print it raw" when parsing fails is worse than no redactor. A
// string that does not look like a URL with credentials is returned unchanged —
// a SQLite path has nothing to hide.
func redactDSN(dsn string) string {
	scheme := strings.Index(dsn, "://")
	if scheme < 0 {
		return dsn
	}
	rest := dsn[scheme+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return dsn
	}
	userinfo := rest[:at]
	colon := strings.Index(userinfo, ":")
	if colon < 0 {
		return dsn
	}
	return dsn[:scheme+3] + userinfo[:colon] + ":***" + rest[at:]
}

// dbPathFlag inspects args for --db <path> / --db=<path> and returns the value
// (empty when absent). Accepted only alongside --dump-schema; the server itself
// keeps taking its database path from LIBRARIAN_DB, unchanged.
func dbPathFlag(args []string) string {
	for i, a := range args {
		switch {
		case a == "--db":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
		case strings.HasPrefix(a, "--db="):
			return strings.TrimPrefix(a, "--db=")
		}
	}
	return ""
}

// dumpSchemaFlag inspects args for the --dump-schema mode and returns the
// optional output path (empty means stdout) and ok=true when the flag is
// present. Accepted forms:
//
//	librarian --dump-schema            # JSON to stdout
//	librarian --dump-schema path.json  # JSON to path.json
//	librarian --dump-schema=path.json  # JSON to path.json
//
// ok=false means the flag is absent and the server should boot normally.
func dumpSchemaFlag(args []string) (path string, ok bool) {
	for i, a := range args {
		switch {
		case a == "--dump-schema":
			// The next token, if present and not a flag, is the output path.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1], true
			}
			return "", true
		case strings.HasPrefix(a, "--dump-schema="):
			return strings.TrimPrefix(a, "--dump-schema="), true
		}
	}
	return "", false
}

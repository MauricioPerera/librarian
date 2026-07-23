# CONTRACT-01 — Reporte de ejecución

Núcleo: esquema canónico base + patrón de tipos de contenido + servidor mínimo.

Estado: **COMPLETADO**. Todos los criterios de aceptación en verde, suite corrida dos veces
con resultado idéntico. Sin condiciones de ABORTAR SI disparadas (el modelo completo compila DDL
para SQLite y PostgreSQL sin ajustes).

## Qué se implementó por tarea

### T1 — Esquema canónico base (usuarios, roles, permisos)

`internal/schema/schema.go`. Función `Build() compat.Schema` que construye el modelo entero en Go
(nunca SQL crudo), heredando validación, compilación DDL dual-motor y exportabilidad de `compat`.

- `users`: `id` (uuid, PK, `DEFAULT gen_random_uuid()`), `email` (text, `UNIQUE`), `password_hash`
  (text), `status` (text, `CHECK status IN ('active','suspended','invited')`), `created_at` /
  `updated_at` (timestamp, `DEFAULT CURRENT_TIMESTAMP`), `metadata` (json, nullable).
- `roles` y `permissions`: `id` (uuid, PK, default gen_random_uuid) + `name` (text, `UNIQUE`).
  Tablas de catálogo puro (sin trailer de metadata), sembradas en código.
- `role_permissions` (roles↔permissions) y `user_roles` (users↔roles): PK compuesta + dos
  `FOREIGN KEY ... ON DELETE CASCADE`.

Catálogos fijos como fuente de verdad de siembra (aún no se insertan filas en este contrato, solo
se declaran): `schema.Roles = [administrator, editor, author, contributor]`; `schema.Permissions =
[content.create, content.publish, content.delete, users.manage, roles.manage]`.

El `id` uuid con `DEFAULT gen_random_uuid()` es válido en DEFAULT en ambos motores (compat solo
prohíbe `gen_random_uuid` dentro de un `CHECK` por no determinismo; acá no aplica).

### T2 — Patrón reusable de "tipo de contenido"

`internal/schema/content_type.go`. Firma elegida:

```go
func ContentType(name string, ownColumns []compat.Column) compat.Table
```

Inyecta consistentemente y sin boilerplate: `id` (uuid PK, default gen_random_uuid), `author_id`
(uuid NOT NULL, FK → `users(id)` ON DELETE CASCADE), las `ownColumns` del tipo, y el trailer común
`created_at` / `updated_at` (timestamp, default CURRENT_TIMESTAMP) + `metadata` (json nullable, la
columna de escape equivalente a `wp_postmeta`).

Se prueba con el tipo de ejemplo `articles` (`title` text NOT NULL, `body` text NOT NULL,
`published_at` timestamp nullable), integrado al `Schema` de T1 y validado igual (ambos motores).

### T3 — Servidor HTTP mínimo

- `cmd/librarian/main.go`: binario que al arrancar abre/crea el archivo libSQL, aplica el schema
  T1+T2 de forma idempotente y **solo entonces** escucha. Puerto por `LIBRARIAN_ADDR` (default
  `:8080`); ruta del archivo por `LIBRARIAN_DB` (default `librarian.db`).
- `internal/server/server.go`: `NewMux()` con stdlib `http.ServeMux` y patrón método+ruta
  (`GET /health` → `200 {"status":"ok"}`). Sin router externo.
- `internal/store/store.go`: `Open()` (abre SQLite vía `compat.OpenSQLite`) y `EnsureSchema()`
  idempotente.

## Decisiones de diseño (donde el contrato dejaba margen) y trade-offs

1. **`author_id` NOT NULL con FK ON DELETE CASCADE** (T2). El contrato solo pedía "FK author_id →
   users(id)". Se eligió NOT NULL + CASCADE: al borrar un usuario se borra su contenido. `SET NULL`
   contradiría el NOT NULL; `RESTRICT` bloquearía el borrado de usuarios. Trade-off: no hay
   orphaning de contenido en v1; un contrato futuro puede revisarlo si se requiere soft-delete.

2. **Idempotencia de arranque por inspección de catálogo** (T3). `compat.Store.ApplySchema` emite
   `CREATE TABLE` plano, que fallaría en un segundo arranque. `EnsureSchema` primero llama
   `InspectSchema`; si todas las tablas canónicas ya están presentes, es no-op; si no, aplica. Así
   el "parar y re-arrancar sobre el mismo archivo" no intenta recrear tablas. Trade-off: el guard
   es por presencia de tablas (nombre), no un diff estructural completo — suficiente para esta base;
   la evolución de esquema real es materia de un contrato de migraciones posterior.

3. **Catálogos (`roles`/`permissions`) sin `metadata`**. Son tablas de lookup sembradas en código,
   no entidades de contenido; se mantienen mínimas (id + name UNIQUE). Solo `users` y los tipos de
   contenido llevan el trailer `created_at/updated_at/metadata`.

4. **Sin columna `vector(N)` en este contrato.** El modelo canónico las soporta, pero los vectores
   son alcance de un contrato posterior; no incluirlas mantiene el round-trip `Exact` sin riesgo y
   respeta el alcance de T1/T2.

5. **Versiones de motor centralizadas** en `internal/schema` (`SQLiteVersion = {Major:3}`,
   `PostgresVersion = {Major:17}`, con `SQLiteTarget`/`PostgresTarget`), coincidiendo con las que
   usa la propia suite de `compat`.

6. **`internal/store` e `internal/server` separados de `main`** para que los criterios de round-trip
   e HTTP se prueben con `httptest`/archivo temporal sin dejar procesos en foreground.

## ABORTAR SI — no disparado

El criterio de aborto era: algún constructo del modelo no compila DDL válido para ambos motores. No
ocurrió: `CompileDDL` devuelve statements sin error para SQLite y para Postgres sobre el schema
completo (ver criterio abajo). No hubo que replantear ni forzar nada del modelo.

## Salida REAL de cada criterio de aceptación

### `go build ./...` y `go vet ./...` limpios

```
$ go build ./...
build exit: 0
$ go vet ./...
vet exit: 0
```

### `go test ./... -count=1` verde, corrido dos veces (sin flaky)

```
=== RUN 1 ===
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/schema	1.336s
ok  	github.com/MauricioPerera/librarian/internal/server	1.820s
ok  	github.com/MauricioPerera/librarian/internal/store	2.306s
exit1: 0
=== RUN 2 ===
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/schema	1.313s
ok  	github.com/MauricioPerera/librarian/internal/server	1.808s
ok  	github.com/MauricioPerera/librarian/internal/store	2.255s
exit2: 0
```

Resultado idéntico en ambas corridas → sin flaky.

### `Schema.Validate()` sin error (T1+T2 completo)

`TestSchemaValidates` (`internal/schema/schema_test.go`):

```
=== RUN   TestSchemaValidates
--- PASS: TestSchemaValidates (0.00s)
```

### `CompileDDL` sin error para `compat.SQLite` NI `compat.Postgres`

`TestCompileDDLBothEngines` — compila el schema completo para ambos motores y verifica > 0
statements:

```
=== RUN   TestCompileDDLBothEngines
=== RUN   TestCompileDDLBothEngines/sqlite
=== RUN   TestCompileDDLBothEngines/postgres
--- PASS: TestCompileDDLBothEngines (0.00s)
    --- PASS: TestCompileDDLBothEngines/sqlite (0.00s)
    --- PASS: TestCompileDDLBothEngines/postgres (0.00s)
```

### Round-trip real: `ApplySchema` sobre libSQL real (archivo temporal) + `InspectSchema` → `Exact == true`

`TestRoundTripExact` (`internal/store/store_test.go`) usa `filepath.Join(t.TempDir(), "roundtrip.db")`
(archivo real, no `:memory:`), aplica el schema, inspecciona y exige `Exact`:

```
=== RUN   TestRoundTripExact
--- PASS: TestRoundTripExact (0.01s)
```

### Test HTTP real: `GET /health` → 200, body `{"status":"ok"}`

`TestHealth` (`internal/server/server_test.go`) con `httptest.NewServer` + `http.Get`:

```
=== RUN   TestHealth
--- PASS: TestHealth (0.00s)
```

### Arranque idempotente: aplicar, parar, re-arrancar sobre el MISMO archivo → no falla

`TestEnsureSchemaIdempotent` (`internal/store/store_test.go`): abre el archivo, `EnsureSchema`,
`Close`; reabre el MISMO archivo y `EnsureSchema` de nuevo → sin error:

```
=== RUN   TestEnsureSchemaIdempotent
--- PASS: TestEnsureSchemaIdempotent (0.01s)
```

### Final: suite completa 2× verde

Cubierto arriba (RUN 1 / RUN 2, exit 0 ambas).

## Notas

- `go.mod`: tras `go mod tidy`, `github.com/MauricioPerera/sqlite-postgres-compat v0.1.0` quedó como
  dependencia **directa** (antes figuraba `// indirect` porque el repo aún no la importaba).
  `go.sum` recibió los checksums transitivos correspondientes (modernc.org/sqlite, pgx, etc.). No se
  cambió la versión de `compat`.
- No se tocó nada fuera de `D:\Repo\librarian`. No se commiteó (lo hace el orquestador).
- No quedó ningún proceso HTTP en foreground: el test usa `httptest.Server` con `defer Close()`.

## Archivos creados

- `internal/schema/schema.go`
- `internal/schema/content_type.go`
- `internal/schema/schema_test.go`
- `internal/store/store.go`
- `internal/store/store_test.go`
- `internal/server/server.go`
- `internal/server/server_test.go`
- `cmd/librarian/main.go`
- `docs/reports/CONTRACT-01-REPORT.md` (este archivo)

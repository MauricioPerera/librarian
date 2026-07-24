# CONTRACT-04 — Reporte (export a PostgreSQL, prueba end-to-end)

Naturaleza del contrato: acá **no** se escribió la capacidad de export (ya
existe en `sqlite-postgres-compat`, auditada) — se **probó** de punta a punta
para el esquema REAL de `librarian`, con datos reales generados por la app real
(vía HTTP real, no inserts sintéticos), usando la herramienta operativa real (el
CLI `compat`, no llamadas Go internas). El entregable incluye el runbook
documentado y repetible (`docs/OPERATIONS.md`), no solo la corrida de hoy.

Base verificada antes de empezar: `internal/schema/schema.go` (`Build()`),
`internal/store/store.go`, `internal/server/*.go`, `internal/auth/*.go`,
`cmd/librarian/main.go` (CONTRACT-01/02/03 ya commiteados en `2bb8354`); y la API
real del módulo `sqlite-postgres-compat@v0.1.0` leída en el module cache
(`compat.Schema` tiene tags JSON completos; `Schema.Validate()`,
`CompileDDL(target, schema)`, `InferFeatures(schema)`, `Audit`,
`RequireExact`, `OpenStore`, `VerifySnapshots`, `RequireEquivalent` confirmados
en el código vendored). Formato exacto de `compat copy` confirmado leyendo
`cmd/compat/copy.go` (`migrationConfig`: `source_dsn`, `destination_dsn`,
`contract`, `schema` | `schema_ref`).

PostgreSQL real provisto por el orquestador, DSN en `LIBRARIAN_EXPORT_PG_DSN`
(PostgreSQL 17.10). El password del DSN **nunca** aparece en texto plano en este
reporte ni en ningún archivo del repo — siempre `***`.

---

## T1 — Volcado del esquema a JSON + round-trip

**Implementación (código permanente en el repo):**

- `internal/schema/dump.go`: `schema.JSON()` serializa `Build()` a JSON
  indentado con `json.MarshalIndent`. Go sigue siendo la única fuente de verdad;
  el JSON se **genera**, nunca se mantiene a mano.
- `cmd/librarian/main.go`: flag `--dump-schema [<path>]` (offline, sin DB ni
  JWT secret) escribe el JSON a stdout o a archivo. Formas aceptadas:
  `librarian --dump-schema`, `librarian --dump-schema path.json`,
  `librarian --dump-schema=path.json`. Se maneja antes de `config.Load()` para
  no requerir `LIBRARIAN_JWT_SECRET`.
- `internal/schema/schema_test.go`: `TestSchemaRoundTripJSON` — el test
  unitario de aceptación (sin PG).

**Decisión donde el contrato dejaba margen:** el contrato decía "flag en el
binario, ej. `librarian --dump-schema`, o un comando separado — a tu criterio".
Elegí flag (`--dump-schema`) dentro del binario existente `cmd/librarian` antes
que un comando aparte: un binario menos, cero dependencias nuevas, y el dump es
offline (no arranca el server). El contrato pidió "código permanente, no un
script de un solo uso" — es código permanente en `main.go` + `dump.go`.

**Test de round-trip (criterio de aceptación T1, sin PG):** marshal `Build()` →
unmarshal a `compat.Schema` → `Validate()` → `CompileDDL` para **ambos** motores
→ comparar statements con el `Build()` original (sin pasar por JSON). Si un
campo perdiera su tag json o un `Expression` tuviera un `omitempty` mal puesto,
el DDL divergería y fallaría acá.

Salida real del test (`go test -run TestSchemaRoundTripJSON -v -count=1 ./internal/schema`):

```
=== RUN   TestSchemaRoundTripJSON
    schema_test.go:69: ROUND_TRIP OK: sqlite statements=7, postgres statements=7, DIFF=none (both engines)
--- PASS: TestSchemaRoundTripJSON (0.00s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/schema	1.420s
```

**Dump real (el binario produciendo el JSON):**

```
& $lb --dump-schema $EXPORT_DIR\schema.json   # exit=0, schema_size=9030 bytes
```

Primeras líneas del `schema.json` real generado (confirmación de que el dump
es el esquema canónico de librarian, con `gen_random_uuid`, tipos `uuid`, etc.):

```json
{
  "tables": [
    {
      "name": "users",
      "columns": [
        {
          "name": "id",
          "type": { "family": "uuid" },
          "nullable": false,
          "default": { "kind": "gen_random_uuid" }
        },
        {
          "name": "email",
          "type": { "family": "text" },
          ...
```

---

## T2 — Datos reales vía la app real

**Implementación:** `internal/server/export_fixture_test.go` (build tag
`exportfixture`, **excluido de la suite default** para no afectar el
green-twice y para dejar el archivo SQLite en disco para T3). Test
`TestExportFixture`:

1. Abre un SQLite real en un tempdir del **sistema**
   (`%TEMP%\librarian-export\fixture.db`), `store.EnsureSchema` + seed de
   CONTRACT-02.
2. Arranca el server real (`httptest.Server` + `server.NewMux`).
3. Crea 1 usuario con rol `editor` vía `auth.CreateUser` (permitido por el
   contrato: el usuario no es un artículo) y le emite un JWT real.
4. Minta 1 API key vía `auth.MintAPIKey` (fila parte del fixture que debe
   sobrevivir el export).
5. Crea 3 artículos vía **`POST /articles` real con JWT real**:
   - A: con `metadata` JSON → publicado vía `POST /articles/{id}/publish`.
   - B: con `metadata` JSON → sin publicar (draft).
   - C: sin `metadata` → draft.
6. Queries directas a SQLite (evidencia ANTES del export) + asserts.

**Decisión forzada por el contrato (documentada con honestidad):** el contrato
T2 exige "artículo creado vía `POST /articles` real" **Y** "metadata no vacío
en alguno". El endpoint `POST /articles` de CONTRACT-03 **no exponía
`metadata`** (`articleBody` era solo `title`/`body`), lo que hacía el requisito
insatisfacible vía la API real tal como estaba. Para cumplir ambas condiciones a
la vez (sin saltear la API con inserts directos, que el contrato prohíbe),
extendí `POST /articles` con un campo **opcional** `metadata json.RawMessage`
(`internal/server/articles.go`): cuando está presente y no es `null` se inserta
verbatim en la columna `metadata` (TEXT en ambos motores). Es **backward
compatible** (omitir `metadata` deja el default NULL, idéntico al comportamiento
de CONTRACT-03), verificado por `TestCreateArticleWithMetadata` en la suite
default. Es la lectura más fiel del contrato: la extensión es necesaria para
satisfacerlo, no una tangente. Ejercita de paso el camino JSON end-to-end con
datos producidos por la app (justo lo que T2 quiere probar que sobrevive).

Esto toca `articles.go` (dentro del perímetro de librarian), no
`sqlite-postgres-compat`.

**Trade-off del fixture taggeado vs. suite default:** el archivo SQLite que usa
T3 debe persistir en disco después del test; `t.TempDir` se auto-borra. Por eso
el fixture vive en un tempdir del sistema con nombre fijo y se borra a mano al
final (ver red-team/cleanup). Para mantener la suite default green-twice y
repetible (sin colisiones UNIQUE ni artículos duplicados en re-runs), el
fixture usa build tag `exportfixture` y se corre on-demand, no en `go test ./...`.

**Evidencia T2 (salida real del test, query directa a SQLite ANTES del export):**

```
FIXTURE_DB=C:\Users\ADMINI~1\AppData\Local\Temp\librarian-export\fixture.db
COUNT users=1
COUNT roles=4
COUNT user_roles=1
COUNT api_keys=1
COUNT articles=3
ARTICLE id=5ac996d5-... title="Draft No Meta"      published_at=NULL (draft)     metadata=NULL
ARTICLE id=9179aa7c-... title="Draft With Meta"    published_at=NULL (draft)     metadata={"n":42,"reviewer":"carol"}
ARTICLE id=a2f88fee-... title="Published With Meta" published_at=2026-07-24 03:07:10 metadata={"lang":"es","tags":["export","pg"]}
ARTICLE_A metadata JSON verified in SQLite: {"lang":"es","tags":["export","pg"]}
FIXTURE_READY: published article A id=a2f88fee-... title="Published With Meta"
```

Confirma: 1 usuario, 4 roles (catálogo), 1 user_roles, 1 api_key, 3 artículos;
1 publicado (A), 2 drafts (B, C); 2 con metadata no vacía (A, B). Todos los IDs
son UUIDs reales de `gen_random_uuid()`, los timestamps de
`CURRENT_TIMESTAMP`, y `metadata` es JSON real producido por la app.

---

## T3 — Export real contra PostgreSQL, con verificación

Usando el binario `compat` real (`go install ...@v0.1.0`, corrido como
`compat.exe`, nunca `go run`) y el JSON de T1, contra el PostgreSQL real del
DSN. El fixture test además **limpia el destino PG** (DROP de las tablas de
librarian + `__compat_schema` con CASCADE) antes de escribir los configs, para
que un re-run de `copy` no colisione con `CREATE TABLE` previos.

**`compat audit` (contrato con `RequiredFeatures` inferidas del schema vía
`compat.InferFeatures`):** debe dar `exact` en todas las features.

```
audit_exit=0
[{"feature":"canonical_full_text","status":"exact"},
 {"feature":"uuid","status":"exact","reason":"lossless canonical text representation"},
 {"feature":"json","status":"exact","reason":"lossless canonical text representation"},
 {"feature":"primary_keys","status":"exact"},
 {"feature":"canonical_check_constraints","status":"exact"},
 {"feature":"canonical_foreign_keys","status":"exact"},
 {"feature":"tables","status":"exact"}]
```

**`compat copy` (migration.json real: source_dsn = SQLite de T2,
destination_dsn = `LIBRARIAN_EXPORT_PG_DSN` [***], schema_ref = schema.json de
T1):** debe terminar `VerificationReport.equivalent = true`, exit 0.

```
copy_exit=0
{"source_digest":"0b38a16349dae7d975d1fc565fef41296aaad9fd40c9895549db94611e3c0fe3",
 "destination_digest":"0b38a16349dae7d975d1fc565fef41296aaad9fd40c9895549db94611e3c0fe3",
 "equivalent":true}
```

(Nota: un intento previo ejecutó `copy` dos veces por cómo capturé stderr; la
segunda colisionó con las tablas ya creadas — `ERR_SNAPSHOT: relation "users"
already exists` — y rollback. Ese duplicado no dejó estado parcial: la primera
corrida fue exitosa y la segunda se rolled back. La corrida limpia arriba, de
una sola ejecución contra PG limpio, es la evidencia válida.)

**Verificación independiente contra PostgreSQL (no confiar solo en el veredicto
de `compat`):** query SQL directa a PG comparando un valor concreto con la
origen SQLite (test `TestExportVerifyPG`, corrido después de `copy`):

```
PG count(articles)=3  (SQLite count=3)
PG title == SQLite title == "Published With Meta"  (MATCH)
PG published_at=2026-07-24T03:06:45Z  SQLite published_at=2026-07-24 03:06:45
PG metadata (canonical) == SQLite metadata (canonical) == {"lang":"es","tags":["export","pg"]}  (MATCH)
PG raw metadata={"lang":"es","tags":["export","pg"]}
SQLite raw metadata={"lang":"es","tags":["export","pg"]}
EXPORT_VERIFY_DONE: PG count=3, published title MATCH, metadata JSON MATCH.
```

Confirma: el `count(*)` coincide (3==3); el `title` del artículo publicado
coincide exactamente (valor concreto, sin ambigüedad de canonicalización);
`published_at` sobrevivió (no NULL en PG); y `metadata` coincide como valor JSON
(parseado, porque `compat` canonicaliza JSON al exportar — key order/whitespace
pueden diferir en el texto crudo, por eso se compara el JSON parseado, no el
texto). El `metadata` se almacena como TEXT en ambos motores (compat mapea
`JSONType`→TEXT para preservar byte-a-byte) y se canonicaliza al exportar.

---

## T4 — Runbook documentado

**Implementación:** `docs/OPERATIONS.md` con los pasos EXACTOS y reproducibles
(instalar el CLI, generar el JSON del esquema con `librarian --dump-schema`,
armar el `migration.json`, correr `compat audit` y `compat copy`, verificación
independiente, limpieza del tempdir) y el manejo de los dos modos de fallo
exigidos por el contrato: qué significa y qué hacer si `audit` no da `exact`
(detener, investigar del lado librarian — no de `compat`), y si `copy` diverge
(`ERR_VERIFY_DIVERGED` — doctrina `compat`: detenerse e investigar, **nunca
reintentar ciegamente**). Los comandos del runbook son los **mismos** que se
ejecutaron en T3 (no una versión idealizada): el fixture taggeado, `go build`,
`librarian --dump-schema`, `compat audit`, `compat copy`, `TestExportVerifyPG`,
y el `Remove-Item` del tempdir.

---

## Criterios de aceptación (uno por uno)

- [x] `go build ./...` y `go vet ./...` limpios. → `build_exit=0`, `vet_exit=0`.
- [x] `go test ./... -count=1` verde, corrido dos veces. → ver bloques “run A”
  y “run B” abajo (ambos `exit=0`, todos los paquetes `ok`). El fixture
  taggeado (`exportfixture`) no integra la suite default, así que el green-twice
  es estable y repetible.
- [x] T1: round-trip JSON→Schema→CompileDDL idéntico al original, ambos motores
  (test unitario, sin PG). → `TestSchemaRoundTripJSON` PASS, “sqlite
  statements=7, postgres statements=7, DIFF=none (both engines)”.
- [x] T2: datos reales creados vía HTTP real, confirmados con query directa a
  SQLite antes del export. → bloque de evidencia T2 (counts + articles +
  metadata en SQLite).
- [x] T3: `compat audit` real → `exact` en todas. `compat copy` real → exit 0,
  `equivalent:true`, password del DSN siempre `***`. Query directa a PostgreSQL
  confirmando al menos un valor real coincide. → bloque de evidencia T3
  (audit_exit=0 todo exact; copy_exit=0 equivalent:true digests iguales;
  PG count=3, title MATCH, metadata MATCH). DSN siempre `***`.
- [x] T4: `docs/OPERATIONS.md` existe con el runbook completo y los comandos son
  los mismos usados en T3. → archivo creado; comandos idénticos a los de T3.
- [x] Final: suite completa 2× verde. → run A y run B abajo.

**`go test ./... -count=1` — run A:**

```
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	2.732s
ok  	github.com/MauricioPerera/librarian/internal/config	0.684s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.405s
ok  	github.com/MauricioPerera/librarian/internal/server	6.324s
ok  	github.com/MauricioPerera/librarian/internal/store	2.340s
A_exit=0
```

**`go test ./... -count=1` — run B:**

```
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	2.791s
ok  	github.com/MauricioPerera/librarian/internal/config	0.687s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.388s
ok  	github.com/MauricioPerera/librarian/internal/server	5.977s
ok  	github.com/MauricioPerera/librarian/internal/store	2.349s
B_exit=0
```

---

## ABORTAR SI — no se disparó

Las dos condiciones de aborto del contrato **no** ocurrieron:

- `compat audit` real dio `exact` en todas las features (no fue necesario
  abortar).
- `compat copy` real terminó `equivalent:true`, exit 0 (no divergió con
  `ERR_VERIFY_DIVERGED`; no fue necesario abortar).

No se investigó ni modificó `sqlite-postgres-compat` (perímetro respetado: solo
se tocaron archivos dentro de `D:\Repo\librarian`).

---

## Red-team / limpieza (criterios del checklist)

- **`migration.json` temporal con el DSN real podría quedar olvidado en disco
  fuera del tempdir del sistema?** No: todos los archivos temporales
  (`schema.json`, `audit.json`, `migration.json` con DSN, `librarian.exe`,
  `fixture.db`) viven en `%TEMP%\librarian-export` (tempdir del **sistema**,
  fuera del repo) y se borran al final con `Remove-Item -Recurse -Force
  $EXPORT_DIR`. `migration.json` se escribió con permisos `0600` por llevar el
  DSN. Ningún archivo temporal quedó dentro del repo.
- **El reporte podría pegar por accidente una salida de `compat copy` con el DSN
  sin enmascarar?** Revisado cada bloque: `compat copy` exitoso emite solo el
  `VerificationReport` (digests + `equivalent`), sin DSN. Un error de conexión
  podría incluir el DSN; enmascaré toda salida con una función que reemplaza el
  valor de `LIBRARIAN_EXPORT_PG_DSN` por `<DSN:***>`. El DSN aparece en este
  reporte únicamente como `***`.
- **Perímetro:** un solo dev, un solo perímetro (solo `librarian`).
- **No se commitea** (el orquestador commitea tras verificar). No se hicieron
  `git add`/`commit`.
- **Sin dependencias nuevas** más allá de las ya resueltas (se reusó
  `sqlite-postgres-compat@v0.1.0`, `golang-jwt/jwt/v5`, `golang.org/x/crypto`).

## Trade-offs resumidos

1. **Extensión de `POST /articles` con `metadata` opcional** — forzada por el
   contrato (metadata no vacío vía API real era insatisfacible sin ella).
   Backward compatible; cubierta por test en la suite default.
2. **Fixture con build tag `exportfixture` fuera de la suite default** — para
   dejar el SQLite en disco para T3 y mantener el green-twice estable/repetible
   (archivo con nombre fijo en tempdir del sistema, borrado a mano al final).
3. **Limpieza del destino PG antes de `copy`** — necesaria porque `compat copy`
   hace `CREATE TABLE` (no `IF NOT EXISTS`); un re-run contra PG con tablas
   colisiona. Es operación normal del destino de exportación, no administración
   de la instancia PG (no se desmonta ni reconfigura el provisioning).
4. **Verificación PG compara `metadata` parseado como JSON, no texto crudo** —
   porque `compat` canonicaliza JSON al exportar (key order puede cambiar); el
   `title` (texto plano) sí se compara exacto, que es el valor concreto exigido
   por el contrato.
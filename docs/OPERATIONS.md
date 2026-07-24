# OPERATIONS — Exportar una instancia de `librarian` a PostgreSQL

Runbook reproducible para exportar una instancia real de `librarian` (SQLite
embebido) a PostgreSQL bajo demanda, usando el CLI `compat` de
`sqlite-postgres-compat` — sin ventana de corte, sin reescribir la app.

Este es el camino que un operador usaría de verdad. Los comandos son los
MISMOS que se ejecutaron y verificaron en CONTRACT-04 T3 (no una versión
idealizada). La fuente de verdad del esquema es Go (`schema.Build()`); el JSON
del esquema se **genera** con `librarian --dump-schema`, nunca se mantiene a
mano.

## Fundamento

`compat copy` **exige** el esquema explícito en su JSON de config (`schema_ref`
a un archivo o `schema` inline) — NO lo descubre de la base origen. Por eso el
paso 1 genera el JSON desde el binario de librarian. El contrato de migración
lleva `source_dsn` (el archivo SQLite real con datos) y `destination_dsn` (el
PostgreSQL real). `compat copy` internamente infiere las `required_features`
del esquema, corre un `audit` exigido `exact`, exporta un snapshot de la origen,
lo importa en el destino, re-exporta el snapshot del destino y compara digests:
`equivalent == true` + exit 0 es la certificación de equivalencia.

## Prerrequisitos

- Go 1.26+ (el módulo `librarian`).
- El CLI `compat` instalado una sola vez:

  ```powershell
  go install github.com/MauricioPerera/sqlite-postgres-compat/cmd/compat@v0.1.0
  # queda en $env:USERPROFILE\go\bin\compat.exe  (en el PATH de Go)
  ```

  > No uses `go run` para medir exit codes de `compat`: en Windows `go run`
  > colapsa cualquier exit≠0 a 1, invalidando la verificación. Corre el
  > binario real (`compat.exe ...`).

- PostgreSQL destino accesible, con su DSN en la variable de entorno
  `LIBRARIAN_EXPORT_PG_DSN` (pgx: p.ej.
  `postgres://user:password@host:5432/dbname?sslmode=disable`). El password
  **nunca** se escribe en ningún archivo del repo ni del reporte: se lee de la
  variable en memoria y siempre se enmascara como `***` al pegar salidas.
- La instancia SQLite de librarian que se quiere exportar (archivo real con
  datos generados por la app).

## Paso a paso

En PowerShell. `$EXPORT_DIR` es un directorio temporal del **sistema** (no
dentro del repo) donde viven el JSON del esquema, el contrato de auditoría y el
config de migración. Se borra al final.

### 0. Preparar el directorio de trabajo y la variable de entorno

```powershell
$EXPORT_DIR = Join-Path $env:TEMP "librarian-export"
New-Item -ItemType Directory -Force -Path $EXPORT_DIR | Out-Null
# DSN del PostgreSQL destino (el password vive solo en esta variable):
# $env:LIBRARIAN_EXPORT_PG_DSN = "postgres://user:***@host:5432/db?sslmode=disable"
$compat = Join-Path $env:USERPROFILE "go\bin\compat.exe"
$lb     = Join-Path $EXPORT_DIR "librarian.exe"
```

### 1. Poblar la origen y generar los configs (datos reales vía HTTP real)

El test taggeado `TestExportFixture` levanta librarian de verdad sobre un
archivo SQLite real, crea los datos a través de la API HTTP real (usuario +
roles + API key vía `MintAPIKey` + artículos vía `POST /articles` con JWT real,
uno publicado vía `POST /articles/{id}/publish`, con `metadata` JSON no vacío),
confirma los datos con queries directas a SQLite, **limpia el PostgreSQL
destino** (DROP de las tablas de librarian + `__compat_schema`, para que un
re-run de `copy` no colisione) y escribe `audit.json` + `migration.json`
(`schema_ref = schema.json`) en `$EXPORT_DIR`.

```powershell
go test -tags exportfixture -run TestExportFixture -count=1 -v ./internal/server
```

Saluda, entre otras, líneas como:

```
FIXTURE_DB=C:\...\Temp\librarian-export\fixture.db
COUNT users=1  COUNT roles=4  COUNT user_roles=1  COUNT api_keys=1  COUNT articles=3
EXPORT_DIR=C:\...\Temp\librarian-export
CONFIGS_WRITTEN: audit.json, migration.json (schema_ref=schema.json).
PG destination cleaned (librarian tables dropped if present).
```

> Para una instancia **de producción** (no la fixture de prueba), en lugar de
> este test se apunta `migration.json` → `source_dsn` al archivo SQLite real de
> la instancia, se genera `schema.json` con el paso 2, y se omite la limpieza
> del destino si el PG está vacío. La limpieza del destino (DROP … CASCADE) es
> solo para repetibilidad; en un cutover real el destino estaría vacío.

### 2. Generar el JSON del esquema desde el binario de librarian

```powershell
go build -o $lb ./cmd/librarian
& $lb --dump-schema (Join-Path $EXPORT_DIR "schema.json")
```

`--dump-schema` serializa `schema.Build()` a JSON indentado y sale (no necesita
`LIBRARIAN_JWT_SECRET` ni base de datos — el esquema es puro código). Acepta
también `librarian --dump-schema` (a stdout) o `librarian --dump-schema=path`.
Este JSON es el `schema_ref` que consume `compat copy`.

### 3. Auditar el contrato (debe dar `exact` en todo)

```powershell
& $compat audit (Join-Path $EXPORT_DIR "audit.json")
Write-Output "audit_exit=$LASTEXITCODE"
```

Salida esperada (stdout, una línea JSON con un `Finding` por feature, todas
`status:"exact"`) y `audit_exit=0`:

```json
[{"feature":"canonical_full_text","status":"exact"},
 {"feature":"uuid","status":"exact","reason":"lossless canonical text representation"},
 {"feature":"json","status":"exact","reason":"lossless canonical text representation"},
 {"feature":"primary_keys","status":"exact"},
 {"feature":"canonical_check_constraints","status":"exact"},
 {"feature":"canonical_foreign_keys","status":"exact"},
 {"feature":"tables","status":"exact"}]
```

### 4. Exportar (`compat copy`)

```powershell
& $compat copy (Join-Path $EXPORT_DIR "migration.json")
Write-Output "copy_exit=$LASTEXITCODE"
```

Salida esperada (stdout, `VerificationReport` con digests iguales y
`equivalent:true`) y `copy_exit=0`:

```json
{"source_digest":"<hex>","destination_digest":"<mismo hex>","equivalent":true}
```

> Si `copy` imprime un error de conexión que incluya el DSN, enmascará el
> password como `***` antes de pegarlo en cualquier reporte.

### 5. Verificación independiente contra PostgreSQL (no confiar solo en `compat`)

El test taggeado `TestExportVerifyPG` conecta al PostgreSQL destino, cuenta
los artículos y compara un valor concreto (el `title` del artículo publicado, y
su `metadata` parseada como JSON) contra la origen SQLite:

```powershell
go test -tags exportfixture -run TestExportVerifyPG -count=1 -v ./internal/server
```

Salida esperada:

```
PG count(articles)=3  (SQLite count=3)
PG title == SQLite title == "Published With Meta"  (MATCH)
PG published_at=2026-07-24T03:04:11Z  SQLite published_at=2026-07-24 03:04:11
PG metadata (canonical) == SQLite metadata (canonical) == {"lang":"es","tags":["export","pg"]}  (MATCH)
EXPORT_VERIFY_DONE: PG count=3, published title MATCH, metadata JSON MATCH.
```

### 6. Limpiar el directorio temporal

```powershell
Remove-Item -Recurse -Force $EXPORT_DIR
```

Esto borra `schema.json`, `audit.json`, `migration.json` (que lleva el DSN),
`librarian.exe` y la fixture SQLite. El password del DSN nunca quedó en disco
fuera de este directorio temporal.

## Qué hacer si algo falla

### `compat audit` no da `exact`

`audit_exit=1` y un `Finding` con `status` distinto de `exact` (p.ej.
`transformed`, `unsupported`, `unknown`). Significado: una capacidad que el
esquema de librarian usa no es equivalente exacta entre SQLite y PostgreSQL.

**Detenerse e investigar del lado de librarian** — no de `compat` (ese repo está
auditado y estable). Casi siempre significa que el esquema de librarian salió
de la gramática canónica (una expresión fuera de la Sección 3, un CHECK no
determinista con `gen_random_uuid`, un `VIRTUAL` en vez de `STORED`, etc.).
Corregir `schema.Build()` en Go, regenerar el JSON (paso 2) y re-auditar. No
reintentar ciegamente: un audit no-exact es un hecho, no un transient.

### `compat copy` diverge (`ERR_VERIFY_DIVERGED`)

`copy_exit=1`, code `ERR_VERIFY_DIVERGED`, y el `VerificationReport` con
`source_digest != destination_digest`. Significado: el snapshot exportado y el
re-importado en PostgreSQL NO son equivalentes — los datos no sobrevivieron el
viaje idénticamente.

**Doctrina `compat`: detenerse e investigar, nunca reintentar a ciegas.** Un
diverged indica un desajuste real (p.ej. un valor que una engine canonicaliza
distinto a la otra). Investigar el `VerificationReport` (los dos digests) y, si
aplica, el contenido divergente. No se toca `sqlite-postgres-compat`: si algo
falla acá es del lado de cómo `librarian` arma su esquema/config, no de
`compat`. Corregir en librarian y re-correr desde el paso 1 (limpia el destino).

## Notas de diseño relevantes

- **Go es la única fuente de verdad del esquema.** El JSON se genera, no se
  mantiene a mano; así las dos formas no pueden diverger (garantizado por el
  test de round-trip `TestSchemaRoundTripJSON` en la suite default).
- **El destino se limpia antes de cada `copy`** porque `compat copy` hace
  `CREATE TABLE` (no `IF NOT EXISTS`); un re-run contra un PG con las tablas ya
  presentes falla con `relation "users" already exists` (`ERR_SNAPSHOT`). La
  limpieza (`DROP TABLE IF EXISTS … CASCADE`) es parte normal de operar el
  destino de exportación, no de administrar la instancia PostgreSQL.
- **`metadata` JSON** se almacena como `TEXT` en ambos motores (compat mapea
  `JSONType` → `TEXT` para preservar el payload byte-a-byte) y se canonicaliza
  al exportar; por eso la verificación compara el `metadata` parseado como JSON,
  no el texto crudo.
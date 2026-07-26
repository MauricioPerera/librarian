# OPERATIONS — Exportar una instancia de `librarian` a PostgreSQL

Runbook reproducible para exportar una instancia real de `librarian` (SQLite
embebido) a PostgreSQL bajo demanda, usando el CLI `compat` de
`sqlite-postgres-compat` — sin reescribir la app.

Este es el camino que un operador usaría de verdad. Los comandos son los
MISMOS que se ejecutaron y verificaron en CONTRACT-04 T3 y, sobre la base REAL
de producción, el 2026-07-25 (no una versión idealizada). La fuente de verdad
del esquema es Go (`schema.Build()`); el JSON del esquema se **genera** con
`librarian --dump-schema`, nunca se mantiene a mano.

## Qué garantiza esto — y qué NO

> **CAMBIO DE CONTRACT-21.** La frase que abría esta sección —"`librarian` no
> CORRE sobre PostgreSQL"— **ya no es cierta**. Desde CONTRACT-21 el binario
> arranca y sirve contra PostgreSQL 17 con `pgvector`, eligiendo el motor por
> configuración (ver § "Elegir el motor"). Lo que sigue describiendo este
> runbook es la otra mitad: **mover los DATOS** de una instancia SQLite ya en
> producción a PostgreSQL. Son cosas distintas y las dos hacen falta —
> CONTRACT-21 tiene como premisa explícita la **instalación en limpio** y NO
> implementa ninguna ruta de migración de datos existentes.

Lo que este runbook garantiza es que el esquema y los datos se trasladan sin
pérdida y sin reescritura, verificado por digest.

`compat copy` —el camino principal de este runbook— es una migración por
**snapshot**: se exporta un estado y se importa. Las escrituras que ocurran
durante la copia **no viajan**, así que exige detener el servicio mientras dura.

Para no detenerlo durante toda la copia existe `compat cutover`, y **ya está
ejercitado contra `librarian`**: ver
[Migración con el servicio arriba](#migración-con-el-servicio-arriba-compat-cutover).

### El recorrido completo, ejecutado de punta a punta

El 2026-07-26 se hizo una **instalación en limpio siguiendo únicamente el README
y este documento**, para encontrar lo que quien ya conoce el sistema no ve. Salió
bien, y las tres cosas que salieron mal eran de la documentación —están
corregidas más abajo, cada una donde se tropezó—, no del producto:

```
compilar → --bootstrap → arrancar sobre SQLite → poblar por HTTP real
  → --dump-schema → compat audit → compat copy → arrancar sobre PostgreSQL

copy:  source_digest == destination_digest, equivalent:true
sobre el destino: login con la MISMA contraseña, artículos y contenido dinámico
                  presentes, y una relación entre tipos dinámicos INTACTA
                  (borrar la fila referenciada sigue dando 400: la FK es real)
```

Lo último no se había probado nunca por este camino: que una **relación entre
tipos dinámicos** (CONTRACT-27) sobreviva la migración *como clave foránea real
en el destino*, y no solo como el uuid guardado en una columna.

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

  **La versión no se escribe de memoria: se lee del `go.mod`**, que es la única
  fuente de verdad de con qué versión se probó este proyecto.

  ```powershell
  # La versión sale del go.mod, no de este documento.
  $v = (Select-String -Path go.mod -Pattern 'sqlite-postgres-compat (v\S+)').Matches.Groups[1].Value
  go install "github.com/MauricioPerera/sqlite-postgres-compat/cmd/compat@$v"
  # queda en $env:USERPROFILE\go\bin\compat.exe  (en el PATH de Go)
  ```

  > Este documento fijaba `@v0.4.0` mientras el `go.mod` ya pedía `v0.5.1`, y en
  > la misma línea decía "usá la misma versión que el go.mod": se contradecía a
  > sí mismo. Por eso ahora el comando **deriva** la versión en vez de afirmarla.
  > Si un tag recién publicado falla con un 500 de `sum.golang.org`, es
  > propagación del checksum: reintentá una vez antes de diagnosticar.

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
- **La extensión `pgvector` habilitada en el destino.** Ver abajo: no es
  opcional y la auditoría NO te avisa si falta.

### `pgvector` es un prerrequisito duro (y el audit no lo detecta)

`articles.embedding` es una columna `vector(1536)` (CONTRACT-05). En PostgreSQL
compila a `vector(N)`, que **requiere la extensión `pgvector` en el destino**.

Contra un Postgres sin la extensión, el export falla así:

```json
{"status":"error","code":"ERR_SNAPSHOT","message":"apply base schema: ERROR: type \"vector\" does not exist (SQLSTATE 42704)"}
```

**Esto es deliberado, no un bug de `compat`**: el paquete declara la capacidad
explícitamente en vez de degradarla en silencio a `TEXT` (ver su `AGENTS.md`).
Pero la consecuencia operativa importa y hay que conocerla: **el paso 3 no lo
detecta**. `compat audit` devuelve `canonical_vectors: exact` porque es una
comprobación ESTÁTICA del contrato entre motores, no una sonda del destino. El
fallo aparece recién en el paso 4, al aplicar el esquema.

Por eso el chequeo va acá, antes de todo lo demás:

```powershell
psql $env:LIBRARIAN_EXPORT_PG_DSN -c "CREATE EXTENSION IF NOT EXISTS vector;"
psql $env:LIBRARIAN_EXPORT_PG_DSN -tAc "select extversion from pg_extension where extname='vector'"
# p.ej. 0.8.5
```

Si el destino es un PostgreSQL administrado que no ofrece `pgvector`, el export
**no se puede hacer** tal cual: hay que decidir primero qué pasa con esa
columna. No lo descubras el día del corte.

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

#### La forma de los dos archivos, para el caso real

Este documento nombraba los campos pero **no mostraba los archivos**, así que
quien migraba una instancia real tenía que deducir su forma leyendo el código de
un test o un reporte de contrato archivado. Acá están, y son todo lo que hace
falta escribir a mano:

`audit.json` — el contrato entre motores:

```json
{
  "source":      {"engine": "sqlite",   "version": {"major": 3}},
  "destination": {"engine": "postgres", "version": {"major": 17}},
  "required_features": ["canonical_full_text", "uuid", "json", "primary_keys",
    "canonical_check_constraints", "canonical_foreign_keys", "tables",
    "canonical_vectors"]
}
```

`migration.json` — el mismo contrato, más de dónde a dónde. **Modo `0600`: lleva
el DSN con la contraseña.**

```json
{
  "source_dsn": "librarian.db",
  "destination_dsn": "postgres://usuario:password@host:5432/base?sslmode=disable",
  "contract": { "…lo mismo que audit.json…" },
  "schema_ref": "schema.json"
}
```

`schema_ref` es relativo al directorio del propio `migration.json`, y apunta al
archivo que produce el paso 2.

### 2. Generar el JSON del esquema desde el binario de librarian

```powershell
go build -o $lb ./cmd/librarian
& $lb --dump-schema (Join-Path $EXPORT_DIR "schema.json") --db $DB_PATH
```

`--dump-schema` serializa el esquema canónico a JSON indentado y sale. Sigue sin
necesitar `LIBRARIAN_JWT_SECRET`, pero **desde CONTRACT-13 SÍ necesita la base de
datos**: el esquema canónico ya no es puro código, es `schema.Build()` **más** las
tablas de los tipos de contenido dinámicos persistidos en la propia base. Un dump
que no la leyera produciría un `schema_ref` que deja esas tablas — y todas sus
filas — fuera del export, en silencio.

La base se localiza, en este orden: `--db <path>` / `--db=<path>`, luego
`LIBRARIAN_DB`, luego `librarian.db`. **Si ese archivo no existe, el comando
FALLA con exit≠0**; nunca cae de vuelta al esquema de solo-código ni deja que
SQLite cree una base vacía (un path mal escrito produciría un dump verosímil pero
incompleto). Si la base existe pero no tiene la tabla de registro
`content_types` (una base anterior a CONTRACT-13), emite el esquema de código y
eso **es** completo para esa base: una definición solo puede vivir en esa tabla.

Acepta también `librarian --dump-schema --db <path>` (a stdout) o
`librarian --dump-schema=path.json --db=<path>`. Este JSON es el `schema_ref`
que consume `compat copy`.

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

Las features son las que `InferFeatures` deriva del esquema, así que la lista
CRECE con el esquema: desde CONTRACT-05 aparece también `canonical_vectors`
(`status: exact`).

**`required_features: []` hace que este paso no compruebe NADA — y salga 0.**
Medido siguiendo este mismo documento en una instalación en limpio:

```
$ compat audit audit.json
[]
audit_exit=0
```

Una lista vacía es un contrato que no exige nada, así que no hay nada que
auditar y el resultado es una lista vacía de hallazgos con código de salida
exitoso: **indistinguible de un audit que pasó**. Es el único paso del
procedimiento que puede mentirte por omisión, y este documento decía que la
dejaras vacía a la vez que mostraba siete hallazgos como salida esperada. Las
dos cosas no podían ser ciertas.

**Regla: si el paso 3 imprime `[]`, el paso 3 no se hizo.** Tiene que imprimir
una línea por feature, todas `exact`. Si tu esquema usa capacidades que no están
en la lista de arriba, agregalas; la lista crece con el esquema y por eso el
`compat copy` del paso 4 las infiere por su cuenta — pero **`copy` infiriendo no
sustituye al audit**, porque para cuando `copy` corre ya estás escribiendo en el
destino.

`compat copy` sí infiere las features solo, así que el `contract` que va DENTRO
de `migration.json` puede llevar la lista vacía sin perder nada. El que no puede
es `audit.json`.

**`canonical_vectors: exact` no significa que el destino pueda recibirlo.**
Significa que la capacidad se traduce sin pérdida entre motores. Que el destino
tenga `pgvector` es un hecho de infraestructura que este paso no mira — ver los
prerrequisitos.

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

Para una instancia real (no la fixture), ese test no aplica. Consultá el destino
a mano, y **mirá estas tres cosas** — son las que un digest verde no distingue
de un problema:

**a) Las tablas dinámicas y la IDENTIDAD de sus filas.** Son las que más
transformaciones atravesaron: `CONTRACT-18` reconstruye la tabla entera en cada
edición de campos.

```powershell
psql $env:LIBRARIAN_EXPORT_PG_DSN -tAc "select id, created_at from cpt_eventos"
```

Los `id` y `created_at` tienen que ser **los mismos** que en la origen. Un
registro con los mismos valores pero identidad nueva es otro registro que se le
parece, y toda referencia externa a él ya está rota.

**b) El recuento de tablas.** Esperá las del esquema canónico **+1**: `compat`
crea `__compat_schema`, su propia metadata. (Las otras tres tablas internas del
paquete —`__compat_change_journal`, `__compat_applied_changes`,
`__compat_capture_state`— solo aparecen con captura de cambios, que `copy` no
usa.)

**c) La columna vectorial, CON DATOS.** Este punto existe porque en la ejecución
real del 2026-07-25 casi se omite: `articles` estaba **vacía** en producción, así
que el export "pasó" sin haber ejercitado nunca la columna `vector(1536)`. Un
digest verde sobre una tabla vacía no prueba nada sobre su tipo más delicado.

Si la tabla vectorial está vacía en la origen, cargá una fila de prueba **en una
copia** antes de exportar, y confirmá en el destino que llegó como tipo NATIVO y
que es operable:

```powershell
psql $env:LIBRARIAN_EXPORT_PG_DSN -tAc "select format_type(a.atttypid, a.atttypmod) from pg_attribute a join pg_class c on c.oid = a.attrelid where c.relname='articles' and a.attname='embedding'"
# esperado: vector(1536)   <- no 'text'
psql $env:LIBRARIAN_EXPORT_PG_DSN -tAc "select vector_dims(embedding) from articles limit 1"                        # 1536
psql $env:LIBRARIAN_EXPORT_PG_DSN -tAc "select round((embedding <=> embedding)::numeric, 10) from articles limit 1" # 0
```

El operador `<=>` corriendo sobre el dato migrado es la prueba de que no es texto
copiado: es un vector que pgvector entiende.

> Nota de formato, para no reportar un falso positivo: pgvector re-imprime los
> componentes en su forma canónica (`0.000000` se ve como `0`). No es pérdida —
> el digest de `compat` compara la forma canónica del portador, y por eso da
> igual.

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

### `compat copy` falla con `type "vector" does not exist` (`ERR_SNAPSHOT`)

Le falta `pgvector` al destino. **No es un problema de librarian ni de compat**:
es infraestructura faltante, y la auditoría no podía avisarlo. Ver
[el prerrequisito](#pgvector-es-un-prerrequisito-duro-y-el-audit-no-lo-detecta),
habilitá la extensión y re-corré desde el paso 4. No hay nada que corregir en el
esquema.

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

## Migración con el servicio arriba (`compat cutover`)

Ejercitado contra `librarian` el 2026-07-25, con una instancia real y escrituras
concurrentes. Lo que sigue es lo que se ejecutó y lo que se aprendió.

### Lo primero, porque cambia cómo se planifica

**`cutover` NO significa "nunca dejar de escribir".** Significa que la ventana
sin servicio es el **drenaje**, no la copia.

El drenaje termina cuando la captura ve `drain_polls` sondeos consecutivos sin
cambios nuevos. Si la aplicación sigue escribiendo, **esa condición no se cumple
nunca**. Medido: con una escritura cada 0.4 s y sondeo cada 0.5 s, el cutover
completó auditoría, captura y snapshot, y quedó drenando indefinidamente — a los
10 minutos el journal tenía 1234 entradas y seguía creciendo. No es un fallo del
paquete: es lo que la fase significa.

Entonces la secuencia real es:

```
1. Arrancar el cutover          ← servicio ARRIBA, escribiendo normalmente
2. Auditoría, captura, snapshot ← servicio ARRIBA (es la parte larga)
──────── acá empieza la caída, y es corta ────────
3. Detener las escrituras
4. El drenaje converge, verifica digests y devuelve "ready"
5. Apuntar el servicio al destino y arrancarlo
──────── acá termina ────────
```

La ganancia frente a `compat copy` es real y es grande: la ventana pasa de "toda
la copia" a "lo que tarde en drenar lo escrito mientras copiaba".

### El procedimiento

La configuración es la misma de `compat copy` más un bloque `options`:

```json
"options": { "poll_interval_ms": 500, "drain_polls": 3, "batch_limit": 500 }
```

```bash
compat cutover --dry-run cutover.json   # solo lectura: audita y muestra el plan
compat cutover cutover.json             # el real
```

El `--dry-run` no instala nada ni escribe: audita, cuenta filas por tabla y dice
si el destino ya tiene tablas. Corrélo siempre antes.

Salida de un cutover exitoso:

```
compat cutover: audit: exact coverage for 10 required features
compat cutover: capture: change capture installed on source
compat cutover: snapshot: imported into destination
compat cutover: catch-up: drained after 41 changes
{"status":"ready","source_digest":"6c955ae6…","destination_digest":"6c955ae6…","changes_applied":41}
```

`status: ready` con los dos digests iguales es la certificación. **El destino
todavía no está en uso**: apuntar el servicio es el paso 5, y es tuyo.

### Verificación, además del digest

Lo mismo que para `compat copy` (identidad de las filas, recuento de tablas,
columna vectorial con datos), y una específica de este camino: **que la ÚLTIMA
escritura anterior al quiesce esté en el destino**. Es lo que distingue un
drenaje completo de uno que cortó antes de tiempo.

En el ensayo: 47 artículos en los dos lados, y el artículo `durante-42` —el
último que la aplicación escribió antes de detenerse— presente en el destino.

La prueba que cierra el asunto es arrancar `librarian` contra el destino y
**servir desde ahí**: en el ensayo, autenticación con la misma credencial de la
fuente, el contenido migrado visible, y una escritura nueva devolviendo `201`.

## Batería dual-motor de `internal/auth` (CONTRACT-19)

Independiente del flujo de exportación de arriba: verifica que las funciones
públicas de `internal/auth` devuelven **lo mismo** contra SQLite real y contra
PostgreSQL 17 real. Está fuera de la suite default con el build tag
`dualengine` (mismo patrón que `exportfixture`) porque necesita un PostgreSQL
vivo. Sin `COMPAT_POSTGRES_DSN` **saltea** en vez de pasar en falso.

```powershell
$env:COMPAT_POSTGRES_DSN = "postgres://user:***@host:5432/db?sslmode=disable"
go test -tags dualengine -run TestDualEngineAuth -count=1 -v ./internal/auth
```

- Crea un **schema PostgreSQL propio y único por corrida**
  (`librarian_c19_<nanos>`), aplica ahí el esquema canónico completo (tablas +
  vistas) y lo borra con `DROP SCHEMA … CASCADE` al terminar. No toca `public`
  más allá de leer el tipo `vector` de pgvector, que se resuelve por
  `search_path`.
- El lado SQLite usa el camino de producción (`store.Open` → `store.EnsureSchema`
  → `store.SeedCatalogs`), así que la batería también prueba que el arranque real
  crea las vistas de CONTRACT-19.
- Requiere `pgvector` en el PostgreSQL destino, igual que la exportación: el
  esquema canónico declara `articles.embedding vector(1536)`.

## Batería dual-motor de `internal/server` (CONTRACT-20)

La parte 2 de la misma migración. A diferencia de la de `internal/auth`, esta
maneja el **mux HTTP real**: cada observación es un código de estado y un cuerpo
de respuesta producidos por los mismos handlers que atiende un cliente. Mismo
build tag, mismo comportamiento sin DSN (saltea).

```powershell
$env:COMPAT_POSTGRES_DSN = "postgres://user:***@host:5432/db?sslmode=disable"
go test -tags dualengine -run TestDualEngineServer -count=1 -v ./internal/server
```

Cubre las cinco superficies del contrato: el CRUD de `articles` **con y sin
`embedding`** (comparado componente a componente), el de `products` incluida la
violación de `sku` duplicado, el de `terms` con jerarquía y sus vistas, la
resolución de permisos de `authz.go`, y el CRUD genérico de un tipo **dinámico
creado durante la prueba**. Compara explícitamente el ORDEN de cada listado y
los 404 sobre ids inexistentes y malformados.

Además:

```powershell
go test -tags dualengine -run TestDualEngineVectorPrecision -count=1 -v ./internal/server
```

mide el **límite de precisión de `vector`**: `pgvector` almacena `float4`, así
que una componente que necesita más de precisión simple se redondea en
PostgreSQL y se conserva entera en SQLite. Es una propiedad de la columna
`vector(1536)` que declaró CONTRACT-05, no del camino de lectura/escritura;
el test la mide y la reporta en vez de esconderla.

> **Colación.** PostgreSQL ordena `TEXT` por la colación de la base
> (típicamente `en_US.utf8`) y SQLite por bytes, así que un `ORDER BY` sobre
> texto con puntuación o mayúsculas NO da la misma secuencia en los dos motores.
> Los listados NO paginados imponen su orden final en Go (comparación byte a
> byte, que es la que SQLite ya hace, así que el orden que ve producción no
> cambia); los paginados de `internal/server` ordenan por `created_at` (ancho
> fijo) e `id` (UUID), formas para las que ambas comparaciones coinciden.
> CONTRACT-20B extendió esto a `internal/auth` —`ListUsers`, `ListAPIKeys`,
> `RolePermissions` y `rolesForUser`— y consolidó los helpers compartidos en
> `internal/dual`. Ver la nota COLLATION en `internal/dual/dual.go`.

---

## Elegir el motor (CONTRACT-21)

Desde CONTRACT-21 el binario arranca y sirve sobre **SQLite/libSQL o
PostgreSQL 17**, y lo decide la configuración. Dos variables, una sola decisión:

| Variable | Valores | Qué hace |
|---|---|---|
| `LIBRARIAN_ENGINE` | `sqlite` (por defecto), `postgres` | elige el motor |
| `LIBRARIAN_DB` | ruta de archivo, o DSN de PostgreSQL | la conexión |

```powershell
# SQLite — exactamente lo que corre hoy; no hace falta declarar nada
$env:LIBRARIAN_DB = "/opt/librarian/data/librarian.db"

# PostgreSQL
$env:LIBRARIAN_ENGINE = "postgres"
$env:LIBRARIAN_DB     = "postgres://user:***@host:5432/librarian?sslmode=disable"
```

**La elección es inequívoca y falla cerrada.** Una configuración contradictoria
NO arranca, y el mensaje dice qué se esperaba. En particular, un `LIBRARIAN_DB`
que es una URL de PostgreSQL con `LIBRARIAN_ENGINE` sin definir **se rechaza**:
caer a SQLite ahí crearía un archivo local vacío y el servicio quedaría sano,
sirviendo de una base vacía, que es el modo de falla que este diseño existe para
volver imposible.

```
librarian: LIBRARIAN_DB is a PostgreSQL connection URL but LIBRARIAN_ENGINE is not set
(which defaults to sqlite): refusing to start on SQLite with a PostgreSQL DSN, because
that would silently create an empty local database file and serve from it.
Set LIBRARIAN_ENGINE=postgres, or point LIBRARIAN_DB at a file path
```

`--dump-schema` usa la MISMA resolución, así que el dump y la instancia nunca
pueden discrepar sobre qué es este despliegue. `--db` sigue sobrescribiendo solo
el DSN.

### pgvector es un prerrequisito, no un detalle

El esquema canónico declara `articles.embedding vector(1536)` (CONTRACT-05), así
que en PostgreSQL **la extensión `pgvector` es obligatoria** y el primer arranque
de una instalación en limpio falla sin ella — con un mensaje que lo dice:

```
librarian: the pgvector extension is required on PostgreSQL and its `vector` type is not
resolvable by this connection: librarian's canonical schema declares articles.embedding as
vector(1536) (CONTRACT-05), so the schema cannot be created without it.
Run `CREATE EXTENSION IF NOT EXISTS vector;` in the target database as a superuser, and if it
is installed into a schema other than the one this connection uses, make that schema visible
on the connection's search_path
```

La comprobación es `to_regtype('vector')`, no `pg_extension`: lo que importa no
es que la extensión esté instalada sino que **esta conexión pueda nombrar el
tipo**, que es distinto si `pgvector` vive en un esquema fuera del `search_path`.

### La capacidad vectorial es opcional (CONTRACT-23)

Ese prerrequisito solo aplica si la instalación **declara** la capacidad
vectorial, que es lo que hace por defecto:

| Variable | Valores | Qué hace |
|---|---|---|
| `LIBRARIAN_VECTOR` | `enabled` (por defecto; también `on`/`true`/`1`), `disabled` (también `off`/`false`/`0`) | declara si esta instalación tiene la columna `articles.embedding vector(1536)` |

Con `LIBRARIAN_VECTOR=disabled` el esquema canónico **no declara ningún
`vector(N)`**, así que no hay nada que `pgvector` tenga que resolver: la
instalación arranca y sirve sobre un PostgreSQL administrado que no ofrezca la
extensión. A cambio, un pedido que traiga `embedding` se **rechaza con 400** y
una explicación — no se ignora en silencio — y las lecturas simplemente no traen
el campo. Un valor desconocido de la variable es un error de arranque, nunca un
silencio.

> **LA ELECCIÓN ES IRREVERSIBLE DESPUÉS DEL PRIMER ARRANQUE.** `EnsureSchema`
> crea solo las tablas FALTANTES y jamás altera una existente, así que habilitar
> la capacidad más tarde **no agregaría** la columna y deshabilitarla **no la
> quitaría**. Cambiar la declaración sobre una instalación ya creada **no
> arranca**:
>
> ```
> librarian: this installation was created WITHOUT the vector capability (its articles table
> has no "embedding" column) but the configuration now declares it ENABLED
> (LIBRARIAN_VECTOR=enabled): refusing to start. ... Set LIBRARIAN_VECTOR=disabled to keep
> serving this installation, or create a NEW installation with the capability enabled and move
> the data into it with `compat copy`
> ```
>
> La comprobación se hace contra la BASE (la columna física en el catálogo del
> motor), no contra la metadata `__compat_schema` — esa fila la escribe el mismo
> camino cuya creencia está en duda.

`--dump-schema` refleja la elección de la instalación (la lee de la tabla física
cuando la instalación existe), lo cual importa más de lo que parece: ese artefacto
es el `schema_ref` que consume `compat copy`, así que un dump que declarara la
columna crearía un `vector(1536)` en el DESTINO y le devolvería el requisito de
`pgvector` a la instalación que existe justamente para no tenerlo.

Toda instalación existente tiene la columna, así que el valor por defecto
(`enabled`) es exactamente lo que ya corre: actualizar el binario no cambia nada.

### Instalación en limpio sobre PostgreSQL

```sql
CREATE DATABASE librarian;
\c librarian
CREATE EXTENSION IF NOT EXISTS vector;   -- requiere superusuario
```

(el `CREATE EXTENSION` **solo** si la instalación va a declarar la capacidad
vectorial, que es el valor por defecto; con `LIBRARIAN_VECTOR=disabled` se omite
y no hace falta superusuario)

y arrancar con `LIBRARIAN_ENGINE=postgres`. El binario crea todo el esquema
(tablas, vistas y metadata) en el primer arranque; el segundo es un no-op.

**La primera identidad se crea fuera de banda**, igual que en SQLite (ver
`docs/DEPLOY.md` § "Conseguir una identidad"): una superficie que exige una
identidad no puede ser la que crea la primera. A partir de ahí todo va por HTTP.

## Batería dual-motor de `internal/store` (CONTRACT-21)

```powershell
$env:COMPAT_POSTGRES_DSN = "postgres://user:***@host:5432/db?sslmode=disable"
go test -tags dualengine -run TestDualEngineStore -count=1 -v ./internal/store
```

Es la más importante de las tres: `internal/store` es el único paquete cuyas
sentencias corren dentro de **transacciones que mezclan DDL, SQL propio y
metadata**, y su modo de falla no es una respuesta equivocada sino una base a
medio cambiar. La batería corre el arranque en limpio, la creación y la EDICIÓN
de un tipo dinámico, y **fuerza fallos a mitad de las dos transacciones** (el
`DROP TABLE` bloqueado por una FK, y el `INSERT` de metadata contra una tabla
borrada), comparando el ESTADO de la base después. Los dos motores no se
comportan igual ante un error dentro de una transacción — PostgreSQL la envenena
entera (25P02), SQLite no —, así que la igualdad hay que medirla, no suponerla.

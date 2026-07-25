# CONTRACT-21 — `internal/store` dual-motor y elección de motor (parte 3 de 3)

Base: `009e86a` (CONTRACT-19, 20, 20B y 20C completos y en `main`). Árbol **SIN commitear**, como
pide el contrato.

**Resultado: LISTO. `librarian` arranca y sirve sobre PostgreSQL 17.** No exporta a PostgreSQL:
**corre** sobre PostgreSQL. El binario real, sobre una base recién creada, aplicó su esquema,
creó usuarios, autenticó, otorgó permisos, definió un tipo de contenido dinámico, cargó, editó,
listó y borró contenido, reconstruyó la tabla de ese tipo, y sobrevivió a dos reinicios — todo por
HTTP, con la transcripción completa abajo. El **mismo guion** contra SQLite produce la **misma
salida**, línea por línea, con los uuids y los instantes normalizados.

Lo que faltaba de las partes 1 y 2 está cerrado: `internal/store` ya no tiene una sola sentencia
atada a un motor, sus dos sondas a `sqlite_master` son ahora `compat.Store.TableExists`, y la
atomicidad de `CreateContentType`/`EditContentType` está **medida** en los dos motores, forzando
fallos a mitad de la transacción.

`sqlite-postgres-compat` **no se tocó**: su `git status` quedó exactamente como estaba al empezar
(solo el `experiments/vector/vector-exp.exe` sin trackear que ya estaba). Ninguna dependencia nueva,
ningún permiso nuevo, ningún cambio en el contrato público de las rutas HTTP.

> **Un hallazgo que el orquestador tiene que leer antes de desplegar**: la detección de nombre de
> tipo duplicado comparaba el TEXTO del error de SQLite, así que en PostgreSQL **nunca habría
> disparado** — un nombre repetido habría sido un 500 en vez del 400 que CONTRACT-13 especifica.
> Es la misma clase de defecto que CONTRACT-20 encontró en `products.sku` y `terms.slug`, en el
> único lugar que aquel contrato no podía alcanzar. Corregido con `compat.Store.IsUniqueViolation`
> (§ Decisión 5), y probado en los dos motores.

---

## Resumen de qué se tocó

| Archivo | Qué |
|---|---|
| `internal/config/config.go` | **T2**: `ResolveEngine` — la elección de motor, inequívoca y fail-closed |
| `internal/store/store.go` | **T2**: `Open(engine, dsn)` + `TargetFor`; **T2**: el fallo legible sin `pgvector`; **T1**: 2 sentencias |
| `internal/store/contenttypes.go` | **T1**: sonda #1 → `TableExists`; 2 sentencias; UUIDs en Go; `IsUniqueViolation`; orden en Go |
| `internal/store/contenttypes_edit.go` | **T1**: sonda #2 → `TableExists`; 7 sentencias; UUIDs en Go |
| `internal/server/server.go` | **T2**: `Deps.DB *sql.DB` → `Deps.Store *compat.Store` |
| `internal/server/contenttypes.go`, `ui_contenttypes.go`, `ui_content.go`, `ui_nav.go`, `content.go` | acompañar las firmas (`store.FromDB(h.db)` → `h.store`) |
| `cmd/librarian/main.go` | **T2**: arranque y `--dump-schema` por motor elegido; `redactDSN` |
| `internal/store/dualengine_contract21_test.go` (**nuevo**) | **T3**: batería dual-motor de `internal/store` (tag `dualengine`) |
| `internal/config/config_test.go` | **T2**: `TestResolveEngine`, la matriz de configuración |
| `internal/auth/dualengine_contract19_test.go`, `internal/server/dualengine_contract20_test.go` | el lado PostgreSQL pasa al camino de producción |
| `docs/OPERATIONS.md` | elegir el motor, `pgvector`, instalación en limpio, la batería nueva |

**Borrado**: `store.FromDB` (§ Decisión 2).

---

## T2 — La elección de motor: la forma, y por qué

**Decisión: una variable explícita `LIBRARIAN_ENGINE`, CONTRASTADA contra la forma del DSN.**

| Variable | Valores | Qué hace |
|---|---|---|
| `LIBRARIAN_ENGINE` | `sqlite` (por defecto), `postgres` | elige el motor |
| `LIBRARIAN_DB` | ruta de archivo, o DSN de PostgreSQL | la conexión (nombre histórico, sin cambios) |

Las dos alternativas que el contrato menciona se evaluaron y **ninguna sola alcanza**:

- **Inferirlo del DSN, solo.** Lee bien para `postgres://…`, pero el lado SQLite es "cualquier otra
  cosa": un error de tipeo (`postgress://`), un DSN que perdió el esquema en un template, o una
  variable vacía significan **SQLite en silencio**, y SQLite **crea el archivo con gusto**. El
  proceso arranca sano, sirviendo una base vacía, mientras el operador cree estar en PostgreSQL. Es
  exactamente el fallo que el contrato nombra, y es el peor de todos porque nada está lo bastante
  roto como para notarlo.
- **Una variable explícita, sola.** El espejo: quien pone `LIBRARIAN_DB` con una URL de PostgreSQL y
  se olvida de `LIBRARIAN_ENGINE` obtiene el mismo SQLite vacío — y esta vez la configuración
  **decía en voz alta** cuál era la intención.

Así que la variable decide y **un DSN que la CONTRADICE es un error de arranque que dice qué se
esperaba**. El default (variable ausente) sigue siendo SQLite — que es lo que corre hoy en
producción — pero solo cuando el DSN no parece de PostgreSQL, así que el default nunca puede ser lo
que se traga una intención de PostgreSQL.

Salida REAL del binario, con todas las configuraciones equivocadas (password enmascarado):

```
### A. pgvector missing (engine=postgres, clean database)
exit=1
librarian: the pgvector extension is required on PostgreSQL and its `vector` type is not resolvable
by this connection: librarian's canonical schema declares articles.embedding as vector(1536)
(CONTRACT-05), so the schema cannot be created without it. Run `CREATE EXTENSION IF NOT EXISTS
vector;` in the target database as a superuser, and if it is installed into a schema other than the
one this connection uses, make that schema visible on the connection's search_path

### B. postgres DSN, engine NOT set — must refuse, never fall back to SQLite
exit=1
librarian: LIBRARIAN_DB is a PostgreSQL connection URL but LIBRARIAN_ENGINE is not set (which
defaults to sqlite): refusing to start on SQLite with a PostgreSQL DSN, because that would silently
create an empty local database file and serve from it. Set LIBRARIAN_ENGINE=postgres, or point
LIBRARIAN_DB at a file path

### C. engine=postgres, DSN is a file path
exit=1
librarian: LIBRARIAN_ENGINE=postgres but LIBRARIAN_DB ("librarian.db") is not a PostgreSQL
connection URL: expected a postgres:// or postgresql:// URL, or a libpq keyword/value string
containing host= or dbname=

### D. engine=postgres, no DSN
exit=1
librarian: LIBRARIAN_ENGINE=postgres requires LIBRARIAN_DB to be set to a PostgreSQL connection URL
(for example postgres://user:password@host:5432/librarian?sslmode=disable); there is no default DSN
for PostgreSQL

### E. unknown engine
exit=1
librarian: LIBRARIAN_ENGINE="mysql" is not a supported engine: expected "sqlite" (the default) or
"postgres"
```

Los mismos casos, más los que SÍ tienen que arrancar, están fijados en `TestResolveEngine`
(12 casos, `internal/config/config_test.go`).

### El `Target` y la conexión salen del mismo lugar — por construcción, no por convención

El contrato lo exige y estaba escrito a mano en **dos** lugares:

```go
// internal/server/server.go:48 (antes)
store: &compat.Store{Target: schema.SQLiteTarget, DB: deps.DB},
// internal/store/contenttypes.go (antes) — store.FromDB
return &compat.Store{Target: schema.SQLiteTarget, DB: db}
```

Los dos eran correctos **solo porque había un motor**. Con dos, un literal así es la forma de correr
un pool de PostgreSQL emitiendo `?` y compilando DDL de SQLite, y compila perfecto.

La resolución tiene tres piezas y ninguna es opcional:

1. **`store.Open(engine, dsn)`** resuelve el `Target` y llama a `compat.OpenStore(target, dsn)`, que
   elige el driver **desde ese mismo target**. El `Store` devuelto no puede llevar otro.
2. **`store.FromDB` se BORRÓ.** Era la puerta trasera: tomaba un `*sql.DB` desnudo y le adosaba
   `SQLiteTarget`. Sus 7 llamadas pasaron a `h.store`, que es el store real de la instancia.
3. **`server.Deps.DB *sql.DB` → `server.Deps.Store *compat.Store`.** `NewMux` ya no compone ningún
   target: recibe el que abrió el proceso. `Deps.Store == nil` es un error explícito, porque ya no
   hay motor que pueda asumir.

Después de esto, `grep` sobre todo el código de producción no encuentra **ni un solo**
`compat.Store{...}` literal.

### El fallo legible sin `pgvector`

`requireVectorType` (`internal/store/store.go`) corre al principio de `EnsureSchema`, solo en
PostgreSQL, y produce el mensaje del bloque A de arriba en vez de
`ERROR: type "vector" does not exist (SQLSTATE 42704)`.

**Pregunta la resolubilidad del TIPO, no la presencia de la extensión**, y eso responde de una la
pregunta de red-team "¿y si `pgvector` está pero en otro esquema?":

```sql
SELECT to_regtype('vector') IS NOT NULL
```

`pg_extension` diría "sí, está instalada" mientras el `CREATE TABLE` sigue fallando porque el tipo
no está en el `search_path` de esta sesión. `to_regtype` resuelve el nombre como lo haría el parser,
así que contesta la pregunta que de verdad importa — *¿puede ESTA conexión nombrar este tipo?* — y
su respuesta NULL no es un error, así que una extensión ausente no aborta ninguna transacción.

### `--dump-schema` en los dos motores

Usa **la misma** `config.ResolveEngine`, así que el dump y la instancia no pueden discrepar sobre
qué es este despliegue. `--db` sigue sobrescribiendo solo el DSN. La guarda "el archivo tiene que
existir" pasó a ser SQLite-only, y no por comodidad: un DSN de PostgreSQL no nombra ningún archivo,
y PostgreSQL **no crea** una base a partir de un nombre mal escrito como SQLite crea un archivo — el
error de conexión es ruidoso, que es la propiedad que esa guarda existe para preservar.

### `redactDSN`

El DSN dejó de ser una ruta inofensiva: una URL de PostgreSQL lleva las credenciales, y la línea de
arranque lo imprimía tal cual. Un password en el log del servicio es una fuga que ningún contrato
posterior puede deshacer. Salida real:

```
librarian: schema ready on postgres://postgres:***@31.220.22.176:5443/librarian_c21?sslmode=disable (postgres), listening on 127.0.0.1:8099
```

---

## T1 — `internal/store` dual-motor

### Las dos sondas → `compat.Store.TableExists`

`registryPresent` (`contenttypes.go`) y `physicalTableExists` (`contenttypes_edit.go`) eran
`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, con una guarda que
fallaba con "librarian's runtime engine is SQLite". El RECON del contrato es exacto: `TableExists`
existe para esos dos lugares.

Lo que las dos hacían por comentario, ahora lo hace el primitivo por diseño: consulta el catálogo
FÍSICO (`sqlite_master` / `pg_class`), **deliberadamente no** `__compat_schema` — que es justo lo que
estas dos operaciones están por reescribir, así que la metadata no puede ser también el oráculo — y
en PostgreSQL restringe la búsqueda a `current_schema()`, que es exactamente donde aterriza el
`CREATE TABLE` sin calificar de este mismo código.

Verificado además que una VISTA responde `false` (`tableExists view(user_role_names)=false`), lo cual
importa: si respondiera `true`, un tipo de contenido no podría crearse con el nombre de una vista
existente y el diff discreparía del DDL.

### Las sentencias

Cero `?` literales en el paquete. Salida real:

```
$ grep -rn '?' internal/store/*.go | grep -v '_test.go'
internal/store/contenttypes.go:155:// schema?", and it is what every one of the three former schema.Build() call
internal/store/store.go:50:// then emits `?` against PostgreSQL) is not expressible through this API. That
internal/store/store.go:165:// actually matters — "can THIS session name this type?" — and its NULL answer is
internal/store/store.go:250:// so the same statement is emitted with `?` on SQLite and `$1, $2` on
```

Las cuatro son **comentarios**. Ninguna sentencia. (La prueba fuerte no es el grep: es que la mitad
PostgreSQL de la batería pasa, y un `?` suelto sería error de sintaxis ahí.)

Mapa:

| Función | Sentencias | Camino |
|---|---|---|
| `writeFullSchemaMetadata` | 1 (upsert de metadata) | crudo + `dual.Bind` |
| `seedNames` | 1 (×3 catálogos) | crudo + `dual.Bind` |
| `registryPresent` | sonda | **`compat.TableExists`** |
| `LoadContentTypeDefinitions` | 1 (LEFT JOIN, sin parámetros) | crudo, orden final en Go |
| `CreateContentType` | 2 (INSERT tipo + INSERT campo ×N) | crudo + `dual.Bind`, UUID en Go |
| `LoadContentTypeFields` | 2 | crudo + `dual.Bind` |
| `physicalTableExists` | sonda | **`compat.TableExists`** |
| `applyRegistryEdit` | 5 (SELECT, DELETE, UPDATE park, UPDATE final, INSERT) | crudo + `dual.Bind`, UUID en Go |
| `compileRebuild` | 2 (`INSERT … SELECT`, sin parámetros) | ya era dual, sin cambios |

Todas van por SQL crudo y **ninguna por rutina**, por la razón estructural que CONTRACT-19 y 20 ya
documentaron y que acá es todavía más terminante: `CallRoutine`/`QueryRoutine` abren **su propia**
transacción, y estas sentencias corren dentro de la transacción que la función posee. Anidarlas
rompería la atomicidad, que es el único punto de este paquete. Sobre SQLite además deadlockearía
(compat fija el pool a una conexión).

### Decisión 1 — `RETURNING id` → UUID generado en Go

`CreateContentType` leía el id con `INSERT … RETURNING id`. Se resolvió como los dos contratos
anteriores: `dual.NewUUID()` genera el id antes del INSERT y el `DEFAULT gen_random_uuid()` de la
columna se conserva como red de seguridad para escrituras que no vengan de la app. Extendido a
`content_type_fields.id`. `content_types.created_at` pasa a escribirse desde la app con `dual.Now()`
(RFC3339Nano de ancho fijo), como toda la familia `timestamp` de este proyecto desde CONTRACT-20C.

Beneficio colateral que importa acá y no importaba en los otros dos: los INSERT de campos ya no
dependen de un valor que la sentencia anterior tuvo que devolver.

### Decisión 2 — El ORDEN de las definiciones se impone en Go

`LoadContentTypeDefinitions` conserva `ORDER BY t.name, f.ordinal` como base estable — es lo que
agrupa los campos de un tipo y los pone en orden de `ordinal`, que es un INTEGER y ambos motores
ordenan igual — pero el orden de los TIPOS se re-impone byte a byte con `dual.SortByKeys`.

**No es cosmético**: ese orden es el orden de las tablas en el ESQUEMA CANÓNICO COMPUESTO, así que
decide el contenido de `--dump-schema` y el orden en que `ApplySchema` crea tablas.

Y el fixture **no es complaciente** — la lección de CONTRACT-20B es que una comparación cuyos datos
no pueden producir una diferencia no prueba nada. Los dos nombres elegidos (`blog_notas` y
`bloganuncios`) discrepan de verdad, medido en vivo contra los dos motores:

```
[sqlite]   RAW SQL ORDER BY name = [blog_notas bloganuncios eventos]  (Go-imposed order = [blog_notas bloganuncios eventos])
[postgres] RAW SQL ORDER BY name = [bloganuncios blog_notas eventos]  (Go-imposed order = [blog_notas bloganuncios eventos])
```

El SQL crudo **diverge**; el orden que la aplicación observa, no. Consecuencia comprobada abajo: el
`--dump-schema` de los dos motores es **byte-idéntico**.

### Decisión 3 — La atomicidad, y por qué los dos motores llegan al mismo estado por caminos distintos

Este es el punto delicado del contrato y merece decirse con precisión, porque **no es suerte**.

PostgreSQL marca la transacción ENTERA como abortada en la primera sentencia que falla: toda
sentencia posterior falla con `25P02` y solo se acepta `ROLLBACK`. SQLite no: una sentencia fallida
deja la transacción usable. Esa divergencia es **invisible** en `CreateContentType` y
`EditContentType` porque las dos **retornan en el primer error** y lo siguiente que corre es el
`defer tx.Rollback()`: nunca se emite una sentencia después de un fallo.

O sea: la atomicidad se preserva, pero por una propiedad del flujo de control, no del motor. Está
escrita en el código como comentario porque cualquier edición futura que intente "recuperarse" de un
error a mitad de transacción y seguir **rompería la atomicidad solo en PostgreSQL**, y en silencio.

El **park de dos fases** de `applyRegistryEdit` (CONTRACT-18) se conserva por la misma razón,
ampliada: PostgreSQL también valida los índices UNIQUE por sentencia, así que el cross-rename
violaría la restricción igual — pero ahí la consecuencia es peor, porque la transacción queda
envenenada con el DDL ya ejecutado. El park hace la violación inalcanzable en los dos.

### Decisión 4 — La metadata y el seed conservan `ON CONFLICT`

A diferencia de CONTRACT-19/20, que **eliminaron** todo `ON CONFLICT DO NOTHING` porque el conflicto
era evitable por construcción, acá se conserva en `writeFullSchemaMetadata`
(`ON CONFLICT("key") DO UPDATE SET "value" = excluded."value"`) y en `seedNames`
(`ON CONFLICT(name) DO NOTHING`): el conflicto **no** es evitable — la fila de metadata existe en
todo arranque después del primero, y el seed es idempotente por diseño. La forma es byte-idéntica en
SQLite 3.24+ y PostgreSQL 9.5+, y es la que usa el propio `writeSchemaMetadata` de compat.

### Decisión 5 — `IsUniqueViolation` en vez del texto del mensaje (EL HALLAZGO)

```go
// antes
return strings.Contains(msg, "UNIQUE constraint failed") &&
	strings.Contains(msg, schema.ContentTypesTable+".name")
```

Esa es la redacción de **SQLite**. PostgreSQL dice `duplicate key value violates unique constraint`
y reporta `SQLSTATE 23505`. En PostgreSQL el match **nunca habría disparado**: un nombre de tipo
repetido habría dejado de ser el 400 limpio que CONTRACT-13 especifica y se habría vuelto un 500,
sin que nada dejara de compilar, hasta que un administrador repitiera un nombre.

Ahora usa `compat.Store.IsUniqueViolation`, que clasifica por el **código estructurado del driver**
(`SQLITE_CONSTRAINT_UNIQUE`/`PRIMARYKEY`, `SQLSTATE 23505`) con `errors.As`.

La limitación documentada del primitivo (no dice CUÁL restricción) no molesta acá: `content_types`
tiene `PRIMARY KEY(id)` y `UNIQUE(name)`, y el `id` es un UUID v4 que esta misma función generó tres
líneas antes — el mismo argumento que `products`/`terms` usan para las suyas.

Probado en los dos motores: `createContentType duplicate err=yes sentinel=true types=1`, y por HTTP
`POST /content-types duplicate -> 400 {"error":"a content type with this name already exists"}` en
SQLite **y** en PostgreSQL.

---

## Verificación — salida REAL

### build / vet / gofmt

```
$ go build ./...        (sin salida = OK)
$ go vet ./...          VET-OK
$ gofmt -l .            GOFMT-CLEAN  (sin salida)
$ go vet -tags exportfixture ./internal/server && go vet -tags dualengine ./...
VET-TAGS-OK
```

### `go test ./... -count=1`, dos veces

```
=== RUN 1 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.244s
ok  	github.com/MauricioPerera/librarian/internal/auth	4.351s
ok  	github.com/MauricioPerera/librarian/internal/config	1.164s
ok  	github.com/MauricioPerera/librarian/internal/dual	1.159s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.184s
ok  	github.com/MauricioPerera/librarian/internal/server	34.494s
ok  	github.com/MauricioPerera/librarian/internal/store	3.670s
=== RUN 2 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.289s
ok  	github.com/MauricioPerera/librarian/internal/auth	4.455s
ok  	github.com/MauricioPerera/librarian/internal/config	1.167s
ok  	github.com/MauricioPerera/librarian/internal/dual	1.177s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.335s
ok  	github.com/MauricioPerera/librarian/internal/server	33.179s
ok  	github.com/MauricioPerera/librarian/internal/store	3.595s
```

### Las tres baterías dual-motor, contra PostgreSQL 17 real

Motor destino verificado en vivo antes de empezar:
`PostgreSQL 17.10 (Debian 17.10-1.pgdg12+1) on x86_64-pc-linux-gnu`, `pgvector` disponible,
`datcollate=en_US.utf8` (la colación que hace discrepantes los pares del fixture).

```
$ export COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable'
=== C19 ===
ok  	github.com/MauricioPerera/librarian/internal/auth	31.817s
=== C20 ===
ok  	github.com/MauricioPerera/librarian/internal/server	49.111s
=== C21 ===
ok  	github.com/MauricioPerera/librarian/internal/store	27.521s
```

Las de CONTRACT-19 y CONTRACT-20 son ahora **más fuertes que cuando se escribieron**: su lado
PostgreSQL construía el esquema con `compat.ApplySchema` y un seed a mano, porque `internal/store`
todavía era SQLite-only. Ahora pasa por el **camino de producción** (`store.Open` →
`store.EnsureSchema` → `store.SeedCatalogs`), igual que el lado SQLite.

### La batería de `internal/store` — 40 observaciones idénticas

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineStore -count=1 -v ./internal/store
=== RUN   TestDualEngineStore
    dualengine_contract21_test.go:86: transcript (40 lines, identical on both engines):
        boot#1 ensureSchema err=none
        boot#1 seedCatalogs err=none
        boot#2 ensureSchema err=none
        boot#2 seedCatalogs err=none
        catalogs roles=4 permissions=8 taxonomies=2
        clean install tables=14 views=7 routines=30 dynamic=0
        tableExists users=true absent=false
        tableExists view(user_role_names)=false
        author created=true
        createContentType eventos err=none
        after create table-exists=true registry-types=1 registry-fields=4
        metadata knows the dynamic table=true
        stored fields=[titulo:text lugar:text asistentes:integer gratis:boolean]
        createContentType duplicate err=yes sentinel=true types=1
        createContentType squatted err=yes
        ATOMIC create: types=1 fields=4 metadata-has-squatted=false
        definitions order=[blog_notas bloganuncios eventos]
        composed schema dynamic-table-order=[cpt_blog_notas cpt_bloganuncios cpt_eventos]
        dump-schema bytes=80272 sha-like-prefix={
          "tables": [
            {
          
        dump-schema digest=55d1009d244f913c
        editContentType err=none added=[resumen] removed=[lugar gratis] renamed=[[titulo encabezado]]
        after edit fields=[encabezado:text asistentes:integer resumen:text]
        after edit carried row encabezado="Charla" asistentes=40 resumen-null=true
        after edit identities preserved=true
        crossRename err=none fields=[resumen:text asistentes:integer encabezado:text]
        crossRename data followed identity resumen="Charla"
        ROLLBACK-midway err=yes
        ROLLBACK-midway fields=[resumen:text asistentes:integer encabezado:text] identities-intact=true
        ROLLBACK-midway table-columns=[id author_id resumen asistentes encabezado created_at updated_at metadata]
        ROLLBACK-midway row-intact resumen="Charla"
        ROLLBACK-midway metadata-intact=true staging-left=false
        ROLLBACK-midway fields-unchanged=true
        ROLLBACK-midway connection usable=true
        ROLLBACK-metadata err=yes
        ROLLBACK-metadata fields=[resumen:text asistentes:integer encabezado:text] identities-intact=true
        ROLLBACK-metadata table-columns=[id author_id resumen asistentes encabezado created_at updated_at metadata]
        ROLLBACK-metadata row-intact resumen="Charla"
        ROLLBACK-metadata staging-left=false
        ROLLBACK-metadata connection usable=true
        probes run outside transactions=true
    dualengine_contract21_test.go:100: OK: 40 observations identical on SQLite and PostgreSQL 17
--- PASS: TestDualEngineStore (25.26s)
```

Lo que ahí está probado, señalado:

- **Instalación en limpio, dos arranques.** `boot#1`/`boot#2` — la lección de CONTRACT-11/13 es que
  el bug de metadata solo aparece en el SEGUNDO arranque, cuando la fila del primero se leyó de
  vuelta. Y `seedCatalogs` dos veces sin duplicar (`roles=4 permissions=8 taxonomies=2`).
- **ATOMICIDAD DE `CreateContentType`**: el fallo se induce como en el test SQLite-only — una tabla
  con el nombre destino existe físicamente pero es desconocida para la metadata, así que el diff
  dice "falta" y el `CREATE TABLE` falla **después** de que las filas de definición ya se
  insertaron. Resultado idéntico en los dos motores: `types=1 fields=4` (los de `eventos`, ninguno
  del squatted) y `metadata-has-squatted=false`.
- **ATOMICIDAD DE `EditContentType`, en las dos posiciones peligrosas.**
  `ROLLBACK-midway` fuerza el fallo en el `DROP TABLE` del paso 3 —después de crear la staging y
  copiar las filas— con una tabla que referencia la dinámica por FK. `ROLLBACK-metadata` lo fuerza
  en el paso 8, el último, después de las DOS copias y de la actualización del registro. En ambos:
  las columnas físicas intactas, la fila intacta, el registro intacto, las **identidades** de los
  campos intactas, la metadata intacta y **cero tablas de staging sobrevivientes**.
- **La conexión queda usable después del rollback** (`connection usable=true`): en PostgreSQL una
  transacción abortada que NO se hubiera revertido haría fallar toda sentencia posterior con 25P02.
  Es la comprobación que distingue "se revirtió" de "se rompió".
- **El cross-rename `a`↔`b`** sigue mapeando por identidad, en los dos motores.
- **El esquema canónico compuesto** sale igual: mismo orden de tablas dinámicas, mismo tamaño de
  dump (`bytes=80272`) y mismo digest (`55d1009d244f913c`).

---

## T3 — LA VERIFICACIÓN QUE CIERRA LA PROMESA

### La aplicación REAL sirviendo sobre PostgreSQL

Binario compilado (`go build ./cmd/librarian`), base **recién creada**, sin datos heredados:

```sql
DROP DATABASE IF EXISTS librarian_c21 WITH (FORCE);
CREATE DATABASE librarian_c21;
```

Primer arranque, **sin `pgvector`** — el primer obstáculo real de una instalación en limpio:

```
$ LIBRARIAN_ENGINE=postgres LIBRARIAN_DB='postgres://postgres:***@31.220.22.176:5443/librarian_c21?sslmode=disable' ./librarian
exit=1
librarian: the pgvector extension is required on PostgreSQL and its `vector` type is not resolvable
by this connection: librarian's canonical schema declares articles.embedding as vector(1536)
(CONTRACT-05), so the schema cannot be created without it. Run `CREATE EXTENSION IF NOT EXISTS
vector;` in the target database as a superuser, and if it is installed into a schema other than the
one this connection uses, make that schema visible on the connection's search_path
```

Se hace lo que el mensaje dice, y arranca:

```
$ CREATE EXTENSION IF NOT EXISTS vector;   -- OK
$ LIBRARIAN_ENGINE=postgres LIBRARIAN_JWT_SECRET=... LIBRARIAN_ADDR=127.0.0.1:8099 ./librarian
librarian: schema ready on postgres://postgres:***@31.220.22.176:5443/librarian_c21?sslmode=disable (postgres), listening on 127.0.0.1:8099
$ curl -s http://127.0.0.1:8099/health
{"status":"ok"}
```

La primera identidad se crea **fuera de banda** —una superficie que exige identidad no puede ser la
que crea la primera—, exactamente como manda `docs/DEPLOY.md` para SQLite: un usuario con rol
`administrator` y **solo** `users.manage` + `roles.manage`. Todo lo demás va por HTTP.

```
bootstrapped admin admin@example.com on postgres (id=0c21a11a-…-000000000001), granted [users.manage roles.manage] to administrator
```

### LA TRANSCRIPCIÓN HTTP CONTRA POSTGRESQL

Salida real, sin editar salvo el enmascarado del password (que no aparece: el binario ya lo
enmascara solo):

```
== 1. health ==
GET /health -> 200 {"status":"ok"}
== 2. session login (the bootstrapped admin) + JSON login ==
POST /login (form) -> 303  (303 = session issued)
GET /admin/users (session) -> 200
POST /auth/login (admin, JSON) -> token issued: yes
== 3. GRANT PERMISSIONS over HTTP (roles.manage) ==
POST /admin/roles/administrator/permissions -> 303
POST /admin/roles/editor/permissions -> 303
== 4. CREATE A USER over HTTP (users.manage) ==
POST /admin/users -> 303
users listed: admin@example.com redactora@example.com 
== 5. AUTHENTICATE the new user (JSON) ==
POST /auth/login -> token issued: yes
GET /whoami -> 200 {"auth":"jwt","email":"redactora@example.com","roles":["editor"],"user_id":"c832cdc0-3d3d-4686-9ce2-c5b41bcc17cf"}
POST /auth/login wrong password -> 401 {"error":"invalid credentials"}
POST /content-types as editor (lacks content_types.manage) -> 403 {"error":"forbidden"}
== 6. CREATE A DYNAMIC CONTENT TYPE over HTTP (content_types.manage) ==
POST /content-types -> 201 {"name":"resenas","fields":[{"name":"titular","type":"text"},{"name":"puntaje","type":"integer"},{"name":"precio","type":"decimal"},{"name":"verificada","type":"boolean"},{"name":"leida_el","type":"date"}]}
POST /content-types duplicate -> 400 {"error":"a content type with this name already exists"}
GET /content-types -> 200 {"content_types":[{"name":"resenas","fields":[{"name":"titular","type":"text"},{"name":"puntaje","type":"integer"},{"name":"precio","type":"decimal"},{"name":"verificada","type":"boolean"},{"name":"leida_el","type":"date"}]}]}
== 7. CONTENT CRUD on the type that did not exist a second ago ==
POST /content/resenas -> {"author_id":"c832cdc0-…","created_at":"2026-07-25T23:03:59.1067193Z","id":"e4f7bb5f-443f-4cd0-81bb-d1d5c99146a1","leida_el":"2024-03-01","metadata":null,"precio":"12.50","puntaje":9,"titular":"z-primera","updated_at":"2026-07-25T23:03:59.1067193Z","verificada":true}
POST /content/resenas (2nd) -> 201 {"author_id":"c832cdc0-…","created_at":"2026-07-25T23:03:59.736385Z","id":"aa11ec39-bdc9-4bd6-9a0a-53ae92f0207d","leida_el":null,"metadata":null,"precio":null,"puntaje":3,"titular":"a-segunda","updated_at":"2026-07-25T23:03:59.736385Z","verificada":false}
POST /content/resenas wrong field type -> 400 {"error":"field \"titular\" must be a JSON string (declared type text)"}
PUT /content/resenas/{id} -> 200 {"author_id":"c832cdc0-…","created_at":"2026-07-25T23:03:59.1067193Z","id":"e4f7bb5f-…","leida_el":"2030-01-01","metadata":null,"precio":"99.99","puntaje":10,"titular":"z-primera-editada","updated_at":"2026-07-25T23:04:00.3534883Z","verificada":false}
GET /content/resenas/{id} -> 200 {"author_id":"c832cdc0-…","created_at":"2026-07-25T23:03:59.1067193Z","id":"e4f7bb5f-…","leida_el":"2030-01-01","metadata":null,"precio":"99.99","puntaje":10,"titular":"z-primera-editada","updated_at":"2026-07-25T23:04:00.3534883Z","verificada":false}
GET /content/resenas (list) -> 200 {"items":[{"author_id":"c832cdc0-…","created_at":"2026-07-25T23:03:59.736385Z","id":"aa11ec39-…","leida_el":null,"metadata":null,"precio":null,"puntaje":3,"titular":"a-segunda","updated_at":"2026-07-25T23:03:59.736385Z","verificada":false},{"author_id":"c832cdc0-…","created_at":"2026-07-25T23:03:59.1067193Z","id":"e4f7bb5f-443f-4cd0-81bb-d1…
GET /content/resenas/{unknown} -> 404 {"error":"content not found"}
== 8. articles: the vector(1536) column, on the real engine ==
POST /articles (embedding of 1536 components) -> id issued: yes
GET /articles/{id} embedding components read back -> 1536
POST /articles/{id}/publish -> 200
GET /articles -> "title":"nota con embedding" 
DELETE /articles/{id} -> 204
DELETE /articles/{id} twice -> 404
== 9. EDIT the dynamic content type: a real table REBUILD, over HTTP ==
GET /content-types/resenas -> {"name":"resenas","fields":[{"id":"4a48683f-63cf-4edd-9309-18b70b04120a","name":"titular","type":"text"},{"id":"27d128ee-86dc-45f0-8676-7eb7577d30ea","name":"puntaje","type":"integer"},{"id":"4098e63a-c2ca-4216-ba93-8497215e72ce","name":"precio","type":"decimal"},{"id":"2782e8ad-15ba-4c8e-a31b-c154b80eb774","name":"verificada","type":"boolean"},{"id":"5965f741-4749-4778-8be4-7f15709770ef","name":"leida_el","type":"da…
PUT without confirming the removals (must REFUSE, nothing written) -> 400 {"confirm_remove":["precio","verificada","leida_el"],"error":"removing the field(s) precio, verificada, leida_el destroys their stored data irreversibly; confirm each one explicitly to proceed","nothing_was_done":true,"removes_data_of":["precio","verificada","leida_el"]}
PUT with the removals confirmed -> 200 {"added":["resumen"],"fields":[{"id":"4a48683f-…","name":"encabezado","type":"text"},{"id":"27d128ee-…","name":"puntaje","type":"integer"},{"id":"155da47e-…","name":"resumen","type":"text"}],"name":"resenas","removed":["precio","verificada","leida_el"],"renamed":[{"from":"titular","to":"encabezado"}]}
GET /content/resenas after the rebuild (data carried by identity) -> 200 {"items":[{"author_id":"c832cdc0-…","created_at":"2026-07-25T23:03:59.736385Z","encabezado":"a-segunda","id":"aa11ec39-…","metadata":null,"puntaje":3,"resumen":null,"updated_at":"2026-07-25T23:03:59.736385Z"},{"author_id":"c832cdc0-…","created_at":"2026-07-25T23:03:59.1067193Z","encabezado":"z-primera-editada","id":"e4f7bb5f-443f-4cd0-81bb-…
== 10. DELETE the content, and confirm it is gone ==
DELETE /content/resenas/{id} -> 204
DELETE again -> 404
GET /content/resenas final -> 200 {"items":[{"author_id":"c832cdc0-…","created_at":"2026-07-25T23:03:59.736385Z","encabezado":"a-segunda","id":"aa11ec39-…","metadata":null,"puntaje":3,"resumen":null,"updated_at":"2026-07-25T23:03:59.736385Z"}],"type":"resenas"}
```

Punto por punto, lo que el contrato exige:

| Exigencia | Dónde |
|---|---|
| crear un usuario | `POST /admin/users -> 303` → `users listed: admin@… redactora@…` |
| autenticarse | `POST /login (form) -> 303` (sesión) y `POST /auth/login -> token issued: yes` (JWT), + `GET /whoami -> 200` con el rol correcto |
| otorgar permisos | `POST /admin/roles/{administrator,editor}/permissions -> 303`, comprobado por el 403 que el editor recibe en `/content-types` y por el 201 que sí obtiene en `/content/resenas` |
| crear un tipo de contenido dinámico | `POST /content-types -> 201` — **crea una tabla real en PostgreSQL** |
| crear contenido | `POST /content/resenas` ×2, con los cinco tipos de campo |
| editar contenido | `PUT /content/resenas/{id} -> 200` |
| listar | `GET /content/resenas -> 200`, `GET /articles`, `GET /content-types` |
| borrar | `DELETE -> 204`, repetido `-> 404` |

Y de yapa, porque estaban ahí y son lo más caro de este esquema: el `vector(1536)` real de pgvector
(**1536 componentes escritas y leídas de vuelta**), el `publish`, y una **reconstrucción de tabla
completa** (`PUT /content-types/resenas`) que renombra un campo, borra tres y agrega uno, con los
datos siguiendo la IDENTIDAD del campo, más la negativa a destruir datos sin confirmación explícita.

### El mismo guion contra SQLite: nada cambió

El mismo script, mismo binario, `LIBRARIAN_ENGINE=sqlite`:

```
librarian: schema ready on sqlite-t3.db (sqlite), listening on 127.0.0.1:8098
```

y el diff de las dos transcripciones, con uuids e instantes normalizados:

```
$ diff <(norm sq-scenario.txt) <(norm pg-scenario.txt)
### The two HTTP transcripts, with uuids and timestamps normalised
IDENTICAL (52 lines): the SAME script produces the SAME observable results on SQLite and on PostgreSQL 17
```

Cero diferencias. Cada código de estado, cada mensaje de error, cada valor de campo, el orden de
cada listado y el tipo JSON de cada campo (`"puntaje":3` entero, `"verificada":false` booleano,
`"precio":"12.50"` decimal como texto) salen iguales de los dos motores.

### El ciclo de reinicio contra PostgreSQL — dos arranques, no uno

```
### RESTART CYCLE — the dynamic type was created by the previous process
-- boot #2 --
librarian: schema ready on postgres://postgres:***@31.220.22.176:5443/librarian_c21?sslmode=disable (postgres), listening on 127.0.0.1:8099
content types after restart #2: {"content_types":[{"name":"resenas","fields":[{"name":"encabezado","type":"text"},{"name":"puntaje","type":"integer"},{"name":"resumen","type":"text"}]}]}
-- boot #3 --
librarian: schema ready on postgres://postgres:***@31.220.22.176:5443/librarian_c21?sslmode=disable (postgres), listening on 127.0.0.1:8099
content rows survive restart: {"items":[{"author_id":"c832cdc0-…","created_at":"2026-07-25T23:03:59.736385Z","encabezado":"a-segunda","id":"aa11ec39-…","metadata":null,"puntaje":3,"resumen":null,"updated_at":"2026-07-25T23:03:59.736385Z"}],"type":"resenas"}
boot logs identical (no re-creation, no error): yes
```

Los dos arranques son limpios, con la forma **posterior a la reconstrucción** (`encabezado`,
`puntaje`, `resumen`), y no intentan recrear nada. Dos, no uno, porque el bug de metadata que casi
tira producción en CONTRACT-11 solo aparece en el segundo.

### `--dump-schema` contra ambos motores

```
$ LIBRARIAN_ENGINE=postgres LIBRARIAN_DB=… ./librarian --dump-schema pg-schema.json
exit=0  bytes=70324
  cpt_resenas ['id', 'author_id', 'encabezado', 'puntaje', 'resumen', 'created_at', 'updated_at', 'metadata']

$ LIBRARIAN_ENGINE=sqlite LIBRARIAN_DB=sqlite-t3.db ./librarian --dump-schema sq-schema.json
sqlite exit=0 bytes=70324
canonical JSON byte-identical across engines: YES
```

**Byte-idéntico.** Es la respuesta directa a la pregunta de red-team sobre si un tipo dinámico creado
en un motor y en el otro produce el mismo esquema canónico: sí, y es lo que hace comparables los dos
dumps y fiel el export. Sin el orden impuesto en Go (§ Decisión 2) no lo sería.

---

## Red-team: las preguntas del contrato, respondidas con evidencia

| Pregunta | Respuesta | Evidencia |
|---|---|---|
| **¿El DDL transaccional revierte igual en los dos motores cuando falla a mitad?** | Sí, **y no por suerte**. PostgreSQL envenena la transacción entera (25P02) y SQLite no; es invisible porque las dos funciones retornan en el primer error y lo siguiente es el `defer tx.Rollback()`. Está escrito en el código para que una edición futura no lo rompa en silencio. Medido en las dos posiciones peligrosas: el `DROP TABLE` del medio y la escritura de metadata del final. | `ROLLBACK-midway` y `ROLLBACK-metadata` en la batería: columnas, filas, registro, identidades y metadata intactos, cero staging, en los dos motores. Más `connection usable=true`, que distingue "revirtió" de "quedó rota" |
| **¿`TableExists` responde bien para una tabla dinámica recién creada dentro de la misma transacción?** | La pregunta no llega a plantearse, y eso es un diseño, no una omisión: **toda sonda de este paquete corre sobre el POOL, fuera de cualquier transacción abierta**. Sobre SQLite tiene que ser así (compat fija el pool a una conexión; sondear con una transacción abierta deadlockearía) y sobre PostgreSQL es el mismo código. Lo que sí se mide es la respuesta inmediatamente DESPUÉS del commit. | `after create table-exists=true` y `probes run outside transactions=true` |
| **¿Qué pasa si el DSN de PostgreSQL es válido pero la base no existe?** | Falla ruidosamente al conectar, con el DSN enmascarado. No hay ningún camino que cree una base ni que caiga a SQLite. | El arranque contra `librarian_c21` antes de crearla, y el caso B de la matriz de configuración |
| **¿Y si `pgvector` está pero en otro esquema?** | Es exactamente por eso que la comprobación es `to_regtype('vector')` y no `pg_extension`: lo que importa no es que esté instalada sino que **esta conexión pueda nombrar el tipo**. El mensaje de error nombra el `search_path` explícitamente. | § "El fallo legible sin pgvector"; el mensaje real del bloque A |
| **¿El `search_path` afecta a `TableExists`?** | Sí, y es lo correcto: en PostgreSQL restringe a `current_schema()`, que es donde aterriza el `CREATE TABLE` sin calificar de este mismo código. Preguntar por otro esquema respondería otra pregunta. La batería lo aprovecha: corre en un esquema propio por corrida y **verifica `current_schema()`** antes de crear nada. | `openPostgresForStore`, que aborta si `current_schema() != librarian_c21_<nanos>` |
| **¿Un tipo dinámico creado en SQLite y el mismo creado en PostgreSQL producen el mismo esquema canónico?** | **Sí, byte a byte** — pero solo porque el orden se impone en Go. El `ORDER BY` de SQL **diverge de verdad** con los nombres del fixture. | `canonical JSON byte-identical across engines: YES`; y la divergencia cruda medida: sqlite `[blog_notas bloganuncios]` vs postgres `[bloganuncios blog_notas]` |
| **Extra: ¿el nombre duplicado sigue dando 400 y no 500?** | Ahora sí en los dos. Antes, en PostgreSQL habría sido 500 (§ Decisión 5). | `POST /content-types duplicate -> 400` en las DOS transcripciones HTTP, y `sentinel=true` en la batería |
| **Extra: ¿una VISTA se confunde con una tabla en la sonda?** | No, en ninguno de los dos. | `tableExists view(user_role_names)=false` |

---

## Lo que NO se hizo, y por qué

1. **No hay ninguna ruta de migración de datos existentes**, ni compatibilidad con filas escritas por
   versiones anteriores. Es la premisa explícita del contrato ("instalación EN LIMPIO"). Mover los
   datos de una instancia SQLite en producción a PostgreSQL sigue siendo el runbook de
   `docs/OPERATIONS.md` (`compat copy`), que es un trabajo distinto. Ese documento se corrigió: su
   primera línea afirmaba que "librarian no CORRE sobre PostgreSQL", y eso ya no es verdad.
2. **No se agregó una ruta HTTP para crear la primera identidad.** Sigue siendo un paso fuera de
   banda, como en SQLite y como documenta `docs/DEPLOY.md`. Una superficie que exige identidad no
   puede ser la que crea la primera, y abrir una excepción sería un agujero permanente.
3. **No se tocó el límite de precisión de `pgvector`** (float4), medido y documentado en
   CONTRACT-20. Sigue siendo una propiedad de la columna `vector(1536)` que declaró CONTRACT-05.
4. **`docs/DEPLOY.md` no se reescribió.** Sigue siendo correcto: describe el despliegue SQLite que
   corre hoy. Un despliegue sobre PostgreSQL necesitaría su propio runbook (unidad systemd con
   `LIBRARIAN_ENGINE`, backup por `pg_dump` en vez de copia de archivo, etc.), que es trabajo
   operativo, no de este contrato. `docs/OPERATIONS.md` ya tiene lo que hace falta para arrancar.

---

## ¿Le falta algo a `compat`? — No

Evaluado durante la implementación, con los mismos criterios que los dos contratos anteriores:

1. **`TableExists` fue exactamente lo que hacía falta** para las dos sondas, incluida la propiedad
   que las hacía imposibles de escribir en el consumidor (`sqlite_master` no existe en PostgreSQL y
   `pg_class`+`current_schema()` no existe en SQLite) y la que las hacía peligrosas (no consultar
   `__compat_schema`). Cero fricción.
2. **`IsUniqueViolation` resolvió el hallazgo** sin ninguna extensión. Su limitación documentada (no
   dice cuál restricción) no molesta acá por el argumento del UUID generado en Go.
3. **`OpenStore(target, dsn)` es lo que hace inexpresable el desparejo target/conexión.** Es
   precisamente la primitiva que este contrato necesitaba y ya estaba.
4. **`CompileDropView` sigue sin existir**, y sigue sin importar: `DROP VIEW IF EXISTS "x"` es
   byte-idéntico en ambos motores y sin parámetros (CONTRACT-19 ya lo argumentó). Verificado ahora
   contra PostgreSQL real: `applyViews` funciona sin cambios.
5. **El `ORDER BY` de una rutina sigue sin aceptar colación.** Sigue sin ser un hueco por la razón
   que CONTRACT-20 dio: la colación es una propiedad del despliegue de PostgreSQL, no de la
   librería. Este contrato lo resolvió como los anteriores, ordenando en Go.

**`sqlite-postgres-compat` no se modificó.** `git status` en ese repo quedó exactamente como estaba
al empezar.

---

## Cómo reproducir T3 (el orquestador va a correrlo)

```powershell
# 0. Binario
go build -o librarian.exe ./cmd/librarian

# 1. Base LIMPIA en PostgreSQL 17
#    DROP DATABASE IF EXISTS librarian_c21 WITH (FORCE); CREATE DATABASE librarian_c21;

# 2. Arrancar SIN pgvector -> tiene que fallar con el mensaje legible (exit 1)
$env:LIBRARIAN_ENGINE = "postgres"
$env:LIBRARIAN_DB     = "postgres://postgres:***@31.220.22.176:5443/librarian_c21?sslmode=disable"
$env:LIBRARIAN_JWT_SECRET = "t3-secret"
$env:LIBRARIAN_ADDR   = "127.0.0.1:8099"
.\librarian.exe

# 3. CREATE EXTENSION IF NOT EXISTS vector;  en librarian_c21, y arrancar de nuevo
# 4. Bootstrap fuera de banda del primer admin (users.manage + roles.manage), como docs/DEPLOY.md
# 5. El guion HTTP de arriba, y el MISMO contra LIBRARIAN_ENGINE=sqlite
```

Las tres baterías:

```powershell
$env:COMPAT_POSTGRES_DSN = "postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable"
go test -tags dualengine -run TestDualEngineAuth   -count=1 -v ./internal/auth
go test -tags dualengine -run TestDualEngineServer -count=1 -v ./internal/server
go test -tags dualengine -run TestDualEngineStore  -count=1 -v ./internal/store
```

---

## Archivos tocados

**Nuevos**

- `internal/store/dualengine_contract21_test.go` — la batería dual-motor de `internal/store` (T3)
- `docs/reports/CONTRACT-21-REPORT.md` — este informe

**Modificados (producción)**

- `internal/config/config.go` — `ResolveEngine`, `LooksLikePostgresDSN`, `Config.{Engine,DSN}`
- `internal/store/store.go` — `Open(engine, dsn)`, `TargetFor`, `requireVectorType`, T1
- `internal/store/contenttypes.go` — sonda #1, T1, `IsUniqueViolation`, orden en Go; **`FromDB` borrado**
- `internal/store/contenttypes_edit.go` — sonda #2, T1
- `internal/server/server.go` — `Deps.Store *compat.Store`
- `internal/server/contenttypes.go`, `ui_contenttypes.go`, `ui_content.go`, `ui_nav.go`, `content.go` — firmas
- `cmd/librarian/main.go` — arranque y `--dump-schema` por motor; `redactDSN`
- `docs/OPERATIONS.md` — corrección de la afirmación obsoleta + § "Elegir el motor" + la batería nueva

**Modificados (tests — andamiaje, cero aserciones tocadas)**

`git diff --stat -- '*_test.go'` da 15 archivos, y la totalidad del cambio en los preexistentes es
de firma (`store.Open(engine, dsn)`, `store.SeedCatalogs(ctx, store)`, `server.Deps{Store: …}`,
`store.FromDB(db)` → el helper `storeFor(db)` que ya existía). Las únicas líneas nuevas con
contenido son `TestResolveEngine` (+118 en `internal/config/config_test.go`) y el cambio de los
fixtures PostgreSQL de las baterías 19/20 al camino de producción (−46/−37 netos: borran código, no
lo agregan).

**NO tocados**

- `sqlite-postgres-compat` (todo el repo)
- `internal/schema` (ni una línea)
- `internal/auth`, `internal/dual` (ni una línea de producción)
- El contrato público de las rutas HTTP

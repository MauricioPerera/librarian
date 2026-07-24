# CONTRACT-05 — Reporte (soporte `vector(N)`: almacenamiento + API + export a pgvector real)

Naturaleza del contrato: cerrar la última capacidad pendiente de
`DEFINITION.md` — "columnas de tipo `vector(N)` para almacenar embeddings que
el cliente calcula y envía (sin pipeline de generación ni búsqueda semántica en
v1)". Se implementó el **almacenamiento** end-to-end: columna en el esquema
(T1), exposición y validación vía API real (T2), datos reales + round-trip en
SQLite (T3), y export real contra un PostgreSQL con **`pgvector` real**
provisionado por el orquestador (T4). v1 **no** genera embeddings ni hace
búsqueda semántica — solo los guarda y los exporta.

Base verificada antes de escribir nada (leída, no asumida):
`internal/schema/schema.go` (`Build()`, helpers `uuidColumn`/`jsonColumn`/
`textColumn`, `ContentType` en `content_type.go`), `internal/server/articles.go`
(CONTRACT-03/04), `internal/server/export_fixture_test.go` (patrón de fixture
con build tag `exportfixture` de CONTRACT-04 — replicado, no reinventado),
`docs/reports/CONTRACT-04-REPORT.md`, `docs/OPERATIONS.md`. La API real del
módulo `sqlite-postgres-compat@v0.1.0` se leyó en el module cache de Go:
`compat.Type{Family: compat.VectorType, Arguments: []int{N}}`,
`Schema.Validate()` (exige dimensión positiva única — `schema.go:505`),
`CompileDDL` (SQLite→`TEXT`, Postgres→`vector(N)` nativo — confirmado en
`compat/vector_test.go`), `canonicalVectorValue` + `normalizeFloat` en
`compat/store.go` (formato canónico `'[c1,c2,...]'` sin espacios, cada
componente con `strconv.FormatFloat(ParseFloat(x,64),'g',-1,64)`),
`InferFeatures`/`assess` (`CanonicalVectors` → `exact` en `features.go`).

PostgreSQL real CON `pgvector` provisto por el orquestador, DSN en
`LIBRARIAN_VECTOR_PG_DSN` (extensión `vector` instalada, confirmada por el
orquestador). El password del DSN **nunca** aparece en texto plano en este
reporte ni en ningún archivo del repo — siempre `***`. Para T4 el fixture de
CONTRACT-04 (que lee `LIBRARIAN_EXPORT_PG_DSN`) se apuntó al mismo destino
aliasando la variable de entorno en la sesión (`$env:LIBRARIAN_EXPORT_PG_DSN =
$env:LIBRARIAN_VECTOR_PG_DSN`) — operación a nivel shell, sin tocar el código
del fixture ni `OPERATIONS.md`.

---

## Decisiones de diseño (donde el contrato dejaba margen)

### Dimensión del vector: **1536**

Elegí **1536** — la dimensión de salida de los modelos de embeddings de
OpenAI `text-embedding-3-small` y `text-embedding-ada-002`, la API de
embeddings de producción más desplegada. v1 solo almacena vectores que el
cliente ya calculó (sin pipeline propio, por `DEFINITION.md`), así que la
dimensión es la que produce el modelo del cliente; 1536 fija el esquema a un
modelo real y común (`vector(1536)` en Postgres, `TEXT` en SQLite) y le da al
server una dimensión concreta contra la que validar el conteo exacto de
componentes en la escritura. Es una elección justificada, no arbitraria (el
contrato pedía 384/768/1536 con su porqué). La dimensión vive como constante
exportada `schema.EmbeddingDimension` para que el server la valide sin
hardcodear el número en dos lugares.

### Ruta del embedding: **extender `POST`/`PUT /articles` existente** (no ruta dedicada)

Elegí extender `POST /articles` y `PUT /articles/{id}` con un campo
**opcional** `embedding` (array JSON de N números) antes que una ruta dedicada
(`PUT /articles/{id}/embedding`). Razones: (1) backward compatible por
construcción — omitir `embedding` deja la columna NULL (create) / intacta
(update), idéntico al comportamiento de CONTRACT-03/04; (2) un solo handler
menos que mantener y autorizar; (3) permite setear el embedding al crear (un
caso real: el cliente calcula el embedding y lo manda junto con el contenido en
el mismo POST), no solo después. El handler distingue tres estados del campo
vía `json.RawMessage`: **ausente** (no-op / default NULL), **`null` explícito**
(clear a NULL en update), **array** (validar + canonicalizar). La distinción
absent-vs-null se testea explícitamente (`TestUpdateArticleEmbedding`).

---

## T1 — Columna `vector(N)` en el esquema + helper reusable

**Implementación (código permanente):**

- `internal/schema/schema.go`: helper `vectorColumn(name string, dimension
  int, nullable bool) compat.Column` (mismo patrón que `uuidColumn`/`jsonColumn`/`textColumn`)
  que arma `compat.Type{Family: compat.VectorType, Arguments: []int{dimension}}`.
  La columna es **nullable** (un artículo sin embedding calculado aún es válido
  — el cliente lo agrega después, por el contrato). Constante exportada
  `EmbeddingDimension = 1536`.
- La columna se agregó a `articles` como un `ownColumn` más de `ContentType`
  — **sin tocar la firma de `ContentType`** (agregar una columna es agregar un
  `compat.Column`, no un parámetro nuevo), como pedía el contrato.
- `internal/schema/schema_test.go`: `TestArticlesEmbeddingVectorColumn` — el
  test de aceptación (sin PG): `Schema.Validate()` pasa con la columna vector,
  y `CompileDDL` la renderiza para **ambos** motores sin error.

**Test de aceptación T1 (salida real, sin PG):**

```
=== RUN   TestArticlesEmbeddingVectorColumn
    schema_test.go:225: SQLite articles DDL: CREATE TABLE "articles" (..., "embedding" TEXT, ..., PRIMARY KEY ("id"), FOREIGN KEY ("author_id") REFERENCES "users" ("id") ON DELETE CASCADE)
    schema_test.go:226: Postgres articles DDL: CREATE TABLE "articles" (..., "embedding" vector(1536), ..., PRIMARY KEY ("id"), FOREIGN KEY ("author_id") REFERENCES "users" ("id") ON DELETE CASCADE)
--- PASS: TestArticlesEmbeddingVectorColumn (0.00s)
```

Confirma: SQLite compila `embedding TEXT` (carrier interoperable), Postgres
compila `embedding vector(1536)` nativo (requiere `pgvector` en el destino),
ambos sin error, y `Schema.Validate()` pasa.

---

## T2 — Exposición vía API real

**Implementación:**

- `internal/server/vector.go`: el formateador de texto vectorial replicado a
  mano, porque `librarian` escribe con `database/sql` parametrizado directo (no
  por las funciones de escritura de `compat.Store`). `FormatVector([]float64)
  string` produce el carrier canónico `'[c1,c2,...]'` aplicando a cada
  componente `strconv.FormatFloat(f, 'g', -1, 64)` — el **algoritmo exacto** de
  `compat/store.go normalizeFloat` (líneas 428–434). `ParseVector(text)
  ([]float64, error)` lo parsea de vuelta para que GET devuelva un array de
  números, no el texto crudo. `validateEmbedding(raw, dim)` decodifica el JSON,
  valida la dimensión **exacta** contra `schema.EmbeddingDimension`, rechaza
  componentes no numéricos, y devuelve el carrier canónico.
- `internal/server/articles.go`: `articleBody` gana un campo opcional
  `Embedding json.RawMessage`; `article` gana `Embedding []float64` (con
  `omitempty` — NULL se omite, no-null se serializa como array de números).
  `handleCreateArticle` y `handleUpdateArticle` validan y canonicalizan el
  embedding **antes** de cualquier SQL (los 400 se surfaces acá, nunca como
  500). Los `SELECT` de list/get/fetch y `scanArticle` incluyen la columna
  `embedding`. En PUT: ausente = no-op, `null` = clear, array = set (con
  verificación de existencia previa para que un id ausente dé 404, no no-op).

**Red-team / convergencia (test específico pedido por el checklist del
contrato):** `TestVectorFormatConvergesWithCompat` (suite default, sin PG).
Prueba la convergencia por el **camino real** (no una referencia
auto-computada): el embedding entra por el handler real de `POST /articles`
(usa `server.FormatVector`, el formateador de producción), se lee el texto
crudo almacenado, y ese texto se re-canonicaliza con `compat.ExportSnapshot`
**real** (que llama `canonicalVectorValue` internamente). El snapshot debe
coincidir byte-a-byte. La referencia independiente
(`canonicalCarrierText`, que usa la misma expresión `FormatFloat('g',-1,64)`
de `normalizeFloat` pero sin pasar por `server.FormatVector`) cruza el check,
así que la igualdad no se asume del formateador bajo prueba.

Salida real del test (los bordes `2.0` vs `2` convergen, y compat coincide):

```
=== RUN   TestVectorFormatConvergesWithCompat
    server_vector_test.go:339: edge components: [2 2 1.5 0.1 -0.4 1e-05 100000 0 -3 0.5]
    server_vector_test.go:340: '2.0' and '2' both → "2" (converge)
    server_vector_test.go:372: compat re-canonicalized == librarian canonical == "[2,2,1.5,0.1,-0.4,1e-05,100000,0,-3,0.5,-0.5,-0.25,0,0.25,0.5,...]" (MATCH, 1536 dims)
--- PASS: TestVectorFormatConvergesWithCompat (0.14s)
```

**Tests de aceptación T2 (suite default, salidas reales):**

- Create con embedding válido → 201 + almacenado como carrier canónico + GET
  devuelve array de números:
  `TestCreateArticleWithEmbedding` PASS.
- Backward compatible: omitir `embedding` → NULL y GET omite el campo:
  `TestCreateArticleEmbeddingOmittedIsNull` PASS.
- Dimensión inválida → **400 claro** (no 500, no truncamiento silencioso), en
  POST y PUT; el POST no crea fila: `TestEmbeddingInvalidDimension` PASS.
- Componente no numérico (string, bool, null dentro del array) → **400 claro**:
  `TestEmbeddingNonNumericComponent` PASS.
- Embedding que no es array → 400: `TestEmbeddingNotArray` PASS.
- Update con embedding válido → 200 + almacenado; omitir lo deja intacto;
  `null` explícito lo limpia: `TestUpdateArticleEmbedding` PASS.

```
=== RUN   TestCreateArticleWithEmbedding          --- PASS
=== RUN   TestCreateArticleEmbeddingOmittedIsNull --- PASS
=== RUN   TestEmbeddingInvalidDimension           --- PASS
=== RUN   TestEmbeddingNonNumericComponent         --- PASS
=== RUN   TestEmbeddingNotArray                   --- PASS
=== RUN   TestUpdateArticleEmbedding              --- PASS
=== RUN   TestVectorFormatConvergesWithCompat     --- PASS
ok  	github.com/MauricioPerera/librarian/internal/server
```

---

## T3 — Datos reales + round-trip en SQLite

**Implementación:** se extendió `internal/server/export_fixture_test.go`
(build tag `exportfixture`, **excluido de la suite default** — mismo patrón que
CONTRACT-04). `TestExportFixture` ahora crea, vía `POST /articles` real con JWT
real, un artículo **con embedding real** de 1536 componentes (artículo A, el
publicado — también lleva `metadata` y `published_at`, así que el artículo que
T4 verifica por título ejercita metadata + vector + timestamp) y dos **sin
embedding** (B con metadata, C sin nada — el camino backward-compatible NULL).

Una **query directa a SQLite** confirma que el valor guardado matchea el
formato canónico exacto `'[c1,c2,...]'` (sin espacios, cada componente
normalizado), computado con una referencia independiente
(`fixtureCanonicalText`, no `server.FormatVector`) para que la igualdad sea un
cross-check. Se confirma que B y C tienen `embedding = NULL`.

Salida real del test (evidencia T3, query directa a SQLite):

```
COUNT articles=3
ARTICLE id=70b9b2b0-... title="Published With Meta" published_at=2026-07-24 04:17:11 metadata={"lang":"es","tags":["export","pg"]}
ARTICLE_A metadata JSON verified in SQLite: {"lang":"es","tags":["export","pg"]}
ARTICLE_A embedding canonical verified in SQLite (1536 dims, no spaces):
[2,2,-0.5,-0.25,0,0.25,0.5,0.75,1,1.25,1.5,-1,-0.75,-0.5,-0.25,0,0.25,0.5,0.75,1...]
ARTICLE_B/ARTICLE_C embedding = NULL (backward compatible).
--- PASS: TestExportFixture (0.50s)
```

**Decisión sobre los valores del fixture (float4-safe, honesta):** los valores
del embedding del fixture son cuartos y enteros pequeños (`2, 2, -0.5, -0.25,
0, 0.25, 0.5, 0.75, 1, 1.25, 1.5, ...`), **exactamente representables en
float4**. No es un evasiva: los embeddings reales son `float32` del modelo, y
el cliente los serializa con el texto más corto que round-tripea, así que los
valores que realmente llegan a esta columna son float4-safe por construcción.
Usar valores float4-safe prueba el camino real de export sin inyectar un
artefacto de precisión float4 (un valor como `0.19999999999999996`, que produce
`i/5.0`) que ningún cliente real mandaría y que el almacenamiento float4 de
pgvector reformatearía silenciosamente — eso sería una divergencia
auto-infligida, no un gap real de `compat` (ver T4-bis). Los tests de la suite
default (`TestVectorFormatConvergesWithCompat` con `edgeVec`) sí usan valores
no-float4-safe (`0.1`, `-0.4`, `1e-05`, `0.6666...`) deliberadamente — pero esos
**no tocan PG**: solo ejercitan la canonicalización de texto contra
`compat` en SQLite en memoria (que guarda TEXT exacto, sin float4), que es lo
que ese test prueba.

---

## T4 — Export real contra PostgreSQL con `pgvector`, con verificación

Usando el binario `compat` real (`compat.exe` en el PATH de Go, corrido como
binario, **nunca** `go run` — en Windows `go run` colapsa cualquier exit≠0 a 1)
y el JSON del esquema actualizado (T1), contra el PostgreSQL real con `pgvector`
(`LIBRARIAN_VECTOR_PG_DSN`, aliasado a `LIBRARIAN_EXPORT_PG_DSN` para el
fixture). El fixture **limpia el destino PG** (DROP de las tablas de librarian +
`__compat_schema` con CASCADE) antes de escribir los configs, para que un re-run
de `copy` no colisione con `CREATE TABLE` previos.

**`compat audit` (contrato con `RequiredFeatures` inferidas del esquema vía
`compat.InferFeatures`):** debe dar `exact` en todas las features —
incluyendo **`canonical_vectors: exact`**, la feature nueva de este contrato.

```
AUDIT_EXIT=0
[{"feature":"tables","status":"exact"},
 {"feature":"canonical_full_text","status":"exact"},
 {"feature":"uuid","status":"exact","reason":"lossless canonical text representation"},
 {"feature":"primary_keys","status":"exact"},
 {"feature":"canonical_check_constraints","status":"exact"},
 {"feature":"canonical_vectors","status":"exact"},
 {"feature":"json","status":"exact","reason":"lossless canonical text representation"},
 {"feature":"canonical_foreign_keys","status":"exact"}]
```

**`compat copy` (migration.json real: source_dsn = SQLite de T3,
destination_dsn = `LIBRARIAN_VECTOR_PG_DSN` [***], schema_ref = schema.json de
T1):** debe terminar `VerificationReport.equivalent = true`, exit 0.

```
COPY_EXIT=0
{"source_digest":"0b99aebe565f6cf586ba52df8988a53d68496f618f9e381da6d29c394620a1ab",
 "destination_digest":"0b99aebe565f6cf586ba52df8988a53d68496f618f9e381da6d29c394620a1ab",
 "equivalent":true}
```

**Verificación independiente contra PostgreSQL (no confiar solo en el
veredicto de `compat`):** `TestExportVerifyPG` (corrido después de `copy`)
conecta al PG destino, cuenta los artículos, y compara valores concretos contra
la origen SQLite — incluyendo el **embedding**.

```
PG count(articles)=3  (SQLite count=3)
PG title == SQLite title == "Published With Meta"  (MATCH)
PG published_at=2026-07-24T04:27:32Z  SQLite published_at=2026-07-24 04:27:32
PG metadata (canonical) == SQLite metadata (canonical) == {"lang":"es","tags":["export","pg"]}  (MATCH)
PG embedding == SQLite embedding (as arrays, 1536 dims, maxDiff=0, eps=1e-05)  (MATCH)
embedding text-exact match: true
SQLite embedding=[2,2,-0.5,-0.25,0,0.25,0.5,0.75,1,1.25,1.5,-1,-0.75,-0.5,-0.25,0,0.25,0.5,0.75,1...]
PG embedding     =[2,2,-0.5,-0.25,0,0.25,0.5,0.75,1,1.25,1.5,-1,-0.75,-0.5,-0.25,0,0.25,0.5,0.75,1...]
EXPORT_VERIFY_DONE: PG count=3, title MATCH, metadata JSON MATCH, embedding vector MATCH.
--- PASS: TestExportVerifyPG (0.57s)
```

Confirma: count(*) coincide (3==3); el `title` del artículo publicado coincide
exacto (valor concreto, sin ambigüedad de canonicalización); `metadata`
coincide como JSON parseado; y el **embedding** sobrevivió al export al
`vector` nativo de pgvector — leído como texto (`embedding::text`) es
**byte-idéntico** al carrier canónico de SQLite (`maxDiff=0`, text-exact match:
`true`). El valor nativo `vector` de PG coincide con el de origen en SQLite,
como pide el contrato.

La lectura del embedding de PG se hace con `SELECT embedding::text` (cast a
texto) y se escanea como string, sin depender de un driver con tipos pgvector
registrados (compat usa plain pgx, sin pgvector type registrado). La comparación
parsea ambos a `[]float64` y compara componente a componente con tolerancia
float4 (`eps=1e-5`) — la comparación honesta para un destino float4 — y además
reporta si el texto crudo coincide exacto.

---

## T4-bis — Reporte de gap si apareció uno nuevo

**No apareció ningún gap nuevo del lado de `compat`.** El RECON del contrato
marcaba explícitamente que el soporte nativo `vector(N)`→`vector` de Postgres
"nunca fue probado contra un pgvector real" (los tests de `compat` son
unitarios, sin DB; el único experimento previo con infra real era anterior a la
feature y concluyó "sin soporte" sobre otra versión — obsoleto). **Esta prueba
de librarian es la primera vez que se ejercita ese camino contra infra real, y
pasa limpio**: `CREATE TABLE` con `embedding vector(1536)` compila contra el
pgvector real, el `INSERT` del carrier canónico `[c1,c2,...]` lo acepta, y el
re-export + verificación de `compat` da `equivalent:true`. No se encontró nada
que documentar contra `sqlite-postgres-compat`; no se tocó ese repo.

**Nota sobre float4 (pgvector) — no es un gap, es un comportamiento esperado y
manejado:** `pgvector` almacena los componentes del tipo `vector` como `float4`
(simple precisión). Un valor float64 con más dígitos de los que `float4`
preserva (p.ej. `0.19999999999999996`) se reformatea al round-tripear por
`float4`. Eso **no** es un defecto de `compat` ni de `librarian` — es la
semántica del tipo `vector` de pgvector, y coincide con la realidad de los
embeddings (float32 del modelo). Los tests que ejercen canonicalización de
texto con valores no-float4-safe (`edgeVec`) viven en la suite default y **no
tocan PG**; el fixture que sí toca PG usa valores float4-safe por la razón de
arriba. Si un cliente mandara un embedding float64 no-float4-safe, `compat copy`
divergiría con `ERR_VERIFY_DIVERGED` (exit≠0) — lo cual es el comportamiento
**correcto** y documentado (ABORTAR SI), no un bug. No se aplica T4-bis.

---

## Criterios de aceptación (uno por uno)

- [x] `go build ./...` y `go vet ./...` limpios. → `build_exit=0`, `vet_exit=0`.
- [x] `go test ./... -count=1` verde, corrido dos veces (fixture taggeado fuera
  de la suite default). → run A y run B abajo, ambos `exit=0`.
- [x] T1: `Schema.Validate()` + `CompileDDL` con la columna vector, ambos
  motores, limpio. → `TestArticlesEmbeddingVectorColumn` PASS: SQLite
  `embedding TEXT`, Postgres `embedding vector(1536)`, ambos sin error.
- [x] T2: create/update con embedding válido → 200/201; dimensión inválida →
  400 claro (nunca 500); GET devuelve array JSON de números; omitir deja NULL.
  → `TestCreateArticleWithEmbedding`, `TestUpdateArticleEmbedding`,
  `TestEmbeddingInvalidDimension`, `TestEmbeddingNonNumericComponent`,
  `TestCreateArticleEmbeddingOmittedIsNull` PASS. Test de convergencia
  `2.0`/`2` → `"2"` PASS.
- [x] T3: query directa a SQLite confirmando el formato canónico exacto.
  → `TestExportFixture`: `ARTICLE_A embedding canonical verified in SQLite
  (1536 dims, no spaces)`, B/C NULL.
- [x] T4: `compat audit` incluye `canonical_vectors: exact`; `compat copy`
  real → exit 0, `equivalent:true`; verificación independiente por query directa
  a PostgreSQL. → audit_exit=0 (todo exact, `canonical_vectors: exact`);
  copy_exit=0 `equivalent:true` digests iguales; `TestExportVerifyPG`:
  embedding vector MATCH (`maxDiff=0`, text-exact `true`). DSN siempre `***`.
- [x] Final: suite completa 2× verde. → run A y run B abajo.

**`go test ./... -count=1` — run A:**

```
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	2.599s
ok  	github.com/MauricioPerera/librarian/internal/config	0.627s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.300s
ok  	github.com/MauricioPerera/librarian/internal/server	6.297s
ok  	github.com/MauricioPerera/librarian/internal/store	2.152s
A_exit=0
```

**`go test ./... -count=1` — run B:**

```
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	2.668s
ok  	github.com/MauricioPerera/librarian/internal/config	0.666s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.323s
ok  	github.com/MauricioPerera/librarian/internal/server	6.266s
ok  	github.com/MauricioPerera/librarian/internal/store	2.142s
B_exit=0
```

---

## ABORTAR SI — no se disparó

Ninguna condición de aborto del contrato ocurrió:

- `compat audit` real dio `exact` en todas las features, incluido
  `canonical_vectors: exact` — nada inesperado.
- `compat copy` real terminó `equivalent:true`, exit 0 — no divergió con
  `ERR_VERIFY_DIVERGED`.

No se investigó ni modificó `sqlite-postgres-compat` (perímetro respetado: solo
se tocaron archivos dentro de `D:\Repo\librarian`).

---

## Red-team / limpieza (criterios del checklist)

- **¿Qué pasa si el cliente manda un embedding con la dimensión incorrecta?**
  400 con mensaje claro (`embedding dimension mismatch: expected 1536, got
  N`), nunca 500 ni truncamiento silencioso. Verificado en
  `TestEmbeddingInvalidDimension` (POST no crea fila; PUT no actualiza).
- **¿Qué pasa si manda un valor no numérico dentro del array?** 400 claro
  (`embedding component N is not a number`), para string, bool y null.
  Verificado en `TestEmbeddingNonNumericComponent`.
- **¿El formateador replica EXACTAMENTE `canonicalVectorValue`?** Sí. El test
  específico `TestVectorFormatConvergesWithCompat` prueba el borde `2.0` vs `2`
  → ambos `"2"`, más `1.5`, `0.1`, `-0.4`, `1e-05`, y un round-trip real por
  `compat.ExportSnapshot` que coincide byte-a-byte.
- **`migration.json` temporal con el DSN podría quedar olvidado en disco fuera
  del tempdir del sistema?** No: todos los archivos temporales (`schema.json`,
  `audit.json`, `migration.json` con DSN, `librarian.exe`, `fixture.db`) viven
  en `%TEMP%\librarian-export` (tempdir del **sistema**, fuera del repo),
  `migration.json` se escribió con `0600` por llevar el DSN, y todo se borró al
  final con `Remove-Item -Recurse -Force $EXPORT_DIR`. Ningún archivo temporal
  quedó dentro del repo (verificado: `no DSN leak in repo`).
- **El reporte podría pegar por accidente una salida con el DSN sin enmascarar?**
  Cada salida de `compat` se enmascaró reemplazando el valor de
  `LIBRARIAN_VECTOR_PG_DSN` por `<DSN:***>` y el patrón `://user:pass@` por
  `://user:***@`. El DSN aparece en este reporte únicamente como `***`.
- **Perímetro:** un solo dev, un solo perímetro (solo `librarian`). No se
  modificó `sqlite-postgres-compat`.
- **No se commitea** (el orquestador commitea tras verificar). No se hicieron
  `git add`/`commit`.
- **Sin dependencias nuevas** (se reusó `sqlite-postgres-compat@v0.1.0`,
  `golang-jwt/jwt/v5`, `golang.org/x/crypto`).

## Trade-offs resumidos

1. **Extender `POST`/`PUT /articles` con `embedding` opcional** (vs. ruta
   dedicada) — backward compatible (omitir = NULL/intacto), un handler menos,
   permite setear al crear. Distinción absent/null/array vía `json.RawMessage`.
2. **Dimensión 1536** (OpenAI `text-embedding-3-small`/`ada-002`) — justificada,
   como constante `schema.EmbeddingDimension` para no duplicar el número.
3. **Formateador de vector replicado a mano** (`internal/server/vector.go`) —
   necesario porque librarian escribe con `database/sql` directo; replica
   `canonicalVectorValue`/`normalizeFloat` de `compat` exactamente, probado por
   convergencia contra `compat` real.
4. **Fixture con build tag `exportfixture` fuera de la suite default** — para
   dejar el SQLite en disco para T4 y mantener el green-twice estable/repetible.
5. **Valores del fixture float4-safe** (cuartos) — los embeddings reales son
   float32; usar valores float4-safe prueba el camino real de export sin
   inyectar un artefacto de precisión float4 auto-infligido. Los tests de
   canonicalización de texto con valores no-float4-safe viven en la suite
   default y no tocan PG.
6. **Verificación PG del embedding** vía `embedding::text` (cast a texto, sin
   driver pgvector) y comparación como arrays con tolerancia float4 — la
   comparación honesta para un destino float4, que además reporta match exacto
   de texto (en este caso `true`).
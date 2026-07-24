# Contrato 05 — Soporte `vector(N)` (almacenamiento, sin pipeline de embeddings)

Prerrequisitos: `CONTRACT-01`..`CONTRACT-04` completados y verificados (`236f4f8`). Este contrato
cierra la última capacidad pendiente de `DEFINITION.md`: "Columnas de tipo `vector(N)` para
almacenar embeddings que el cliente calcula y envía (sin pipeline de generación de embeddings ni
búsqueda semántica integrada en v1)".

## RECON ya resuelto (no re-verificar)

- `sqlite-postgres-compat@v0.1.0` **ya tiene soporte nativo de `vector(N)`**, auditado con tests
  unitarios en el módulo (`compat/vector_test.go`, `Schema.Validate()` exige dimensión positiva
  única, `CompileDDL` compila ambos motores): en SQLite compila a `TEXT` (carrier interoperable,
  ver `compat/ddl.go`), en PostgreSQL compila a `vector(N)` nativo (**requiere la extensión
  `pgvector` instalada en el destino** — si falta, `CREATE TABLE` falla con un error claro, no
  degrada en silencio). El valor se canonicaliza como texto `'[c1,c2,...]'` sin espacios
  (`compat/store.go canonicalVectorValue`), rechazando componentes que no matcheen la dimensión
  declarada. La feature se reporta como `canonical_vectors` en `InferFeatures`/`audit`.
- **Gap real encontrado en esta RECON (no arreglar, solo tenerlo en cuenta):** ese soporte nativo
  de `vector(N)`→`vector` de Postgres **nunca fue probado contra un pgvector real** — los tests
  del módulo son unitarios (sin DB), y el único experimento con infra real
  (`docs/reports/VECTOR-COMPAT-REPORT.md`, 2026-07-22) es ANTERIOR a que existiera esta feature
  nativa y concluyó "sin soporte" sobre una versión distinta del código — está obsoleto para esta
  pregunta. Esta prueba de `librarian` es, de hecho, la primera vez que se ejercita ese camino
  contra infra real.
- Un contenedor Postgres corriente (`postgres:17-alpine`, el que usó CONTRACT-04) **NO trae la
  extensión `pgvector`** — hace falta una imagen que la incluya (p.ej. `pgvector/pgvector:pg17`) o
  instalar la extensión a mano en la imagen base. El orquestador provisiona esta infra nueva
  (nombre/puerto distintos de `pg-librarian-export` de CONTRACT-04, ya desmontado) — no la crees
  vos, usá el DSN que te pasen por `LIBRARIAN_VECTOR_PG_DSN`.
- `ContentType(name, ownColumns)` (`internal/schema/content_type.go`) ya acepta cualquier
  `[]compat.Column` para las columnas propias de un tipo — agregar una columna vector ahí es
  agregar un `compat.Column` más, no tocar la firma de la función.

## T1 — Columna `vector(N)` en el esquema + helper reusable

FIX/OBJETIVO: un helper `vectorColumn(name string, dimension int) compat.Column` en
`internal/schema/schema.go` (mismo patrón que `uuidColumn`/`jsonColumn`), NULLABLE (un artículo
sin embedding calculado aún es válido — el cliente lo agrega después). Aplicalo agregando una
columna `embedding vector(N)` a `articles` (vía `ContentType`, vos elegís cómo pasarla sin romper
la firma existente — a tu criterio, documentalo). Dimensión: elegí un valor concreto y
documentado con su porqué (p.ej. 384, 768, 1536 — dimensiones reales de modelos de embeddings
conocidos; el contrato no fuerza cuál, pero tiene que ser una elección justificada, no arbitraria).
Test de aceptación (sin DB real): `Schema.Validate()` + `CompileDDL` para AMBOS motores con la
columna nueva — debe compilar limpio en los dos.

## T2 — Exposición vía API real

FIX/OBJETIVO: extendé `POST /articles` y `PUT /articles/{id}` (o una ruta dedicada, ej.
`PUT /articles/{id}/embedding` — a tu criterio, documentá el porqué) para aceptar el embedding
como un array JSON de N números (`[0.12, -0.4, ...]`), validar la dimensión EXACTA contra la
declarada en el esquema (rechazo 400 claro si no matchea, nunca un 500), y almacenarlo como texto
canónico `'[c1,c2,...]'` (el mismo formato que `compat` usa — replicalo a mano, ya que `librarian`
escribe con `database/sql` parametrizado directo, no a través de las funciones de escritura de
`compat.Store`; no reinventes el formato, calcá `canonicalVectorValue` de `compat/store.go`: sin
espacios, cada componente normalizado). `GET /articles/{id}` y el listado devuelven el embedding
(cuando no es NULL) como array JSON de números, no como el texto crudo. Omitir el campo en
create/update dejalo NULL — backward compatible con CONTRACT-03/04.

## T3 — Datos reales + round-trip

FIX/OBJETIVO: extendé (o replicá el patrón de) `internal/server/export_fixture_test.go` — al
menos un artículo real creado con un embedding real (vía la API real de T2, valores arbitrarios
pero de la dimensión correcta) y al menos uno SIN embedding (NULL, camino backward-compatible).
Confirmá con una query directa a SQLite que el valor guardado matchea el formato canónico
`'[c1,c2,...]'` esperado por `compat` (para que el `schema.json` de `--dump-schema` y este dato
sean consistentes de punta a punta).

## T4 — Export real contra PostgreSQL con `pgvector`, con verificación

FIX/OBJETIVO: usando el binario `compat` real y el JSON de esquema actualizado (T1), corré
`compat audit` (debe incluir `canonical_vectors: exact` ahora, junto con las features ya conocidas
de CONTRACT-04) y `compat copy` contra el PostgreSQL real con `pgvector` (`LIBRARIAN_VECTOR_PG_DSN`)
— si la extensión no está instalada en el destino, `compat copy` SI puede fallar de forma legítima
(ver ABORTAR SI); documentá el mensaje real del motor si eso pasa, no lo ocultes. Si compila y
corre: debe terminar `VerificationReport.equivalent = true`, exit 0. Verificación ADICIONAL
independiente: query SQL directa a PostgreSQL (`SELECT embedding FROM articles WHERE id = ...`)
confirmando que el valor nativo `vector` de PG, leído y comparado como texto/array, coincide con
el valor de origen en SQLite.

## T4-bis — Reporte del gap si aparece uno nuevo

Si en T4 aparece un comportamiento inesperado del lado de `compat` (no de `librarian`) — por
ejemplo el DDL nativo `vector(N)` no compila contra `pgvector` real por alguna razón que los tests
unitarios de `compat` no cubrían — **NO lo arregles** (fuera de tu perímetro). Documentalo en el
reporte con la evidencia REAL (mensaje de error completo del motor, comando exacto que lo
disparó) para que el orquestador decida si abre un hallazgo aparte contra `sqlite-postgres-compat`.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces (el fixture con embedding, si deja
  artefacto persistente para T4, aislado con build tag — mismo patrón que CONTRACT-04).
- [ ] T1: `Schema.Validate()` + `CompileDDL` con la columna vector, ambos motores, limpio.
- [ ] T2: create/update con embedding válido → 200/201, dimensión inválida → 400 claro (nunca
  500), GET devuelve array JSON de números. Backward compatible: omitir el campo deja NULL.
- [ ] T3: query directa a SQLite confirmando el formato canónico exacto del valor guardado.
- [ ] T4: `compat audit` incluye `canonical_vectors: exact` (o documenta por qué no, si legítimo).
  `compat copy` real → exit 0, `equivalent:true` (o el fallo real documentado si la extensión no
  estaba, ver ABORTAR SI). Verificación independiente por query directa a PostgreSQL.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat` bajo ninguna
  circunstancia — cualquier gap de ESE repo se documenta, no se arregla (ver T4-bis).
- El password del DSN (`LIBRARIAN_VECTOR_PG_DSN`) nunca en texto plano en ningún archivo del repo
  ni del reporte — siempre `***`. Archivos temporales con el DSN en un tempdir del sistema,
  borrados al final.
- NO commitear (el orquestador commitea tras verificar).
- ABORTAR SI: `compat audit` da algo distinto de `exact` para `canonical_vectors` de forma
  INESPERADA (sin relación con la extensión faltante — ver abajo), o `compat copy` diverge con
  `ERR_VERIFY_DIVERGED` (no un fallo de `CREATE TABLE` por extensión ausente, que es un resultado
  legítimo a documentar, no un abort). Si `compat copy` falla porque `pgvector` no está instalada
  en el destino que te dieron, ESO es un problema de infra del orquestador, no tuyo — documentalo
  y respondé BLOQUEADO, no lo intentes resolver instalando la extensión vos mismo (fuera de tu
  perímetro: no tenés por qué tener acceso de superusuario al Postgres real).

## Checklist antes de delegar

- [ ] RECON corrido: soporte nativo de `compat` confirmado (arriba), gap de "nunca probado con
  pgvector real" identificado, infra `pgvector` real provisionada por el orquestador ANTES de
  delegar (no asumida).
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿qué pasa si el cliente manda un embedding con la dimensión incorrecta? (400, no
  500 ni truncamiento silencioso). ¿Qué pasa si manda un valor no numérico dentro del array?
  (rechazo claro). ¿El helper de formato de texto vectorial replica EXACTAMENTE
  `canonicalVectorValue` de `compat` (sin espacios, normalización de floats), o diverge en algún
  caso (`2.0` vs `2`, notación científica)? — pedile al dev un test específico para ese borde.
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Condiciones de aborto explícitas, incluida la distinción entre "gap real de compat →
  documentar y BLOQUEADO" vs. "regresión de librarian → abortar y arreglar".

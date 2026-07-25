# CONTRACT-20 — `internal/server` dual-motor (parte 2 de 3)

Base: `83a9e19` (CONTRACT-19 completo y desplegado). Árbol **SIN commitear**, como pide el contrato:
el orquestador commitea y despliega tras verificar.

**Resultado: LISTO.** `internal/server` ya no tiene una sola sentencia SQL atada a un motor: sus 45
sentencias pasaron a rutinas canónicas sobre vistas (todas las LECTURAS) o a SQL crudo compuesto con
`compat.Placeholder` (todas las ESCRITURAS). El **mux HTTP real** devuelve **exactamente lo mismo**
contra SQLite real y contra PostgreSQL 17 real — **121 observaciones idénticas**, línea por línea,
incluidos todos los códigos de estado, todos los mensajes de error y **el ORDEN de cada listado**.

La aplicación **sigue corriendo solo en SQLite**, como el contrato establece: elegir motor es
CONTRACT-21 y no se adelantó nada de eso.

**Dos hallazgos que el orquestador tiene que leer antes de desplegar** (§ "Hallazgos"):

1. La batería **atrapó una divergencia real de ORDEN por COLACIÓN** — la clase de bug que el
   contrato señala como la más peligrosa. Está corregida dentro de `internal/server`.
2. Esa misma divergencia **existe hoy, sin corregir, en `internal/auth` (CONTRACT-19)**, que está
   fuera de mi perímetro. No la toqué. Hay que decidir qué hacer.

---

## Resumen de qué se tocó

| Archivo | Qué |
|---|---|
| `internal/schema/server_dual.go` (**nuevo**) | T1: 4 vistas + 16 rutinas canónicas + el generador de rutinas por tipo dinámico |
| `internal/schema/schema.go` | `Build()` compone `authViews()+serverViews()` y `authRoutines()+serverRoutines()` |
| `internal/schema/dynamic.go` | `BuildWith` genera las 2 rutinas de lectura de cada tipo dinámico |
| `internal/server/dual.go` (**nuevo**) | plomería neutral: `bind`/`bindList`, `quote`, UUID v4, instante canónico, constructores de `compat.Value`, accesores de fila, y el orden byte-a-byte |
| `internal/server/articles.go` | T2: 13 sentencias (el del `vector`) |
| `internal/server/terms.go` | T2: 13 sentencias |
| `internal/server/content.go` | T2: 7 sentencias (la decisión abierta) |
| `internal/server/products.go` | T2: 7 sentencias |
| `internal/server/authz.go` | T2: 2 sentencias |
| `internal/server/ui_apikeys.go`, `ui_roles.go`, `ui_users.go` | T2: 1 sentencia c/u |
| `internal/server/server.go` | `handlers.authStore` → `handlers.store`; `registerRoutes` extraído de `NewMux` |
| `internal/server/ui.go` | solo acompañar el rename del campo |
| `internal/server/dualengine_contract20_test.go` (**nuevo**) | T3: batería dual-motor sobre el mux HTTP real (tag `dualengine`) |
| `docs/OPERATIONS.md` | cómo se corre la batería + la nota de colación |

`sqlite-postgres-compat` **no se tocó**. Ver § "¿Le falta algo a `compat`?".

`internal/store` **no se tocó en absoluto** (ni siquiera lo mínimo que el contrato permitía).
`server.Deps` **no cambió**. `store.Open` **no cambió**.

---

## LO PRIMERO QUE SE MIDIÓ: el `vector` contra PostgreSQL real

El contrato manda comprobar esto **antes de diseñar nada más**, porque condiciona todo
`articles.go`. Se hizo con una sonda descartable contra el PostgreSQL 17 provisto, ANTES de escribir
una línea de la migración. Salida real:

```
server: PostgreSQL 17.10 (Debian 17.10-1.pgdg12+1) on x86_64-pc-linux-gnu, compiled by gcc 12.2.0, 64-bit
PROBE-0 articles.embedding physical type on PostgreSQL = "vector(1536)"
PROBE-1 raw bind (no cast)      err=<nil>
PROBE-2 raw bind (explicit CAST) err=<nil>
PROBE-3 raw select Go type=string len=3099 head="[1.5,-2,0.1,3.4028235e+38,0.12345679,0,0,..." tail="...,0,0,0]"
PROBE-3b scan into string err=<nil> head="[1.5,-2,0.1,3.4028235e+38,0.12345679,0,0,..."
PROBE-4 QueryRoutine kind=vector head="[1.5,-2,0.1,3.4028235e+38,0.12345679,0,0,..."
PROBE-4 round-trip identical to written carrier = false
PROBE-4 written head="[1.5,-2,0.1,3.4028235e+38,0.123456789012345,0,0,..."
PROBE-5 wrong-dimension raw bind err=ERROR: expected 1536 dimensions, not 3 (SQLSTATE 22000)
PROBE-6 NULL embedding bind err=<nil>
```

### Qué respondió, y qué se decidió con eso

**1. Enlazar el texto portador como parámetro plano FUNCIONA, sin conversión (PROBE-1).**
En `INSERT INTO articles (…, embedding) VALUES (…, $4)` PostgreSQL infiere el tipo del parámetro
**de la columna destino**, así que la función de entrada de `pgvector` parsea `'[c1,c2,...]'` tal
cual. El `CAST` explícito también funciona (PROBE-2) pero **NO se usó**: `CAST($4 AS vector)` es
sintaxis que solo entiende PostgreSQL, dentro de una sentencia que también tiene que correr en
SQLite — es decir, exactamente la divergencia que este contrato borra. **La conversión mínima
necesaria es NINGUNA.** Es el mejor resultado posible: el riesgo alto del contrato no condiciona el
diseño de `articles.go`.

**2. `NULL` se enlaza sin problema (PROBE-6)**, lo que permitió fusionar las ramas "poner embedding"
y "borrar embedding" en una sola sentencia con un parámetro que a veces es `nil`.

**3. Al LEER, el SQL crudo NO sirve (PROBE-3).** El driver devuelve un `string`, pero es la
representación que `pgvector` decide, no el texto que SQLite tiene guardado. Por eso **todas las
lecturas de `articles` van por `QueryRoutine`** con la columna declarada `vector(1536)`: compat
canonicaliza por familia declarada y los dos motores entregan la MISMA forma `'[c1,c2,...]'`.
Esto no fue una preferencia de estilo, fue la medición.

**4. La dimensión equivocada se comporta distinto en el SQL crudo (PROBE-5):** PostgreSQL la rechaza
(`SQLSTATE 22000`), SQLite (donde la columna es `TEXT`) la aceptaría. **No es observable por HTTP**
porque `validateEmbedding` valida en Go ANTES de tocar SQL y devuelve 400 en ambos motores — está
comprobado en la batería (`articles POST wrong-dimension status=400` idéntico en los dos).

### El límite que el enlace NO puede arreglar, medido y documentado

**`pgvector` almacena `float4` (precisión simple).** Una componente que necesita más que precisión
simple se redondea en PostgreSQL y se conserva entera en SQLite. Es una propiedad de la columna
`vector(1536)` que declaró CONTRACT-05, **no del camino de lectura/escritura de CONTRACT-20**: el
enlace, el texto de la sentencia y la canonicalización son idénticos en los dos motores.

Se mide y se reporta en vez de esconderse, con un test propio. Salida real:

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineVectorPrecision -count=1 -v ./internal/server
=== RUN   TestDualEngineVectorPrecision
    [sqlite]   exact-representable  wrote=0.5 read=0.5 identical=true
    [sqlite]   high-precision       wrote=0.123456789012345 read=0.123456789012345 identical=true
    [postgres] exact-representable  wrote=0.5 read=0.5 identical=true
    [postgres] high-precision       wrote=0.123456789012345 read=0.12345679 identical=false
    MEASURED PRECISION BOUNDARY: pgvector stores float4, so 0.123456789012345 is read back as
    0.12345679 on PostgreSQL while SQLite's TEXT carrier returns 0.123456789012345. This is a
    property of the vector(1536) column CONTRACT-05 declared, not of the CONTRACT-20 read/write path.
--- PASS: TestDualEngineVectorPrecision (8.33s)
```

El test **asserta** lo que sí es contrato de este repo (una componente exacta en binario32
round-trippea idéntica en ambos motores; SQLite conserva la precisión completa) y **loguea** lo que
es redondeo de `pgvector`. Por eso la batería principal usa componentes exactas en `float32`: aísla
lo que el contrato controla de una propiedad del almacenamiento que ningún código de consumidor
puede cambiar.

**Consecuencia operativa para el orquestador:** si algún día se migra de verdad a PostgreSQL con
embeddings de alta precisión ya cargados, `compat copy`/`cutover` reportará `equivalent=false` para
esas filas. No es un bug de esta migración; es el precio declarado de `vector(N)` nativo. La
alternativa (declarar `embedding` como `text` y perder los índices vectoriales de pgvector) es una
decisión de CONTRACT-05, no de este contrato, y **no se tocó**.

---

## Decisiones de diseño (con su porqué)

### 1. El criterio que reparte todo: LECTURAS → rutina, ESCRITURAS → SQL crudo

No es una preferencia, es el criterio mecánico que el contrato establece, aplicado sin excepciones:

**Toda LECTURA va por `QueryRoutine`**, porque una rutina canonicaliza cada valor por su familia
**declarada**, y eso es lo único que hace que las tres familias físicamente divergentes se lean
igual en los dos motores:

| Familia | SQLite | PostgreSQL |
|---|---|---|
| `boolean` | `INTEGER` (0/1) | `BOOLEAN` |
| `decimal` | `TEXT` | `NUMERIC` |
| `vector` | `TEXT` (portador) | `vector(N)` nativo (pgvector) |

**Toda ESCRITURA queda en SQL crudo con `compat.Placeholder`**, porque todas caen en uno de los dos
casos que `CallRoutine` no puede cubrir:

- **el conteo de filas decide la respuesta HTTP** (`updateArticle*`, `deleteArticleByID`,
  `updateProductFields`, `deleteProductByID`, `deleteTermByID`, `updateContentRow`,
  `deleteContentRow`): `CallRoutine` devuelve **solo `error`**. El contrato prohíbe explícitamente
  simularlo con una lectura previa — agregaría un viaje y una carrera donde hoy no hay ninguna;
- **corren dentro de una transacción que el consumidor abre** (`insertTerm`, `updateTerm`,
  `setContentTerms`, y los resolvedores `taxonomyIDForName`/`termExists`/`resolveParent` que se
  ejecutan dentro de ellas): `CallRoutine` y `QueryRoutine` abren la SUYA, así que no se pueden
  anidar. Es el mismo límite estructural que CONTRACT-19 documentó para `roleIDsForNames`.

**Resultado: `internal/server` no usa `CallRoutine` en ningún lado, y no es un olvido.** Las dos
inserciones que técnicamente podrían usarlo (`insertProduct`, `insertArticle`) quedaron en SQL crudo
por coherencia y porque la de `articles` tiene forma variable (§ decisión 4).

SQL crudo compuesto con `Placeholder` y sentencias estándar **es dual-motor y es una solución
legítima**, como el contrato dice. La prueba independiente y más fuerte que cualquier grep: la mitad
PostgreSQL de la batería pasa, y un `?` suelto sería error de sintaxis en PostgreSQL igual que un
`$1` lo sería en SQLite.

### 2. La decisión ABIERTA del contrato: las lecturas de `content.go` → **rutinas generadas**

El contrato la marca como "la única decisión de diseño realmente abierta". Se eligió **generar las
rutinas por tipo en `schema.BuildWith`**, y la razón no es simetría con el resto: es que el código
que había **ya era código por motor, disfrazado de programación defensiva**.

Esto es lo que `content.go` tenía para leer UNA columna entera:

```go
func driverInt(v any) (int64, error) {
	switch t := v.(type) {
	case int64:   return t, nil
	case int:     return int64(t), nil
	case bool:    if t { return 1, nil }; return 0, nil
	case float64: return int64(t), nil
	case string:  return strconv.ParseInt(strings.TrimSpace(t), 10, 64)
	case []byte:  return strconv.ParseInt(strings.TrimSpace(string(t)), 10, 64)
	...
```

y la rama booleana intentaba un entero y **caía a `bool`** si fallaba. Esas ramas existen porque la
respuesta genuinamente difiere: **SQLite guarda un `BOOLEAN` como `INTEGER` y devuelve `int64`;
PostgreSQL lo guarda como `BOOLEAN` y devuelve un `bool` de Go**. Lo mismo con `decimal`
(`TEXT` vs `NUMERIC`). Mantener SQL crudo ahí habría dejado el `?` fuera pero la **ramificación por
motor adentro**, que es literalmente lo que el contrato define como el objetivo ("cero SQL atado a
un motor", no "cero `?`").

Declarar la familia mueve ese conocimiento **al esquema**, donde se enuncia una vez y se verifica.
`scanRow` + `jsonValue` + `driverText` + `driverInt` (4 funciones, ~90 líneas de adivinanza) se
reemplazaron por **una** función total, `rowJSON`, sobre una sola representación.

**Costo, dicho sin adornos:** `BuildWith` genera 2 rutinas por tipo dinámico, y cada lectura compone
el esquema de **ese** tipo con `schema.BuildWith([]ContentTypeDefinition{def})` — una función pura,
sin viaje a la base (la petición ya hacía un viaje para traer la definición). No se compone el
esquema de toda la instancia: una lectura de `eventos` necesita `eventos` declarado y nada más.

Las **escrituras** de `content.go` siguen en SQL crudo: el conteo de columnas se conoce solo en
runtime (una lista de acciones de rutina es estática) y `updateContentRow`/`deleteContentRow`
necesitan `RowsAffected`. El modelo de seguridad del archivo **no cambió**: los identificadores
siguen viniendo solo de la definición persistida a través de `quoteIdentifier`, y todos los valores
siguen enlazados. La batería lo prueba con un valor que parece SQL:
`content POST sql-looking-value status=201 verbatim=true` y
`content injection users-table-intact=true count=1`.

### 3. `articles.go`: 4 variantes → **1 sentencia**, no 4 rutinas

El contrato advierte: "si migrarlo tal cual produce cuatro rutinas casi iguales, es señal de que ahí
conviene SQL crudo". Se hizo eso y un paso más: las cuatro variantes del `INSERT` (según haya o no
`metadata` y `embedding`) se **colapsaron en una sola** sentencia que agrega las columnas opcionales
realmente presentes. El caso omitido sigue dejando la columna en su `NULL` por defecto —
comportamiento byte-idéntico, **una** sentencia que mantener dual-motor en vez de cuatro.

Igual en el `UPDATE`: las variantes "poner embedding" y "borrarlo" son la MISMA sentencia con un
valor enlazado distinto (el texto portador, o `nil` → SQL `NULL`), y la tercera simplemente omite la
asignación. De 3 ramas a 2. Enlazar `NULL` en vez de escribir el literal `NULL` en el texto es lo
que permite la fusión, y PostgreSQL lo acepta contra la columna `vector` nativa igual que el
portador (PROBE-6).

### 4. `RETURNING id` → UUID generado en Go, y `CURRENT_TIMESTAMP` → parámetro

Igual que CONTRACT-19 decisiones 3 y 4, por las mismas razones, extendido a las cuatro tablas que
este contrato escribe (`articles`, `products`, `terms`, y las dinámicas). Los `DEFAULT
gen_random_uuid()` y `DEFAULT CURRENT_TIMESTAMP` se **conservan** como red de seguridad.

Lo de `created_at` **no es cosmético y no es opcional**: es la clave PRIMARIA de orden de
`listArticles`, `listProducts` y `listContentRows`, es `TEXT` en ambos motores, y los dos motores
renderizan `CURRENT_TIMESTAMP` distinto (`"2026-07-25 18:38:25"` en SQLite, resolución de segundo;
`"2026-07-25 18:38:26.471192+00"` en PostgreSQL — medido en CONTRACT-19). Ordenar esos dos textos no
da la misma secuencia. Escribiendo el instante desde la aplicación en la forma que compat **declara**
para la familia, el texto almacenado —y por lo tanto el orden— es idéntico.

**Un matiz que se agregó sobre CONTRACT-19:** el instante se escribe con **nanosegundos de ancho
fijo** (`2006-01-02T15:04:05.000000000Z07:00`). No es un formato nuevo — es un valor RFC3339Nano
válido, que compat parsea como cualquier otro y **re-renderiza recortado al leerlo**, así que nada
observable cambia. El ancho fijo existe por el `ORDER BY`: `time.RFC3339Nano` de Go **recorta los
ceros finales**, lo que hace que los textos almacenados difieran en longitud y en la posición de la
`Z` final. Ver § decisión 5.

### 5. LA COLACIÓN — la divergencia que la batería atrapó de verdad

Esta es la sección importante del informe. La batería falló en su primera corrida completa, y falló
por la razón exacta por la que el contrato la exige.

**Síntoma real (primera corrida, antes del arreglo):**

```
line 1 diverges:
  sqlite  : authz jwt-branch roles=[administrator] order=[content.create content.delete content.publish content.update content_types.manage roles.manage terms.manage users.manage]
  postgres: authz jwt-branch roles=[administrator] order=[content.create content.delete content.publish content_types.manage content.update roles.manage terms.manage users.manage]
line 3 diverges:  (misma causa, unión de dos roles)
line 12 diverges: (misma causa, rolesWithPermissions)
```

Mismas filas, **secuencia distinta**, con el MISMO `ORDER BY permission_name` declarado en la rutina.

**Causa, medida y no supuesta.** Sonda contra el PostgreSQL real:

```
PROBE datcollate="en_US.utf8" datctype="en_US.utf8"
PROBE default collation order = [content.create content.publish content_types.manage content.update]
PROBE C collation order       = [content.create content.publish content.update content_types.manage]
PROBE uuid order              = [0a1b2c3c-… 0a1b2c3d-… 0a1b2c3e-…]   (igual en ambos)
```

**PostgreSQL ordena `TEXT` por la colación de la base; SQLite lo ordena por BYTES.** En
`en_US.utf8` el `.` y el `_` no tienen peso primario, así que `content_types.manage` va ANTES de
`content.update`; en SQLite `content.update` va primero porque `.` (0x2E) < `_` (0x5F).

**Por qué no se puede arreglar en el esquema.** Expresarlo requeriría `COLLATE "C"` en PostgreSQL y
`COLLATE BINARY` en SQLite — sintaxis distinta, o sea exactamente el SQL por motor que este contrato
borra — y el `ORDER BY` declarado de una rutina de compat es **un nombre de columna**, sin colación
que darle. **No es un hueco de compat**: es una propiedad del despliegue de PostgreSQL (la colación
de la base), no algo que la librería pueda decidir por el consumidor.

**Cómo se resolvió, dentro del perímetro:**

- **Listados NO paginados** (permisos por rol, catálogo de roles, listado de términos, términos
  asignados): la rutina conserva su `ORDER BY` como base estable y el **orden final se impone en
  Go**, con comparación byte a byte (`sortStrings` / `sortByKeys` en `dual.go`). Es independiente
  del motor por construcción: es el orden de la aplicación, no la opinión de la base sobre el orden
  de la aplicación.
- **Listados PAGINADOS** (`articles`, `products`, contenido dinámico): NO pueden usar eso — el
  `LIMIT`/`OFFSET` elige las filas dentro del motor, así que reordenar después reordenaría una
  página en vez de elegirla. Son seguros por otro argumento: sus claves son `created_at`, ahora de
  **ancho fijo** (§ decisión 4), e `id`, un UUID cuyos guiones están en offsets idénticos en todos
  los valores. Para ambas formas la comparación por colación y la comparación por bytes **coinciden
  demostrablemente**: si dos cadenas tienen la puntuación en las mismas posiciones, quitarla preserva
  el orden relativo de lo que queda. Verificado en la sonda para UUIDs y en la batería para las tres
  paginaciones.

Después del arreglo: **121 observaciones idénticas**.

Efecto observable por HTTP: **ninguno**. `permissionsFor` solo se consulta con "¿contiene X?", y los
listados de términos ya venían ordenados; lo que cambia es que ahora el orden está **garantizado**
en vez de heredado del motor.

### 6. `IsUniqueViolation` en vez del texto del mensaje — el ítem de red-team

`products.isUniqueSKUViolation` y `terms.isUniqueSlugViolation` comparaban el TEXTO
`"UNIQUE constraint failed"`, que es la redacción de **SQLite**. PostgreSQL dice
`"duplicate key value violates unique constraint"`. **En PostgreSQL el match no habría disparado
nunca**: el `sku` duplicado habría dejado de ser un 400 limpio y se habría vuelto un 500,
silenciosamente, sin que nada dejara de compilar, hasta que un usuario repitiera un sku en
producción. Es exactamente el escenario que el AGENTS.md de compat describe como la razón de existir
del primitivo.

Ahora ambos usan `compat.Store.IsUniqueViolation`, que clasifica por el **código estructurado del
driver** (`SQLITE_CONSTRAINT_UNIQUE`/`PRIMARYKEY`, `SQLSTATE 23505`) con `errors.As`, así que
sobrevive al envoltorio y responde igual en los dos motores.

La limitación documentada del primitivo (no dice CUÁL constraint) no molesta aquí: `products` tiene
dos claves únicas, la PK y `sku`, y la PK es un UUID v4 que este paquete genera por inserción — el
`sku` es la única con la que un llamador puede chocar. Igual en `terms` con `(taxonomy_id, slug)`.

Probado en los dos motores y en las dos rutas:

```
products POST duplicate-sku status=400 error="a product with this sku already exists"
products PUT  duplicate-sku status=400 error="a product with this sku already exists"
products PUT  duplicate-sku unchanged sku=SKU-ONE
terms   POST duplicate-slug status=400 error="a term with this slug already exists in this taxonomy"
```

### 7. `ON CONFLICT DO NOTHING` → eliminado, conflicto imposible por construcción

Mismo razonamiento y mismo método que CONTRACT-19 decisión 5, aplicado a `setContentTerms`: el
`DELETE` que precede a los `INSERT` vacía el conjunto destino, así que el ÚNICO conflicto posible
con la PK compuesta es un id repetido en la lista del **llamador**. `dedupe()` lo hace **imposible**
en vez de tolerado, y desaparece la dependencia de una cláusula upsert cuya forma aceptada varía por
motor y versión. Comportamiento idéntico:
`terms ASSIGN article duplicates status=200 order=[Amphibians] len=1`.

### 8. `handlers.authStore` → `handlers.store`, y `registerRoutes` extraído

`Deps` **no cambió** (sigue tomando `DB *sql.DB`), como el contrato exige. El campo que CONTRACT-19
introdujo para `auth` perdió el `auth` del nombre porque ahora es la puerta de TODO el paquete.
`db` sobrevive solo para lo que un `*compat.Store` no ofrece: `BeginTx` (las transacciones que
`terms.go` posee) y el `*sql.DB` que los helpers de `internal/store` siguen tomando.

`NewMux` se partió en `NewMux` + `(h *handlers) registerRoutes(mux)`, **sin ningún cambio de
comportamiento**, para que la batería pueda montar las MISMAS rutas sobre un `handlers` construido
con el store de PostgreSQL. `NewMux` sigue siendo el único constructor público y sigue componiendo
el store de SQLite: **esa línea sigue siendo la única que CONTRACT-21 tiene que cambiar.**

### 9. Órdenes que se volvieron TOTALES

Además de la colación, tres listados tenían un orden **parcial**, que empata y deja que cada motor
desempate a su manera:

| Listado | Antes | Ahora | Por qué es total |
|---|---|---|---|
| `listTerms`, `assignedTermsFor` | `(taxonomy, name)` | `(taxonomy, name, slug)` | `slug` es UNIQUE dentro de una taxonomía |
| `listArticles`, `listProducts`, `listContentRows` | `created_at DESC` | `created_at DESC, id ASC` | `id` es la clave primaria |

La secuencia de cualquier conjunto que no tenía empate **no cambia**, que es por qué ninguna
aserción existente se movió. En los paginados importa mucho más que en los otros: sin orden total,
el MISMO `LIMIT`/`OFFSET` puede seleccionar **filas distintas** en cada motor.

---

## Qué se implementó, por tarea

### T1 — Vistas y rutinas en el esquema canónico

**4 vistas nuevas** (`internal/schema/server_dual.go`), una por cada JOIN escrito a mano que había:

| Vista | Reemplaza | Columnas expuestas |
|---|---|---|
| `role_name_permission_names` | `authz.go` (rama JWT) | `role_name`, `permission_name` |
| `term_records` | `terms.go` listar + leer por id | `id`, `taxonomy`, `name`, `slug`, `parent_id` |
| `article_assigned_terms` | `terms.go` `assignedTermsFor` (artículos) | `article_id`, `term_id`, `term_name`, `term_slug`, `taxonomy` |
| `product_assigned_terms` | `terms.go` `assignedTermsFor` (productos) | `product_id`, `term_id`, `term_name`, `term_slug`, `taxonomy` |

La rama de API-key de `authz.go` **reutiliza `role_permission_names` de CONTRACT-19** sin duplicarla.
`assignedTermsFor` necesita DOS vistas y no una porque la relación de una acción de lectura es un
nombre **declarado**, nunca provisto por el llamador — que es justamente la propiedad que antes
estaba garantizada solo por un comentario auditado a mano alrededor de una concatenación
(`FROM `+junction+``).

**16 rutinas de lectura** sobre el esquema de código, más **2 por cada tipo dinámico** generadas en
`BuildWith`:

| Rutina | Relación | Orden declarado |
|---|---|---|
| `server_permissions_by_role_id` | vista `role_permission_names` | `permission_name` |
| `server_role_id_by_name` / `server_role_name_by_id` | `roles` | — |
| `server_list_roles` | `roles` | `name` |
| `server_list_terms` | vista `term_records` | `taxonomy, name, slug` |
| `server_term_by_id` | vista `term_records` | — |
| `server_article_assigned_terms` | vista `article_assigned_terms` | `taxonomy, term_name, term_slug` |
| `server_product_assigned_terms` | vista `product_assigned_terms` | `taxonomy, term_name, term_slug` |
| `server_list_products` | `products` | `created_at DESC, id` (+ LIMIT/OFFSET) |
| `server_product_by_id` / `server_product_exists` | `products` | — |
| `server_list_articles` | `articles` | `created_at DESC, id` (+ LIMIT/OFFSET) |
| `server_article_by_id` / `server_article_exists` / `server_article_published_at` / `server_article_embedding` | `articles` | — |
| `content_list_<tabla>` (por tipo) | tabla dinámica | `created_at DESC, id` (+ LIMIT/OFFSET) |
| `content_read_<tabla>` (por tipo) | tabla dinámica | — |

Cada acción `select` **declara sus columnas de salida con su familia**, que es lo que hace idéntico
el escaneo en los dos motores. `articles.embedding` se declara `vector(1536)` y `products.price`
`decimal`, que son las dos declaraciones que más trabajo hacen.

`server_article_exists` es una rutina propia en vez de reusar la lectura por id a propósito: corre
antes de cada update y de cada publish, y la lectura completa arrastraría un vector de 1536
componentes para responder un sí/no.

Vistas y rutinas viajan con el esquema canónico (están en `Build()`/`BuildWith()`), así que
`--dump-schema` / `compat copy` las llevan y el round-trip JSON existente las cubre sin tocarlo.
`store.EnsureSchema` ya recreaba las vistas desde `want.Views` (CONTRACT-19 decisión 7), así que las
nuevas **se crean solas en el camino de upgrade** — sin tocar `internal/store`, y verificado por
`TestEnsureSchemaRecreatesViews`, que sigue verde.

### T2 — Migrar `internal/server`

**Cero `?` literales.** Salida real del grep sobre los archivos de producción:

```
$ grep -rn '?' internal/server/*.go | grep -v '_test.go'
internal/server/articles.go:142:// handleListArticles lists articles with simple ?limit=&offset= paging
internal/server/content.go:15:// file: a SQL IDENTIFIER (a table or column name) cannot be bound with a `?`
internal/server/content.go:31://     a `?` parameter. `";DROP TABLE users;--"` stored in a text field is stored
internal/server/content.go:250:// the Go value to bind as a `?` parameter. Every rejection is an errBadField
internal/server/content.go:330:// ?limit=&offset= paging articles/products use. Requires only a valid identity.
internal/server/dual.go:7:// Before this contract every statement in `server` was hand-written SQL with `?`
internal/server/dual.go:52:// place that knows about `?` vs `$n` — compat's.
internal/server/products.go:129:// handleListProducts lists products with simple ?limit=&offset= paging (default
internal/server/ui.go:46:// ?v= query param so that a CDN or browser caching them as
internal/server/ui_roles.go:171:// STILL hold roles.manage?
internal/server/ui_users.go:328:	return "No se pudo crear el usuario (¿email ya registrado?)."
```

Diez son **comentarios**; la única línea de código es un mensaje de UI en castellano. **Ninguna
sentencia.**

Y no queda ni un `CURRENT_TIMESTAMP`, ni un `RETURNING`, ni un `ON CONFLICT` en SQL (solo en
comentarios que explican por qué se fueron):

```
$ grep -rn "CURRENT_TIMESTAMP\|RETURNING\|ON CONFLICT" internal/server/*.go | grep -v '_test.go'
   → 14 coincidencias, TODAS en comentarios
```

Mapa de las 45 sentencias:

| Archivo | Sentencias | Camino |
|---|---|---|
| `articles.go` | 13 | 5 lecturas → `QueryRoutine`; 8 escrituras → SQL crudo + `Placeholder` |
| `terms.go` | 13 | 4 lecturas → `QueryRoutine` sobre vista; 9 (escrituras + resolvedores en transacción) → SQL crudo |
| `content.go` | 7 | 2 lecturas → rutinas generadas por tipo; 3 escrituras + 2 sondas → SQL crudo |
| `products.go` | 7 | 3 lecturas → `QueryRoutine`; 4 escrituras → SQL crudo |
| `authz.go` | 2 | 1 → `QueryRoutine` sobre vista; 1 (IN de arity variable) → SQL crudo sobre vista |
| `ui_apikeys.go`, `ui_roles.go`, `ui_users.go` | 1 c/u | `QueryRoutine` |

### T3 — Verificación

Batería en `internal/server/dualengine_contract20_test.go`, build tag `dualengine` (mismo patrón que
CONTRACT-19). Sin `COMPAT_POSTGRES_DSN` **saltea**, no pasa en falso.

**Es más fuerte que la de CONTRACT-19**: aquella manejaba las funciones públicas de Go; esta maneja
el **mux HTTP real** (`registerRoutes` sobre un `handlers` por motor), así que cada observación es un
código de estado y un cuerpo de respuesta producidos por los mismos handlers que atiende un cliente.
Incluye el login real por `POST /auth/login`.

- Lado SQLite: camino de **producción** (`store.Open` → `EnsureSchema` → `SeedCatalogs`), así que la
  batería también prueba que el arranque real crea las vistas de CONTRACT-20.
- Lado PostgreSQL: **schema propio y único por corrida** (`librarian_c20_<nanos>`), `ApplySchema` del
  esquema canónico completo, `DROP SCHEMA … CASCADE` al terminar. `search_path` con `public` en
  segundo lugar solo para resolver el tipo `vector`; se verifica con `SELECT current_schema()` que el
  aislamiento es real antes de crear nada.
- El tipo dinámico se crea **durante la prueba**, con `compat.CompileDDL` + dos inserts
  parametrizados, en vez de `store.CreateContentType` — ese helper escribe metadata de
  `__compat_schema` con sentencias que `internal/store` todavía no migró (CONTRACT-21). Lo que esta
  batería tiene que probar es la CAPA CRUD GENÉRICA, que es lo que CONTRACT-20 migró.

---

## Verificación — salida REAL

### build / vet / gofmt

```
=== go build ./... ===
(ok)
=== go vet ./... ===
(ok)
=== go vet -tags {exportfixture,dualengine} ===
VET-TAGS-OK
=== gofmt -l . ===
(empty = ok)
```

### `go test ./... -count=1`, dos veces

```
=== RUN 1 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.471s
ok  	github.com/MauricioPerera/librarian/internal/auth	4.367s
ok  	github.com/MauricioPerera/librarian/internal/config	0.687s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.439s
ok  	github.com/MauricioPerera/librarian/internal/server	32.669s
ok  	github.com/MauricioPerera/librarian/internal/store	3.709s
=== RUN 2 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.457s
ok  	github.com/MauricioPerera/librarian/internal/auth	4.332s
ok  	github.com/MauricioPerera/librarian/internal/config	0.670s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.386s
ok  	github.com/MauricioPerera/librarian/internal/server	32.637s
ok  	github.com/MauricioPerera/librarian/internal/store	3.713s
```

### Tests existentes: NI UNA aserción tocada

```
$ git diff --stat -- '*_test.go'
 internal/server/guard_contract16_internal_test.go | 2 +-
 1 file changed, 1 insertion(+), 1 deletion(-)

$ git diff -- '*_test.go'
-	return &handlers{db: sdb.DB, authStore: sdb, jwtSecret: "guard-secret"}, sdb, ...
+	return &handlers{db: sdb.DB, store: sdb, jwtSecret: "guard-secret"}, sdb, ...
```

**UNA línea, en UN archivo, y es el rename de un campo de `handlers` en el andamiaje.** Cero
aserciones, cero códigos de estado esperados, cero mensajes esperados. Los ~30 archivos de test de
`internal/server` que ejercitan las rutas HTTP y la UI de CONTRACT-01..18 de punta a punta pasan sin
tocarse.

### La batería dual-motor — SQLite real + PostgreSQL 17 real

Motor destino verificado en vivo: `PostgreSQL 17.10 (Debian 17.10-1.pgdg12+1) on x86_64-pc-linux-gnu`,
extensión `vector` presente, `articles.embedding` con tipo físico `vector(1536)`.

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineServer -count=1 -v ./internal/server
=== RUN   TestDualEngineServer
    dualengine_contract20_test.go:89: transcript (121 lines, identical on both engines):
        authz jwt-branch roles=[administrator] order=[content.create content.delete content.publish content.update content_types.manage roles.manage terms.manage users.manage]
        authz jwt-branch roles=[editor author] order=[content.create content.update]
        authz jwt-branch union-with-overlap order=[content.create content.delete content.publish content.update content_types.manage roles.manage terms.manage users.manage]
        authz jwt-branch empty-roles perms=[] len=0
        authz jwt-branch unknown-role perms=[] len=0
        authz apikey-branch role=editor order=[content.create content.update]
        authz apikey-branch unknown-role-id perms=[] len=0
        catalog roleIDForName editor found=true err=false matches=true
        catalog roleIDForName unknown found=false
        catalog actorRoleNames apikey=[editor] err=false
        catalog actorRoleNames vanished-role=[] len=0
        catalog rolesWithPermissions order=[administrator:content.create|content.delete|content.publish|content.update|content_types.manage|roles.manage|terms.manage|users.manage author: contributor: editor:content.create|content.update]
        articles POST no-embedding status=201 title=z-first-created
        articles POST with-embedding status=201 title=m-second-created
        articles POST third status=201 title=a-third-created
        articles POST wrong-dimension status=400 error="embedding dimension mismatch: expected 1536, got 3"
        articles POST non-numeric-component status=400 error="embedding dimension mismatch: expected 1536, got 3"
        articles POST missing-fields status=400 error="title and body are required"
        articles GET with-embedding status=200 title=m-second-created published=<absent>
        articles GET embedding len=1536 all-components-identical=true first-divergent-index=-1 head=[1,-2,0.5,0.25,3.75,-0.125]
        articles GET no-embedding status=200 title=z-first-created embedding-absent=true published-absent=true
        articles LIST status=200 order=[a-third-created m-second-created z-first-created]
        articles LIST embedding-presence=[false true false]
        articles LIST page limit=1 offset=1 status=200 order=[m-second-created]
        articles LIST page limit=2 offset=0 status=200 order=[a-third-created m-second-created]
        articles LIST page limit=0 status=200 order=[]
        articles PUT title-body status=200 body=body one edited
        articles PUT set-embedding status=200
        articles PUT set-embedding read-back len=1536 all-components-identical=true first-divergent-index=-1 head=[2,-4,1,0.5,7.5,-0.25]
        articles PUT clear-embedding status=200
        articles PUT clear-embedding read-back embedding-absent=true body=body two cleared
        articles PUT omitted-embedding untouched len=1536 all-components-identical=true first-divergent-index=-1 head=[4,-8,2,1,15,-0.5]
        articles PUBLISH first status=200 published-present=true
        articles PUBLISH repeat status=200 unchanged=true
        articles GET after-publish published-present=true
        articles GET missing status=404 error="article not found"
        articles GET malformed status=404 error="article not found"
        articles PUT missing status=404 error="article not found"
        articles PUT malformed status=404 error="article not found"
        articles PUBLISH missing status=404 error="article not found"
        articles DELETE missing status=404 error="article not found"
        articles DELETE malformed status=404 error="article not found"
        articles DELETE existing status=204
        articles DELETE twice status=404
        articles LIST after-delete order=[m-second-created z-first-created]
        products POST status=201 title=z-widget price=19.99 sku=SKU-ONE
        products POST high-precision-decimal status=201 price=1234567890.1234567890
        products POST duplicate-sku status=400 error="a product with this sku already exists"
        products POST non-numeric-price status=400 error="price must be a valid decimal number"
        products LIST status=200 order=[a-gadget z-widget]
        products LIST prices=[1234567890.1234567890 19.99]
        products LIST page limit=1 offset=1 status=200 order=[z-widget]
        products GET status=200 title=z-widget price=19.99 sku=SKU-ONE
        products PUT status=200 price=29.95
        products PUT duplicate-sku status=400 error="a product with this sku already exists"
        products PUT duplicate-sku unchanged sku=SKU-ONE
        products GET missing status=404 error="product not found"
        products PUT missing status=404 error="product not found"
        products DELETE malformed status=404 error="product not found"
        products DELETE existing status=204
        products LIST after-delete order=[z-widget]
        terms POST parent status=201 taxonomy=category name=Zoology slug=zoology parent=<nil>
        terms POST child status=201 name=Amphibians parent-set=true
        terms POST same-slug-other-taxonomy status=201 taxonomy=tag
        terms POST duplicate-slug status=400 error="a term with this slug already exists in this taxonomy"
        terms POST unknown-taxonomy status=400 error="unknown taxonomy (must be one of the fixed catalog)"
        terms POST unknown-parent status=400 error="parent_id does not reference an existing term"
        terms PUT self-parent status=400 error="a term cannot be its own parent"
        terms LIST status=200 taxonomies=[category category tag]
        terms LIST names=[Amphibians Zoology Amphibians]
        terms LIST slugs=[amphibians zoology amphibians]
        terms LIST parents-null=[false true true]
        terms GET child status=200 taxonomy=category name=Amphibians parent-matches=true
        terms GET missing status=404 error="term not found"
        terms GET malformed status=404 error="term not found"
        terms PUT status=200 name=Frogs slug=frogs
        terms PUT missing status=404 error="term not found"
        terms DELETE missing status=404 error="term not found"
        terms ASSIGN article status=200 order=[Amphibians Zoology Frogs]
        terms ASSIGN article taxonomies=[category category tag]
        terms ASSIGN article duplicates status=200 order=[Amphibians] len=1
        terms ASSIGN article empty status=200 len=0
        terms ASSIGN article unknown-term status=400 error="one or more term ids do not reference an existing term"
        terms ASSIGN article missing-content status=404 error="articles not found"
        terms embedded-in-article order=[Amphibians Zoology]
        terms ASSIGN product status=200 order=[Zoology Frogs]
        terms embedded-in-product order=[Zoology Frogs]
        terms DELETE parent status=204
        terms child after parent-delete parent-null=true name=Amphibians
        terms article after parent-delete order=[Amphibians]
        content POST status=201 headline=z-first score=9 price=12.50 verified=true read_on=2024-03-01
        content POST types score=float64 verified=bool price=string
        content POST negatives status=201 score=-3 price=0.05 verified=false
        content POST all-omitted status=201 nulls=[headline price_paid read_on score verified]
        content POST wrong-type status=400 error="field \"headline\" must be a JSON string (declared type text)"
        content POST fractional-integer status=400 error="field \"score\" must be a whole JSON number (declared type integer)"
        content POST bad-date status=400 error="field \"read_on\" must be a date in YYYY-MM-DD form (declared type date)"
        content POST unknown-field status=400 error="unknown field \"nosuchfield\" for content type \"reviews\""
        content POST reserved-column status=400 error="unknown field \"id\" for content type \"reviews\""
        content POST sql-looking-value status=201 verbatim=true
        content LIST status=200 type=reviews headlines=[";DROP TABLE users;-- <null> a-second z-first]
        content LIST scores=[0 <null> -3 9]
        content LIST verified=[<null> <null> false true]
        content LIST prices=[<null> <null> 0.05 12.50]
        content LIST dates=[<null> <null> 1999-12-31 2024-03-01]
        content LIST metadata-null=[true true true true]
        content LIST page limit=2 offset=1 status=200 headlines=[<null> a-second]
        content GET status=200 headline=z-first score=9 verified=true price=12.50 date=2024-03-01
        content PUT partial-body status=200 headline=z-first-edited score=10 price-null=true verified-null=true date-null=true
        content PUT full status=200 score=0 price=0.000000000001 verified=true date=2030-01-01
        content GET missing status=404 error="content not found"
        content GET malformed status=404 error="content not found"
        content PUT missing status=404 error="content not found"
        content DELETE missing status=404 error="content not found"
        content DELETE malformed status=404 error="content not found"
        content GET unknown-type status=404 error="content type not found"
        content DELETE existing status=204
        content DELETE twice status=404
        content DELETE injection-row status=204
        content LIST after-delete headlines=[a-second z-first-edited]
        content injection users-table-intact=true count=1
    dualengine_contract20_test.go:103: OK: 121 observations identical on SQLite and PostgreSQL 17
--- PASS: TestDualEngineServer (34.13s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/server	37.897s
```

Las **cinco superficies** que el contrato exige, señaladas en ese transcript:

- **CRUD completo de `articles` con y sin `embedding`, leído de vuelta y comparado componente a
  componente** → `articles GET embedding len=1536 all-components-identical=true
  first-divergent-index=-1`, más el mismo reporte tras un `PUT` que lo reemplaza, tras uno que lo
  borra, y tras uno que lo OMITE (donde tiene que quedar intacto).
- **CRUD de `products` incluida la violación de `sku` duplicado** → `products POST duplicate-sku
  status=400` y `products PUT duplicate-sku status=400` + `unchanged sku=SKU-ONE`. Además un decimal
  de precisión arbitraria (`1234567890.1234567890`) vuelve byte-idéntico en los dos motores.
- **CRUD de `terms` con su jerarquía y su vista** → padre, hijo con `parent_id`, mismo slug en otra
  taxonomía, y el borrado del padre que **huerfana** al hijo (`parent-null=true`) en vez de
  borrarlo, con las filas de junción eliminadas por CASCADE.
- **Resolución de permisos de `authz.go`** → las 7 primeras líneas, ambas ramas (JWT con lista
  variable de nombres, API-key por id), con unión, solapamiento, lista vacía y rol inexistente.
- **CRUD genérico de un tipo DINÁMICO creado durante la prueba** → las 25 líneas de `content`, con
  los **cinco** tipos de campo, incluidos los tres que son físicamente distintos entre motores
  (`content POST types score=float64 verified=bool price=string` prueba que el tipo JSON emitido es
  el correcto en ambos).

**El ORDEN de todo listado, comparado explícitamente** → `articles LIST order=`, `products LIST
order=`, `terms LIST names=/slugs=/taxonomies=`, `terms ASSIGN … order=`, `content LIST headlines=`,
`authz … order=`, y **las paginaciones** (`limit=1 offset=1`, `limit=2 offset=0`, `limit=0`), que son
donde el orden decide QUÉ FILAS trae la página.

**Los 404** → sobre id inexistente Y sobre id malformado, en `GET`/`PUT`/`DELETE`/`PUBLISH` de las
cuatro superficies, más el borrado repetido (`DELETE twice status=404`). Idénticos en ambos motores.

### Contratos anteriores: siguen funcionando igual

La suite completa pasa verde dos veces. Y la batería de CONTRACT-19 sigue verde contra el mismo
PostgreSQL:

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineAuth -count=1 ./internal/auth
ok  	github.com/MauricioPerera/librarian/internal/auth	21.230s
```

---

## Red-team: las preguntas del contrato, respondidas con tests

| Pregunta | Respuesta | Evidencia |
|---|---|---|
| **¿El `ORDER BY` de cada listado es explícito y total? ¿Hay empates que cada motor desempata distinto?** | Ahora sí, y **encontró un bug**: no era el empate, era la COLACIÓN (§ decisión 5). Los listados no paginados imponen el orden en Go; los paginados usan `created_at` de ancho fijo + `id`. Además tres órdenes parciales se volvieron totales (§ decisión 9). | La corrida fallida con las 3 líneas divergentes, la sonda `datcollate="en_US.utf8"`, y las 121 líneas idénticas tras el arreglo |
| **¿`LIMIT`/`OFFSET` sin orden en algún lado?** | No. Los tres listados paginados declaran `ORDER BY` total en la rutina; compat además lo hace obligatorio cuando hay `LIMIT`. `LIMIT 0` es legal y devuelve vacío. | `articles LIST page limit=1 offset=1 / limit=2 offset=0 / limit=0`, `products LIST page`, `content LIST page limit=2 offset=1` |
| **¿`metadata` JSON vuelve idéntico?** | Sí. En `articles` se escribe verbatim y se lee por SQL crudo igual que antes; en las tablas dinámicas la familia `json` declarada la canonicaliza igual en ambos (claves ordenadas, compacto) y una columna NULL sigue siendo `null` en el JSON. | `content LIST metadata-null=[true true true true]`; el `POST` de artículo con `metadata` no cambió de código de estado |
| **¿El `CHECK` de `status` rechaza lo mismo?** | Fuera del alcance de este contrato (`users.status` es de `internal/auth`), y CONTRACT-19 ya lo verifica en los dos motores; su batería sigue verde. Lo que este contrato sí verifica es que TODA la validación de cuerpo devuelve el mismo 400 con el mismo mensaje. | `TestDualEngineAuth` verde; `articles/products/terms/content POST` con datos inválidos, mismos 400 y mismos mensajes |
| **¿La violación de `sku` sigue dando 400 y no 500 — `IsUniqueViolation`, nunca el texto?** | Sí, y era un bug REAL: el texto que se comparaba es el de SQLite, así que en PostgreSQL habría sido 500 (§ decisión 6). Ahora clasifica por código del driver. Igual para el slug de `terms`. | `products POST/PUT duplicate-sku status=400`, `terms POST duplicate-slug status=400`, idénticos en ambos motores |
| **¿Un `embedding` de dimensión equivocada falla igual en los dos?** | Por HTTP sí, idéntico 400 con idéntico mensaje, porque se valida en Go antes de tocar SQL. Al nivel del SQL crudo NO son iguales (PostgreSQL rechaza con `SQLSTATE 22000`, SQLite lo aceptaría como TEXT), lo cual está medido y documentado — la validación de la app es lo que hace que eso no sea observable. | `articles POST wrong-dimension status=400 error="embedding dimension mismatch: expected 1536, got 3"`; PROBE-5 |
| **¿`published_at` nulo vs no nulo se lee igual?** | Sí. La nulidad se decide por el **KIND canónico** (`NullValue`), no por texto vacío, así que un `NULL` y una cadena vacía nunca se confunden. Probado en las dos direcciones, incluida la idempotencia de publicar dos veces. | `articles GET no-embedding … published-absent=true`, `articles PUBLISH first published-present=true`, `PUBLISH repeat unchanged=true`, `GET after-publish published-present=true` |
| **Extra: ¿los `?` que quedan son sentencias?** | No: 10 comentarios y 1 mensaje de UI en castellano. La prueba fuerte no es el grep sino que la mitad PostgreSQL de la batería pasa. | El grep de arriba + las 121 líneas |
| **Extra: ¿el modelo de seguridad de `content.go` sobrevivió al cambio?** | Sí. Identificadores solo desde la definición persistida vía `quoteIdentifier`; valores siempre enlazados. Un valor que parece SQL se guarda y se devuelve como dato. | `content POST sql-looking-value status=201 verbatim=true`, `content injection users-table-intact=true count=1` |

---

## Hallazgos que el orquestador debe conocer

### 1. La misma divergencia de colación existe HOY en `internal/auth` (CONTRACT-19) — NO la toqué

`internal/auth` tiene listados ordenados por texto que **no** pasan por un sort en Go:

- `ListUsers` → `ORDER BY email ASC`. Un email contiene `.` y `@`.
- `ListAPIKeys` → `ORDER BY created_at DESC, label ASC`. `label` es texto arbitrario del admin, y
  `created_at` lo escribe la app con `time.RFC3339Nano`, **que recorta los ceros finales** (el
  problema de ancho variable que este contrato resolvió con `canonicalTimestampLayout`).
- `auth_user_role_names` y `auth_role_permission_names` → ordenan por nombre de rol/permiso; los
  **permisos llevan `.` y `_`**, que es exactamente el caso que falló acá.

La batería de CONTRACT-19 pasa porque sus fixtures no producen un par de valores donde la colación y
el orden por bytes discrepen. **Eso no significa que no discrepen en producción.** Es la misma clase
de bug: rompe en producción sin romper ningún test que no lo mire.

**No lo arreglé** porque `internal/auth` está fuera de mi perímetro y la regla es no ampliar el
alcance en silencio. La corrección es mecánica y pequeña (aplicar `sortStrings`/`sortByKeys` de
`internal/server/dual.go` en `ListUsers`, `ListAPIKeys`, `rolesForUser` y `RolePermissions`, y usar
`canonicalTimestampLayout` en `auth.now()`), pero es una decisión del orquestador: probablemente
merece su propio contrato o un anexo a CONTRACT-21.

### 2. El límite de precisión de `vector` es real y no lo arregla ningún código

Ver § "LO PRIMERO QUE SE MIDIÓ". `pgvector` es `float4`. Si se migra con embeddings de más de
precisión simple ya cargados, la verificación por digest de `compat copy`/`cutover` los marcará como
divergentes. Es el precio declarado de `vector(N)` nativo, decidido en CONTRACT-05.

### 3. Cambios de comportamiento observables (ninguno rompe una ruta ni un test)

1. **Formato de los timestamps que `server` escribe.** `created_at`/`updated_at`/`published_at` de
   `articles`, `products` y las tablas dinámicas pasan de `"2026-07-25 18:38:25"` (SQLite
   `CURRENT_TIMESTAMP`) a RFC3339Nano de ancho fijo. Filas **preexistentes** conservan su formato
   viejo; ambos son parseables por compat, y toda lectura por rutina los **normaliza** a RFC3339Nano
   recortado, así que la UI ve un formato consistente. **Consecuencia a tener presente, igual que en
   CONTRACT-19:** en un listado con filas viejas y nuevas, el orden por texto de `created_at` pone
   todas las nuevas antes que todas las viejas (`'T'` > `' '`), que en la práctica coincide con "más
   nuevas primero" pero no está garantizado por la fecha. Si molesta, se normaliza con un UPDATE de
   una línea al desplegar.
2. **Los ids de `articles`, `products`, `terms` y las tablas dinámicas los genera la app**, no el
   `DEFAULT`. El `DEFAULT gen_random_uuid()` sigue declarado y sigue funcionando para escrituras
   externas.
3. **El orden de los listados de términos y permisos ahora lo impone la aplicación.** Para conjuntos
   sin puntuación conflictiva es el mismo de antes; para el resto, ahora está garantizado.
4. **`metadata` corrupta en una tabla dinámica.** Antes, un `metadata` que no fuera JSON válido se
   devolvía como `null` silenciosamente (había un `json.Valid` de guarda). Ahora la familia `json`
   declarada la rechaza al canonicalizar, y la lectura da 500. Es una fila corrupta escrita fuera de
   esta superficie (que no permite escribir `metadata`), y fallar ruidosamente ante un dato corrupto
   es lo correcto — pero es un cambio y hay que saberlo.

---

## ¿Le falta algo a `compat`? — No

Se evaluó durante la implementación. Los candidatos y por qué **ninguno** justifica tocar el paquete:

1. **`CallRoutine` no devuelve `RowsAffected`.** Es el límite que define este contrato, no un hueco:
   el contrato mismo lo declara como el criterio que decide rutina vs SQL crudo, y SQL crudo con
   `Placeholder` resuelve el caso sin perder nada.
2. **El `ORDER BY` de una rutina no acepta colación.** Es lo que más se pareció a un hueco. **No lo
   es**: la colación es una propiedad del despliegue de PostgreSQL, no de la librería, y agregar
   `COLLATE` al modelo canónico obligaría a compat a elegir una semántica de ordenamiento por el
   consumidor. El consumidor lo resuelve mejor ordenando en Go, que es determinista y no depende de
   cómo esté configurada la base. Lo dejo anotado por si un contrato futuro quiere reabrirlo.
3. **El enlace de `vector` necesita un cast.** **Medido: NO lo necesita** (PROBE-1). No hay nada que
   agregar.
4. **`RETURNING`.** Resuelto en el consumidor generando el UUID, como manda el diseño de compat.

**`sqlite-postgres-compat` no se modificó.** `git status` en ese repo queda exactamente como estaba
al empezar (solo el `experiments/vector/vector-exp.exe` sin trackear que ya estaba).

---

## Cómo se corre la batería (también en `docs/OPERATIONS.md`)

```powershell
$env:COMPAT_POSTGRES_DSN = "postgres://usuario:***@host:puerto/db?sslmode=disable"
go test -tags dualengine -run TestDualEngineServer -count=1 -v ./internal/server
go test -tags dualengine -run TestDualEngineVectorPrecision -count=1 -v ./internal/server
```

Requiere `pgvector` en el destino. Crea y destruye su propio schema PostgreSQL por corrida; no deja
nada atrás.

---

## Archivos tocados

**Nuevos**

- `internal/schema/server_dual.go` — vistas, rutinas y el generador por tipo dinámico (T1)
- `internal/server/dual.go` — plomería neutral del paquete (placeholders, uuid, instante canónico,
  accesores de fila, orden byte a byte)
- `internal/server/dualengine_contract20_test.go` — batería dual-motor sobre el mux HTTP (tag
  `dualengine`) + el test del límite de precisión del vector
- `docs/reports/CONTRACT-20-REPORT.md` — este informe

**Modificados**

- `internal/schema/schema.go` — `Build()` compone las vistas y rutinas de los dos contratos
- `internal/schema/dynamic.go` — `BuildWith` genera las rutinas de lectura por tipo
- `internal/server/articles.go`, `terms.go`, `content.go`, `products.go`, `authz.go`,
  `ui_apikeys.go`, `ui_roles.go`, `ui_users.go` — T2, las 45 sentencias
- `internal/server/server.go` — `store` (rename) + `registerRoutes` extraído de `NewMux`
- `internal/server/ui.go` — solo el rename del campo
- `internal/server/guard_contract16_internal_test.go` — 1 línea de andamiaje (el rename)
- `docs/OPERATIONS.md` — la batería y la nota de colación

**NO tocados**

- `sqlite-postgres-compat` (todo el repo)
- `internal/store` (ni una línea)
- `internal/auth` (ni una línea — ver § Hallazgos 1)
- `store.Open` y cualquier elección de motor (CONTRACT-21)
- `server.Deps` y el contrato público de las rutas HTTP

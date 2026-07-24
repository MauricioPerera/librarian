# CONTRACT-14 — CRUD JSON genérico sobre tipos de contenido dinámicos

Estado: **COMPLETO**. Segundo contrato de la fase 3 (`DEFINITION-CPT-DINAMICOS.md`), sobre
`CONTRACT-13` (`1982648`).

Archivos tocados (todos dentro de `librarian`, ninguno fuera):

| Archivo | Cambio |
|---|---|
| `internal/server/content.go` | **NUEVO** — la capa CRUD genérica completa |
| `internal/server/server.go` | +5 rutas en `NewMux` (nada existente modificado) |
| `internal/server/server_content_test.go` | **NUEVO** — 14 tests de aceptación |
| `docs/reports/CONTRACT-14-REPORT.md` | este informe |

No se tocó `internal/store/store.go` (`EnsureSchema`), ni `internal/schema/*`, ni
`internal/store/contenttypes.go`, ni ningún handler de contratos 01-13. Sin dependencias nuevas
(`go.mod` intacto). **Ningún permiso nuevo**: se reusan los `content.*` existentes.

---

## Superficie entregada

```
GET    /content/{type}        listar   (solo identidad válida; ?limit=&offset=)
GET    /content/{type}/{id}   detalle  (solo identidad válida)
POST   /content/{type}        crear    (content.create)
PUT    /content/{type}/{id}   actualizar (content.update)
DELETE /content/{type}/{id}   borrar   (content.delete)
```

El prefijo `/content/` es dedicado, para que el espacio dinámico no colisione con rutas estáticas
existentes ni futuras. Un test explícito confirma que `/content-types` (CONTRACT-13) **no** quedó
ensombrecido por `/content/{type}`, que era el riesgo real de compartir el prefijo textual
`"/content"`.

---

## T1 — Lectura genérica

`handleListContent` / `handleGetContent` + `listContentRows` / `fetchContentRow` / `scanRow`.

- `{type}` se resuelve **siempre** contra una definición persistida vía
  `store.FetchContentType` (reusada, no se escribió otra forma de leer definiciones). Si no hay
  definición → **404**, y no se construye ninguna query contra ninguna tabla dinámica.
- La lista de columnas del `SELECT` se arma desde la definición: las 5 comunes (`id`,
  `author_id`, `created_at`, `updated_at`, `metadata`) más una por cada `FieldDefinition`, en
  orden de declaración, para que los destinos del `Scan` calcen posicionalmente.
- Paginado `?limit=&offset=` con el mismo helper permisivo `queryIntDefault` de articles/products
  (un valor basura cae al default, no da error).
- Un tipo con cero filas devuelve `[]`, nunca `null` ni 404 (`out := make([]map[string]any, 0)`).
- Un `{id}` malformado (no-UUID) es 404, nunca 500 — el id va como parámetro `?`, así que
  simplemente no matchea nada.

### El núcleo técnico: escanear columnas cuyo tipo se conoce solo en runtime

Éste era el punto pedido para documentar. Cómo se resolvió:

1. Las 5 columnas comunes tienen tipo Go fijo y conocido en compilación (`string`,
   `sql.NullString` para `metadata`) — se escanean directo.
2. La **cola dinámica** se escanea en un `[]any` de `*any`. `database/sql` deposita ahí el valor
   nativo del driver, sin conversión: `int64` para `INTEGER`, `string` para `TEXT`, `nil` para
   `NULL`. Esto evita el problema de `sql.NullString`, que convertiría un `int64` a texto y
   perdería para siempre la distinción entero/texto.
3. `jsonValue(field, raw)` convierte cada valor crudo al **tipo JSON que exige su `FieldType`
   declarado**. La definición es la fuente de la verdad del tipo, no el driver.
4. La fila se ensambla como `map[string]any` y `encoding/json` la serializa. Eso es lo que permite
   que **un solo camino de código** produzca un objeto con forma distinta por cada tipo.

Mapeo aplicado (con el almacenamiento real de compat en SQLite entre paréntesis):

| `FieldType` | Almacenamiento | Tipo JSON devuelto |
|---|---|---|
| `text` (TEXT) | texto | `string` |
| `integer` (INTEGER) | `int64` | **número** |
| `boolean` (INTEGER 0/1) | `int64` | **booleano** (`n != 0`) |
| `date` (TEXT) | `YYYY-MM-DD` | `string` |
| `decimal` (TEXT canónico) | texto | `string` — ver trade-off |
| `NULL` (cualquiera) | `nil` | `null` |

`driverText` / `driverInt` normalizan las representaciones alternativas del driver (`[]byte`,
`float64`, `bool`, texto) defensivamente, para que una fila bien formada nunca se convierta en un
500 si el driver cambia de representación.

---

## T2 — Escritura genérica

`handleCreateContent` / `handleUpdateContent` / `handleDeleteContent` + `bindValues` / `bindValue`.

- El body se decodifica como `map[string]json.RawMessage` (objeto JSON plano). Un array, un escalar
  o basura → 400.
- **Campo inexistente en el tipo → 400** con mensaje claro (`unknown field "x" for content type
  "reviews"`). No se ignora silenciosamente: descartar datos que el cliente creía estar guardando
  es el peor de los dos fallos.
- **Tipo JSON incorrecto para el `FieldType` declarado → 400** con mensaje que nombra el campo y
  el tipo esperado. Nunca 500, nunca un valor corrupto guardado (probado con 25 cuerpos
  rechazados y `count(*) = 0` después).
- Autoría: el `author_id` sale de la identidad autenticada. Una identidad de **API key → 403** con
  mensaje explícito (`creating content requires a user identity (API keys have no author)`), nunca
  un autor nulo — misma decisión que articles/products, porque `DynamicTable` inyecta `author_id
  NOT NULL` con FK a `users`.
- `DELETE` devuelve 204; id inexistente → 404 (vía `RowsAffected() == 0`).

Reglas de validación por tipo:

| `FieldType` | Acepta | Rechaza (400) |
|---|---|---|
| `text` | string JSON | número, booleano, objeto, array |
| `integer` | número JSON entero | `"5"`, `5.5`, `true`, notación exponencial |
| `decimal` | número **o** string JSON decimal | booleano, objeto, `"cheap"` |
| `boolean` | `true`/`false` | `1`, `"true"` |
| `date` | string `YYYY-MM-DD` **real** | número, `"yesterday"`, `"2026-13-45"`, timestamp RFC3339 |
| cualquiera | `null` o ausente | — (ver decisión de diseño) |

---

## La pieza de seguridad central

**Identificadores se interpolan; valores se parametrizan. Nunca al revés.**

- Todo identificador que llega a una query viene de una definición **leída de la base de datos**
  (`store.FetchContentType` → `LoadContentTypeDefinitions`, que re-valida cada fila con
  `d.Validate()` → `schema.ValidateIdentifier`). El segmento `{type}` del path se usa **solo como
  valor de comparación** para encontrar esa definición; nunca se interpola.
- Los nombres de campo vienen de `def.Fields`, jamás del body. Una clave del body se **compara
  contra** la definición y después se descarta — el string del cliente nunca llega a la query.
- `quoteIdentifier` es el **único** lugar del archivo donde un nombre dinámico se vuelve texto SQL,
  y re-corre `schema.ValidateIdentifier` antes de citar. Es un guardia fail-closed: las filas
  persistidas ya se validaron al entrar y otra vez al salir, así que un fallo acá sería un bug
  interno, no un error del cliente. Como el alfabeto permitido es `[a-z][a-z0-9_]*`, no hay comilla,
  punto y coma, espacio ni unicode posible: las comillas dobles son redundancia, no la protección.
- Las 5 columnas comunes son **constantes de compilación** de `content.go` (`colID`, `colAuthorID`,
  …), nunca derivadas de una request.
- Todo valor va como `?`. Un string que es un payload de inyección completo se guarda y se devuelve
  byte a byte, como dato.

---

## Decisiones de diseño (las tres que el contrato dejó abiertas)

### 1. Campos faltantes → NULL, idéntico en crear y actualizar

**Qué**: un campo ausente del body, o explícitamente `null`, se guarda como `NULL`. Ningún campo es
obligatorio.

**Por qué**: el modelo de definiciones de CONTRACT-13 **no tiene noción de "requerido"** (un campo
es solo nombre + tipo), y `schema.DynamicTable` declara toda columna dinámica `Nullable: true`
exactamente por eso, con un comentario que lo justifica. Inventar acá una regla de obligatoriedad
sería una segunda fuente de verdad, invisible, que la API de definiciones no puede expresar y que
el admin que creó el tipo nunca aceptó. Además, v1 no puede `ALTER TABLE`, así que un campo
declarado hoy no se puede volver opcional mañana: la elección honesta es nullable.

**Consecuencia (documentada y testeada)**: `PUT` es un **reemplazo completo** de los campos propios
de la fila — un campo omitido se resetea a `NULL`. Es exactamente la semántica que ya tienen
`PUT /articles/{id}` y `PUT /products/{id}` (setean todas sus columnas propias desde el body). Una
actualización parcial sería `PATCH`, y en este proyecto no hay `PATCH`.

### 2. Un body con el nombre de una columna común (`id`, `author_id`, `created_at`, …) → 400

**Qué**: `{"id": "..."}`, `{"author_id": "..."}`, `{"created_at": "..."}`, `{"updated_at": "..."}`
y `{"metadata": {...}}` son todos **400 "unknown field"**. No pisan nada.

**Por qué funciona sin ningún caso especial**: los cinco nombres están en
`schema.ReservedNames()` (derivados llamando a `ContentType("x", nil)`, no hardcodeados), así que
CONTRACT-13 **rechaza con 400** cualquier definición que declare un campo con esos nombres. Por lo
tanto ninguna clave así puede matchear un campo de ningún tipo, y cae en el mismo 400 de campo
desconocido que cualquier typo. La invariante se sostiene sola: no hay camino de código en el que un
valor de la request llegue a una columna común. `id` y los timestamps salen de los defaults de
columna, `author_id` de la identidad autenticada, y `metadata` no es escribible por esta superficie.

### 3. `decimal` se devuelve como **string** JSON, no como número

**Por qué**: compat mapea `DecimalType` a `TEXT` en SQLite **precisamente porque** IEEE-754 no puede
sostener un decimal de precisión arbitraria. Emitir un número JSON reintroduciría el redondeo que la
decisión de almacenamiento existe para evitar. Es además la misma elección que ya hace
`products.price` (CONTRACT-11), así que las dos superficies concuerdan. En escritura se aceptan
**ambos**, número JSON (`9.99`) y string JSON (`"9.99"`), reusando el helper `canonicalDecimal` de
products — un helper, una garantía.

Evidencia de que la decisión sirve: `123456789012345678901234567890.123456789` sobrevive
byte a byte (ver salida de `TestDynamicContentDecimalPrecision` abajo).

---

## Trade-offs asumidos

- **`PUT` reemplaza todo** (no hay actualización parcial). Consistente con el resto del proyecto,
  pero significa que un cliente que solo quiere cambiar un campo debe reenviar los demás. La
  alternativa (`PATCH`) es una superficie nueva que este contrato no pide.
- **Sin filtros ni ordenamiento por campo dinámico** en el listado (solo `created_at DESC` +
  paginado). Un `?where=` genérico sería el próximo lugar donde entra entrada del usuario a una
  query, y merece su propio contrato con su propio red-team.
- **`metadata` es de solo lectura** por esta superficie. La columna existe y se devuelve, pero
  escribirla no está en el contrato.
- **Sin filtro por autor**: cualquier identidad con `content.update`/`content.delete` puede tocar
  filas de cualquier autor. Es exactamente el modelo de articles/products; cambiarlo sería un
  cambio de política que aplica a toda la app, no a esta superficie.
- **`resolveType` hace una lectura completa del registro por request** (`FetchContentType` carga
  todas las definiciones y filtra en Go). Se eligió reusar la función existente antes que escribir
  una segunda forma de leer definiciones, como manda el contrato. Es una tabla de administración
  con decenas de filas como mucho; si algún día molesta, el lugar de arreglarlo es
  `internal/store/contenttypes.go`, para todos los llamadores a la vez.
- **El gate de permiso corre antes de resolver el tipo**: un `POST /content/inexistente` sin
  `content.create` da 403, no 404. Es el comportamiento estándar del middleware del proyecto y el
  que no filtra qué tipos existen a un caller sin permisos.

---

## Verificación — salida REAL

### `go build` + `go vet`

```
$ go build ./... && go vet ./... && echo BUILD_VET_OK
BUILD_VET_OK
```

### Suite completa, dos veces

```
$ go test ./... -count=1        # RUN 1
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.363s
ok  	github.com/MauricioPerera/librarian/internal/auth	3.735s
ok  	github.com/MauricioPerera/librarian/internal/config	0.673s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.362s
ok  	github.com/MauricioPerera/librarian/internal/server	23.667s
ok  	github.com/MauricioPerera/librarian/internal/store	2.463s

$ go test ./... -count=1        # RUN 2
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.332s
ok  	github.com/MauricioPerera/librarian/internal/auth	3.773s
ok  	github.com/MauricioPerera/librarian/internal/config	0.668s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.361s
ok  	github.com/MauricioPerera/librarian/internal/server	23.703s
ok  	github.com/MauricioPerera/librarian/internal/store	2.510s
```

### Criterio T1 — listar y leer por id con los tipos JSON correctos

`TestDynamicContentRoundTrip`. El tipo `reviews` se crea en el mismo test vía la API real de
CONTRACT-13 (`POST /content-types`) con los 5 tipos de campo.

```
    T2 CREATE OK: map[author_id:ba29cda2-... created_at:2026-07-24 23:35:24 headline:A great read
      id:653ef0ef-7ec3-4dea-9f17-b5701e4c7a5f metadata:<nil> price_paid:19.99 read_on:2026-07-24
      score:5 updated_at:2026-07-24 23:35:24 verified:true]
    T1 LIST OK: type=reviews items=[map[author_id:ba29cda2-... headline:A great read
      id:653ef0ef-... metadata:<nil> price_paid:19.99 read_on:2026-07-24 score:5 verified:true]]
    T1 DETAIL OK: map[... headline:A great read score:5 verified:true price_paid:19.99
      read_on:2026-07-24 ...]
    T2 UPDATE OK: map[... headline:Actually mediocre price_paid:0.01 read_on:2026-07-25 score:2
      verified:false ...]
    T2 DELETE OK: 204, then 404 on read, empty list
--- PASS: TestDynamicContentRoundTrip (0.18s)
```

El test **no** compara strings: afirma el **tipo Go** de cada valor decodificado, que es la prueba
directa del tipo JSON (`float64` ⇔ número JSON, `bool` ⇔ booleano JSON, `string` ⇔ string JSON):

```go
if v, ok := row["score"].(float64); !ok || v != score { t.Fatalf(...) }     // entero → número
if v, ok := row["verified"].(bool); !ok || v != verified { t.Fatalf(...) }  // booleano → booleano
```

Forma JSON exacta de una fila (`TestDynamicContentJSONShapeIsStable`), con el conjunto de claves
cerrado y verificado en ambos sentidos:

```
    SHAPE OK: {"author_id":"625d0084-...","created_at":"2026-07-24 23:35:36","headline":"h",
      "id":"5f5c4909-...","metadata":null,"price_paid":"1.5","read_on":"2026-02-03","score":1,
      "updated_at":"2026-07-24 23:35:36","verified":false}
```

### Criterio T2 — crear/actualizar/borrar; campo inexistente → 400; tipo incorrecto → 400

`TestDynamicContentFieldValidation` — 25 cuerpos rechazados, cada uno probado **en crear y en
actualizar** (misma regla en ambos):

```
    400 create [unknown field                     ] -> unknown field "nope" for content type "reviews"
    400 create [common column id                  ] -> unknown field "id" for content type "reviews"
    400 create [common column author_id           ] -> unknown field "author_id" for content type "reviews"
    400 create [common column created_at          ] -> unknown field "created_at" for content type "reviews"
    400 create [common column updated_at          ] -> unknown field "updated_at" for content type "reviews"
    400 create [common column metadata            ] -> unknown field "metadata" for content type "reviews"
    400 create [hostile field name                ] -> unknown field "sc\"ore" for content type "reviews"
    400 create [injecting field name              ] -> unknown field "score = 1, headline = 'pwned'" for content type "reviews"
    400 create [semicolon field name              ] -> unknown field "score; DROP TABLE users" for content type "reviews"
    400 create [text given a number               ] -> field "headline" must be a JSON string (declared type text)
    400 create [text given a boolean              ] -> field "headline" must be a JSON string (declared type text)
    400 create [text given an object              ] -> field "headline" must be a JSON string (declared type text)
    400 create [integer given a string            ] -> field "score" must be a whole JSON number (declared type integer)
    400 create [integer given a decimal           ] -> field "score" must be a whole JSON number (declared type integer)
    400 create [integer given a boolean           ] -> field "score" must be a whole JSON number (declared type integer)
    400 create [boolean given a number            ] -> field "verified" must be a JSON boolean (declared type boolean)
    400 create [boolean given a string            ] -> field "verified" must be a JSON boolean (declared type boolean)
    400 create [decimal given a boolean           ] -> field "price_paid" must be a decimal number, as a JSON number or string (declared type decimal)
    400 create [decimal given a non-numeric string] -> field "price_paid" must be a decimal number, as a JSON number or string (declared type decimal)
    400 create [decimal given an object           ] -> field "price_paid" must be a decimal number, as a JSON number or string (declared type decimal)
    400 create [date given a number               ] -> field "read_on" must be a JSON string in YYYY-MM-DD form (declared type date)
    400 create [date given a non-date string      ] -> field "read_on" must be a date in YYYY-MM-DD form (declared type date)
    400 create [date given an impossible date     ] -> field "read_on" must be a date in YYYY-MM-DD form (declared type date)
    400 create [date given a timestamp            ] -> field "read_on" must be a date in YYYY-MM-DD form (declared type date)
    VALIDATION OK: 25 rejected bodies, 0 rows stored, table shape unchanged
--- PASS: TestDynamicContentFieldValidation (0.20s)
```

Cero 500 y **cero filas guardadas** tras los 50 requests rechazados (25 × crear + 25 × actualizar),
y la forma de la tabla `reviews` verificada sin cambios con `PRAGMA table_info` directo.

Complementos de T2:

```
    NULL SEMANTICS OK: omitted == explicit null == NULL, identical on create and update
    SERVER-OWNED OK: author_id=755fa7e2-... taken from the JWT identity; id/timestamps from column defaults
    ZERO-FIELD OK: create/update/delete work, any body key is a 400
    PAGING OK: limit/offset honoured, garbage falls back to the default
    EMPTY/BAD-ID OK: [] for an empty type; malformed ids → 404 on GET/PUT/DELETE; users intact
```

### Criterio T3 — round-trip completo

Cubierto arriba por `TestDynamicContentRoundTrip`: crear con los 5 tipos → listar → leer por id →
actualizar → borrar → 404 al releer, con valores reales de vuelta en cada paso.

### Criterio T3 — aislamiento entre dos tipos dinámicos

`TestDynamicContentIsolationBetweenTypes`: dos tipos (`reviews` con 5 campos, `recipes` con 2),
3 y 2 filas respectivamente.

```
    ISOLATION OK: reviews=3 recipes=2, no field leakage, cross-type id → 404 and no row touched
```

Además de los conteos, cada fila se inspecciona para confirmar que **no lleva campos del otro
tipo**, y un id de `reviews` consultado/borrado bajo `/content/recipes/{id}` da 404 sin tocar
ninguna fila (`count(*)` de `reviews` sigue en 3 después del intento de borrado cruzado).

### Criterio T3 — SEGURIDAD (lo más importante)

**`{type}` inexistente y `{type}` hostil** — `TestDynamicContentHostileTypeNames`. Cada nombre se
prueba contra **las cinco rutas** (GET lista, GET detalle, POST, PUT, DELETE) = 100 requests:

```
    404/400 [unknown                 ] "nope" on all five routes
    404/400 [code table users        ] "users" on all five routes
    404/400 [code table articles     ] "articles" on all five routes
    404/400 [code table products     ] "products" on all five routes
    404/400 [registry table          ] "content_types" on all five routes
    404/400 [registry fields table   ] "content_type_fields" on all five routes
    404/400 [api keys table          ] "api_keys" on all five routes
    404/400 [compat internal         ] "__compat_schema" on all five routes
    404/400 [double quote            ] "re\"views" on all five routes
    404/400 [quote-escape injection  ] "reviews\" ; DROP TABLE \"users" on all five routes
    404/400 [semicolon drop          ] "reviews; DROP TABLE users" on all five routes
    404/400 [drop users              ] "users; DROP TABLE users; --" on all five routes
    404/400 [union select            ] "reviews UNION SELECT * FROM users" on all five routes
    404/400 [comment                 ] "reviews--" on all five routes
    404/400 [backtick                ] "`reviews`" on all five routes
    404/400 [single quote            ] "reviews' OR '1'='1" on all five routes
    404/400 [uppercase               ] "Reviews" on all five routes
    404/400 [unicode                 ] "reseñas" on all five routes
    404/400 [space                   ] "my reviews" on all five routes
    404/400 [leading digit           ] "1reviews" on all five routes
    SYSTEM INTACT: 16 tables (unchanged), users=1 permissions=8 roles=4 content_types=1 (all unchanged)
--- PASS: TestDynamicContentHostileTypeNames (0.23s)
```

La prueba de que **ninguna query se ejecutó** es la consulta directa posterior: existencia
confirmada de `users`, `roles`, `permissions`, `role_permissions`, `api_keys`, `articles`,
`products`, `terms`, `content_types`, `content_type_fields` y `reviews`; conteo total de tablas
idéntico (16); y conteos de filas de las tablas del sistema idénticos antes/después. Nótese que
`users`, `articles`, `products` y `content_types` **existen** como tablas reales y aun así dan 404:
la ruta no puede llegar a ellas porque no hay definición dinámica con esos nombres. El tipo legítimo
`reviews` sigue respondiendo 200 después de toda la batería.

**Campo con nombre hostil en el body → 400**: incluido en la tabla de T2 arriba
(`sc"ore`, `score = 1, headline = 'pwned'`, `score; DROP TABLE users`) — todos 400.

**Valores hostiles guardados y devueltos VERBATIM** — `TestDynamicContentHostileValuesAreStoredVerbatim`:

```
    VERBATIM OK ";DROP TABLE users;--"
    VERBATIM OK "' OR '1'='1"
    VERBATIM OK "\"; DROP TABLE users; --"
    VERBATIM OK "x'); DELETE FROM users WHERE ('1'='1"
    VERBATIM OK "Robert'); DROP TABLE students;--"
    VERBATIM OK "100% \"quoted\" and 'single' \\backslash\\"
    VERBATIM OK "UNION SELECT password_hash FROM users"
    VERBATIM OK "line1\nline2\ttab"
    VERBATIM OK "—unicode— ñ 中文 🙂"
    PARAMETERIZED VALUES PROVEN: 9 injection payloads stored as data, users=1 unchanged, 16 tables unchanged
--- PASS: TestDynamicContentHostileValuesAreStoredVerbatim (0.21s)
```

Cada payload se verifica **tres veces**: en la respuesta del `POST`, en el `GET` por id, y con una
**consulta SQL directa** a la columna (`SELECT headline FROM reviews WHERE id = ?`) — esto último es
lo que prueba que se guardó verbatim y no que el handler lo devolvió de memoria. Un `PUT` con
payload hostil también es inerte. `users` intacta con el mismo número de filas, 16 tablas sin
cambios.

### Criterio T3 — gateo por permiso

`TestDynamicContentPermissionGating`. La identidad de prueba tiene **todos los demás permisos**
(`content_types.manage`, `terms.manage`, `users.manage`, `roles.manage`, `content.publish`) y
ninguno de los tres `content.*` que este contrato usa — así se prueba que el portón es el permiso
específico, no "cualquier grant de admin":

```
    403 [POST   /content/reviews                        ] missing content.create
    403 [PUT    /content/reviews/360c19e1-...           ] missing content.update
    403 [DELETE /content/reviews/360c19e1-...           ] missing content.delete
    GATING OK: 403 per-route without the matching content.* grant, reads open to any identity,
      401 unauthenticated, 1 row untouched
```

Las lecturas (lista y detalle) sí funcionan para esa misma identidad — leer solo exige identidad
válida. Sin autenticación: 401 en las cinco rutas. La fila sobrevivió intacta a todos los intentos
rechazados.

**Crear con API key → 403** — `TestDynamicContentAPIKeyCannotCreate` (la key tiene `content.create`;
el rechazo es por autoría, no por permiso):

```
    API-KEY OK: 403 "creating content requires a user identity (API keys have no author)",
      0 rows inserted, reads still allowed
```

### Red-team: tipo creado en un proceso, consultado desde otra conexión/reinicio

`TestDynamicContentSurvivesRestart` — no asumido, **confirmado**: proceso 1 abre el archivo, define
el tipo, carga una fila y **cierra la conexión y el servidor**; proceso 2 abre el mismo archivo con
una conexión, un mux y un set de handlers nuevos, emite un JWT fresco contra el usuario persistido, y
lee y escribe sin problema, con los tipos JSON correctos (`score` número, `verified` booleano).

```
    RESTART OK: the definition was read back from the database by a new process; read and write both work
```

### Criterio final — todo lo de contratos anteriores sigue igual

`TestPreviousContractsUnaffectedByGenericCRUD`. Corre **con un tipo dinámico definido y con
contenido cargado**, para que la superficie nueva esté plenamente en uso:

- CONTRACT-01: `GET /health` → 200 `{"status":"ok"}`
- CONTRACT-02: `POST /auth/login` → 200 con token; `GET /whoami` → 200 `auth=jwt`
- CONTRACT-03/04: `POST /articles` 201, `GET /articles` 200 con el sobre `{"articles":[...]}`
  intacto, `GET /articles/{id}` 200, `PUT /articles/{id}` 200, `POST /articles/{id}/publish` 200
- CONTRACT-11: `POST /products` 201, `GET /products` 200 con el sobre `{"products":[...]}` intacto,
  `GET /products/{id}` 200
- CONTRACT-12: `POST /terms` 201, `GET /terms` 200, `PUT /articles/{id}/terms` 200,
  `PUT /products/{id}/terms` 200
- CONTRACT-13: `GET /content-types` 200, `GET /content-types/reviews` 200 — y verificado
  explícitamente que `/content/{type}` **no** lo ensombreció
- CONTRACT-06+ (UI):

```
    UI /ui/login              -> 200
    UI /ui/articles           -> 200
    UI /ui/products           -> 200
    UI /ui/users              -> 200
    UI /ui/roles              -> 200
    UI /ui/api-keys           -> 200
    UI /ui/terms              -> 200
    UI /ui/content-types      -> 200
    PREVIOUS CONTRACTS OK: health, login, whoami, articles CRUD+publish, products CRUD,
      terms+assignment, content-types, and every UI route all unchanged
--- PASS: TestPreviousContractsUnaffectedByGenericCRUD (0.26s)
```

A esto se suma que **la suite completa de los 13 contratos anteriores corre verde dos veces**
(salida arriba), incluidos `export_fixture_test.go`, `server_ui_*_test.go` y
`server_contenttypes_test.go` — ninguno modificado.

### Precisión decimal (justificación de la decisión de diseño 3)

```
    DECIMAL OK 123456789012345678901234567890.123456789      -> "123456789012345678901234567890.123456789"
    DECIMAL OK 0.1                                           -> "0.1"
    DECIMAL OK -3.5                                          -> "-3.5"
    DECIMAL OK 7                                             -> "7"
    DECIMAL OK -0.000000001                                  -> "-0.000000001"
```

---

## Checklist de criterios de aceptación

- [x] `go build ./...` y `go vet ./...` limpios
- [x] `go test ./... -count=1` verde, corrido dos veces
- [x] T1: listar y leer por id sobre un tipo dinámico real, con tipos JSON correctos por campo
- [x] T2: crear/actualizar/borrar reales; campo inexistente → 400; tipo de valor incorrecto → 400
- [x] T3: round-trip completo
- [x] T3: aislamiento entre dos tipos
- [x] T3: batería de seguridad (tipo hostil, campo hostil, valores hostiles verbatim, tablas del
      sistema intactas)
- [x] T3: gateo por permiso (`content.create`/`update`/`delete`) + API key → 403
- [x] T3: contratos anteriores sin cambios
- [x] Red-team: columnas comunes no pisables; `{id}` malformado → 404; tipo usable tras reinicio;
      lista vacía → `[]`
- [x] Restricciones: solo `librarian`; sin dependencias nuevas; sin permisos nuevos; sin commit;
      `internal/store/store.go` intacto; contrato público 01-13 sin cambios

## Pendiente para el orquestador

Verificación HTTP real contra el binario y el DEPLOY (protocolo de copia-real-de-producción), más
el commit. El árbol queda con los cambios **sin commitear**.

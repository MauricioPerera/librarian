# CONTRACT-11 — Tipo de contenido `products` (esquema + API JSON + UI + navegación)

Segundo tipo de contenido de primera clase construido con el MISMO helper
`schema.ContentType()` que `articles`, probando que el patrón generaliza a
columnas propias más allá de `title`/`body` (`price` decimal, `sku` único).
CRUD simple, sin workflow de publicación (a diferencia de `articles`).

Estado: **COMPLETO**. `go build`, `go vet`, `gofmt` limpios. Suite completa
verde dos veces. Todos los contratos anteriores (articles/users/roles/api-keys,
JSON y UI) intactos.

## Qué se implementó por tarea

### T1 — Esquema (`internal/schema/schema.go`, `schema_test.go`)

- **Helper `decimalColumn(name, nullable)`** nuevo, siguiendo exactamente la
  forma de `textColumn`/`jsonColumn`. Usa `compat.Type{Family: compat.DecimalType}`
  **sin argumentos** de precisión/escala.
- **`productsTable()`** construye el tipo `products` vía `ContentType("products",
  [...])` con columnas propias `title` (text), `body` (text, descripción),
  `price` (decimal), `sku` (text), y luego **añade** la constraint `unique("sku")`
  a nivel de tabla.
- Agregada a `Build()` junto a `articles`. `products` **no** tiene `published_at`.
- Firma de `ContentType` **NO tocada**. No se agregó ningún permiso al catálogo.

DDL real emitido (test `TestProductsTable`, ambos motores):

```
SQLite:   CREATE TABLE "products" (... "price" TEXT NOT NULL, "sku" TEXT NOT NULL,
          ... PRIMARY KEY ("id"), FOREIGN KEY ("author_id") REFERENCES "users" ("id")
          ON DELETE CASCADE, UNIQUE ("sku"))
Postgres: CREATE TABLE "products" (... "price" NUMERIC NOT NULL, "sku" TEXT NOT NULL,
          ... UNIQUE ("sku"))
```

`price` → `TEXT` en SQLite (carrier canónico), `NUMERIC` sin precisión en
Postgres. `UNIQUE ("sku")` presente en ambos. `Schema.Validate()` y `CompileDDL`
(ambos motores) limpios.

### T2 — API JSON CRUD (`internal/server/products.go`, `server.go`, `server_products_test.go`)

Espejo exacto de `articles.go`, namespace `/products`:

- `POST /products` (content.create), `GET /products` (auth), `GET /products/{id}`
  (auth), `PUT /products/{id}` (content.update), `DELETE /products/{id}`
  (content.delete). **Sin ruta publish** (no hay `published_at`).
- Gateo con los MISMOS permisos genéricos `content.*` que `articles`.
- Campos requeridos: `title`, `body`, `price`, `sku`. 400 claro si falta alguno.
- Identidad API-key rechazada en create (403, sin autor humano), igual que
  `articles`.
- Helpers de acceso a datos compartidos (`insertProduct`, `listProducts`,
  `fetchProduct`, `productExists`, `updateProductFields`, `deleteProductByID`,
  `scanProduct`) reusados por la UI — la UI no reimplementa SQL.
- 404 (nunca 500/panic) para id inexistente o malformado; SQL parametrizado.

### T3 — UI (`internal/server/ui_products.go`, `ui.go`, `ui_nav.go`, `templates/products_*.html`)

Espejo de `ui_articles.go`, namespace `/admin/products`:

- `GET /admin/products` (listar), `GET /admin/products/new` (form), `POST
  /admin/products` (crear), `GET /admin/products/{id}/edit`, `PUT
  /admin/products/{id}` (hx-put), `DELETE /admin/products/{id}` (hx-delete).
- Read routes con `requireSession`; write routes con `requireSessionPermission`
  y los mismos `content.*`. **Sin botón publicar.**
- Reusa `renderNotFound`/`renderForbidden` y el patrón de template-set por página.
- **"Productos" agregado a la navegación única** (`ui_nav.go`) con submenú "Todos
  los productos" / "Añadir nuevo". `layout.html` ya itera `.Nav`, no requirió
  cambios de template.
- Templates: `products_list.html`, `products_row.html` (Título/Precio/SKU/Creado
  + Editar/Borrar, sin Publicar), `products_new.html`, `products_edit.html`.

### T4 — Verificación

Ver "Salida real" abajo. Suite completa verde 2×; 53 tests de contratos
anteriores (articles/users/roles/api-keys, JSON+UI) sin fallos.

## Decisiones de diseño (con su porqué)

1. **`decimalColumn` sin precisión/escala.** `compat.DecimalType` con 0
   argumentos compila a `NUMERIC` (precisión arbitraria) en Postgres y a `TEXT`
   en SQLite. Se guarda el **texto canónico** del decimal, nunca un `float64`:
   SQLite REAL es IEEE-754 y no preserva decimales de precisión arbitraria
   byte-a-byte (rompería el invariante de exportabilidad). El contrato no pidió
   un tipo money acotado, así que NUMERIC sin límite es el default cross-engine
   más seguro.

2. **`unique("sku")` añadida tras `ContentType`.** `ContentType` toma sólo
   nombre + columnas propias y su firma es fija (no se toca). Una UNIQUE de tabla
   se agrega extendiendo `table.Constraints` después de construirla — igual que
   `users`/`api_keys` declaran sus propias uniques. Así el rechazo de SKU
   duplicado es **garantía de esquema**, no de aplicación: una verificación
   app-level puede perder una carrera concurrente; la constraint no.

3. **Traducción del error UNIQUE(sku) → 400.** Los helpers `insertProduct`/
   `updateProductFields` detectan la violación con `isUniqueSKUViolation(err)`,
   que hace `strings.Contains(err.Error(), "UNIQUE constraint failed")` **y**
   `"sku"`. Se eligió match por string en vez de importar el tipo de error de
   `modernc.org/sqlite` para **no agregar dependencias** ni acoplar el server al
   driver; SQLite emite siempre esa frase exacta. Al detectarla, el helper
   devuelve el sentinel `errDuplicateSKU`; el handler lo mapea a **400 con
   mensaje legible** ("a product with this sku already exists" en JSON, "Ya
   existe un producto con ese SKU." en la UI). El texto crudo de SQL nunca llega
   al cliente.

4. **`price` como `json.RawMessage` en el body.** Acepta tanto número JSON
   (`9.99`) como string (`"9.99"`), y preserva el texto exacto del token (sin
   pasar por `float64`, evitando pérdida de precisión). Se valida con
   `validateDecimalText`: signo opcional, dígitos, un solo punto; **rechaza**
   notación exponencial, símbolos de moneda, separadores de miles y basura. Un
   `price` no numérico → **400 claro**, nunca 500 ni valor corrupto guardado.

5. **Estructura de las páginas UI.** Idéntica a `articles` para consistencia:
   lista con tabla (Título/Precio/SKU/Creado/Acciones), form de creación por
   POST plano → 303 a la lista, form de edición por `hx-put` → `HX-Redirect`,
   borrado por `hx-delete` con swap `outerHTML`. La única diferencia intencional
   es la ausencia de control de publicar (products no tiene ese estado).

## Trade-offs

- **Sin campo `metadata` expuesto en el body de products.** La columna existe
  (la inyecta `ContentType`) pero el CRUD de products no la expone, a diferencia
  de `articles` que sí acepta `metadata` opcional. El contrato lista los campos
  `title/body/price/sku` y pide "CRUD simple"; agregar metadata sería alcance
  extra. Queda con su default NULL; un contrato futuro puede exponerla sin
  cambio de esquema.
- **Detección de UNIQUE por string.** Frágil ante un cambio de wording del
  driver, pero evita una dependencia nueva (restricción del contrato) y está
  cubierto por tests que fallarían si el driver cambiara la frase.

## Salida REAL de los criterios de aceptación

### `go build ./...` y `go vet ./...`

```
=== BUILD OK ===
=== VET OK ===
```
`gofmt -l internal/` → sin salida (limpio).

### `go test ./... -count=1` — corrido DOS VECES

Run 1:
```
?   github.com/MauricioPerera/librarian/cmd/librarian    [no test files]
ok  github.com/MauricioPerera/librarian/internal/auth    3.614s
ok  github.com/MauricioPerera/librarian/internal/config  0.659s
ok  github.com/MauricioPerera/librarian/internal/schema  1.298s
ok  github.com/MauricioPerera/librarian/internal/server  15.441s
ok  github.com/MauricioPerera/librarian/internal/store   2.281s
```

Run 2:
```
?   github.com/MauricioPerera/librarian/cmd/librarian    [no test files]
ok  github.com/MauricioPerera/librarian/internal/auth    3.380s
ok  github.com/MauricioPerera/librarian/internal/config  0.623s
ok  github.com/MauricioPerera/librarian/internal/schema  1.251s
ok  github.com/MauricioPerera/librarian/internal/server  15.887s
ok  github.com/MauricioPerera/librarian/internal/store   2.181s
```

### T1 — `decimalColumn` + tabla, ambos motores, `unique("sku")`

`TestProductsTable` PASS. DDL real emitido (SQLite `"price" TEXT`, Postgres
`"price" NUMERIC`, `UNIQUE ("sku")` en ambos) — ver sección T1 arriba.
`TestSchemaValidates`, `TestSchemaRoundTripJSON`, `TestCompileDDLBothEngines`:
PASS.

### T2 — CRUD JSON, SKU duplicado → 400, price no numérico → 400, gateo

```
--- PASS: TestCreateProduct
--- PASS: TestCreateProductAcceptsJSONNumberPrice
--- PASS: TestCreateProductAPIKeyRejected
--- PASS: TestProductMissingFields
--- PASS: TestProductPriceNotNumericIs400        (red-team: "abc","1.2.3","$5","","12e3","1,000",true,{} → 400)
--- PASS: TestProductDuplicateSKUIs400           (red-team: dup en insert Y en update → 400, nunca 500)
--- PASS: TestListAndGetProducts
--- PASS: TestUpdateProduct
--- PASS: TestDeleteProduct
--- PASS: TestProductRoundTrip
--- PASS: TestProductNotFoundAndMalformedID      (inexistente Y malformado → 404, nunca 500)
```

### T3 — CRUD UI por HTTP con cookie real, "Productos" en navegación

```
--- PASS: TestAdminProductsNoSessionRedirectsToLogin
--- PASS: TestAdminProductsSessionWithoutPermissionIs403
--- PASS: TestAdminProductCreateAppearsInList
--- PASS: TestAdminProductCreateValidationReRendersForm   (price no numérico → 400 en form)
--- PASS: TestAdminProductDuplicateSKUReRendersForm       (dup sku → 400 en form)
--- PASS: TestAdminProductEditForm
--- PASS: TestAdminProductUpdate
--- PASS: TestAdminProductDelete
--- PASS: TestAdminProductMalformedIDIsNotFound
--- PASS: TestAdminNavShowsProductos                      ("Productos" + submenú en la nav)
--- PASS: TestAdminProductRoundTrip                       (crear→listar→editar→borrar por HTTP con cookie TLS)
--- PASS: TestAdminProductDeleteWithoutPermissionServerSide (red-team: gate server-side)
```

Todos los tests de UI usan `httptest.NewTLSServer` + cookie jar (`openUITLS`),
como los contratos de UI anteriores (cookie `Secure`).

### T4 — Contratos anteriores intactos (JSON + UI)

53 tests de articles/users/roles/api-keys/login/whoami/UI base, todos PASS,
0 fallos. Extracto:

```
--- PASS: TestCreateArticle            --- PASS: TestUpdateArticle
--- PASS: TestPublishArticle           --- PASS: TestDeleteArticle
--- PASS: TestListAndGetArticles       --- PASS: TestCreateArticleWithMetadata
--- PASS: TestCreateArticleWithEmbedding --- PASS: TestUpdateArticleEmbedding
--- PASS: TestAdminRoundTrip           --- PASS: TestAdminPublish
--- PASS: TestAdminCreateAppearsInList --- PASS: TestAdminEditForm
--- PASS: TestAdminUserCreateAppearsInListAndDetail --- PASS: TestAdminUserStatusChange
--- PASS: TestAdminUserRolesChange     --- PASS: TestAdminRolesViewReflectsRealGrants
--- PASS: TestAdminAPIKeyRoundTrip     --- PASS: TestAdminAPIKeyCreateShowsSecretOnce
--- PASS: TestAdminAPIKeyRevokeIdempotentAndMissing --- PASS: TestAdminAPIKeysWriteWithoutPermission
--- PASS: TestLoginSuccess             --- PASS: TestLoginInvalidCredentials
--- PASS: TestWhoamiJWT                --- PASS: TestWhoamiAPIKey
--- PASS: TestUIJSONRoutesUnaffected   --- PASS: TestUIRoundTrip
(... 53 total, sin fallos)
```

## Archivos

Modificados: `internal/schema/schema.go`, `internal/schema/schema_test.go`,
`internal/server/server.go`, `internal/server/ui.go`, `internal/server/ui_nav.go`.

Nuevos: `internal/server/products.go`, `internal/server/ui_products.go`,
`internal/server/server_products_test.go`,
`internal/server/server_ui_products_test.go`,
`internal/server/templates/products_{list,row,new,edit}.html`,
`docs/reports/CONTRACT-11-REPORT.md`.

Sin dependencias nuevas. Sin cambios al catálogo de permisos. Árbol sin
commitear (el orquestador commitea y despliega tras verificar).

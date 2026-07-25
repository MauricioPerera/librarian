# CONTRACT-15 — UI genérica: crear tipos y administrar su contenido

Base: `893a28c` (CONTRACT-01..14 completos). Sin commitear: el árbol queda con los cambios para
que el orquestador integre y despliegue.

Cierra la fase 3 de `DEFINITION-CPT-DINAMICOS.md`: hasta acá los tipos dinámicos existían solo por
API JSON (CONTRACT-13 los define, CONTRACT-14 hace su CRUD), así que un admin no técnico no podía
usarlos. Este contrato agrega **solo presentación** encima de esas capacidades: no se tocó
`internal/store/store.go` ni `internal/schema/*`, no hay permisos nuevos, no hay dependencias
nuevas (Go ni JS) y el contrato público de las rutas 01-14 no cambia.

## Archivos

Nuevos:

- `internal/server/ui_contenttypes.go` — T2, UI de gestión de TIPOS.
- `internal/server/ui_content.go` — T3, UI genérica de CONTENIDO de cualquier tipo dinámico.
- `internal/server/server_ui_content_test.go` — batería de aceptación T1-T4 (+ guard test de T1).
- `internal/server/templates/content_types_list.html`, `content_types_new.html`,
  `content_type_field_row.html`, `content_list.html`, `content_row.html`, `content_fields.html`,
  `content_new.html`, `content_edit.html` — 8 archivos FIJOS, agregados a la lista de `//go:embed`.
  No se genera ningún template en runtime: reciben la definición del tipo **como dato** y renderizan
  los campos con `range`.

Modificados: `internal/server/ui.go` (constructor `h.page`, embed, rutas), `ui_nav.go` (menú
consciente de los tipos dinámicos), `ui_articles.go` / `ui_products.go` / `ui_terms.go` /
`ui_users.go` / `ui_apikeys.go` (migración mecánica al constructor), `assets/app.css` (3 clases).

## T1 — Menú consciente de los tipos dinámicos

### El problema estructural

`navSections` era un `var` estático de paquete y `pageData.Nav()` un método sobre un struct **por
valor**, sin acceso a base y sin retorno de error. La lista de tipos dinámicos vive en la base, así
que el menú deja de poder ser estático. Había ~20 sitios que construían
`pageData{Title, Authenticated, Email, Path}` a mano; agregar "un campo más que hay que acordarse de
pasar" habría fallado en silencio en la próxima página que alguien agregue.

### Mecanismo elegido: constructor único + campo no exportado + guard test

1. `pageData` gana un campo **no exportado** `dynamic []navSection`. Los templates no lo tocan; solo
   `Nav()` lo lee, y `Nav()` ahora recorre `navSections` (estáticas) **seguidas de** `p.dynamic`.
2. Se agrega **un solo constructor**, `func (h *handlers) page(r *http.Request, title string) pageData`
   (`ui.go`). Toma el request, saca la Identity del contexto que `requireSession` /
   `requireSessionPermission` ya pusieron, arma `Path: r.URL.Path` y llena `dynamic` leyendo las
   definiciones persistidas (`store.LoadDefinitions`).
3. Los ~20 sitios pasaron de un literal de 4 campos a `h.page(r, "Título")`. Es **más corto** que lo
   que reemplaza, así que no hay incentivo para escribir el literal a mano.
4. **El cierre**: `TestPageDataIsBuiltOnlyByTheConstructor` lee el fuente del propio paquete y
   **falla** si aparece un literal `pageData{` en cualquier archivo que no sea `ui.go`.

### Por qué garantiza que una página nueva no puede olvidarlos

- El campo es no exportado y lo llena un solo lugar: no hay forma de "pasar el dato mal", solo de no
  usar el constructor.
- No usar el constructor es exactamente el patrón que el guard test detecta, y lo detecta en
  `go test ./...` — es decir, en CI, no en producción con un admin confundido. El fallo pasa de
  **silencioso** a **ruidoso**, que es lo que el contrato pide.
- Prueba real de que el guard muerde (se agregó temporalmente `var _ = pageData{Title: "x"}` a
  `ui_nav.go`):

```
--- FAIL: TestPageDataIsBuiltOnlyByTheConstructor (0.00s)
    server_ui_content_test.go:55: pageData{...} literal outside ui.go in [ui_nav.go] — build page view models with h.page(r, title) or the dynamic content types silently vanish from that page's sidebar
FAIL
FAIL	github.com/MauricioPerera/librarian/internal/server	4.560s
```

(revertido inmediatamente; el mismo test en verde después: `ok  github.com/MauricioPerera/librarian/internal/server	4.161s`)

### Consecuencias de diseño

- **Lectura por request, sin caché.** Una query indexada por página administrativa. A cambio: un
  tipo creado en otra pestaña aparece en la sidebar en la siguiente carga, sin reiniciar y sin
  invalidación de caché. Se verifica explícitamente en `TestDynamicTypeAppearsInSidebarOfEveryAuthenticatedPage`,
  que crea el tipo con la **misma sesión viva** y lo ve aparecer.
- **Degradación, no 500.** Si la lectura falla, `dynamicNavSections` devuelve `nil`: la sidebar es
  decoración y un hipo de base no puede convertir una página que funciona en un error.
- `renderForbidden` / `renderNotFound` pasaron a ser **métodos** `h.renderForbidden(w, r)` /
  `h.renderNotFound(w, r)` para poder usar el constructor. Efecto colateral deseado: el 403 y el 404
  ahora también muestran la sidebar completa del usuario logueado.
- En `requireSessionPermission` la Identity se mete en el contexto **antes** de decidir el permiso,
  para que el 403 se renderice con el mismo constructor. La decisión de autorización no cambió.
- Entrada estática nueva "Tipos de contenido" (`/admin/content-types` + `/admin/content-types/new`):
  es un par de rutas fijo, así que va en la mitad estática. Solo las entradas **por tipo** son
  dinámicas (`/admin/content/{type}` + `.../new`).

## T2 — UI de gestión de tipos (`/admin/content-types`)

| Ruta | Gate |
|---|---|
| `GET /admin/content-types` | sesión |
| `GET /admin/content-types/new` | sesión |
| `GET /admin/content-types/new/field` (fragmento htmx) | sesión |
| `POST /admin/content-types` | `content_types.manage` |

No reimplementa nada: la escritura pasa por `store.CreateContentType` (la misma transacción atómica
definición+tabla de CONTRACT-13) bajo el mismo `h.schemaMu`, y la lectura por `store.LoadDefinitions`.

### Cómo se definen N campos variables (decisión de diseño)

**htmx, sin JavaScript propio y sin dependencias nuevas.** El botón "Agregar campo" hace
`hx-get="/admin/content-types/new/field"` con `hx-swap="beforeend"` sobre el contenedor de campos, y
el servidor devuelve **una** fila más renderizada desde el **mismo template fijo**
(`content_type_field_row.html`) que usa el formulario completo. Los templates se siguen parseando al
iniciar el paquete desde una lista fija; no se genera nada en runtime.

Detalle que hace que esto funcione sin JS: las filas usan **nombres repetidos** (`field_name` /
`field_type`), no indexados (`field[0][name]`). `r.PostForm` conserva el orden de envío, así que los
dos slices se cierran posicionalmente, y una fila que el admin dejó en blanco simplemente se ignora.
Eso deja al fragmento **sin estado** — no necesita saber qué índice es —, que es justo lo que
permite agregar filas sin código de cliente. El formulario arranca con 3 filas vacías.

Alternativa descartada: renderizar N filas fijas (p. ej. 10) sin htmx. Funciona pero pone un techo
arbitrario al número de campos; el fragmento no lo pone.

### Errores → formulario re-renderizado, nunca 500 ni JSON crudo

- Nombre inválido (`Recetas`, `mi tipo`, `1tipo`, `users`, `recetas; DROP TABLE users`): se corre
  `def.Validate()` **antes** de tocar la base (mismo orden que el handler JSON, para que la UI no sea
  una puerta más laxa a la misma acción) → 400 HTML con el formulario y el banner
  `Definición inválida: …`.
- Nombre duplicado: `store.ErrDuplicateContentType` → 400 HTML,
  `Ya existe un tipo de contenido con ese nombre.`
- Cualquier otro fallo de creación también se muestra como error de formulario (no como página 500):
  la creación entera es una transacción, así que no quedó nada persistido y el admin puede corregir
  y reintentar.
- Sin `content_types.manage` → 403 HTML (`renderForbidden`), sin tabla creada.

## T3 — UI genérica de contenido (`/admin/content/{type}`)

| Ruta | Gate |
|---|---|
| `GET /admin/content/{type}` | sesión |
| `GET /admin/content/{type}/new` | sesión |
| `POST /admin/content/{type}` | `content.create` |
| `GET /admin/content/{type}/{id}/edit` | sesión |
| `PUT /admin/content/{type}/{id}` | `content.update` |
| `DELETE /admin/content/{type}/{id}` | `content.delete` |

Permisos: los `content.*` de siempre. `content_types.manage` gatea **definir** un tipo, no cargarle
contenido; son acciones distintas y no se mezclan. Ningún permiso nuevo.

**Cero acceso a datos y cero validación reimplementados.** Todo pasa por los helpers de CONTRACT-14
(`listContentRows`, `fetchContentRow`, `insertContentRow`, `updateContentRow`, `deleteContentRow`) y
por `bindValues`/`bindValue`. El único trabajo propio de este archivo es traducir el formulario HTML
al `map[string]json.RawMessage` que `bindValues` ya entiende, y traducir una fila almacenada de
vuelta a controles. Consecuencia: la UI y la API JSON aceptan y rechazan exactamente lo mismo, con
los mismos mensajes.

**Seguridad idéntica a CONTRACT-14.** `resolveTypeUI` es el gemelo HTML de `resolveType`: mismo
`store.FetchContentType`, el segmento `{type}` solo se usa como valor **ligado** de comparación, y si
no resuelve a una definición persistida es 404 y **no se construye ninguna query contra ninguna
tabla dinámica**. La única diferencia con la API es el cuerpo del 404: HTML, nunca el sobre JSON.
Es la única puerta de entrada de los seis handlers.

### Mapeo tipo de campo → control

| FieldType | Control | Por qué |
|---|---|---|
| `text` | `<input type="text">` | — |
| `integer` | `<input type="number" step="1">` | — |
| `decimal` | `<input type="text" inputmode="decimal">` | **NO `number`**: el paso decimal del navegador puede reescribir el valor, y el proyecto guarda decimales como TEXT canónico a propósito (igual que `products.price`) |
| `boolean` | checkbox + hidden acompañante | ver abajo |
| `date` | `<input type="date">` | coincide con el `YYYY-MM-DD` canónico que guarda la columna |

`integer` se manda a `bindValue` como token crudo (no como string JSON) para que `"abc"` o `"1.5"`
los rechace el validador de CONTRACT-14 con su mensaje preciso, en vez de coercionarse en silencio.

### La trampa del checkbox — cómo se resuelve

Un checkbox desmarcado **no se envía**. Con el criterio de CONTRACT-14 ("campo ausente → NULL"),
guardaría NULL en vez de `false`: una divergencia silenciosa entre la API JSON y el form HTML.

Solución: un `<input type="hidden" name="X" value="false">` renderizado **inmediatamente antes** del
`<input type="checkbox" name="X" value="true">`. El navegador siempre manda al menos un valor:
`["false"]` desmarcado, `["false","true"]` marcado. El lector (`lastFormValue`) toma el **último**,
así que el estado del checkbox gana cuando está presente y llega `false` cuando no.

Consecuencia deliberada: desde el formulario **un booleano nunca se puede escribir como NULL**. Es lo
correcto: un checkbox tiene dos estados, no tres. Un NULL solo puede llegar por la API JSON o por
escritura externa, y el formulario de edición es honesto al respecto (siguiente punto).

Verificado con consulta directa a la base en
`TestAdminContentUncheckedCheckboxStoresFalseNotNull`: `activa` queda `Valid=true, Int64=0`
(false/0) y **no** NULL; marcado queda 1.

### `false` vs `NULL` en el formulario de edición

- Tipos no booleanos: se distinguen por el control vacío. Control vacío ⇒ el valor almacenado es
  NULL; enviarlo vacío vuelve a escribir NULL (`TestAdminContentEmptyTextStoresNull`). Nota honesta:
  eso significa que **la cadena vacía no es almacenable desde el formulario** (se guarda NULL).
- Booleano: un checkbox no puede distinguirlos, así que un NULL se renderiza desmarcado **más** un
  marcador explícito `(sin valor / NULL — al guardar quedará en «no»)`. No se le oculta nada al
  admin. Verificado en `TestAdminContentEditFormDistinguishesFalseFromNull`.
- En el **listado** también se distinguen: `no` vs `—` (em dash) para NULL.

### Otros detalles

- Errores de validación → formulario re-renderizado con `Revisá los campos: <mensaje del validador>`,
  **preservando lo que el usuario cargó**, con 400 y sin insertar nada.
- Update = reemplazo total de los campos propios (misma semántica que el PUT JSON y que
  articles/products). Respuesta `HX-Redirect` al listado, igual que `/admin/products`.
- Delete: 200 con cuerpo vacío → el swap `outerHTML` de htmx saca la fila.
- Un tipo con **cero campos** es creable y su listado funciona (las columnas son un `range`, no un
  header hardcodeado) — `TestAdminContentTypeWithZeroFields`.
- Un tipo con **muchos campos** (24 en el test) no explota: la tabla va dentro de
  `.table-scroll { overflow-x: auto }`. No promete ser lindo; promete no romper el layout de la
  página — `TestAdminContentTypeManyFieldsDoesNotExplode`.

## T4 — Verificación (salida real)

### `go build` / `go vet`

```
$ cd /d/Repo/librarian && go build ./... && go vet ./... && echo "=== BUILD+VET CLEAN ==="
=== BUILD+VET CLEAN ===
```

`gofmt -l internal/` no lista nada.

### Suite completa, dos veces

```
$ go test ./... -count=1     ### RUN 1
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.542s
ok  	github.com/MauricioPerera/librarian/internal/auth	5.040s
ok  	github.com/MauricioPerera/librarian/internal/config	0.693s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.429s
ok  	github.com/MauricioPerera/librarian/internal/server	28.689s
ok  	github.com/MauricioPerera/librarian/internal/store	3.084s

$ go test ./... -count=1     ### RUN 2
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.543s
ok  	github.com/MauricioPerera/librarian/internal/auth	4.482s
ok  	github.com/MauricioPerera/librarian/internal/config	0.698s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.433s
ok  	github.com/MauricioPerera/librarian/internal/server	34.256s
ok  	github.com/MauricioPerera/librarian/internal/store	3.044s
```

### Criterio por criterio

Toda la batería corre sobre el **servidor HTTP real con cookie de sesión real**
(`openUITLS` → `httptest.NewTLSServer` + `cookiejar`, obligatorio porque una cookie `Secure` se
descarta sobre HTTP plano) y **sin tocar la API JSON**.

| Criterio | Test | Resultado |
|---|---|---|
| T1: los tipos dinámicos aparecen en la sidebar de TODAS las páginas autenticadas | `TestDynamicTypeAppearsInSidebarOfEveryAuthenticatedPage` (11 páginas: `/`, articles, articles/new, products, terms, users, users/new, roles, api-keys, content-types, content-types/new, + las dos del propio tipo) | PASS |
| T1: mecanismo a prueba de olvidos | `TestPageDataIsBuiltOnlyByTheConstructor` (+ prueba negativa arriba) | PASS |
| T2: crear un tipo desde la UI real | `TestAdminContentTypesListAndCreate` (lista vacía → form con `hx-get` → fragmento de 1 fila → creación → fila en blanco ignorada → tabla real creada) | PASS |
| T2: nombre inválido → formulario con error, nunca 500 ni JSON | `TestAdminContentTypeInvalidNameReRendersForm` (5 nombres, incluido `recetas; DROP TABLE users`; verifica 400 + `text/html` + banner + nada persistido + `users` intacta) | PASS |
| T2: nombre duplicado → formulario con error | `TestAdminContentTypeDuplicateNameReRendersForm` | PASS |
| T3: CRUD completo por UI | `TestAdminDynamicContentFullFlow` | PASS |
| T3: `{type}` inexistente → 404 HTML | `TestAdminContentUnknownTypeIsHTML404` | PASS |
| T4: flujo completo de admin por HTTP | `TestAdminDynamicContentFullFlow` | PASS |
| T4: checkbox desmarcado → `false`/0, no NULL (consulta directa) | `TestAdminContentUncheckedCheckboxStoresFalseNotNull` | PASS |
| T4: seguridad (`{type}` hostil, sin `content_types.manage`, sin `content.create`) | `TestAdminContentUnknownTypeIsHTML404`, `TestAdminContentTypeWithoutPermissionIs403`, `TestAdminContentCreateWithoutPermissionIs403` | PASS |
| Red-team: tipo con 0 campos | `TestAdminContentTypeWithZeroFields` | PASS |
| Red-team: tipo con muchos campos | `TestAdminContentTypeManyFieldsDoesNotExplode` | PASS |
| Red-team: `false` vs NULL en el form de edición | `TestAdminContentEditFormDistinguishesFalseFromNull` | PASS |
| Red-team: tipo creado con sesión abierta aparece sin reiniciar | dentro de `TestDynamicTypeAppearsInSidebarOfEveryAuthenticatedPage` | PASS |
| Rutas sin sesión → 302 `/login` | `TestAdminContentNoSessionRedirectsToLogin` | PASS |

Detalle de `TestAdminDynamicContentFullFlow` (el flujo que haría un admin de verdad, todo por HTTP
con cookie, sin API JSON):

1. crea el tipo `recetas` desde la UI con **un campo de cada uno de los 5 tipos**
   (`titulo` text, `raciones` integer, `costo` decimal, `publicada` boolean, `fecha` date);
2. comprueba que aparece en la sidebar de `/admin/articles` (una página que no sabe nada de tipos);
3. entra a su listado (estado vacío correcto);
4. crea una fila con los 5 campos (checkbox marcado: `["false","true"]`, lo que manda un navegador);
5. la fila aparece en el listado con todos los valores (`Tarta de manzana`, `8`, `12.50`, `sí`,
   `2026-07-24`) **y** la consulta directa confirma los tipos SQL correctos
   (decimal como TEXT canónico `"12.50"`, boolean como `1`, fecha como `2026-07-24`);
6. el formulario de edición viene precargado (incluido el `checked`) y con el `hx-put` correcto;
7. la edita (desmarcando el booleano) → `HX-Redirect` a `/admin/content/recetas`, cambios visibles;
8. la borra → el listado queda vacío;
9. `PUT` con id inexistente y `DELETE` con id malformado → 404 (nunca 500).

Batería de seguridad de `{type}` (para cada uno: `GET` listado, `GET .../new`, `GET .../{id}/edit`,
`POST` y `DELETE`): `noexiste`, `users`, `articles`, `content_types`, `users; DROP TABLE users--`,
`'; DROP TABLE users; --`, `../users` → **todos 404 HTML** (`No encontrado`, sin sobre JSON), y
después se confirma que siguen existiendo y siendo legibles `users`, `articles`, `products`, `terms`,
`roles`, `permissions`, `api_keys`, `content_types`, `content_type_fields`. Nótese que `users` y
`articles` son tablas REALES: no son tipos de contenido dinámicos, así que la UI genérica no les da
acceso — la resolución contra una definición persistida es lo que lo impide.

### Contratos anteriores: sin cambios

El paquete `internal/server` corre **141 tests, 0 fallos**. Están ahí, en verde, todos los de los
contratos previos — JSON y UI de articles, products, users, roles, api-keys, terms y content-types,
más la API genérica de CONTRACT-14:

```
$ go test ./internal/server/ -count=1 -v | grep -c "^--- PASS"
141
$ go test ./internal/server/ -count=1 -v | grep -c "^--- FAIL"
0
```

Nominalmente, entre otros: `TestListAndGetArticles`, `TestCreateArticle`, `TestUpdateArticle`,
`TestPublishArticle`, `TestDeleteArticle`, `TestAdminRoundTrip`, `TestAdminPublish`,
`TestCreateProduct`, `TestProductRoundTrip`, `TestAdminProductRoundTrip`, `TestAdminNavShowsProductos`,
`TestAdminUserCreateAppearsInListAndDetail`, `TestAdminUserRolesChange`,
`TestAdminRolesViewReflectsRealGrants`, `TestAdminAPIKeyRoundTrip`, `TestAdminTermCRUDAndNav`,
`TestAssignTermsRoundTripArticle`, `TestCreateContentTypeCreatesRealTable`,
`TestCreateContentTypeHostileNamesAre400`, `TestCreateContentTypeIsCreateOnly`,
`TestDynamicContentRoundTrip`, `TestDynamicContentHostileTypeNames`,
`TestDynamicContentDecimalPrecision`, `TestDynamicContentNullsAndOmittedFields`,
`TestDynamicContentSurvivesRestart`, `TestPreviousContractsUnaffectedByGenericCRUD`,
`TestUIJSONRoutesUnaffected`, `TestVectorFormatConvergesWithCompat`.

Ninguno de esos tests se modificó. La única razón por la que podían haberse roto era el refactor de
`pageData`, y es exactamente lo que su verde demuestra: el contenido y los códigos de estado de
todas las páginas previas son los mismos.

## Trade-offs asumidos

1. **Una query por página renderizada** para la sidebar. Se elige frescura sobre micro-optimización:
   es una página administrativa y evita toda una clase de bugs de caché rancia. Si algún día molesta,
   el lugar a cambiar es uno solo (`h.dynamicNavSections`).
2. **El guard test es textual**, no un análisis AST. Es deliberado: barato, sin dependencias, y su
   mensaje de error explica exactamente qué hacer. Un falso positivo solo es posible escribiendo
   `pageData{` en un comentario fuera de `ui.go`, que es un precio aceptable.
3. **Un booleano no se puede poner en NULL desde el formulario.** Es la contracara directa de cerrar
   la trampa del checkbox; documentado en la UI misma y en los tests.
4. **La cadena vacía no es almacenable desde el formulario** (se guarda NULL). Un formulario HTML no
   distingue `""` de "no cargado", y NULL es la lectura honesta. La API JSON sí puede escribir `""`.
5. **La sidebar degrada a "sin entradas dinámicas" si la base falla**, en vez de fallar la página.
6. **Ruta extra `/admin/content-types/new/field`** (fragmento htmx). No forma parte del contrato
   público de nada previo, requiere sesión y no lee ni escribe datos; es el precio de tener N campos
   variables sin JavaScript propio.
7. Los enlaces "Nuevo tipo" / "Añadir nuevo" se muestran a cualquier sesión (como `/admin/users/new`
   ya hacía); el gate es servidor-side en el POST, que es donde tiene que estar.

## Fuera de alcance (sin cambios respecto de la definición)

Editar los campos de un tipo ya creado, borrar un tipo, relaciones/FK y tipos de campo avanzados
siguen fuera de alcance (`DEFINITION-CPT-DINAMICOS.md`). El listado de tipos lo dice explícitamente
en pantalla: *"Los campos de un tipo quedan congelados al crearlo"*.

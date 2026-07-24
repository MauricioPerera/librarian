# CONTRACT-07 — UI de `articles` (CRUD) — Reporte

Segundo contrato de la fase 2 (UI). Construye sobre la fundación de sesión/layout
de CONTRACT-06. HEAD de partida: `6fe5476`. Sin dependencias Go nuevas. Sin tocar
nada fuera de `librarian`. Árbol dejado sin commitear.

## Resumen por tarea

### T1 — Middleware de sesión + permiso, y rutas de escritura gateadas

**Gap cerrado:** no existía ningún middleware que combinara "sesión de navegador
por cookie" + "permiso específico". `requirePermission` (`authz.go`) resuelve la
identidad solo desde el header `Authorization` (API JSON); `requireSession`
(`ui.go`) solo exige sesión sin chequear permiso.

- Se agregó **`requireSessionPermission(permission string) func(http.Handler) http.Handler`**
  en `internal/server/ui.go`. Reusa la resolución cookie→`Identity` y el mismo
  `permissionsFor` de la API. En fallo NO escribe el envelope JSON de `writeError`:
  - sin sesión (cookie ausente/malformada/mal firmada/expirada) → **302 redirect a `/login`**;
  - sesión válida pero sin el permiso → **página HTML 403 simple** (`error_403.html`);
  - error de DB cargando permisos → `http.Error` texto plano 500 (no el envelope JSON de la API).
- Se extrajo **`sessionIdentity(r)`** como único resolver cookie→`Identity`, usado
  por `requireSession` y `requireSessionPermission` (sin duplicar la plomería
  cookie/JWT). `requireSession` quedó refactorizado para usarlo — comportamiento
  idéntico (tests de CONTRACT-06 siguen verdes).
- Mapeo de permisos idéntico a la API JSON: `content.create` / `content.update` /
  `content.publish` / `content.delete`. Las rutas de LECTURA (`GET /admin/articles`,
  `/new`, `/{id}/edit`) solo exigen sesión (`requireSession`), igual que la API no
  gatea lectura por permiso.

### T2 — Listar y crear

- **`GET /admin/articles`** — tabla con título, estado (badge Publicado/Borrador) y
  fecha de creación. Estado vacío con enlace a crear.
- **`GET /admin/articles/new`** — formulario de creación (título, cuerpo).
- **`POST /admin/articles`** (gateado `content.create`) — valida título/cuerpo con el
  **mismo mensaje que la API** (`"title and body are required"`), inserta con el
  autor = usuario de sesión (`identityFromContext` → `UserID`) y **redirige 303** a la
  lista. En fallo de validación re-renderiza el form con el error (400), sin crear fila.

### T3 — Editar, publicar, borrar (htmx)

- **`GET /admin/articles/{id}/edit`** — formulario precargado. Id inexistente/malformado → **404 HTML**.
- **`PUT /admin/articles/{id}`** (gateado `content.update`) — vía `hx-put` desde el form.
- **`POST /admin/articles/{id}/publish`** (gateado `content.publish`) — vía `hx-post` desde la fila.
- **`DELETE /admin/articles/{id}`** (gateado `content.delete`) — vía `hx-delete` desde la fila.
- Id inexistente/malformado en cualquiera de estas → **404 HTML** (`error_404.html`), nunca 500 ni JSON.

## Decisiones de diseño (con porqué)

- **Nombre del middleware:** `requireSessionPermission(permission)`. Simetría directa
  con `requirePermission` de la API (mismo shape `func(http.Handler) http.Handler`),
  deja claro que es "sesión + permiso" vs. "header + permiso".
- **Namespace de rutas:** `/admin/articles` (distinto de `/articles` de la API JSON,
  que comparte el mismo `ServeMux` — un mismo método+path no puede registrarse dos
  veces). Las rutas literales/con `{id}` ganan por precedencia sobre el catch-all `GET /`.
- **Mecanismo de swap/redirect por operación:**
  - *Crear:* form HTML plano `method=post` → **303 redirect** a la lista. Una creación
    no tiene fila donde hacer swap; el redirect es el resultado natural y el navegador
    lo sigue sin htmx.
  - *Editar/actualizar:* `hx-put` desde el form; en éxito el server responde
    **`HX-Redirect: /admin/articles`** (200 sin cuerpo) → htmx navega client-side a la
    lista sin recargar el documento actual. Elegido sobre swap de fragmento porque
    tras editar la acción siguiente del usuario es la lista. El contrato permite
    explícitamente "swap del fragmento o redirect simple".
  - *Publicar:* `hx-post` desde la fila; el server responde **el `<tr>` actualizado**
    (fragmento único), htmx lo intercambia in-place (`hx-swap="outerHTML"` sobre la
    fila) — la fila pasa de Borrador a Publicado sin recargar.
  - *Borrar:* `hx-delete` desde la fila; el server responde **200 con cuerpo vacío** y
    el `outerHTML` swap de htmx elimina la fila.
- **Estructura de páginas/templates:** un template set por página (layout + página),
  siguiendo el patrón de CONTRACT-06 (evita colisión en la definición compartida
  `content`). La fila se aísla en `articles_row.html` (`{{define "article_row"}}`) y se
  parsea **tanto standalone** (para el fragmento de publish) **como dentro del set de
  lista** (que la recorre con `{{template "article_row" .}}`). Páginas de error 403/404
  como sets propios reusando el layout.
- **Reuso de acceso a datos (sin duplicar SQL):** se extrajeron helpers compartidos en
  `articles.go` — `insertArticleBasic`, `listArticles`, `articleExists`,
  `updateArticleTitleBody`, `publishArticleByID`, `deleteArticleByID` — y los handlers
  JSON de CONTRACT-03 se recablearon para llamarlos. Así la UI corre exactamente el
  mismo SQL parametrizado que la API, sin copiar queries. El `fetchArticle` existente se
  reusa tal cual para el form de edición. **El contrato público JSON no cambió** (sus
  tests siguen verdes sin tocarlos).

## Trade-offs

- Los botones de la fila (Publicar/Borrar) se renderizan siempre; el gateo es
  server-side (probado con red-team directo por curl+cookie). No se ocultan por
  permiso — sería solo cosmético y el contrato pide gateo del lado servidor, no
  decorativo del cliente.
- La lista usa límite fijo 100 (sin paginación en la UI) — suficiente para el alcance
  del contrato; la API JSON conserva su `?limit=&offset=`.
- El id de artículo en los tests se resuelve raspando el HTML de la lista (prueba que
  la fila creada está realmente en la UI, no solo en la DB).

## Verificación (salida real)

### `go build ./...` y `go vet ./...` — limpios

```
===== BUILD =====
build: OK (no output)
===== VET =====
vet: OK (no output)
```

### `go test ./... -count=1` — verde, dos veces

```
===== TEST RUN 1 =====
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	2.626s
ok  	github.com/MauricioPerera/librarian/internal/config	0.637s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.321s
ok  	github.com/MauricioPerera/librarian/internal/server	9.414s
ok  	github.com/MauricioPerera/librarian/internal/store	2.233s
===== TEST RUN 2 =====
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	2.754s
ok  	github.com/MauricioPerera/librarian/internal/config	0.666s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.352s
ok  	github.com/MauricioPerera/librarian/internal/server	9.232s
ok  	github.com/MauricioPerera/librarian/internal/store	2.254s
```

### T1 — middleware nuevo (por criterio)

```
--- PASS: TestAdminNoSessionRedirectsToLogin (0.05s)      # sin sesión → 302 /login (read + write)
--- PASS: TestAdminSessionWithoutPermissionIs403 (0.15s)  # sesión sin permiso → 403 HTML (no JSON, no 500)
--- PASS: TestAdminSessionWithPermissionPasses (0.15s)    # sesión con permiso → pasa (303 a la lista)
```

### T2 — crear aparece en la lista

```
--- PASS: TestAdminCreateAppearsInList (0.16s)            # POST real → aparece en GET con título + Borrador + autor de sesión
--- PASS: TestAdminCreateValidationReRendersForm (0.14s)  # validación re-renderiza form, 0 filas creadas
```

### T3 — editar/publicar/borrar + 404

```
--- PASS: TestAdminEditForm (0.14s)                       # form precargado; id inexistente → 404 HTML
--- PASS: TestAdminUpdate (0.15s)                         # hx-put → HX-Redirect + persistido; id inexistente → 404
--- PASS: TestAdminPublish (0.16s)                        # hx-post → fragmento fila "Publicado" + DB set; id inexistente → 404
--- PASS: TestAdminDelete (0.17s)                         # hx-delete → 200 vacío + fila borrada; id inexistente → 404
--- PASS: TestAdminMalformedIDIsNotFound (0.16s)          # id no-UUID en put/publish/delete → 404 (no 500)
```

### T4 — round-trip + red-team gateo server-side

```
--- PASS: TestAdminRoundTrip (0.21s)                      # login → crear → aparece → editar → publicar → borrar → desaparece
--- PASS: TestAdminDeleteWithoutPermissionServerSide (0.26s)  # sesión sin content.delete, hx-delete directo (curl+cookie) → 403, fila sobrevive
```

### Rutas de contratos anteriores intactas (confirmación explícita)

CONTRACT-03 (JSON `articles`), CONTRACT-05 (embedding) y CONTRACT-06 (login/logout/home)
— todos verdes sin tocar sus tests:

```
--- PASS: TestCreateArticle (0.23s)
--- PASS: TestCreateArticleWithMetadata (0.15s)
--- PASS: TestCreateArticleAPIKeyRejected (0.05s)
--- PASS: TestListAndGetArticles (0.22s)
--- PASS: TestUpdateArticle (0.24s)
--- PASS: TestPublishArticle (0.25s)
--- PASS: TestDeleteArticle (0.22s)
--- PASS: TestNotFound (0.13s)
--- PASS: TestMalformedIDIsNotFound (0.14s)
--- PASS: TestWhoamiJWT (0.12s)
--- PASS: TestWhoamiAPIKey (0.04s)
--- PASS: TestWhoamiRevokedAPIKeyRejected (0.04s)
--- PASS: TestWhoamiNoCredentials (0.03s)
--- PASS: TestWhoamiGarbageToken (0.04s)
--- PASS: TestUIStaticAssetsEmbedded (0.04s)
--- PASS: TestUILoginSuccessSetsCookie (0.12s)
--- PASS: TestUILoginInvalidGenericError (0.18s)
--- PASS: TestUILogoutClearsCookie (0.04s)
--- PASS: TestUIHomeNoCookieRedirects (0.04s)
--- PASS: TestUIHomeInvalidCookieRedirects (0.04s)
--- PASS: TestUIRoundTrip (0.12s)
--- PASS: TestUIForgedJWTCookieRejected (0.04s)
--- PASS: TestUIExpiredJWTCookieRejected (0.04s)
--- PASS: TestUIJSONRoutesUnaffected (0.13s)
--- PASS: TestCreateArticleWithEmbedding (0.14s)
--- PASS: TestCreateArticleEmbeddingOmittedIsNull (0.13s)
--- PASS: TestUpdateArticleEmbedding (0.14s)
```

## Archivos

Nuevos:
- `internal/server/ui_articles.go` — handlers admin, template sets, render helpers.
- `internal/server/templates/articles_list.html`, `articles_row.html`,
  `articles_new.html`, `articles_edit.html`, `error_403.html`, `error_404.html`.
- `internal/server/server_ui_articles_test.go` — tests de aceptación T1–T4 + red-team.

Modificados:
- `internal/server/articles.go` — extracción de helpers de acceso a datos (contrato JSON sin cambios).
- `internal/server/ui.go` — `sessionIdentity`, `requireSessionPermission`, embed ampliado, registro de rutas admin.
- `internal/server/assets/app.css` — estilos de tabla/badges/botones admin.
- `internal/server/templates/home.html` — enlace a `/admin/articles`.

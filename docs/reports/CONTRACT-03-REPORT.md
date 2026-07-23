# CONTRACT-03 — Reporte de implementación

API CRUD de tipos de contenido con autorización por rol. Implementado sobre
`main` sin commitear (el orquestador commitea tras verificar). Base real leída
antes de escribir: `internal/schema/schema.go`, `internal/server/server.go`,
`internal/auth/{users,jwt,apikey}.go`, `internal/store/store.go`,
`internal/schema/content_type.go` y los tests de CONTRACT-02.

## Resumen por tarea

### T1 — Extender el catálogo de permisos
Agregado `"content.update"` a `schema.Permissions`
(`internal/schema/schema.go`). Es la única fila que faltaba. El seed
idempotente (`store.SeedCatalogs`) es data-driven sobre esa slice, así que lo
recoge sin tocar su lógica — confirmado con test. `Schema.Validate()` y
`CompileDDL` no se ven afectados (es un dato de catálogo, no un cambio de
esquema).

### T2 — Middleware de autorización reusable
Nuevo archivo `internal/server/authz.go`. Extraje la resolución de identidad
(JWT-then-API-key, mismo orden que `handleWhoami`) a una función reusable
`resolveIdentity(ctx, db, secret, token) (*Identity, bool)`. Sobre ella:

- `Identity` (Kind `jwt`|`apikey`, UserID/Email/Roles para JWT,
  RoleID/Label para API key) se pasa por `context.Context` (clave
  `identityKey{}`), no por variable global.
- `(h *handlers) requireAuth(next http.Handler) http.Handler` — pide solo
  identidad válida (401 si no). Usado por las rutas de lectura.
- `(h *handlers) requirePermission(perm string) func(http.Handler) http.Handler`
  — autentica, carga el set de permisos desde `role_permissions`, 403 si el
  permiso pedido no está, 401 si no autentica. Es el wrapper `http.Handler`
  reusable que pide el contrato (los handlers no llaman un check a mano).
- Set de permisos: para JWT, unión de permisos de todos sus roles
  (`permissionsForRoles`, `SELECT DISTINCT ... WHERE r.name IN (...)`); para
  API key, permisos del único `role_id` (`permissionsForRoleID`). Todo
  parametrizado.

**Refactor de `handleWhoami`**: ahora llama a `resolveIdentity` y despacha por
`id.Kind`, conservando **exactamente** la misma respuesta pública de
CONTRACT-02 (`{auth,user_id,email,roles}` para JWT; `{auth,label,role_id}`
para API key) y los mismos códigos. No duplica lógica de resolución. El
contrato público de `/whoami` y `/auth/login` no se tocó.

### T3 — CRUD de `articles`
Nuevo archivo `internal/server/articles.go`, rutas wired en
`server.NewMux` con patrones wildcard de Go 1.26
(`POST /articles`, `GET /articles`, `GET /articles/{id}`,
`PUT /articles/{id}`, `POST /articles/{id}/publish`,
`DELETE /articles/{id}`). `database/sql` directo contra `compat.Store.DB`,
mismo patrón parametrizado que CONTRACT-02, sin ORM ni dependencias nuevas.

- `POST` → 201, draft (`published_at` NULL), `author_id` = usuario del JWT.
- `GET` lista → 200, `?limit=&offset=` (default limit 20), orden por
  `created_at DESC`.
- `GET {id}` → 200 o 404 si no existe.
- `PUT {id}` → actualiza `title`/`body` (no `published_at`); 404 si no existe.
- `POST {id}/publish` → setea `published_at = CURRENT_TIMESTAMP` solo si era
  NULL (idempotente: no-op si ya publicado, `published_at` no cambia); 404 si
  no existe.
- `DELETE {id}` → 204 o 404.
- Envelope de error `{"error": "<msg>"}` en todos los fallos, igual que
  CONTRACT-02.

### T4 — Verificación de autorización end-to-end
`internal/server/server_articles_test.go` (mismo paquete que los tests de
CONTRACT-02, reusa `openAuthMux`/`doJSON`/`roleID`). Cubre, por cada permiso
relevante (`content.create`, `content.update`, `content.publish`,
`content.delete`): con permiso → success; autenticado sin permiso → 403; sin
autenticar → 401. Más: `GET /articles` y `GET /articles/{id}` con cualquier
identidad autenticada (JWT sin permisos y API key) → 200; sin autenticar → 401.

## Decisión de diseño (la que me tocaba)

**¿Qué pasa si una API key (sin usuario humano) hace `POST /articles`? →
rechazada con `403` y mensaje claro: "creating an article requires a user
identity (API keys have no author)".**

Por qué: `articles.author_id` es `NOT NULL` con FK → `users(id)` ON DELETE
CASCADE (ver `schema.ContentType`). Un article **debe** tener un autor humano;
una API key no tiene `users.id` detrás. La alternativa "permitir con
`author_id` NULL" es **estructuralmente imposible** — la columna no admite
NULL. Entre "rechazar con error claro" y "permitir con NULL", solo la primera
es viable; y además es la correcta: fallar temprano y explícito (403 + mensaje)
evita que un servicio intente crear contenido sin autoría y produce mejor UX
de API que un 500 de violación de FK más tarde. El permiso `content.create`
puede estar presente en el rol de la key, pero el gate de autoría vive en el
handler (post-middleware): la key autentica y tiene el permiso, pero le falta
la identidad de usuario que el esquema exige.

Test: `TestCreateArticleAPIKeyRejected` — key minteada contra rol con
`content.create` → 403 + mensaje no vacío.

## Otras decisiones donde el contrato dejaba margen

- **Estado de `role_permissions`**: `SeedCatalogs` solo siembra **nombres** de
  roles/permisos, no qué rol tiene qué permiso. En los tests se asignan
  permisos explícitamente con un helper `grant` (`INSERT ... ON CONFLICT DO
  NOTHING`), igual que lo haría un admin. No se agregó seed de grants en
  producción — el contrato no lo pide y mantenerlo como operación de admin es lo
  correcto.
- **`PUT` semántica full-replace**: exige `title` y `body` no vacíos (400 si
  falta alguno). El contrato dice "actualiza title/body"; elegí reemplazo
  completo de ambos antes que PATCH parcial — más simple y suficiente para v1.
- **`DELETE` responde `204`** (sin body). El contrato aceptaba 200/204.
- **`limit=0`/offset negativo → default** (`queryIntDefault` trata `n<0` como
  default). `limit=0` explícito devuelve 0 filas (LIMIT 0 en SQLite) — entrada
  de paginación fuera de los criterios de aceptación, documentada acá.
- **Error de DB al cargar permisos en el middleware → 500**, no 401/403: el
  caller **sí** está autenticado, el fallo es interno. Distinto del 401 (no
  autentica) y del 403 (autentica, sin permiso).
- **Red-team id malformado**: un id no-UUID se compara como string contra la
  columna UUID (texto en SQLite) → 0 filas → **404**, nunca 500/panic. No se
  valida formato UUID en el handler a propósito: la base ya rechaza sin error,
  y 404 es uno de los códigos que el contrato acepta ("404/400"). Test
  explícito: `TestMalformedIDIsNotFound`.

## Trade-offs

- **Permisos se cargan por request** (sin cache). v1 no tiene cache de grants;
  el catálogo es chico y la consulta es una JOIN simple. Un contrato futuro
  puede cachear si el perfil lo pide.
- **Autorización solo por permiso de rol, sin distinción de autoría** (alcance
  explícito del contrato v1): quien tiene `content.update` edita cualquier
  fila. La distinción "propio vs cualquiera" queda para un contrato posterior,
  sin rearquitecturar — el middleware ya pasa la `Identity` por contexto, así
  que un gate de autoría futuro se agrega en el handler.
- **`/whoami` ahora pasa por `resolveIdentity`** que no consulta permisos
  (solo identidad), así que su comportamiento y costo son idénticos a los de
  CONTRACT-02 — no agrega un hit extra de DB. Los permisos solo se cargan en
  rutas gated.

## Criterios de aceptación — salida real

`go build ./...` y `go vet ./...`: limpios (sin output, sin errores).

`go test ./... -count=1` — corrido DOS veces:

**Run #1:**
```
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	2.753s
ok  	github.com/MauricioPerera/librarian/internal/config	0.651s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.293s
ok  	github.com/MauricioPerera/librarian/internal/server	5.305s
ok  	github.com/MauricioPerera/librarian/internal/store	2.263s
```

**Run #2:**
```
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	2.733s
ok  	github.com/MauricioPerera/librarian/internal/config	0.663s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.282s
ok  	github.com/MauricioPerera/librarian/internal/server	5.256s
ok  	github.com/MauricioPerera/librarian/internal/store	2.225s
```

### `content.update` sembrado por el seed existente
`TestContentUpdatePermissionSeeded` — PASS. Tras `SeedCatalogs` (dentro de
`openAuthMux`), `SELECT id FROM permissions WHERE name='content.update'`
devuelve una fila no vacía.

### POST /articles con content.create
`TestCreateArticle` — PASS: con permiso → 201 + fila real en DB (`SELECT title`
confirma `"Hello"`); sin permiso (rol sin grants) → 403; sin autenticar → 401.

### GET /articles y GET /articles/{id}
`TestListAndGetArticles` — PASS: JWT sin permisos → 200 (lista y get uno); API
key → 200 (lista); sin autenticar → 401 (lista y get).

### PUT /articles/{id} con content.update
`TestUpdateArticle` — PASS: con permiso → 200 + cambios persistidos
(`SELECT title,body` confirma `New`/`NewBody`); sin permiso → 403 y la fila
queda intacta (`title` sigue `New`).

### POST /articles/{id}/publish con content.publish
`TestPublishArticle` — PASS: sin permiso → 403 (sigue NULL); primer publish →
200 + `published_at` seteado (confirmado en DB con `sql.NullString`); segundo
publish → 200 (no falla) y `published_at` **idéntico** al primero (idempotente).

### DELETE /articles/{id} con content.delete
`TestDeleteArticle` — PASS: sin permiso → 403 y la fila sigue; con permiso →
204 + la fila ya no existe.

### Rutas sobre id inexistente → 404 (no 500/panic)
`TestNotFound` — PASS: GET/PUT/publish/DELETE sobre un UUID de formato válido
pero inexistente → 404 los cuatro.

### Red-team: id con formato inválido
`TestMalformedIDIsNotFound` — PASS: `not-a-uuid`, `x'OR'1'='1`, `!!!`, `abc`
→ 404 los cuatro (nunca 500). Sanity: `SELECT COUNT(*) FROM articles` sigue
funcionando — la tabla existe, la inyección no hizo nada (query
parametrizado).

### CONTRACT-02 sigue pasando tras el refactor de T2
`go test ./internal/server/ -run 'TestLogin|TestWhoami' -count=1 -v`:
```
=== RUN  TestLoginSuccess          --- PASS (0.14s)
=== RUN  TestLoginInvalidCredentials --- PASS (0.18s)
=== RUN  TestWhoamiJWT             --- PASS (0.13s)
=== RUN  TestWhoamiAPIKey          --- PASS (0.05s)
=== RUN  TestWhoamiRevokedAPIKeyRejected --- PASS (0.05s)
=== RUN  TestWhoamiNoCredentials    --- PASS (0.04s)
=== RUN  TestWhoamiGarbageToken     --- PASS (0.05s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/server	4.275s
```
El refactor de `handleWhoami` (ahora vía `resolveIdentity`) no rompió
`/whoami` ni `/auth/login`: respuesta y códigos idénticos a CONTRACT-02.

## ABORTAR SI — no se disparó
El refactor de T2 preservó el contrato público de `/whoami` y `/auth/login`
(respuesta/códigos sin cambios), verificado por los tests de CONTRACT-02
corridos tal cual. No hubo condición de aborto.

## Archivos tocados
- `internal/schema/schema.go` — T1 (agregado `content.update`).
- `internal/server/authz.go` — nuevo, T2 (middleware + resolución reusable).
- `internal/server/articles.go` — nuevo, T3 (CRUD).
- `internal/server/server.go` — T2 (refactor `handleWhoami`) + T3 (rutas en
  `NewMux`).
- `internal/server/server_articles_test.go` — nuevo, T4.

Nada fuera de `D:\Repo\librarian`. `sqlite-postgres-compat` intacto. Sin
`git add/commit`. Sin dependencias nuevas.
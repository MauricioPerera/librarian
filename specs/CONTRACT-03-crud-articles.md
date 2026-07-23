# Contrato 03 — API CRUD de tipos de contenido con autorización por rol

Prerrequisitos: `CONTRACT-02` completado y verificado (`aaab3b1`). Autenticación dual funcionando
(`/auth/login` → JWT, API keys), catálogo de roles/permisos poblado. Este contrato es el primero
donde el catálogo de permisos deja de ser solo datos y empieza a hacer cumplir algo real: rutas
de la API protegidas por permiso, no solo por identidad válida.

> Alcance deliberado (v1, documentado explícito): autorización por PERMISO DE ROL únicamente, sin
> distinción de autoría (quien tiene `content.update` puede editar cualquier fila, no solo la
> propia). La distinción "editar contenido propio vs. cualquiera" (como WordPress real) queda
> fuera de este contrato — se agrega en uno posterior si se necesita, sin rearquitecturar lo de
> acá.

## RECON ya resuelto (no re-verificar)

- Catálogo de permisos actual (`internal/schema/schema.go`, `Permissions`): `content.create`,
  `content.publish`, `content.delete`, `users.manage`, `roles.manage`. **Falta `content.update`**
  — T1 lo agrega.
- La resolución de identidad (JWT o API key → roles/permiso) hoy vive INLINE dentro de
  `handleWhoami` (`internal/server/server.go`), no es reusable por otras rutas. T2 la extrae.
- Go 1.26 (`go.mod`) soporta patrones de ruta con wildcard en `http.ServeMux`
  (`GET /articles/{id}`, `r.PathValue("id")`) — stdlib, sin dependencia nueva.

## T1 — Extender el catálogo de permisos

FIX/OBJETIVO: agregar `content.update` a `schema.Permissions`. Sembrado por el seed idempotente
de CONTRACT-02 (T1) sin tocar su lógica — solo el dato. `Schema.Validate()`/`CompileDDL` no se
ven afectados (es una fila de catálogo, no cambia el esquema).

## T2 — Middleware de autorización reusable

Hoy la resolución JWT-o-API-key vive duplicada como lógica inline en `handleWhoami`. Sin
extraerla, cada ruta nueva de T3 repetiría el mismo bloque.

FIX/OBJETIVO: una función/middleware (`RequirePermission(permission string) func(http.Handler)
http.Handler` o equivalente — firma exacta a criterio del dev, pero debe ser un `http.Handler`
wrapper reusable, no una función que cada handler llama a mano) que: (a) resuelve identidad
(intenta JWT vía `auth.VerifyJWT`, si falla intenta API key vía `auth.VerifyAPIKey` — mismo orden
que `handleWhoami` hoy); (b) si ninguna resuelve → `401`; (c) si resuelve, obtiene el conjunto de
permisos de la identidad (para JWT: unión de permisos de todos sus `Roles`; para API key: permisos
del único `RoleID` asociado) vía `role_permissions`; (d) si el permiso pedido no está en el
conjunto → `403`; (e) si está, deja pasar la request al handler siguiente, con la identidad
disponible para el handler (vía `context.Context`, no una variable global).

Refactorizá `handleWhoami` para usar la MISMA resolución de identidad de este middleware (sin
duplicar lógica) — puede seguir siendo pública (sin requerir permiso), solo reusa la función de
resolución.

## T3 — CRUD de `articles`

FIX/OBJETIVO: rutas REST sobre `articles` (el tipo de contenido de ejemplo de CONTRACT-01),
usando `database/sql` directo contra `compat.Store.DB` (mismo patrón que T2 de CONTRACT-02, no
un ORM):

- `POST /articles` — crea (borrador: `published_at` NULL). Requiere `content.create`. Body:
  `{title, body}`. `author_id` = id del usuario autenticado (si la identidad es JWT/usuario) —
  si la identidad es una API key (sin usuario humano detrás), definí y documentá qué pasa
  (¿rechazar con error claro "requires a user identity", o permitir con `author_id` NULL si la
  columna lo permite? — decisión de diseño, documentala en el reporte con el porqué).
- `GET /articles` — lista (paginación simple: `?limit=&offset=`, default `limit=20`). Requiere
  solo identidad válida (cualquier rol autenticado, sin permiso específico — ver nota de alcance
  arriba: lectura no está permission-gated en v1).
- `GET /articles/{id}` — uno por id. Mismo requisito que la lista. `404` si no existe.
- `PUT /articles/{id}` — actualiza `title`/`body` (NO `published_at` — eso es la ruta de abajo).
  Requiere `content.update`. `404` si no existe.
- `POST /articles/{id}/publish` — setea `published_at = CURRENT_TIMESTAMP` si estaba NULL (no-op
  si ya estaba publicado — idempotente). Requiere `content.publish`. `404` si no existe.
- `DELETE /articles/{id}` — borra. Requiere `content.delete`. `404` si no existe.

Todas las respuestas: JSON, mismo estilo de envelope de error que CONTRACT-02
(`{"error": "<mensaje>"}`) en fallos.

## T4 — Verificación de autorización end-to-end

FIX/OBJETIVO: tests HTTP reales (`httptest`) que prueben, para AL MENOS una ruta protegida por
cada permiso nuevo/existente relevante (`content.create`, `content.update`, `content.publish`,
`content.delete`): (a) identidad CON el permiso → succeeds; (b) identidad autenticada pero SIN el
permiso → `403`; (c) sin autenticar → `401`. Más: `GET /articles` con cualquier identidad
autenticada (sin permiso específico) → `200`.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] `content.update` sembrado por el seed existente (test: tras seed, `SELECT` confirma la fila).
- [ ] Test real: `POST /articles` con `content.create` → `201` + fila real en DB; sin el permiso
  → `403`; sin autenticar → `401`.
- [ ] Test real: `GET /articles` y `GET /articles/{id}` con cualquier identidad autenticada (sin
  permiso específico) → `200`; sin autenticar → `401`.
- [ ] Test real: `PUT /articles/{id}` con `content.update` → `200` + cambios persistidos; sin el
  permiso → `403`.
- [ ] Test real: `POST /articles/{id}/publish` con `content.publish` → `200` + `published_at`
  seteado; llamarlo DOS veces → segunda vez no falla (idempotente) y `published_at` no cambia.
- [ ] Test real: `DELETE /articles/{id}` con `content.delete` → `200`/`204` + fila ya no existe;
  sin el permiso → `403`.
- [ ] Test real: cualquier ruta sobre un `id` inexistente → `404` (no `500`, no pánico).
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias nuevas (stdlib + lo ya resuelto en CONTRACT-01/02 alcanza — `http.ServeMux`
  con wildcards, `database/sql` directo).
- T1→T4 son sustancialmente secuenciales (T2 depende de nada nuevo pero T3 depende de T2, T4
  depende de T3) — un solo perímetro, un solo dev.
- NO commitear (el orquestador commitea tras verificar).
- ABORTAR SI: el refactor de T2 rompe el comportamiento existente de `/whoami` o `/auth/login` de
  CONTRACT-02 de forma irresoluble sin cambiar su contrato público (respuesta/códigos) → PARAR,
  documentar con evidencia (qué test de CONTRACT-02 se rompe y por qué), no forzar.

## Checklist antes de delegar

- [ ] RECON corrido: catálogo de permisos y ubicación de la lógica de identidad confirmados
  arriba (no re-investigar).
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿un `id` con formato inválido (no UUID) en la URL podría dar `500` en vez de
  `404`/`400`? → probarlo explícito. ¿`PUT`/`DELETE`/`publish` sobre un id de OTRO tipo de
  contenido (si existiera) devuelven `404` limpio, no un error de SQL crudo filtrado al cliente?
  → mismo criterio, aunque hoy solo exista `articles`.
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Condiciones de aborto explícitas (arriba).

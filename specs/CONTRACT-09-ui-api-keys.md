# Contrato 09 — UI de API keys (alta, revocación, listado)

Prerrequisitos: `CONTRACT-01`..`CONTRACT-08` completos (`e3a129b`). Cuarto y último contrato de
la fase 2 (UI) — cierra `DEFINITION-UI.md` por completo.

Alcance fijado por `DEFINITION-UI.md`: alta (mostrar el secreto una única vez), revocación,
listado (sin volver a mostrar el secreto).

## RECON ya resuelto (no re-investigar)

- **Gap real encontrado al preparar este contrato (arreglarlo es parte de T1, no un extra):**
  `auth.RevokeAPIKey(ctx, db, secret string)` (`internal/auth/apikey.go`) revoca buscando por el
  **secreto en texto plano** (lo hashea y busca por `key_hash`) — pero el secreto NUNCA se guarda
  más allá del momento de creación (por diseño, `MintAPIKey` lo devuelve una sola vez y no lo
  persiste). La UI de listado, por lo tanto, JAMÁS tiene el secreto disponible para revocar por
  ese camino. Necesitás una función NUEVA (`RevokeAPIKeyByID(ctx, db, id string) error` o el
  nombre que prefieras) que revoque por `id` (la fila, no el secreto) — mismo criterio de
  idempotencia que la función existente (revocar una key ya revocada no es error). NO cambies la
  firma de `RevokeAPIKey` existente (la usa la ruta JSON si existe, o queda como utilidad; no la
  toques si no hace falta).
- `auth.ListAPIKeys` NO existe — agregala (mismo paquete, mismo patrón `database/sql`
  parametrizado). Necesita el NOMBRE del rol para mostrarlo en la UI (no solo el `role_id`), así
  que hacé el `JOIN` con `roles` en la query, no una resolución N+1 aparte.
- **Decisión de permiso YA TOMADA (no la re-decidas):** el catálogo de permisos es fijo en código
  (`schema.Permissions`) y este contrato NO agrega uno nuevo (agregar `apikeys.manage` sería
  expandir el catálogo, fuera del alcance que fijó `DEFINITION-UI.md`). Las API keys son un
  recurso de control de acceso igual que los usuarios — gateá TODAS las rutas de escritura de este
  contrato (crear, revocar) con el permiso YA EXISTENTE `users.manage`, el mismo que gatea
  CONTRACT-08.
- Namespace de rutas: `/admin/api-keys` (paralelo a `/admin/users`).
- `internal/schema/schema.go` `apiKeysTable()`: `label`, `key_hash` (nunca se muestra),
  `role_id` (FK a `roles`), `created_at`, `revoked_at` (nullable — no-null = revocada). Sin
  `UNIQUE` en `label` — dos keys pueden tener el mismo label, no lo trates como identificador.
- El patrón de CONTRACT-06/07/08 sigue aplicando tal cual: `requireSession` (lectura),
  `requireSessionPermission("users.manage")` (escritura), un template set por página,
  `renderNotFound`/`renderForbidden` reusables, `httptest.NewTLSServer` para tests con cookie.

## T1 — Datos base (fix de revocación por id + listado)

FIX/OBJETIVO: `auth.RevokeAPIKeyByID` (RECON, nueva) y `auth.ListAPIKeys` (RECON, nueva, con
`JOIN roles` para el nombre del rol) — ambas con tests unitarios propios, independientes de la
UI. `ListAPIKeys` debe exponer si cada key está revocada (y desde cuándo) sin exponer nunca
`key_hash`.

## T2 — Crear (con secreto mostrado una única vez)

FIX/OBJETIVO: `GET /admin/api-keys/new` — formulario (label + selector de rol, de
`schema.Roles`). `POST /admin/api-keys` (gateado `users.manage`) — llama `auth.MintAPIKey`
(reusala tal cual, no la reimplementes) y, en vez de redirigir, renderiza DIRECTAMENTE una página
de éxito que muestra el secreto en texto plano CON una advertencia clara ("no se va a volver a
mostrar, copialo ahora") y un link para volver al listado. Esa es la ÚNICA vez que el secreto
existe en cualquier respuesta HTTP de este contrato — ninguna otra ruta (listado, detalle si lo
hubiera) lo vuelve a mostrar ni lo persiste en ningún lado más allá de ese render.

## T3 — Listar y revocar

FIX/OBJETIVO: `GET /admin/api-keys` — lista (label, rol, creada, estado: activa/revocada,
NUNCA el secreto ni el hash). Botón de revocar por fila (gateado `users.manage`) vía
`hx-post`/`hx-delete` a `/admin/api-keys/{id}/revoke` (a tu criterio el verbo, documentalo) que
llama `RevokeAPIKeyByID` y actualiza la fila in-place (mismo patrón htmx que CONTRACT-07 con
`publish`: la fila cambia de "activa" a "revocada", no desaparece — una key revocada sigue
siendo un registro histórico válido, no se borra). Una key ya revocada mostrada de nuevo (llamada
repetida) no rompe nada (idempotente) y el botón de revocar deja de tener sentido en esa fila
(mostrá el estado en vez del botón, a tu criterio la UI exacta).

## T4 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces, `httptest.NewTLSServer`
para lo que dependa de la cookie):
- Round-trip completo: crear una key vía UI real → capturar el secreto de la página de éxito →
  usarlo como `Authorization: Bearer <secret>` contra una ruta JSON real protegida (`GET
  /whoami`) → 200 con `"auth":"apikey"`. Revocar esa key vía UI → repetir la misma llamada a
  `/whoami` con el mismo secreto → 401 (`ErrAPIKeyRejected`).
- Gateo por permiso: sesión SIN `users.manage` intentando `POST /admin/api-keys` o revocar
  directo por HTTP (curl+cookie, no solo desde el botón) → 403 HTML, sin efecto.
- Confirmá que el listado NUNCA incluye el secreto ni el `key_hash` en el HTML (buscalo
  explícitamente en el body de la respuesta y confirmá su ausencia).
- Confirmá explícitamente que las rutas de CONTRACT-01..08 (JSON completo incluyendo `/whoami`
  con API key existente si hay tests de eso, UI de articles/usuarios/roles/login) siguen
  funcionando exactamente igual — pegá esa salida.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: `RevokeAPIKeyByID` y `ListAPIKeys` con tests unitarios propios (incluyendo el `JOIN`
  con el nombre real del rol).
- [ ] T2: alta real vía UI, secreto mostrado UNA VEZ con advertencia clara; ninguna otra ruta lo
  vuelve a exponer.
- [ ] T3: listado sin secreto/hash; revocar reflejado in-place; key revocada no desaparece del
  listado (registro histórico).
- [ ] T4: round-trip completo (crear→usar la key real en una llamada JSON real→revocar→la misma
  llamada ahora 401) probado; gateo por permiso probado con red-team explícito; ausencia de
  secreto/hash en el HTML confirmada; rutas de contratos anteriores confirmadas sin cambios.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias Go nuevas.
- NO commitear (el orquestador commitea tras verificar).
- NO agregues un permiso nuevo al catálogo (`users.manage` es el permiso decidido, ver RECON) ni
  toques `schema.Permissions`/`schema.Roles`.
- El secreto en texto plano NUNCA aparece en ningún log, archivo, ni ruta HTML más allá de la
  ÚNICA página de éxito de T2 — revisalo vos mismo antes de entregar (buscá literalmente el
  prefijo `lbk_` en cualquier otra respuesta/template que no sea esa).
- No rompas ningún contrato/formato existente (JSON, UI de login/home/articles/usuarios/roles) —
  tienen tests propios que deben seguir en verde SIN que los toques.

## Checklist antes de delegar

- [ ] RECON corrido: gap de `RevokeAPIKey`-por-secreto-vs-por-id identificado (crítico, sin esto
  la UI no puede revocar nada), permiso `users.manage` confirmado como el gateo (no se inventa
  uno nuevo), namespace `/admin/api-keys` decidido.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿la página de listado, vista con las herramientas del navegador (o un `curl` +
  grep del body), expone el secreto o el hash en CUALQUIER forma (atributo `value`, comentario
  HTML, JSON embebido)? Debe ser una ausencia verificada, no asumida. ¿Revocar una key que ya
  estaba revocada (doble click, o un curl repetido) rompe algo? Debe ser un no-op idempotente.
  ¿Una key con `role_id` de un rol que después se le sacaron todos los permisos sigue
  autenticando pero sin acceso a nada gateado? (comportamiento esperado y correcto — no es un bug
  de este contrato, pero confirmalo si tenés tiempo, no es obligatorio).
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación EN NAVEGADOR pendiente la hace el orquestador (Claude Browser) después de
  integrar.

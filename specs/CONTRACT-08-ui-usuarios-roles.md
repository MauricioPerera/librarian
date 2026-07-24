# Contrato 08 — UI de usuarios y roles (asignación, no edición del catálogo)

Prerrequisitos: `CONTRACT-01`..`CONTRACT-07` completos (`f1862dc`). Tercer contrato de la fase 2.

Alcance fijado por `DEFINITION-UI.md`: gestión de usuarios (alta, edición, estado, asignación de
roles EXISTENTES) + vista de solo lectura del catálogo de roles/permisos. NO se crean ni editan
roles ni permisos — el catálogo sigue fijo en código (`schema.Roles`, `schema.Permissions`).

## RECON ya resuelto (no re-investigar)

- **Gap real de seguridad encontrado (arreglarlo es parte de T1, no un extra — sin esto el toggle
  de estado de la UI sería decorativo):** la tabla `users` ya tiene una columna `status` (`CHECK
  IN ('active','suspended','invited')`, `internal/schema/schema.go`), pero
  `auth.VerifyCredentials` (`internal/auth/users.go`) NUNCA la lee ni la chequea — hoy un usuario
  con `status='suspended'` puede loguearse exactamente igual que uno `active`. Como parte de este
  contrato, extendé `VerifyCredentials` para rechazar login de cualquier usuario cuyo `status` no
  sea `'active'`, con el MISMO `ErrInvalidCredentials` genérico que ya usa (anti-enumeración: un
  intento de login de un usuario suspendido no debe dar un mensaje distinto al de "no existe" o
  "password incorrecta" — filtrar eso sería el mismo tipo de leak que la función ya evita para
  usuarios inexistentes). Mismo criterio de timing: si agregás el chequeo de status DESPUÉS del
  bcrypt compare (no antes), no introducís una rama más rápida que filtre el estado por timing.
- El catálogo de permisos YA incluye `users.manage` y `roles.manage`
  (`internal/schema/schema.go`), sin uso hasta ahora — `users.manage` es el permiso que gatea
  TODAS las rutas de escritura de este contrato (crear/editar/cambiar estado/asignar roles).
  `roles.manage` NO se usa en este contrato (la vista de roles/permisos es de solo lectura, exige
  solo sesión — igual que el resto de las vistas de lectura de la UI); queda reservado para una
  futura capacidad fuera de alcance (catálogo editable en runtime), no lo actives sin necesidad.
- `internal/auth/users.go` NO tiene hoy `ListUsers`, `UpdateUserStatus` ni una función para
  reemplazar la asignación de roles de un usuario existente — son funciones NUEVAS que agregás acá
  (mismo paquete, mismo patrón `database/sql` parametrizado que `CreateUser`/`VerifyCredentials`;
  reusá `roleIDsForNames`/`rolesForUser` donde aplique, no las dupliques).
- `internal/server/authz.go` ya tiene `permissionsForRoleID` — reusalo (o el patrón que use) para
  la vista de solo lectura de permisos-por-rol.
- `internal/server/ui.go`/`ui_articles.go` (CONTRACT-06/07) son el patrón a seguir:
  `requireSession` para lectura, `requireSessionPermission("users.manage")` para escritura, un
  template set por página, helpers `renderNotFound`/`renderForbidden` ya reusables tal cual.
- Namespace de rutas: `/admin/users` (paralelo a `/admin/articles`, mismo `ServeMux`, sin
  colisión).

## T1 — Fix de seguridad (status) + middleware/datos base

FIX/OBJETIVO: (a) el fix de `VerifyCredentials` del RECON, con un test específico (usuario
`suspended` → login rechazado, mismo mensaje genérico, verificado por timing-neutral igual que el
caso existente); (b) `auth.ListUsers`, `auth.UpdateUserStatus`, y una función para reemplazar la
asignación de roles de un usuario (nombre a tu criterio) — todas con tests unitarios propios,
independientes de la UI.

## T2 — Listar, crear, ver detalle

FIX/OBJETIVO: `GET /admin/users` — lista (email, estado, roles). `GET /admin/users/new` +
`POST /admin/users` (gateado `users.manage`) — alta con email/password/roles iniciales
(checkboxes de los 4 roles fijos), reusando `auth.CreateUser`. `GET /admin/users/{id}` — detalle
con estado actual y roles actuales.

## T3 — Editar estado y roles

FIX/OBJETIVO: desde el detalle (o una vista de edición separada, a tu criterio), gateado
`users.manage`: cambiar `status` (selector con los 3 valores válidos del `CHECK`) y reemplazar la
asignación de roles (checkboxes). Un id inexistente/malformado en cualquier ruta de este contrato
→ 404 HTML (mismo patrón que `articles`). Usá htmx para las interacciones de escritura (mismo
patrón que CONTRACT-07: `hx-put`/`hx-post` con swap o `HX-Redirect`, a tu criterio, documentalo).

## T4 — Vista de solo lectura de roles y permisos

FIX/OBJETIVO: `GET /admin/roles` (o el path que seas consistente, documentalo) — lista los 4
roles fijos y, para cada uno, sus permisos actualmente otorgados (vía `role_permissions`). Exige
SOLO sesión (como el resto de las vistas de lectura) — sin edición, ningún botón de escritura acá.

## T5 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces, `httptest.NewTLSServer` para
lo que dependa de la cookie):
- Round-trip completo: crear usuario vía UI → aparece en la lista → asignar un rol → suspender →
  intentar login con ese usuario (JSON `/auth/login` O el form `/login` de UI, tu elección, pero
  probalo) → rechazado con el mensaje genérico → reactivar (`active`) → login exitoso.
- Gateo por permiso: sesión SIN `users.manage` intentando `POST /admin/users` o cambiar estado
  directo por HTTP (curl+cookie, no solo desde el botón) → 403 HTML.
- Vista de roles/permisos: confirmá que refleja el estado REAL de `role_permissions` (otorgá un
  permiso a un rol en el test, confirmá que aparece; no hardcodees la vista con el catálogo fijo
  de nombres sin consultar la tabla real).
- Confirmá explícitamente que las rutas de CONTRACT-01..07 (JSON completo, login/logout/home,
  articles UI) siguen funcionando exactamente igual — pegá esa salida.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: fix de `VerifyCredentials` con test explícito (usuario suspendido rechazado, mensaje
  genérico). Funciones de datos nuevas con tests unitarios propios.
- [ ] T2: alta real vía UI, aparece en la lista y en el detalle con los datos correctos.
- [ ] T3: cambio de estado y de roles reflejado correctamente; id inexistente → 404 HTML.
- [ ] T4: vista de roles/permisos refleja el estado real de `role_permissions`, no un hardcode.
- [ ] T5: round-trip completo (crear→asignar rol→suspender→login rechazado→reactivar→login OK)
  probado; gateo por permiso probado con red-team explícito; rutas de contratos anteriores
  confirmadas sin cambios.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias Go nuevas.
- NO commitear (el orquestador commitea tras verificar).
- NO se crean ni editan roles ni permisos desde la UI (el catálogo sigue fijo en código) — si en
  algún punto sentís que "hace falta" para completar la tarea, es una señal de que estás fuera de
  alcance: PARÁ y documentalo, no lo implementes.
- El fix de `VerifyCredentials` (T1) es SEGURIDAD — no lo hagas apurado. Mismo mensaje genérico,
  mismo timing-neutral, test explícito. Si tenés dudas de diseño ahí, documentalas en el reporte
  en vez de improvisar.
- No rompas ningún contrato/formato existente (JSON de `articles`/`auth`/`whoami`, UI de
  `articles`/home/login) — tienen tests propios que deben seguir en verde SIN que los toques.

## Checklist antes de delegar

- [ ] RECON corrido: gap de `VerifyCredentials`+`status` identificado (arriba, crítico), permisos
  `users.manage`/`roles.manage` confirmados en el catálogo, namespace `/admin/users` decidido.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿qué pasa si alguien intenta asignarle a un usuario un rol que NO está en el
  catálogo fijo (nombre inventado)? (debe rechazarse, mismo criterio que `roleIDsForNames` ya
  usa). ¿Un usuario `invited` (no `active`, no `suspended`) puede loguearse? (según el fix de T1,
  NO — solo `active` puede). ¿El cambio de estado de un usuario A CAMBIA algo de una sesión YA
  ACTIVA de ese usuario (JWT ya emitido)? Documentá la respuesta REAL (probablemente NO hasta que
  expire el JWT de 24h — es una limitación conocida, no la arregles acá, pero DECILA en el reporte
  si es el caso, no la escondas).
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación EN NAVEGADOR pendiente la hace el orquestador (Claude Browser) después de
  integrar.

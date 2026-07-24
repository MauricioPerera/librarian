# CONTRACT-08 — Reporte de implementación (UI de usuarios y roles)

Tercer contrato de la fase 2 (UI). Prerrequisitos CONTRACT-01..07 en `f1862dc`.
Implementación sobre `D:\Repo\librarian`, sin dependencias Go nuevas, sin tocar
`sqlite-postgres-compat`, sin commitear (lo hace el orquestador).

Archivos tocados / creados:

- `internal/auth/users.go` — fix de seguridad + funciones de datos nuevas (T1).
- `internal/auth/users_contract08_test.go` — tests unitarios T1 (NUEVO).
- `internal/server/ui_users.go` — handlers y rutas `/admin/users` + `/admin/roles` (NUEVO).
- `internal/server/ui.go` — `//go:embed` extendido + registro de rutas.
- `internal/server/templates/users_list.html`, `users_new.html`, `users_detail.html`,
  `roles_list.html` — vistas (NUEVOS).
- `internal/server/templates/home.html` — enlaces de navegación a usuarios/roles.
- `internal/server/assets/app.css` — estilos de badges de estado y checkboxes de roles.
- `internal/server/server_ui_users_test.go` — tests de aceptación T2-T5 + red-team (NUEVO).

---

## T1 — Fix de seguridad (status) + datos base

### (a) Fix de `auth.VerifyCredentials`

**El gap:** la columna `users.status` (`CHECK IN ('active','suspended','invited')`)
existía pero `VerifyCredentials` nunca la leía — un usuario `suspended` se logueaba
igual que uno `active`, y el toggle de estado de la UI habría sido decorativo.

**El fix:** el `SELECT` ahora trae también `status`, y DESPUÉS del `bcrypt.CompareHashAndPassword`
se rechaza cualquier `status != "active"` con el **mismo** `ErrInvalidCredentials`
genérico (no hay mensaje distinto para "suspendido" vs "no existe" vs "password
incorrecta" → anti-enumeración). El chequeo va **después** del compare a propósito:
poner el test de status antes crearía una rama más rápida para el usuario suspendido
y filtraría el estado por timing — exactamente el leak que la función ya evita para
emails inexistentes con el `dummyHash`. Documentado en el comentario de la función.

`invited` cae en la misma regla: solo `active` autentica (constante `statusActive`).

### (b) Funciones de datos nuevas (mismo patrón `database/sql` parametrizado)

- `ListUsers(ctx, db) ([]UserRecord, error)` — todos los usuarios con status + roles,
  ordenados por email. Reusa `rolesForUser` por usuario (N+1 aceptable en página admin,
  catálogo chico; evita duplicar una query de agregación).
- `GetUser(ctx, db, id) (UserRecord, bool, error)` — detalle por id; `found=false`
  para id inexistente **o malformado** (nunca error SQL crudo → el handler mapea a 404).
- `UpdateUserStatus(ctx, db, id, status)` — valida `status` contra `UserStatuses`
  (`ErrInvalidStatus` antes de tocar SQL, defensa en profundidad sobre el CHECK);
  `RowsAffected == 0` → `ErrUserNotFound`.
- `SetUserRoles(ctx, db, userID, roleNames)` — reemplaza el set completo en una
  transacción: verifica existencia del usuario (`ErrUserNotFound`), resuelve nombres
  con `roleIDsForNames` **antes** de mutar (rol inventado → `ErrUnknownRole`, tabla
  intacta), borra `user_roles` del usuario, inserta el nuevo set. `roleNames` vacío
  = quitar todos los roles (válido).

Se agregaron los sentinels `ErrUserNotFound`, `ErrUnknownRole`, `ErrInvalidStatus`
y el slice exportado `UserStatuses` (fuente única para validación + selector de la UI).
`roleIDsForNames` ahora envuelve `ErrUnknownRole` (antes texto plano) — reusado por
`CreateUser` y `SetUserRoles`; ningún test previo dependía del mensaje.

### Verificación T1 (salida real)

```
=== RUN   TestVerifyCredentialsSuspendedRejected
--- PASS: TestVerifyCredentialsSuspendedRejected (0.32s)
=== RUN   TestListUsers
--- PASS: TestListUsers (0.13s)
=== RUN   TestGetUser
--- PASS: TestGetUser (0.08s)
=== RUN   TestUpdateUserStatus
--- PASS: TestUpdateUserStatus (0.08s)
=== RUN   TestSetUserRoles
--- PASS: TestSetUserRoles (0.09s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/auth	2.990s
```

`TestVerifyCredentialsSuspendedRejected` verifica: (1) usuario activo con password
correcta loguea; (2) `suspended` e `invited` con password correcta → rechazados con
`ErrInvalidCredentials`, mensaje **byte-idéntico** al de usuario inexistente
(assert explícito de no-leak); (3) reactivar restaura el login.
`TestSetUserRoles` incluye el red-team de rol inventado (`superadmin` → `ErrUnknownRole`,
roles sin cambiar).

---

## T2 — Listar, crear, ver detalle

Rutas (paralelas a `/admin/articles`, mismo `ServeMux`):

- `GET /admin/users` — lista (email, estado, roles). Solo sesión.
- `GET /admin/users/new` — form de alta con checkboxes de los 4 roles fijos. Solo sesión.
- `POST /admin/users` — alta (email/password/roles), **gateado `users.manage`**.
  Reusa `auth.CreateUser` (que hashea y crea el usuario como `active`). Éxito → 303 a
  la lista. Email vacío/password vacío o rol inventado/email duplicado → re-render 400
  con el error, preservando email y checkboxes.
- `GET /admin/users/{id}` — detalle. Solo sesión. Id inexistente/malformado → 404 HTML.

Precedencia de rutas: `GET /admin/users/new` (literal) gana sobre `GET /admin/users/{id}`
(wildcard) por reglas de `ServeMux` Go 1.22+, igual que articles.

### Verificación T2 (salida real)

```
=== RUN   TestAdminUserCreateAppearsInListAndDetail
--- PASS: TestAdminUserCreateAppearsInListAndDetail (0.20s)
=== RUN   TestAdminUserCreateUnknownRoleRejected
--- PASS: TestAdminUserCreateUnknownRoleRejected (0.19s)
```

Alta real vía POST form → aparece en la lista (email + badge `active`) y en el detalle
(email, estado, roles `editor`/`author`), confirmado también contra la DB. Red-team de
rol inventado → 400 y cero filas creadas.

---

## T3 — Editar estado y roles

Desde el detalle (página **combinada** detalle + edición, ver decisiones de diseño),
gateado `users.manage`:

- `POST /admin/users/{id}/status` — `<select>` con los 3 valores del CHECK, enviado por
  `hx-post`. Éxito → header `HX-Redirect` al mismo detalle. Id inexistente → 404 HTML;
  valor de status inválido (request crafteado) → 400.
- `POST /admin/users/{id}/roles` — checkboxes de los 4 roles, `hx-post`, reemplaza el set
  completo. Éxito → `HX-Redirect` al detalle. Id inexistente → 404; rol inventado → 400.

### Verificación T3 (salida real)

```
=== RUN   TestAdminUserStatusChange
--- PASS: TestAdminUserStatusChange (0.19s)
=== RUN   TestAdminUserRolesChange
--- PASS: TestAdminUserRolesChange (0.19s)
=== RUN   TestAdminUserDetailMissingIs404
--- PASS: TestAdminUserDetailMissingIs404 (0.14s)
```

Cambio de estado y de roles reflejado en la DB tras el `hx-post` real (con verificación
del header `HX-Redirect`); id inexistente en detalle/status/roles → 404 HTML (no JSON).

---

## T4 — Vista de solo lectura de roles y permisos

`GET /admin/roles` — solo sesión (ningún botón de escritura). Lista los 4 roles fijos
y, por cada uno, sus permisos **actualmente otorgados** leídos de `role_permissions` vía
`rolesWithPermissions` (que reusa `permissionsForRoleID` de `authz.go`). **No** hardcodea
el catálogo de nombres: consulta la tabla real.

### Verificación T4 (salida real)

```
=== RUN   TestAdminRolesViewReflectsRealGrants
--- PASS: TestAdminRolesViewReflectsRealGrants (0.16s)
```

El test confirma que `editor` **no** muestra `content.publish` antes del grant, luego
otorga el permiso en la tabla real y confirma que aparece en la vista, acotado a la fila
del rol `editor` (no un match global). Prueba que refleja `role_permissions`, no un hardcode.

---

## T5 — Verificación end-to-end

### Round-trip completo (salida real)

```
=== RUN   TestAdminUserRoundTripLoginRejection
--- PASS: TestAdminUserRoundTripLoginRejection (0.39s)
```

Crear usuario vía UI (`POST /admin/users`) → asignar rol → puede loguear (JSON
`/auth/login` → 200) → **suspender vía la UI** (`hx-post` de estado) → login rechazado
(401) con mensaje **idéntico** al de usuario inexistente (assert de no-leak) → reactivar
vía UI → login exitoso (200). Prueba que el toggle de la UI está cableado al fix real de T1.

### Red-team: gateo por permiso server-side (salida real)

```
=== RUN   TestAdminUsersWriteWithoutPermissionServerSide
--- PASS: TestAdminUsersWriteWithoutPermissionServerSide (0.19s)
```

Sesión válida SIN `users.manage` (rol `author`) mandando `POST /admin/users` y
`POST /admin/users/{id}/status` **directo con la cookie** (no desde el botón) → 403 HTML
(marcador "Sin permiso", no envelope JSON), y nada cambia server-side (cero usuarios
creados, estado del target sin tocar). El gate es server-side.

### Rutas de contratos anteriores sin cambios (salida real)

```
--- PASS: TestUIJSONRoutesUnaffected (0.13s)
--- PASS: TestUIRoundTrip (0.12s)
--- PASS: TestAdminRoundTrip (0.18s)
--- PASS: TestAdminDeleteWithoutPermissionServerSide (0.23s)
--- PASS: TestWhoamiJWT / TestWhoamiAPIKey / TestLoginSuccess / TestHealth ... (todos PASS)
--- PASS: TestCreateArticle / TestListAndGetArticles / TestPublishArticle / TestDeleteArticle ... (todos PASS)
--- PASS: TestUILoginSuccessSetsCookie / TestUILogoutClearsCookie / TestUIHome* / TestUIForgedJWT* ... (todos PASS)
```

JSON completo (auth/whoami/articles), login/logout/home y articles UI siguen verdes sin
que se tocaran sus archivos ni sus tests.

---

## Suite completa 2× (salida real)

```
$ go build ./...
(sin salida — OK)
$ go vet ./...
(sin salida — OK)

===== RUN 1 =====
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	3.449s
ok  	github.com/MauricioPerera/librarian/internal/config	0.683s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.351s
ok  	github.com/MauricioPerera/librarian/internal/server	10.984s
ok  	github.com/MauricioPerera/librarian/internal/store	2.171s
===== RUN 2 =====
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	3.516s
ok  	github.com/MauricioPerera/librarian/internal/config	0.672s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.359s
ok  	github.com/MauricioPerera/librarian/internal/server	11.048s
ok  	github.com/MauricioPerera/librarian/internal/store	2.231s
```

`gofmt -l` sobre los archivos nuevos/modificados: limpio (sin salida).

---

## Decisiones de diseño (con su porqué)

1. **Detalle y edición combinados en `GET /admin/users/{id}`.** Una sola página muestra
   email/estado/roles actuales y, debajo, dos formularios (cambiar estado, asignar roles).
   Porqué: el flujo admin natural es "abro el usuario, veo su estado, lo cambio ahí mismo";
   separar una vista de edición aparte duplicaría navegación sin valor. Los datos de solo
   lectura y los controles de escritura conviven, pero los controles siguen gateados
   server-side por `users.manage` (no basta con esconder el botón).

2. **Path de la vista de roles/permisos: `GET /admin/roles`.** Consistente con
   `/admin/users` y `/admin/articles`. Solo sesión (como el resto de las lecturas de la UI);
   `roles.manage` **no** se usa (queda reservado para un futuro catálogo editable, fuera de
   alcance).

3. **Mecanismo de swap tras cambios: `HX-Redirect` al detalle** (no fragmento in-place).
   Porqué: tanto el cambio de estado como el de roles alteran varias partes del detalle a la
   vez (badge de estado, línea de roles, `<option>` seleccionada, checkboxes marcados). Un
   solo redirect re-renderiza todo desde la fuente de verdad sin mantener a mano un fragmento
   parcial. Mismo patrón ya probado en el update de articles (CONTRACT-07). El alta usa POST
   form → 303 a la lista (no hay fila que swappear).

4. **Reuso de `renderNotFound`/`renderForbidden` tal cual.** El contrato lo endosa
   explícitamente. Nota menor: sus textos mencionan "artículo"/"Volver a los artículos"; se
   priorizó el reuso sobre textos a medida (los tests validan status 404/403 + que no sea
   JSON, no el texto). Si se quisiera pulir el copy sería un cambio cosmético aparte.

## Respuesta pedida: ¿el cambio de estado afecta una sesión YA ACTIVA?

**No, hasta que expire el JWT (24h).** `requireSession`/`sessionIdentity` solo revalidan la
firma y expiración del JWT en cada request; **no** releen `users.status` ni `user_roles`.
Un usuario suspendido con una cookie de sesión viva conserva acceso a la UI hasta que su JWT
expire (`sessionMaxAgeSeconds = 24h`). `VerifyCredentials` rechaza el **próximo** login. Es
una **limitación conocida y documentada**, no un bug a arreglar en este contrato: cerrarla
(revalidación de status/roles por request, o un deny-list de tokens) es una decisión de
alcance nueva. Documentado también en el comentario de cabecera de `ui_users.go`.

## Fuera de alcance respetado

No se crea ni edita ningún rol/permiso desde la UI: el catálogo sigue fijo en código
(`schema.Roles`, `schema.Permissions`). La UI solo **asigna** roles existentes y **muestra**
permisos. No hizo falta salirse de este límite en ningún punto.

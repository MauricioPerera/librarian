# CONTRACT-16 — Gestión de permisos por rol (reporte)

Cierra el hueco 1 de `docs/PENDIENTES.md` (severidad ALTA): la tabla
`role_permissions` ya tiene una vía de escritura real dentro del producto. El
permiso `roles.manage`, que existía desde CONTRACT-02 sin ningún consumidor, por
fin gatea algo. **No se agregó ningún permiso nuevo** y los catálogos de roles y
permisos siguen fijos en código: este contrato edita GRANTS, no crea ni borra
roles ni permisos.

Base: `15a1857` (CONTRACT-01..15). Sin dependencias nuevas. Sin commits.

---

## Qué se implementó, por tarea

### T1 — Capa de datos (`internal/auth/roles.go`, nuevo)

- `auth.SetRolePermissions(ctx, db, roleName, permissionNames)` — reemplazo
  **atómico** del conjunto completo de permisos de un rol, calcado de
  `auth.SetUserRoles` (misma estructura, mismos invariantes):
  1. una sola transacción (`BeginTx` + `defer tx.Rollback()`);
  2. resolver el rol → si no existe, `ErrRoleNotFound` (→ 404) sin tocar nada;
  3. resolver **todos** los nombres de permiso contra el catálogo sembrado
     **antes** de mutar → un nombre desconocido aborta con
     `ErrUnknownPermission` (→ 400) y la tabla queda intacta;
  4. `DELETE` del conjunto actual + `INSERT ... ON CONFLICT DO NOTHING` del nuevo;
  5. `Commit`.
- Conjunto vacío = operación válida (revoca todo). Es la semántica legítima de
  "este rol no puede nada", y es también la que necesitaría un camino de
  reparación/bootstrap; por eso la guarda anti-bloqueo **no** vive acá sino en el
  llamador, que es quien sabe QUIÉN está ejecutando el cambio.
- `auth.RolePermissions(ctx, db, roleName) ([]string, bool, error)` — lectura por
  nombre de rol para el formulario de edición; `found=false` (nunca error crudo)
  para un rol fuera del catálogo, para que el handler responda 404.
- Sentinelas nuevos: `auth.ErrRoleNotFound`, `auth.ErrUnknownPermission`.
- Tests propios, independientes de la UI: `internal/auth/roles_contract16_test.go`.
  Las aserciones leen `role_permissions` con SQL directo (helper `grantedPerms`),
  no a través de `auth.RolePermissions`, para que no sean auto-cumplidas.

### T2 — Guarda anti-bloqueo (`internal/server/ui_roles.go`)

`(*handlers).actorKeepsRolesManage(ctx, id, editedRole, newPerms) (bool, error)`
responde: **con el estado RESULTANTE**, ¿quien ejecuta el cambio seguiría
teniendo `roles.manage`? Si no, se rechaza.

### T3 — UI (`internal/server/ui_roles.go` + `templates/roles_edit.html`)

- `GET /admin/roles/{name}/edit` — checkboxes con **todos** los permisos del
  catálogo (`schema.Permissions`), marcados los que el rol tiene hoy (leídos en
  vivo). Es el espejo exacto de `roleChecks` (ui_users.go) para permisos:
  `permissionChecks`.
- `POST /admin/roles/{name}/permissions` — guarda reemplazando el conjunto.
- Ambas rutas gateadas por `requireSessionPermission("roles.manage")`.
- El listado `/admin/roles` sigue siendo de **solo sesión** (sin cambio de
  contrato) y ahora muestra un link "Editar permisos" por fila, visible solo si
  la sesión tiene `roles.manage` (`adminRolesPage.CanManage`). Es cosmético: la
  puerta autoritativa está en las rutas, y un POST crafteado sin el permiso
  recibe 403 igual.
- Rechazo de la guarda = **error de formulario** (banner `<p class="error">` en la
  página re-renderizada), nunca 500 ni JSON crudo.

### T4 — Verificación

- `internal/server/guard_contract16_internal_test.go` (paquete `server`, interno):
  los 4 casos de la guarda uno por uno, incluido el de la API key.
- `internal/server/server_ui_roles_test.go` (paquete `server_test`, TLS + cookie):
  flujo completo por HTTP, efecto real 201→403→201, 403/404/400, y los casos
  (a)(b)(c) también end-to-end.
- `docs/PENDIENTES.md` — el hueco 1 queda marcado como RESUELTO (se conserva el
  relato de cómo se descubrió).

---

## Decisiones de diseño (con su porqué)

### 1. La ruta exacta

```
GET  /admin/roles/{name}/edit
POST /admin/roles/{name}/permissions
```

- **Clave = nombre del rol, no id.** Los roles son un catálogo fijo con nombre
  único, y la vista de solo lectura ya identifica sus filas por nombre
  (`<tr id="role-{{.Name}}">`). Una URL con nombre es legible y estable; el id es
  un UUID sin significado para quien administra.
- **El write cuelga de un sub-recurso `/permissions`, no del rol.** La URL dice
  exactamente QUÉ se reemplaza: los grants. Deja libre `POST /admin/roles` por si
  algún día se quisiera crear un rol (hoy explícitamente fuera de alcance), sin
  que las dos operaciones colisionen ni se confundan.
- Todo bajo el namespace `/admin/roles` existente, como pedía el contrato: no se
  creó una superficie paralela; `handleAdminRolesList` y `rolesWithPermissions`
  se reusaron tal cual para el listado.

### 2. Cómo se calcula el estado RESULTANTE para la guarda

El corazón del contrato. La guarda **no** mira los permisos actuales del actor:
simula el resultado.

1. Se normalizan los **nombres de rol del actor**: para un JWT son los claims
   verificados del token (la misma fuente que usa `permissionsFor`); para una API
   key es el único rol al que está atada, resuelto desde `role_id` contra el
   catálogo.
2. **Si el rol editado NO es uno de los del actor** → permitido sin más consultas.
   El cambio no puede alterar su conjunto efectivo. (Caso b.)
3. **Si el rol editado SÍ es del actor y el conjunto nuevo todavía incluye
   `roles.manage`** → permitido. Lo conserva por ese mismo rol. Esto es lo que
   hace legal "editar mi propio rol quitándole `content.create`": la guarda es
   sobre `roles.manage`, no sobre "no toques tu rol".
4. **Si no** → el actor lo perdería *por ese rol*, así que la respuesta depende
   enteramente de sus **otros** roles: se leen los grants vivos de esos otros
   roles (`permissionsForRoles`, el helper que ya existía) y se comprueba si
   alguno sigue otorgando `roles.manage`. Con dos roles que ambos lo dan, quitarlo
   de uno pasa; la **segunda** operación se rechaza, porque para entonces ningún
   otro rol lo provee. El bloqueo en dos pasos no es evadible.

Por qué esta forma y no "contar cuántos roles tienen `roles.manage`": el criterio
del contrato es más fuerte que "que quede alguno". Garantiza las dos cosas a la
vez — que sobreviva al menos un rol con el permiso **y** que exista alguien vivo
capaz de usarlo, porque el que queda es el propio actor. Un chequeo global de
"queda ≥1 rol con el permiso" pasaría felizmente si el rol sobreviviente no lo
tuviera asignado ningún usuario: sistema inadministrable con el contador en
verde.

Limitación honesta y heredada: para un JWT los roles salen del token, no de
`user_roles`. Si a un usuario se le cambian los roles mientras su sesión vive, la
guarda razona sobre los roles del token hasta que expire (24h). Es exactamente la
misma limitación ya documentada en CONTRACT-08 (`ui_users.go`), y es coherente:
`permissionsFor` — lo que realmente decide el 403 en cada request — usa esa misma
fuente. Si la guarda usara `user_roles` y el gate usara el token, podrían
discrepar; usando la misma fuente, la guarda predice exactamente lo que el gate
va a hacer.

Falla cerrada: identidad nula → `false`. API key cuyo rol ya no existe → sin
roles → nunca se la considera dueña del rol editado.

### 3. Qué pasa cuando quien edita es una API key

Dos respuestas, y las dos importan:

- **En este contrato, una API key no puede editar permisos en absoluto.** La
  superficie es de sesión/cookie (`requireSessionPermission`), y una API key se
  autentica por header `Authorization: Bearer` contra `requirePermission` (la
  familia JSON). No se agregó ninguna ruta JSON — el contrato pedía UI, y el
  contrato público de las rutas 01-15 no cambia. Así que hoy el riesgo de que una
  key se autoexcluya no es alcanzable por HTTP.
- **Aun así la guarda es agnóstica al tipo de identidad, a propósito.** Recibe un
  `*Identity` y resuelve el nombre del rol de una API key desde su `role_id`. Si
  la key está atada al rol que se edita y el conjunto nuevo no incluye
  `roles.manage`, se autoexcluye igual que un humano y se la frena con el mismo
  código. La regla vive en la guarda, no en la ruta: si mañana se agrega una ruta
  JSON, la hereda en vez de reimplementarla (que es exactamente cómo se produjo
  el hueco original — una capacidad que nadie construyó).

Ese comportamiento está probado directamente en
`TestGuardCaseD_APIKeyIdentity`: autoexclusión → rechazada; editar otro rol →
permitido; editar su propio rol conservando `roles.manage` → permitido.

Ese test es la razón de que exista **un** archivo de test en el paquete interno
`server` (todo el resto del paquete se testea desde `server_test`). Como una API
key no llega a la ruta, afirmar el caso (d) solo en prosa habría sido palabra
contra palabra; testear la función directamente es la forma honesta de mostrarlo.

### 4. Cómo se muestra el rechazo de la guarda en el formulario

Formulario **HTML plano** (`method="post"`), no `hx-post`, y respuesta
**409 Conflict** re-renderizando la página con el banner de error y **las
casillas tal como el admin las envió** (no pierde su edición).

Por qué no htmx acá: el proyecto ya usa dos patrones. `hx-*` para writes cuyos
únicos desenlaces son éxito o 404 (`users_detail.html`), y POST plano para todo
formulario que deba poder re-renderizarse con un banner de error
(`users_new.html`, `products_new.html`, `content_types_new.html`). Este formulario
es del segundo tipo por requisito de T3: htmx, por defecto, **no hace swap de una
respuesta no-2xx**, así que un rechazo por `hx-post` no mostraría nada en el
navegador — el admin apretaría "Guardar" y no pasaría nada visible. Un rechazo
invisible en la única pieza de seguridad del contrato es inaceptable, así que se
eligió el patrón que la UI ya usa para errores de formulario. En éxito:
303 → `/admin/roles/{name}/edit`, que vuelve a renderizar los checkboxes desde la
fuente de verdad.

Por qué 409 y no 400: el request está bien formado y sus valores son válidos; lo
que se rechaza es el **efecto** sobre el estado. 400 queda reservado para el
cuerpo malformado (permiso fuera del catálogo). Que sean códigos distintos hace
que los tests distingan "te frenó la guarda" de "mandaste basura" sin leer el HTML.

### 5. Orden de los chequeos en el POST (decisión menor pero deliberada)

`404 rol inexistente` → `400 permiso fuera del catálogo` → `409 guarda` →
escritura. El 400 va **antes** de la guarda a propósito: un cuerpo crafteado con
un permiso inexistente es un request malformado y debe responder 400 sea cual
fuere el veredicto de la guarda sobre el resto del conjunto. La validación contra
`schema.Permissions` en el handler es además redundante con la que hace
`auth.SetRolePermissions` contra la tabla sembrada — defensa en profundidad
deliberada: la del handler existe para producir un 400 *renderizado*, la de la
capa de datos para que la invariante valga aunque el llamador sea otro.

---

## Trade-offs

- **Reemplazo del conjunto vs. altas/bajas individuales.** Decidido en el
  contrato; se respetó. Ventaja: lo que se persiste es la intención declarada del
  admin, sin lectura-modificación-escritura y por lo tanto sin carrera entre dos
  admins editando el mismo rol. Costo: un guardado con la página vieja pisa un
  cambio ajeno reciente (last-write-wins). Aceptable para una pantalla de
  administración de bajísima frecuencia, y es exactamente el mismo trade-off que
  `SetUserRoles` ya tomó.
- **Sin matriz global.** Editar rol por rol es más lento para una reorganización
  grande, pero cada guardado es una transacción con una intención clara y la
  guarda puede razonar sobre un solo delta. Una matriz global haría el cálculo del
  estado resultante mucho más difícil de auditar, justo en la pieza de seguridad.
- **Link "Editar" condicionado a `roles.manage`.** Cuesta una consulta de permisos
  extra al renderizar el listado. Se pagó para no ofrecer un botón que lleva a un
  403.
- **La guarda protege solo `roles.manage`.** Un admin puede dejarse sin
  `users.manage` o sin `content.create` y no se lo impide. Es intencional: solo
  `roles.manage` es irrecuperable-sin-base-de-datos (es el permiso que permite
  devolverse cualquier otro).
- **Un test en el paquete interno.** Rompe la uniformidad de "todo desde
  `server_test`". Se aceptó para poder probar de verdad el caso de la API key.

---

## Criterios de aceptación — salida REAL

### `go build ./...` y `go vet ./...` limpios

```
$ go build ./... && go vet ./... && echo BUILD_VET_OK
BUILD_VET_OK
```

### `go test ./... -count=1` verde, corrido dos veces

```
$ go build ./... && go vet ./... && go test ./... -count=1
=== FINAL RUN 1 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.335s
ok  	github.com/MauricioPerera/librarian/internal/auth	3.893s
ok  	github.com/MauricioPerera/librarian/internal/config	0.665s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.338s
ok  	github.com/MauricioPerera/librarian/internal/server	46.449s
ok  	github.com/MauricioPerera/librarian/internal/store	2.326s

$ go test ./... -count=1
=== FINAL RUN 2 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.383s
ok  	github.com/MauricioPerera/librarian/internal/auth	3.994s
ok  	github.com/MauricioPerera/librarian/internal/config	0.660s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.317s
ok  	github.com/MauricioPerera/librarian/internal/server	45.504s
ok  	github.com/MauricioPerera/librarian/internal/store	2.497s
```

213 tests en total en el repo, todos verdes en ambas corridas.

### T1 — reemplazo atómico, 404 rol inexistente, 400 permiso desconocido sin mutar

```
$ go test ./internal/auth/... -count=1 -run 'RolePermissions|SetRolePermissions' -v
=== RUN   TestSetRolePermissionsReplacesWholeSet
--- PASS: TestSetRolePermissionsReplacesWholeSet (0.06s)
=== RUN   TestSetRolePermissionsUnknownRole
--- PASS: TestSetRolePermissionsUnknownRole (0.05s)
=== RUN   TestSetRolePermissionsUnknownPermissionIsAtomic
--- PASS: TestSetRolePermissionsUnknownPermissionIsAtomic (0.05s)
=== RUN   TestSetRolePermissionsIsIdempotentOnRepeats
--- PASS: TestSetRolePermissionsIsIdempotentOnRepeats (0.05s)
=== RUN   TestRolePermissionsRead
--- PASS: TestRolePermissionsRead (0.05s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/auth	2.678s
```

Qué prueba cada uno:

- `ReplacesWholeSet`: `{content.create, content.update}` → se reemplaza por
  `{content.publish, terms.manage}` (verificado con SQL directo: los viejos
  desaparecieron, no se mezclaron) → conjunto vacío deja el rol sin permisos →
  otro rol nunca se tocó.
- `UnknownRole`: `ErrRoleNotFound` y `COUNT(*)` de `role_permissions` idéntico
  antes y después.
- `UnknownPermissionIsAtomic`: cuerpo `{content.publish, articles.nuke}` sobre un
  rol que tenía `{content.create}` → `ErrUnknownPermission` y los grants siguen
  siendo exactamente `{content.create}`: **ni el DELETE ni el permiso válido se
  aplicaron**.
- `IsIdempotentOnRepeats` (red-team del contrato): el mismo permiso repetido 3
  veces produce **1 fila** (PK compuesta + `ON CONFLICT DO NOTHING`), confirmado
  con `COUNT(*)`; re-aplicar el mismo conjunto es no-op.

### T2 — los 4 casos de la guarda, cada uno con su resultado real

```
$ go test ./internal/server/... -count=1 -run 'Guard' -v
=== RUN   TestGuardCaseA_SingleRoleRemovingFromItselfIsRejected
--- PASS: TestGuardCaseA_SingleRoleRemovingFromItselfIsRejected (0.06s)
=== RUN   TestGuardCaseB_RemovingFromAnotherRoleIsAllowed
--- PASS: TestGuardCaseB_RemovingFromAnotherRoleIsAllowed (0.06s)
=== RUN   TestGuardCaseC_TwoGrantingRolesRemoveOneIsAllowedThenSecondRejected
--- PASS: TestGuardCaseC_TwoGrantingRolesRemoveOneIsAllowedThenSecondRejected (0.06s)
=== RUN   TestGuardCaseD_APIKeyIdentity
--- PASS: TestGuardCaseD_APIKeyIdentity (0.06s)
=== RUN   TestGuardAllowsUnrelatedPermissionRemovalOnOwnRole
--- PASS: TestGuardAllowsUnrelatedPermissionRemovalOnOwnRole (0.05s)
=== RUN   TestGuardNilIdentityFailsClosed
--- PASS: TestGuardNilIdentityFailsClosed (0.04s)
=== RUN   TestGuardHTTPRejectsSelfLockout
--- PASS: TestGuardHTTPRejectsSelfLockout (0.15s)
=== RUN   TestGuardHTTPAllowsRemovingFromAnotherRole
--- PASS: TestGuardHTTPAllowsRemovingFromAnotherRole (0.17s)
=== RUN   TestGuardHTTPTwoRolesFirstAllowedSecondRejected
--- PASS: TestGuardHTTPTwoRolesFirstAllowedSecondRejected (0.15s)
=== RUN   TestGuardHTTPAllowsUnrelatedRemovalOnOwnRole
--- PASS: TestGuardHTTPAllowsUnrelatedRemovalOnOwnRole (0.15s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/server	6.665s
```

| Caso | Escenario | Resultado real |
|---|---|---|
| **(a)** | Usuario con `roles.manage` por un **solo** rol (`administrator`), quitándoselo a **ese** rol | **RECHAZADO.** Unit: `CaseA` → `keeps=false`. HTTP: `GuardHTTPRejectsSelfLockout` → **409**, banner "No se guardó…", `role_permissions` de `administrator` sigue siendo `[roles.manage]`, y el admin **puede volver a abrir el editor** (no quedó bloqueado). |
| **(b)** | El mismo usuario quitándoselo a **otro** rol que él no tiene (`editor`) | **PERMITIDO.** Unit: `CaseB` → `keeps=true`. HTTP: `GuardHTTPAllowsRemovingFromAnotherRole` → **303**, grants de `editor` quedan `[content.create]`. |
| **(c)** | Usuario con **dos** roles que ambos dan `roles.manage`, quitándoselo a uno | **PERMITIDO** (conserva el otro), y la **segunda** operación **RECHAZADA**. Unit: `CaseC` → primera `keeps=true`, segunda `keeps=false`. HTTP: `GuardHTTPTwoRolesFirstAllowedSecondRejected` → 1ª **303** (`editor` queda sin grants), 2ª **409** (`administrator` conserva `[roles.manage]`). El bloqueo en dos pasos no es evadible. |
| **(d)** | **API key** atada a un rol | **RECHAZADO** si edita su propio rol quitando `roles.manage`; **PERMITIDO** si edita otro rol, y **PERMITIDO** si conserva `roles.manage`. Unit: `CaseD`, los tres sub-casos + key huérfana. A nivel HTTP, además, una API key **no alcanza** la superficie de edición (es solo-sesión) — ver decisión 3. |

Red-team extra del contrato, ambos verdes:

- Quitar un permiso **no relacionado** (`content.create`) del propio rol
  conservando `roles.manage` → **permitido**
  (`GuardAllowsUnrelatedPermissionRemovalOnOwnRole`,
  `GuardHTTPAllowsUnrelatedRemovalOnOwnRole` → 303, grants `[roles.manage]`).
- Identidad nula → falla cerrada (`GuardNilIdentityFailsClosed`).

### T3 — edición real por UI; rechazo de la guarda como error de formulario

```
$ go test ./internal/server/... -count=1 -run 'Role' -v
=== RUN   TestRoleEditFormShowsCatalogWithCurrentGrantsChecked
--- PASS: TestRoleEditFormShowsCatalogWithCurrentGrantsChecked (0.16s)
=== RUN   TestRolesListShowsEditLinkOnlyWithPermission
--- PASS: TestRolesListShowsEditLinkOnlyWithPermission (0.24s)
=== RUN   TestRoleEditWithoutPermissionIs403
--- PASS: TestRoleEditWithoutPermissionIs403 (0.16s)
=== RUN   TestRoleEditUnknownRoleIs404
--- PASS: TestRoleEditUnknownRoleIs404 (0.16s)
=== RUN   TestRoleEditUnknownPermissionIs400WithoutMutating
--- PASS: TestRoleEditUnknownPermissionIs400WithoutMutating (0.15s)
=== RUN   TestRolePermissionsFullFlowAndRealEffect
--- PASS: TestRolePermissionsFullFlowAndRealEffect (0.26s)
=== RUN   TestRoleEmptySetRevokesEverything
--- PASS: TestRoleEmptySetRevokesEverything (0.15s)
PASS
```

- El formulario ofrece **los 8 permisos del catálogo** (assertion nombre por
  nombre) con los otorgados `checked` y los no otorgados sin marcar.
- Todas las pruebas con cookie usan `openUITLS` → `httptest.NewTLSServer`, como
  exige el ciclo cookie-set → cookie-enviada.
- Rechazo de la guarda: cuerpo con `<!DOCTYPE html>` y el banner "No se guardó…",
  **sin** el envoltorio JSON `{"error"` y **sin** 500.
- Listado: `/admin/roles` sigue devolviendo 200 a una sesión sin `roles.manage`
  (solo-sesión, contrato anterior intacto) y no le muestra el link de edición.

### T4 — flujo completo + EFECTO REAL

`TestRolePermissionsFullFlowAndRealEffect`, paso a paso, todo por HTTP con cookie
de sesión real:

1. `author` arranca con `{content.create, content.update}`. Un usuario de ese rol,
   con su JWT real, hace `POST /articles` → **201 Created** (línea base).
2. `GET /admin/roles/author/edit` → **200**, con `content.create` y
   `content.update` renderizados `checked`.
3. `POST /admin/roles/author/permissions` con `{content.update, content.publish}`
   (agrega uno, quita otro) → **303**, `Location: /admin/roles/author/edit`.
4. **Consulta directa a `role_permissions`** (JOIN a `roles`/`permissions`, sin
   pasar por el código que escribió): el conjunto es **exactamente**
   `{content.update, content.publish}`. El formulario recargado lo refleja
   (`content.publish` checked, `content.create` no).
5. **EFECTO REAL:** el **mismo** token de author, sin ningún cambio en el usuario,
   hace `POST /articles` → **403 Forbidden**. El permiso revocado dejó de
   habilitar la ruta que gateaba.
6. Se vuelve a otorgar `content.create` **por la misma UI** → **303**, y el mismo
   token vuelve a obtener **201 Created**.
7. El listado `/admin/roles` muestra el permiso recién otorgado (lee la tabla
   viva, no un catálogo hardcodeado).

Resto de T4:

- **Sin `roles.manage` → 403 al editar**: `TestRoleEditWithoutPermissionIs403` —
  GET del formulario **403** HTML (marcador "Sin permiso", no JSON) y POST directo
  con la cookie (no por botón) **403**, con los grants intactos.
- **Rol inexistente → 404**: `TestRoleEditUnknownRoleIs404` — GET y POST sobre
  `superadmin` → **404** HTML en ambos.
- **Permiso inexistente en body crafteado → 400 sin mutar**:
  `TestRoleEditUnknownPermissionIs400WithoutMutating` — cuerpo
  `{content.publish, articles.nuke}` sobre un rol con `{content.create}` →
  **400**, página renderizada con "Permiso desconocido: articles.nuke", y la
  consulta directa devuelve todavía `[content.create]`.
- **Conjunto vacío**: `TestRoleEmptySetRevokesEverything` → 303 y cero grants.

### Contratos anteriores: sin cambios

Confirmación explícita:

- **Suite completa (213 tests) verde dos veces**, incluida cada acceptance test de
  CONTRACT-01..15. No se modificó ni se borró ni un solo test previo.
- **Guardián de CONTRACT-15 T1 verde**: `TestPageDataIsBuiltOnlyByTheConstructor`
  → PASS. `ui_roles.go` no contiene ningún literal `pageData{`; todas sus páginas
  se construyen con `h.page(r, title)`, incluido el re-render del error.
- **El contrato público de las rutas 01-15 no cambia.** Las únicas dos rutas
  nuevas son las de T3. `GET /admin/roles` conserva su método, su path, su gate
  (solo sesión) y su 200; lo único que cambió es que su tabla agrega una columna
  "Acciones" cuando la sesión tiene `roles.manage`. `TestAdminRolesViewReflects
  RealGrants` (CONTRACT-08) sigue verde sin tocarlo.
- Ninguna función existente cambió de firma. Lo agregado a
  `internal/server/ui_users.go` es aditivo: el campo `CanManage` en
  `adminRolesPage` y su llenado en `handleAdminRolesList`.
- Sin migración de esquema: `role_permissions` ya existía; este contrato solo
  escribe filas en ella.
- Sin dependencias nuevas (`go.mod` intacto). Ningún permiso nuevo
  (`schema.Permissions` intacto). Ningún rol nuevo (`schema.Roles` intacto).

---

## Archivos tocados

Nuevos:

- `internal/auth/roles.go` — T1.
- `internal/auth/roles_contract16_test.go` — tests unitarios de T1.
- `internal/server/ui_roles.go` — T2 (guarda) + T3 (handlers/vistas).
- `internal/server/templates/roles_edit.html` — formulario de checkboxes.
- `internal/server/guard_contract16_internal_test.go` — los 4 casos de la guarda.
- `internal/server/server_ui_roles_test.go` — acceptance HTTP de T3/T4.
- `docs/reports/CONTRACT-16-REPORT.md` — este reporte.

Modificados (aditivo):

- `internal/server/ui.go` — `roles_edit.html` en la lista `//go:embed`;
  `h.registerAdminRoleRoutes(mux)` en `registerUIRoutes`.
- `internal/server/ui_users.go` — `adminRolesPage.CanManage` + su llenado.
- `internal/server/templates/roles_list.html` — columna "Acciones" con el link de
  edición, condicionada a `CanManage`; texto introductorio actualizado.
- `internal/server/assets/app.css` — `.permissions-fieldset` reusa el estilo de
  `.roles-fieldset`.
- `docs/PENDIENTES.md` — hueco 1 marcado como RESUELTO.

Sin commits: el árbol queda con los cambios sin commitear, como pedía el contrato.

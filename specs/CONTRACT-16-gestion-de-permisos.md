# Contrato 16 — Gestión de permisos por rol (cierra el hueco 1 de PENDIENTES.md)

Prerrequisitos: `CONTRACT-01`..`CONTRACT-15` completos y desplegados (`15a1857`).

Cierra el hueco de mayor severidad registrado en `docs/PENDIENTES.md`: la tabla
`role_permissions` — la que decide qué puede hacer realmente cada rol — no tiene NINGUNA vía de
escritura en el producto. Hoy solo se puede modificar con SQL directo contra la base, y de hecho
así se tuvo que arreglar producción cuando se descubrió que estaba vacía. El permiso
`roles.manage` existe en el catálogo desde `CONTRACT-02` y **no lo consume ninguna ruta** —
precisamente porque esta capacidad nunca se construyó.

## Decisiones YA TOMADAS (no las reabras)

- **Guard anti-bloqueo: el usuario que ejecuta el cambio NO puede perder `roles.manage` como
  resultado de ese cambio.** Se rechaza con un error claro. Esto garantiza dos cosas a la vez: que
  siempre quede al menos un rol con el permiso, y que exista alguien vivo capaz de usarlo. Para
  reestructurar, el orden es otorgar primero al rol nuevo y quitar después del viejo.
- **Forma de edición: por rol, con reemplazo del conjunto completo** (entrar a un rol, ver
  checkboxes de todos los permisos del catálogo, guardar). NO una matriz global. Es el mismo
  patrón semántico que `auth.SetUserRoles` (usuarios↔roles) y la asignación de términos —
  reemplazo atómico, no altas/bajas individuales.

## RECON ya resuelto (no re-investigar)

- La vista de solo lectura ya existe: `handleAdminRolesList` + `rolesWithPermissions`
  (`internal/server/ui_users.go`), que lee `role_permissions` en vivo vía
  `permissionsForRoleID` (`internal/server/authz.go`). REUSALA como base de la vista de listado;
  no escribas otra forma de leer los grants.
- `auth.SetUserRoles` (`internal/auth/users.go`) es tu plantilla EXACTA para el reemplazo
  atómico: transacción única, verificar que la entidad existe, resolver TODOS los nombres contra
  el catálogo ANTES de mutar (un nombre desconocido aborta sin tocar nada), borrar el conjunto
  actual, insertar el nuevo. Replicá esa estructura para permisos↔rol.
- `schema.Permissions` y `schema.Roles` son los catálogos fijos en código; los checkboxes se
  arman desde `schema.Permissions`, igual que `roleChecks` arma los de roles desde
  `schema.Roles` (`internal/server/ui_users.go`).
- El permiso que gatea TODA escritura de este contrato es `roles.manage` (ya está en el
  catálogo). NO agregues ninguno.
- **Cómo saber si el usuario que ejecuta perdería `roles.manage`:** su identidad está en el
  contexto (`identityFromContext`), y sus permisos efectivos se resuelven con `permissionsFor`
  (`authz.go`) — que para una identidad JWT los deriva de los NOMBRES de rol del token. Ojo: hay
  que evaluar el estado RESULTANTE (los roles del usuario, con el conjunto nuevo aplicado al rol
  que se está editando), no el actual. Pensá bien este cálculo: es el corazón del guard.
- **Una identidad de API key también puede tener `roles.manage`** (una key está atada a un rol).
  Decidí y documentá qué pasa si una API key edita permisos — el guard debe seguir teniendo
  sentido en ese caso (una key atada al rol que se está editando puede autoexcluirse igual).
- Namespace: extendé `/admin/roles` (hoy solo listado) con la edición por rol. La ruta exacta a
  tu criterio (ej. `/admin/roles/{name}/edit` + `POST /admin/roles/{name}`), documentala.
- **CONTRACT-15 T1 sigue vigente:** toda página autenticada construye su view model con
  `h.page(r, title)`; hay un test guardián que falla si construís un literal `pageData{` fuera de
  `ui.go`. No lo evadas.

## T1 — Capa de datos

FIX/OBJETIVO: una función que reemplace el conjunto de permisos de un rol de forma atómica
(nombre a tu criterio, ej. `auth.SetRolePermissions`), siguiendo la estructura de
`auth.SetUserRoles`: transacción única, rol inexistente → error sentinela (→404), permiso
desconocido → error sentinela (→400) sin mutar nada, conjunto vacío válido (deja el rol sin
permisos). Tests unitarios propios, independientes de la UI.

## T2 — El guard anti-bloqueo

FIX/OBJETIVO: antes de aplicar el cambio, calcular si el usuario que ejecuta la operación
quedaría sin `roles.manage` con el estado RESULTANTE. Si sí → rechazar con un mensaje claro que
explique por qué y qué hacer (otorgar primero al otro rol). Es la pieza de seguridad del
contrato: un bug acá deja el sistema inadministrable sin acceso a la base.

Casos que tu implementación debe resolver correctamente y que los tests deben cubrir uno por uno:
un usuario con `roles.manage` por un solo rol quitándoselo a ese mismo rol (→ rechazado); el
mismo usuario quitándoselo a OTRO rol que él no tiene (→ permitido); un usuario con dos roles,
ambos con `roles.manage`, quitándoselo a uno (→ permitido, conserva el otro); y el caso de la
API key del RECON.

## T3 — UI

FIX/OBJETIVO: desde `/admin/roles`, poder entrar a editar un rol: checkboxes con TODOS los
permisos del catálogo, marcados los que tiene, guardar reemplaza el conjunto. Gateado
`roles.manage`; el listado sigue siendo de solo sesión. Un rechazo del guard se muestra como
error en el formulario (nunca 500, nunca JSON crudo). Mismo patrón htmx que el resto de la UI.

## T4 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces, `httptest.NewTLSServer`
para lo que dependa de cookie):
- Flujo real por HTTP con cookie: entrar a un rol → ver sus permisos actuales marcados → agregar
  uno y quitar otro → guardar → confirmar con consulta directa a `role_permissions` que el
  conjunto quedó exactamente como se pidió.
- **Efecto real del cambio, no solo la fila:** quitarle un permiso a un rol y confirmar que un
  usuario de ese rol EMPIEZA a recibir 403 en la ruta que ese permiso gateaba; volver a
  otorgarlo y confirmar que vuelve a funcionar. Es la prueba de que el cambio surte efecto de
  verdad, no solo de que se escribió una fila.
- Los 4 casos del guard de T2, cada uno con su resultado real.
- Sin `roles.manage` → 403 al editar. Rol inexistente → 404. Permiso inexistente en el body
  (request crafteado) → 400 sin mutar nada.
- Confirmá explícitamente que TODO lo de contratos anteriores sigue funcionando igual.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: reemplazo atómico con tests propios; rol inexistente → 404; permiso desconocido → 400
  sin mutar.
- [ ] T2: los 4 casos del guard resueltos correctamente, cada uno con evidencia.
- [ ] T3: edición real por UI con checkboxes; rechazo del guard mostrado como error de formulario.
- [ ] T4: flujo completo verificado; efecto real (403 → 200) confirmado; contratos anteriores sin
  cambios.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias nuevas.
- NO commitear (el orquestador commitea y despliega tras verificar).
- NINGÚN permiso nuevo — `roles.manage` ya existe y es el que corresponde.
- Los catálogos de roles y permisos siguen fijos en código: este contrato edita GRANTS, no crea
  ni borra roles ni permisos.
- El contrato público de las rutas de contratos 01-15 no cambia.
- Respetá el guardián de CONTRACT-15 (`h.page(r, title)`, nunca un literal `pageData{`).

## Checklist antes de delegar

- [ ] RECON corrido: vista de solo lectura y helpers existentes identificados, `SetUserRoles`
  como plantilla del reemplazo atómico, guard y forma de edición ya decididos.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿el guard se puede evadir editando OTRO rol que el usuario también tiene? (si
  tiene dos roles y ambos dan `roles.manage`, quitarlo de los dos requiere dos operaciones — la
  segunda debe ser rechazada; probalo). ¿Y si el usuario tiene el permiso por un rol y edita ese
  rol quitando OTROS permisos pero conservando `roles.manage`? (debe permitirse). ¿Un request
  crafteado con el mismo permiso repetido N veces? (idempotente, no debe duplicar filas — hay PK
  compuesta, confirmalo). ¿Qué pasa si el rol editado es el del propio usuario y se quita un
  permiso NO relacionado con `roles.manage`, como `content.create`? (permitido: el guard es solo
  sobre `roles.manage`).
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación HTTP real y el DEPLOY (protocolo de copia-real) los hace el orquestador.

# CONTRACT-10 — Navegación estilo WordPress (sidebar con submenús)

Estado: **IMPLEMENTADO** (scope del contrato completo y verde). Ver la nota de
"Fallo pre-existente fuera de alcance" al final: la suite completa tiene UN test
rojo que **no pertenece a este contrato ni lo causa este cambio**.

Contrato: `specs/CONTRACT-10-ui-navegacion-sidebar.md`. Es un rediseño de
**presentación** — cero rutas nuevas, cero cambios de permisos o lógica de
negocio. Solo se agrega la barra lateral y el dato de sección/sub-ítem activo.

## Archivos tocados

- `internal/server/ui_nav.go` (**nuevo**): fuente única del menú + inferencia de
  activo.
- `internal/server/ui.go`: campo `Path` en `pageData` + `Path: "/"` en `renderHome`.
- `internal/server/ui_articles.go`, `ui_users.go`, `ui_apikeys.go`: cada handler
  autenticado pasa `Path: r.URL.Path` a su `pageData` (único cambio; nada de
  rutas/permits/negocio).
- `internal/server/templates/layout.html`: layout de dos columnas (sidebar +
  topbar) para páginas autenticadas; login sin sidebar.
- `internal/server/assets/app.css`: estilos de la sidebar/topbar.

## T1 — Estructura de navegación como fuente única de verdad

En `ui_nav.go` vive `var navSections []navSection` con EXACTAMENTE las
secciones/sub-opciones del RECON (Inicio `/`; Artículos `/admin/articles` +
`/admin/articles/new`; Usuarios `/admin/users` + `/admin/users/new`; Roles y
permisos `/admin/roles`; API keys `/admin/api-keys` + `/admin/api-keys/new`).
Ningún template hardcodea el menú: el layout hace `{{range .Nav}}` sobre esa
estructura. `home.html` conserva sus botones-atajo previos (no son la navegación
canónica; presentación intacta).

## T2 — Layout con sidebar (CSS puro, sin dependencia nueva)

`layout.html` ahora es `.admin-shell` = `aside.sidebar` (columna oscura fija de
240px, marca arriba, secciones abajo) + `.admin-main` (`header.topbar` con título
de página y el control de logout que ya existía + `main.content` con
`{{template "content" .}}` **sin tocar**). La sección activa se resalta (fondo
azul `--sidebar-active-bg`, borde izquierdo blanco) y sus sub-opciones se
expanden solo cuando estás dentro de esa sección. Login: rama `{{else}}`, barra
superior simple sin sidebar (se queda como estaba). Sin JS nuevo, sin
dependencias.

HTML relevante del resaltado activo (fragmento renderizado real en `/admin/articles`):

```
<div class="nav-section is-active">
  <a class="nav-section-link" href="/admin/articles" aria-current="true">Artículos</a>
  <ul class="nav-children">
    <li><a class="nav-child is-active" href="/admin/articles" aria-current="page">Todos los artículos</a></li>
    <li><a class="nav-child" href="/admin/articles/new">Añadir nuevo</a></li>
  </ul>
</div>
```

## T3 — Aplicar a todas las páginas autenticadas

Cada handler que renderiza una página autenticada pasa ahora `Path: r.URL.Path`
(o `"/"` en home) a `pageData`; el layout infiere sección/sub-ítem activo. Se
confirmó una por una que TODAS usan el layout nuevo (evidencia en T4):

- `/` (home) · `/admin/articles` (list) · `/admin/articles/new` · article edit ·
  `/admin/users` (list) · `/admin/users/new` · `/admin/users/{id}` (detail) ·
  `/admin/roles` · `/admin/api-keys` (list) · `/admin/api-keys/new` ·
  api-key created · 403 · 404.

Las páginas de error (403/404) se renderizan con la sidebar (son autenticadas)
pero sin ninguna sección resaltada (no tienen ruta de menú propia): `Path` vacío,
`sectionActive` da falso para todas. Correcto.

**Tests actualizados:** ninguno hizo falta. Se auditaron los `*_test.go` del
paquete: sus aserciones verifican MARCADORES de contenido (email, mensajes de
validación textuales, badges, `hx-put`, ausencia del envelope JSON `"error"`,
cookies, redirecciones), no la estructura del layout viejo (`<nav class="nav">`,
`brand`). Envolver el `content` en la sidebar no toca ninguna de esas aserciones.
El paquete `internal/server` pasa verde sin editar un solo test — se mantiene
íntegra cada aserción funcional (login, crear, permiso, publicar, borrar, revocar).

## T4 — Verificación (navegación real + funcionalidad previa)

Navegación real por la sidebar (login como sesión válida, GET a cada ruta,
inspección del HTML renderizado). Cada página: sidebar presente, sección activa
correcta, sub-ítem activo exacto, submenús expandidos SOLO en la sección activa,
hrefs del menú apuntando a las rutas reales:

```
PATH /                    sidebar=true | activeSection=Inicio -> /             | activeChild=(none)
PATH /admin/articles      sidebar=true | activeSection=Artículos -> /admin/articles     | activeChild=Todos los artículos -> /admin/articles
PATH /admin/articles/new  sidebar=true | activeSection=Artículos -> /admin/articles     | activeChild=Añadir nuevo -> /admin/articles/new
PATH /admin/users         sidebar=true | activeSection=Usuarios -> /admin/users         | activeChild=Todos los usuarios -> /admin/users
PATH /admin/users/new     sidebar=true | activeSection=Usuarios -> /admin/users         | activeChild=Añadir nuevo -> /admin/users/new
PATH /admin/roles         sidebar=true | activeSection=Roles y permisos -> /admin/roles | activeChild=(none)
PATH /admin/api-keys      sidebar=true | activeSection=API keys -> /admin/api-keys      | activeChild=Todas las keys -> /admin/api-keys
PATH /admin/api-keys/new  sidebar=true | activeSection=API keys -> /admin/api-keys      | activeChild=Crear nueva -> /admin/api-keys/new
```

(Los `menuHrefs` observados confirman que en `/admin/articles` aparecen los hijos
de Artículos pero NO los de Usuarios/API keys, y viceversa: los submenús de
secciones inactivas quedan colapsados.) La verificación se hizo con un test
desechable que ya fue **eliminado** del árbol (no queda artefacto).

Funcionalidad de contratos anteriores intacta a través del layout nuevo (tests
reales que recorren el CRUD completo, todos verdes):

```
--- PASS: TestAdminRoundTrip           (crear→editar→publicar→borrar artículo)
--- PASS: TestAdminPublish
--- PASS: TestAdminDelete
--- PASS: TestAdminCreateAppearsInList
--- PASS: TestAdminCreateValidationReRendersForm
--- PASS: TestAdminDeleteWithoutPermissionServerSide  (permiso sigue exigido)
--- PASS: TestAdminUserCreateAppearsInListAndDetail
--- PASS: TestAdminUserStatusChange
--- PASS: TestAdminUserRolesChange
--- PASS: TestAdminUserCreateUnknownRoleRejected
--- PASS: TestAdminUserRoundTripLoginRejection
--- PASS: TestAdminRolesViewReflectsRealGrants
--- PASS: TestAdminAPIKeyRoundTrip
--- PASS: TestAdminAPIKeyCreateShowsSecretOnce
--- PASS: TestAdminAPIKeyRevokeIdempotentAndMissing
--- PASS: TestUIRoundTrip               (login sin sidebar → home con sidebar)
```

## Decisiones de diseño (con su porqué)

1. **Cálculo de sección activa: inferido del path, no pasado explícito.**
   `pageData` gana UN campo (`Path`), y el método `pageData.Nav()` en `ui_nav.go`
   es el ÚNICO lugar que traduce path → sección/sub-ítem activo. Porqué: un solo
   dato (`Path`) resuelve las DOS cosas (sección y sub-ítem) y las páginas de nivel
   de sección cuya ruta no es una entrada de menú (p. ej. el form de edición
   `/admin/articles/{id}/edit`) igual resuelven a su sección por prefijo, sin que
   el handler tenga que recordar a qué sección pertenece. Alternativa descartada:
   pasar una clave de sección explícita por handler — obligaría a pasar además el
   sub-ítem y duplicaría el criterio de "activo" en cada handler.
   - `sectionActive`: Inicio (`/`) matchea SOLO la raíz exacta (si no, estaría
     activa en todas); las demás matchean su ruta o cualquier sub-ruta (prefijo
     `href + "/"`). Los prefijos de las 4 secciones admin son disjuntos, sin
     colisiones.
   - Sub-ítem activo: match EXACTO de path, así "Todos" y "Añadir nuevo" nunca se
     resaltan a la vez.

2. **Estructura CSS de la sidebar.** Columna izquierda oscura de **240px** fija
   (`flex: 0 0 240px`), paleta propia en variables nuevas (`--sidebar-*`) para no
   tocar la paleta clara existente del contenido. Resaltado de sección activa:
   fondo azul (`--sidebar-active-bg`, reusa `--primary`) + texto blanco + borde
   izquierdo blanco de 3px. Submenú: `<ul>` con fondo levemente más oscuro,
   indentado; se renderiza SOLO bajo la sección activa (`{{if and .Children
   .Active}}`), con el ítem activo en blanco/negrita y borde azul. Topbar angosto
   con título de página + logout. El `main.content` dentro del shell pierde los
   márgenes auto centrados (ya no hace falta, la columna lo posiciona) y queda
   alineado a la izquierda con `max-width: 960px`. Sin media queries: el contrato
   no pide responsive y en viewport de escritorio normal no se rompe.

## Trade-offs

- **Sin responsive/hamburger:** el contrato excluye responsive explícitamente. En
  viewport muy angosto la sidebar de 240px reduce el área de contenido, pero no
  rompe. Fuera de alcance.
- **Páginas de error con sidebar sin resaltado:** un 403/404 autenticado muestra
  la sidebar sin sección activa. Es lo correcto (no corresponde a una entrada de
  menú) y evita inventar un ítem "activo" falso.
- **`home.html` conserva sus botones-atajo:** quedan algo redundantes junto a la
  sidebar, pero tocar el cuerpo de home sería scope creep de presentación no
  pedido; se dejó intacto.

## Criterios de aceptación (salida REAL)

- `go build ./...` → `BUILD_OK`. `go vet ./...` → limpio (`VET_OK`).
- **Mi scope (`internal/server`) verde 2×:**
  ```
  ### SERVER PKG RUN 1 ###
  ok  	github.com/MauricioPerera/librarian/internal/server	12.079s
  ### SERVER PKG RUN 2 ###
  ok  	github.com/MauricioPerera/librarian/internal/server	11.832s
  ```
- **Suite completa 2× (idéntica en ambas):**
  ```
  ?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
  --- FAIL: TestIssueAndVerifyJWT (0.00s)
      auth_test.go:128: verify: invalid token
  FAIL	github.com/MauricioPerera/librarian/internal/auth	3.608s
  ok  	github.com/MauricioPerera/librarian/internal/config	0.631s
  ok  	github.com/MauricioPerera/librarian/internal/schema	1.326s
  ok  	github.com/MauricioPerera/librarian/internal/server	11.950s
  ok  	github.com/MauricioPerera/librarian/internal/store	2.319s
  ```
- T1/T2/T3/T4: ver arriba, con HTML y salidas reales.

## Fallo pre-existente FUERA DE ALCANCE (no lo toco — lo documento)

`TestIssueAndVerifyJWT` (`internal/auth/auth_test.go:117`) hardcodea
`now = 2026-07-23 12:00 UTC`, emite un JWT con TTL de 24h y luego lo verifica
contra el **reloj real**. Hoy es **2026-07-24**, ya pasado el vencimiento
(`2026-07-24 12:00 UTC`), así que `VerifyJWT` lo rechaza por expirado
("invalid token"). Es un **time-bomb** en un test de `internal/auth`, un paquete
que CONTRACT-10 **no toca**.

Confirmado que es pre-existente e independiente de este cambio: con `git stash`
de todos mis cambios sobre el HEAD limpio (`2bd833e`), el test **falla igual**:

```
=== STASHED, running auth test on clean tree ===
--- FAIL: TestIssueAndVerifyJWT (0.00s)
    auth_test.go:128: verify: invalid token
```

Por las restricciones del contrato (CERO cambios fuera de presentación; "si
sentís que necesitás tocar algo más… PARÁ y documentalo") **no lo corrijo**. El
arreglo es trivial y del dominio de otro contrato/tarea (usar `time.Now()` o una
fecha relativa en el test). Bloquea el criterio literal "suite completa verde",
pero no por el trabajo de CONTRACT-10: el paquete de este contrato
(`internal/server`) está verde 2×.

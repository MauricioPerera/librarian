# Contrato 10 — Navegación estilo WordPress (sidebar con submenús)

Prerrequisitos: `CONTRACT-01`..`CONTRACT-09` completos (`2bd833e`, `librarian.ardf.dev` en
producción). Este contrato es un rediseño de PRESENTACIÓN — no agrega capacidades nuevas, no
toca ninguna ruta ni su lógica de negocio, solo cómo se navega entre las que ya existen.

Pedido del usuario: tomar como base la UI de administrador de WordPress — barra lateral de menú,
vistas separadas por acción del CRUD (ya existen como rutas separadas desde CONTRACT-07/08/09;
este contrato es sobre cómo se accede a ellas), y submenús por sección.

## RECON ya resuelto (no re-investigar)

- El layout compartido hoy (`internal/server/templates/layout.html`) es una barra superior simple
  (`<nav class="nav">`) sin sidebar. `pageData` (`internal/server/ui.go`) es
  `{Title, Authenticated, Email}` — la fuente de verdad de qué sección está activa NO EXISTE
  todavía, hay que agregarla.
- Las vistas por acción del CRUD YA están separadas por ruta (listar/crear/editar/detalle son
  rutas HTTP distintas desde CONTRACT-07/08/09) — este contrato NO crea rutas nuevas, solo
  cambia cómo se llega a ellas (el menú) y cómo se presentan dentro del layout compartido.
- Secciones y sub-opciones existentes a reflejar en el menú (namespace real de cada una, no lo
  inventes): `/` (Inicio, sin submenú), `/admin/articles` + `/admin/articles/new` (Artículos:
  "Todos los artículos" / "Añadir nuevo"), `/admin/users` + `/admin/users/new` (Usuarios: "Todos
  los usuarios" / "Añadir nuevo"), `/admin/roles` (Roles y permisos, sin submenú — es de solo
  lectura), `/admin/api-keys` + `/admin/api-keys/new` (API keys: "Todas las keys" / "Crear
  nueva").
- La página de login (`templates/login.html`) NO lleva sidebar (no hay sesión todavía) — se
  queda como está, centrada, sin este layout.
- Todas las páginas actuales (`home.html`, `articles_*.html`, `users_*.html`, `roles_list.html`,
  `apikeys_*.html`) usan el patrón `{{define "content"}}...{{end}}` que `layout.html` inyecta
  con `{{template "content" .}}` — el contrato NO cambia ese mecanismo de composición, solo lo
  que rodea a `content` (agrega la sidebar, no reemplaza cómo se arma cada página).

## T1 — Estructura de navegación como fuente única de verdad

FIX/OBJETIVO: en `internal/server/ui.go` (o un archivo nuevo, a tu criterio), definí la
estructura del menú en un solo lugar (ej. un `[]navSection` con `Label`, `Href`, `Children
[]navItem`) que refleje EXACTAMENTE las secciones/sub-opciones del RECON — nada hardcodeado por
duplicado en cada template. Agregá a `pageData` el campo necesario para que el layout sepa qué
sección/sub-ítem está activo dado el path actual (a tu criterio cómo lo calculás — podés
resolverlo en cada handler pasando la sección explícita, o inferirlo del `r.URL.Path`
centralizado en un helper; documentá la decisión).

## T2 — Layout con sidebar (CSS, sin dependencia nueva)

FIX/OBJETIVO: rediseñar `templates/layout.html` + `assets/app.css` a un layout de DOS columnas
estilo WordPress admin: sidebar fijo a la izquierda (oscuro, con las secciones del T1;
la sección activa resaltada; sus sub-opciones visibles/expandidas cuando estás dentro de esa
sección) + una barra superior angosta (título de la página + el control de logout que ya existe)
+ el área de contenido (`{{template "content" .}}`, sin tocar). Responsive básico no es
obligatorio (el contrato no lo pide), pero no rompas el layout en un viewport de escritorio
normal. CSS puro — sin JS nuevo, sin dependencia nueva (htmx ya embebido no hace falta acá,
esto es solo presentación).

## T3 — Aplicar a todas las páginas existentes

FIX/OBJETIVO: cada handler que renderiza una página autenticada (home, articles list/new/edit,
users list/new/detail, roles, api-keys list/new/created) pasa ahora el dato de sección activa
del T1 a su `pageData`. NINGUNA ruta, NINGÚN comportamiento de negocio cambia — es
estrictamente presentación. Los tests EXISTENTES que dependían de la estructura HTML vieja
(si los hay, y probablemente los haya — buscalos) se actualizan para reflejar el layout nuevo,
NUNCA se borran ni se debilitan para que pasen; si un test verificaba una interacción real
(login, crear, permiso), esa aserción se mantiene igual, solo cambia lo que rodea al contenido.

## T4 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces):
- Navegación real: desde la sidebar, click en cada sección y sub-opción, confirmando que lleva
  a la ruta correcta y que la sección se resalta como activa.
- Confirmá que TODAS las funcionalidades de contratos anteriores siguen intactas navegando de
  verdad (crear/editar/publicar/borrar artículo, gestionar usuario, revocar API key) — no alcanza
  con que compile, tiene que funcionar clickeado.
- Confirmá que el login (sin sidebar) y el layout nuevo (con sidebar) conviven sin romper nada.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: estructura de navegación en un solo lugar, reflejando exactamente las secciones reales.
- [ ] T2: layout de sidebar renderiza correctamente, sección activa resaltada.
- [ ] T3: todas las páginas autenticadas usan el layout nuevo; tests actualizados para reflejarlo
  sin debilitar ninguna aserción funcional.
- [ ] T4: navegación real verificada, funcionalidad de contratos anteriores intacta.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias nuevas (ni Go ni JS).
- NO commitear (el orquestador commitea tras verificar).
- CERO cambios de ruta, de permiso, o de lógica de negocio — es un contrato de PRESENTACIÓN. Si
  en algún punto sentís que necesitás cambiar una ruta o un handler más allá de pasarle el dato
  de sección activa, es una señal de que estás fuera de alcance: PARÁ y documentalo.
- No rompas ningún contrato/formato JSON existente (obviamente no debería tocarse, pero
  confirmalo en la verificación).

## Checklist antes de delegar

- [ ] RECON corrido: estructura real de secciones/rutas confirmada (arriba), mecanismo de
  composición de templates existente entendido (no se reemplaza).
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿la sidebar se muestra en TODAS las páginas autenticadas, o quedó alguna vieja
  sin migrar? (verificalo navegando cada una, no solo una muestra). ¿El login sigue funcionando
  sin sidebar (no se rompió por el cambio de layout compartido)?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación EN NAVEGADOR pendiente la hace el orquestador (Claude Browser) después de
  integrar.

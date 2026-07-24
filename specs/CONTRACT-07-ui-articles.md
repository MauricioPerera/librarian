# Contrato 07 — UI de `articles` (CRUD, el flujo primario del panel)

Prerrequisitos: `CONTRACT-01`..`CONTRACT-06` completos (`6fe5476`). Segundo contrato de la fase 2
(UI), construye sobre la fundación de sesión/layout de CONTRACT-06.

## RECON ya resuelto (no re-investigar)

- **Gap real encontrado al preparar este contrato (arreglarlo es parte de T1, no un extra):** el
  middleware `requirePermission` de `internal/server/authz.go` (usado por las rutas JSON de
  `articles`) resuelve la identidad SOLO desde el header `Authorization` (`authenticate` →
  `bearerToken(r)`), NO desde la cookie de sesión que agregó CONTRACT-06 (`requireSession`, en
  `internal/server/ui.go`, es un middleware DISTINTO que solo exige sesión, sin chequeo de
  permiso). Hoy no existe ningún middleware que combine "sesión de navegador" + "permiso
  específico" — sin él, la UI de `articles` no puede gatear sus rutas de escritura (crear,
  editar, publicar, borrar) por permiso. Necesitás un middleware nuevo (nombre a tu criterio, ej.
  `requireSessionPermission(permission string)`) que reuse `requireSession`'s resolución por
  cookie + `permissionsFor` (ya existe en `authz.go`, funciona con cualquier `*Identity`) para el
  chequeo de permiso, y que en caso de fallo NO escriba el envelope JSON de `writeError` (eso es
  del contrato de la API, no lo toques) sino que redirija a `/login` (sin sesión) o renderice una
  página HTML simple de error 403 (sesión válida, sin el permiso) — un humano en un navegador
  nunca debe ver un JSON crudo.
- Rutas JSON existentes de `articles` (`GET/POST /articles`, `GET/PUT/DELETE /articles/{id}`,
  `POST /articles/{id}/publish`) usan paths SIN prefijo. Las rutas HTML de este contrato usan un
  namespace DISTINTO para no colisionar con el `ServeMux` (un mismo método+path no puede
  registrarse dos veces): `/admin/articles` (a tu criterio el prefijo exacto si preferís otro,
  documentalo, pero no reuses los paths de la API JSON).
- htmx real ya está embebido y servido (`CONTRACT-06`, `/static/htmx.min.js`) — usalo para las
  interacciones de escritura (`hx-put`, `hx-delete`, `hx-post`) en vez de un truco de
  method-override en formularios planos; htmx emite el verbo HTTP real.
- `internal/server/articles.go` tiene `fetchArticle`/`scanArticle` y el acceso a datos de
  `articles` vía `database/sql` parametrizado — REUSÁ esos patrones/helpers para la UI (no
  dupliques el SQL a mano); si un helper no es directamente reusable por su forma actual (devuelve
  JSON, por ejemplo), extraé la parte de acceso a datos a algo compartible en vez de copiar la
  query.
- El layout base (`internal/server/ui.go`, `templates/layout.html`) y el patrón de
  `pageData`/`renderX` de CONTRACT-06 son la base — las páginas nuevas siguen el mismo patrón
  (un template set por página + el layout compartido).

## T1 — Middleware de sesión + permiso, y rutas de escritura gateadas

FIX/OBJETIVO: el middleware nuevo del RECON (`requireSessionPermission` o el nombre que
elijas), y las rutas de escritura de `/admin/articles` gateadas con el permiso correcto (mismo
mapeo que la API JSON: `content.create`/`content.update`/`content.publish`/`content.delete`). Las
rutas de LECTURA (`GET /admin/articles`, ver detalle de un artículo si lo separás en su propia
vista) solo exigen sesión válida (`requireSession`), igual que la API JSON no gatea lectura por
permiso.

## T2 — Listar y crear

FIX/OBJETIVO: `GET /admin/articles` — lista todos los artículos (título, estado
publicado/borrador, fecha) en una tabla o listado simple. `GET /admin/articles/new` — formulario
de creación (título, cuerpo). `POST /admin/articles` — crea (gateado `content.create`), reusando
`auth.VerifyCredentials`-style de validación de campos ya existente en la API (título/cuerpo
requeridos, mismo mensaje de error) y redirige (`303`) a la lista, o re-renderiza el form con el
error si falla la validación. El autor es el usuario de la sesión (`identityFromContext`), igual
que la API — un artículo SIEMPRE tiene autor humano.

## T3 — Editar, publicar, borrar (interacciones htmx)

FIX/OBJETIVO: `GET /admin/articles/{id}/edit` — formulario de edición precargado. `PUT
/admin/articles/{id}` (gateado `content.update`) — actualiza vía `hx-put` desde el form de
edición, sin recarga completa de página (swap del fragmento correspondiente o redirect simple, a
tu criterio, documentalo). Publicar (`content.publish`) y borrar (`content.delete`) desde la
LISTA vía botones con `hx-post`/`hx-delete` que actualizan la fila o la remueven sin recargar toda
la página (patrón htmx: `hx-target`/`hx-swap` sobre la fila). Un artículo inexistente en cualquiera
de estas rutas es 404 (página HTML simple, no un JSON crudo) — igual disciplina que la API.

## T4 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces, `httptest.NewTLSServer` para
cualquier test que dependa de la cookie de sesión — mismo motivo que CONTRACT-06):
- Round-trip completo por HTTP: login → crear artículo vía `POST /admin/articles` → aparece en
  `GET /admin/articles` → editar → publicar → borrar → ya no aparece en la lista.
- Gateo por permiso: una sesión SIN el permiso correspondiente (usuario con un rol sin
  `content.create`, por ejemplo) intentando `POST /admin/articles` → 403 HTML, NO un 500 ni un
  JSON crudo. Sin sesión → redirect a `/login` (no un 401).
- Confirmá explícitamente que las rutas JSON de `articles` (`CONTRACT-03`) y la fundación de UI
  (`CONTRACT-06`: login/logout/home) siguen funcionando exactamente igual — pegá esa salida.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: `requireSessionPermission` (o tu nombre) funciona: sin sesión → redirect a `/login`; con
  sesión pero sin el permiso → 403 HTML; con sesión y permiso → pasa.
- [ ] T2: crear vía UI real, aparece en la lista con los datos correctos.
- [ ] T3: editar/publicar/borrar vía UI real, reflejados correctamente; artículo inexistente en
  cualquiera de estas rutas → 404 HTML.
- [ ] T4: round-trip completo probado, gateo por permiso probado, rutas de contratos anteriores
  (JSON `articles`, UI de CONTRACT-06) confirmadas sin cambios.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias Go nuevas.
- NO commitear (el orquestador commitea tras verificar).
- NO cambies el contrato/formato de las rutas JSON existentes de `articles` ni el de
  `requirePermission`/`writeError` — son de CONTRACT-03 y tienen tests propios que deben seguir
  en verde SIN que los toques. El middleware nuevo de T1 es ESO, nuevo, no un reemplazo.
- No dupliques el SQL de acceso a `articles` si podés reusar/extraer del existente — pero si
  reusar exige cambiar la firma de un helper compartido con la API JSON, hacelo con cuidado de no
  romper su contrato (mismo principio de CONTRACT-03 T2: extraer sin romper el contrato público
  del código existente).

## Checklist antes de delegar

- [ ] RECON corrido: gap del middleware de permiso+sesión identificado (arriba), namespace de
  rutas HTML decidido (`/admin/articles`, sin colisión con la API JSON), patrón htmx confirmado
  disponible.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿qué pasa si alguien con sesión válida pero SIN `content.delete` manda un
  `hx-delete` a `/admin/articles/{id}` directo (sin pasar por el botón, ej. con `curl` y la
  cookie)? Debe dar 403, igual que si viniera del botón — el gateo es del lado servidor, no
  decorativo del lado cliente. ¿Un `id` con formato inválido en cualquiera de las rutas de
  escritura? 404, nunca 500.
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación EN NAVEGADOR pendiente la hace el orquestador (Claude Browser) después de
  integrar — el dev no necesita un navegador real, solo tests HTTP con `httptest`.

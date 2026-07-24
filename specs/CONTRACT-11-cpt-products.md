# Contrato 11 — Tipo de contenido `products` (esquema + API JSON + UI + deploy)

Prerrequisitos: `CONTRACT-01`..`CONTRACT-10` completos (`dfff351`, `librarian.ardf.dev` en
producción). Prueba real de que el patrón `ContentType()` generaliza a un segundo tipo de
contenido más allá de `articles` — con columnas propias MÁS ALLÁ de `title`/`body`
(`price`, `sku`), que `articles` nunca ejerció.

Alcance: replicar para `products` exactamente el patrón que `articles` ya tiene en JSON
(CONTRACT-03) y en UI (CONTRACT-07) — CRUD completo, gateado por los MISMOS permisos genéricos
`content.*` (no son específicos de `articles`, son del dominio "contenido" en general — no se
agrega ningún permiso nuevo).

## RECON ya resuelto (no re-investigar)

- `schema.ContentType(name string, ownColumns []compat.Column) compat.Table`
  (`internal/schema/content_type.go`) ya genera `id`/`author_id`/columnas propias/
  `created_at`/`updated_at`/`metadata` — reusala EXACTAMENTE como `articles` la usa en
  `Build()`, sin tocar su firma.
- Columnas propias de `products`: `title` (`textColumn`, reusar), `body` (`textColumn`,
  reusar — descripción del producto), `price` (NUEVO: no existe un helper `decimalColumn`
  todavía — agregalo en `schema.go` siguiendo el patrón de `textColumn`/`jsonColumn`;
  `compat.DecimalType` con 0 argumentos compila a `NUMERIC` sin precisión fija en Postgres y a
  `TEXT` canónico en SQLite — no hace falta fijar precisión/escala para este contrato), `sku`
  (`textColumn`, con una constraint `unique("sku")` a nivel de tabla — un SKU duplicado debe
  rechazarse a nivel de esquema, no solo de aplicación).
- Permisos: `content.create`/`content.update`/`content.publish`/`content.delete` YA son
  genéricos (no se llaman `articles.create`) — gateá `products` con los MISMOS, sin agregar
  nada al catálogo fijo. `products` NO tiene `published_at` (a diferencia de `articles`, un
  producto no tiene estado borrador/publicado en este contrato — es CRUD simple; si más
  adelante hace falta, es una decisión de alcance nueva). Por lo tanto NO hay ruta de
  "publish" para `products`, y `content.publish` no se usa acá — documentalo si te genera duda,
  pero no inventes un publish workflow que el contrato no pidió.
- JSON API: espejo exacto de `internal/server/articles.go` (CONTRACT-03) — mismo patrón de
  handlers, mismo `fetchX`/`scanX`, mismos códigos de estado (404 nunca 500 para id
  inexistente/malformado, 400 en validación). Namespace: `/products` (paralelo a `/articles`).
- UI: espejo exacto de `internal/server/ui_articles.go` (CONTRACT-07) — `/admin/products`
  (listar/crear/editar/borrar), gateado `requireSessionPermission` con los mismos permisos,
  reusando el patrón htmx (`hx-put`/`hx-delete`) y los helpers ya compartidos
  (`renderNotFound`/`renderForbidden`, patrón de template set por página).
- Navegación (CONTRACT-10): agregá "Productos" a la estructura de navegación única
  (`internal/server/ui_nav.go` o donde viva) con su submenú ("Todos los productos" / "Añadir
  nuevo") — mismo patrón que Artículos.
- Deploy: mismo mecanismo ya usado (cross-compilar `GOOS=linux GOARCH=amd64`, subir por SFTP,
  reemplazo atómico, `systemctl restart librarian.service`) — el orquestador lo hace después de
  verificar, no es parte de lo que delegás.

## T1 — Esquema

FIX/OBJETIVO: `decimalColumn` helper + tabla `products` vía `ContentType("products", [...])`
agregada a `Build()`, con la constraint `unique("sku")`. Test de aceptación (sin DB real):
`Schema.Validate()` + `CompileDDL` para AMBOS motores, limpio.

## T2 — API JSON CRUD

FIX/OBJETIVO: `POST /products`, `GET /products`, `GET /products/{id}`, `PUT /products/{id}`,
`DELETE /products/{id}` — mismo patrón de `articles.go`, gateado por los permisos `content.*`
correspondientes (create/update/delete; el listado y el detalle solo exigen autenticación, igual
que `articles`). Campos requeridos: `title`, `body`, `price`, `sku` (mismo criterio de
validación 400 que `articles` para campos faltantes). `sku` duplicado → 400 claro (constraint de
esquema, atrapá el error y traducilo, no dejes que la app se caiga con un error crudo de SQL).

## T3 — UI

FIX/OBJETIVO: `/admin/products` (listar/crear/editar/borrar), mismo patrón htmx que
`/admin/articles`. Agregá "Productos" a la navegación (T1 de CONTRACT-10, mismo archivo).

## T4 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces, `httptest.NewTLSServer`
para lo que dependa de cookie):
- Round-trip JSON: crear producto real → aparece en el listado → editar → borrar.
- Round-trip UI: mismo flujo pero por HTTP con cookie de sesión real (como los contratos
  anteriores de UI).
- SKU duplicado rechazado con 400 claro, no 500.
- Gateo por permiso probado (sesión sin `content.create` → 403/401 según corresponda,
  igual que `articles`).
- Confirmá explícitamente que TODO lo de `articles`/`users`/`roles`/`api-keys` (JSON y UI) sigue
  funcionando exactamente igual — pegá esa salida.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: `decimalColumn` + tabla `products` compilan para ambos motores, `unique("sku")`
  presente.
- [ ] T2: CRUD JSON completo probado, SKU duplicado → 400, gateo por permiso probado.
- [ ] T3: CRUD UI completo probado por HTTP con cookie real, "Productos" en la navegación.
- [ ] T4: contratos anteriores (JSON + UI) confirmados sin cambios.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias nuevas.
- NO commitear (el orquestador commitea y despliega tras verificar).
- NO agregues ningún permiso nuevo al catálogo — reusá `content.*` tal cual.
- NO inventes un workflow de publicación para `products` — no lo pidió el contrato.

## Checklist antes de delegar

- [ ] RECON corrido: patrón exacto de `articles.go`/`ui_articles.go` confirmado como plantilla,
  `decimalColumn` a agregar, `unique("sku")` decidido.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿qué pasa si mandás `price` como un string no numérico? (400 claro, nunca 500
  ni un valor corrupto guardado). ¿Dos productos con el mismo `sku` en requests concurrentes?
  (la constraint de esquema es la garantía real, no una validación de aplicación que puede
  perder una carrera — confirmá que el error de la DB se traduce a 400, no se cae).
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación EN NAVEGADOR/HTTP pendiente y el DEPLOY a producción los hace el orquestador
  después de integrar.

# Contrato 12 — Taxonomías y términos (categorías/tags) sobre `articles`/`products`

Prerrequisitos: `CONTRACT-01`..`CONTRACT-11` completos (`b74b538`, producción real). Agrega la
pieza de WordPress-core que faltaba: términos (categorías/tags) asociables a contenido.

## Decisión de arquitectura YA TOMADA (no la re-abras)

WordPress vincula términos a CUALQUIER post type con una tabla polimórfica única porque TODO post
vive en una sola tabla (`wp_posts`). En `librarian` cada tipo de contenido es su PROPIA tabla
tipada (`articles`, `products`) — se decidió explícitamente NO replicar el modelo polimórfico
(perdería integridad referencial real) y en cambio usar **una tabla de relación por tipo de
contenido** (`article_terms`, `product_terms`), con FK real y cascada a ambos lados. Agregar un
tipo de contenido nuevo en el futuro implica agregar también su tabla `<tipo>_terms` — ya era así
que agregar un CPT implica deploy (`DEFINITION.md`), esto no cambia esa historia.

## RECON ya resuelto (no re-investigar)

- `taxonomies`: catálogo FIJO en código (como `schema.Roles`/`schema.Permissions`), vía el
  helper YA EXISTENTE `catalogTable(name)` (`internal/schema/schema.go`) — llamalo
  `catalogTable("taxonomies")` y agregá un slice `Taxonomies = []string{"category", "tag"}`
  sembrado con el mismo patrón data-driven que `SeedCatalogs` ya usa para roles/permisos
  (`internal/store/store.go`, función `seedNames` — reusala, no la reinventes).
- `terms`: tabla NUEVA (NO usa `ContentType()`, no es contenido — no tiene `author_id`).
  Columnas: `id` (`idColumn()`), `taxonomy_id` (`uuidColumn`, FK `foreignKeyCascade` a
  `taxonomies`), `name` (`textColumn`), `slug` (`textColumn`), `parent_id` (`uuidColumn`
  NULLABLE, FK a `terms.id` con `OnDelete: compat.SetNull` — un término padre borrado no se
  lleva a los hijos, los deja huérfanos de padre, igual que WordPress). Constraint:
  `unique("taxonomy_id", "slug")` — el mismo slug puede repetirse en taxonomías distintas, no
  dentro de la misma. Los términos son DATOS creados en runtime por un admin (a diferencia de
  `taxonomies`, que es el catálogo fijo) — como los usuarios, no como los roles.
- Relación con contenido: `article_terms` y `product_terms` vía el helper YA EXISTENTE
  `junctionTable(name, leftCol, leftTable, rightCol, rightTable)` (`internal/schema/schema.go`,
  ya usado para `role_permissions`/`user_roles`) — `junctionTable("article_terms", "article_id",
  "articles", "term_id", "terms")` y lo mismo para `product_terms`. No hace falta un helper
  nuevo, este YA es genérico.
- Permiso NUEVO (justificado — nada del catálogo existente encaja): `terms.manage`, agregado a
  `schema.Permissions`. Gatea CRUD de `terms` Y la asignación/desasignación de términos a
  contenido. Es la primera vez en el proyecto que se agrega un permiso nuevo — documentá por qué
  (ningún permiso existente cubre "administrar el catálogo de categorías/tags", a diferencia de
  contratos anteriores donde `content.*`/`users.manage` sí encajaban).
- API JSON: `/terms` (CRUD: crear/listar/editar/borrar un término, gateado `terms.manage`) +
  `/articles/{id}/terms` y `/products/{id}/terms` (`PUT` reemplaza el conjunto completo de
  términos asignados, mismo patrón que `auth.SetUserRoles` de CONTRACT-08 — reemplazo atómico,
  no altas/bajas individuales). Un término inexistente asignado → 400 claro.
- UI: `/admin/terms` (listar/crear/editar/borrar categorías y tags, gateado `terms.manage`) +
  checkboxes de términos en los forms de crear/editar `articles`/`products` (mismo patrón que
  los checkboxes de roles en CONTRACT-08). Agregá "Categorías y tags" a la navegación de
  CONTRACT-10.
- Borrar un término: si tiene contenido asignado, el borrado NO debe fallar de forma confusa —
  la fila de `article_terms`/`product_terms` se borra en cascada (mismo patrón `foreignKeyCascade`
  que ya usa el resto del esquema), el contenido simplemente pierde ese término, no se borra.

## T1 — Esquema

FIX/OBJETIVO: `taxonomies` (catálogo fijo + seed), `terms` (con jerarquía opcional vía
`parent_id`), `article_terms`/`product_terms` (junctions). `Schema.Validate()` + `CompileDDL`
para ambos motores, limpio. Test de `SeedCatalogs` extendido: sembrar `taxonomies` dos veces no
duplica ni falla (mismo patrón idempotente que roles/permisos).

## T2 — API JSON de términos

FIX/OBJETIVO: `POST/GET/PUT/DELETE /terms` (CRUD), gateado `terms.manage`; listar es lectura
(solo sesión/token válido, sin gateo de permiso, igual que el resto del proyecto). Slug
duplicado dentro de la misma taxonomía → 400 claro (mismo patrón de traducción de constraint
que `sku` en CONTRACT-11 — no lo reinventes, replicalo).

## T3 — Asignación de términos a contenido

FIX/OBJETIVO: `PUT /articles/{id}/terms` y `PUT /products/{id}/terms` — reemplazan el conjunto
completo de términos asignados (array de ids en el body), gateado `terms.manage`. Un id de
término inexistente en el array → 400, nada se modifica (mismo patrón transaccional que
`SetUserRoles`). `GET /articles/{id}` y `GET /products/{id}` ahora incluyen los términos
asignados en la respuesta (array de `{id, name, slug, taxonomy}` o similar — a tu criterio la
forma exacta, documentala).

## T4 — UI

FIX/OBJETIVO: `/admin/terms` (listar/crear/editar/borrar, gateado `terms.manage`, mismo patrón
htmx que `/admin/articles`). En los forms de crear/editar de `articles` y `products`: checkboxes
de términos disponibles (agrupados por taxonomía — categorías separadas de tags), gateado por el
MISMO permiso que ya gatea crear/editar ese contenido (`content.create`/`content.update`) MÁS
`terms.manage` para la asignación específica — a tu criterio si lo simplificás exigiendo ambos
permisos o si la asignación de términos viaja dentro del mismo permiso de contenido; documentá
la decisión y su porqué. "Categorías y tags" en la navegación de CONTRACT-10.

## T5 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces, `httptest.NewTLSServer`
para cookie):
- Round-trip completo: crear taxonomía... NO, taxonomías son fijas — crear un TÉRMINO
  ("Electrónica" en `category`) → aparece en `/terms` → asignar a un artículo real →
  `GET /articles/{id}` lo incluye → desasignar (reemplazar el set sin ese id) → ya no aparece.
- Jerarquía: crear un término con `parent_id` de otro término real → confirmalo en la
  respuesta. Borrar el padre → el hijo sigue existiendo con `parent_id` NULL (no se lo llevó
  puesto), confirmado con una query real.
- Slug duplicado dentro de la misma taxonomía → 400. El MISMO slug en taxonomías DISTINTAS →
  permitido (confirmalo explícitamente, es el punto del `unique` compuesto).
- Gateo por permiso: sesión/token sin `terms.manage` → 403/401 según corresponda, tanto para
  CRUD de términos como para (re)asignación.
- Confirmá explícitamente que TODO lo de contratos anteriores (JSON y UI de
  articles/products/users/roles/api-keys) sigue funcionando exactamente igual.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: esquema compila para ambos motores, seed de `taxonomies` idempotente.
- [ ] T2: CRUD de términos completo, slug duplicado dentro de la misma taxonomía → 400.
- [ ] T3: asignación/desasignación real vía API, id de término inexistente → 400 sin efecto,
  términos incluidos en la respuesta de `GET` de contenido.
- [ ] T4: UI de términos + checkboxes de asignación en articles/products, nav actualizada.
- [ ] T5: round-trip completo, jerarquía con `SetNull` en borrado del padre confirmada, gateo
  por permiso probado, slug duplicado en taxonomías distintas permitido, contratos anteriores
  confirmados sin cambios.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias nuevas.
- NO commitear (el orquestador commitea y despliega tras verificar).
- El permiso `terms.manage` es el ÚNICO agregado al catálogo — no agregues otros.
- **CRÍTICO (aprendido en CONTRACT-11, no lo repitas):** este contrato agrega tablas nuevas a un
  esquema que YA está desplegado en producción con datos reales. `store.EnsureSchema`
  (`internal/store/store.go`) YA fue arreglado para aplicar solo lo que falta — NO lo toques, y
  NO asumas que hace falta ningún cambio ahí. Si tu verificación local (base nueva, desde cero)
  no alcanza para confirmar esto, decilo en el reporte; el orquestador va a re-verificar el
  redeploy contra una copia real de la base de producción antes de tocarla, como ya hizo en
  CONTRACT-11.

## Checklist antes de delegar

- [ ] RECON corrido: decisión de arquitectura (relación por CPT, no polimórfica) confirmada,
  helpers reusables identificados (`catalogTable`, `junctionTable`), permiso nuevo justificado.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿un término asignado a un artículo que después se BORRA (el artículo, no el
  término) dispara algo raro? (la fila de `article_terms` se borra en cascada vía FK, el
  término sigue existiendo — confirmalo). ¿Ciclo de jerarquía (A es padre de B, B es padre de
  A)? — si el contrato no lo previene explícitamente en el esquema (probablemente no, un check
  de ciclo es aplicación, no esquema), documentá el comportamiento real, no lo asumas.
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación EN NAVEGADOR/HTTP y el DEPLOY a producción (con el protocolo de
  copia-real-antes-de-tocar-produccion de CONTRACT-11) los hace el orquestador después de
  integrar.

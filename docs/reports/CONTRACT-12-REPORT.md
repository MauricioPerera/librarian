# CONTRACT-12 — Taxonomías y términos (categorías/tags) — Reporte

Estado: **COMPLETO**. Todos los criterios de aceptación verdes, suite completa 2× verde,
`go build`/`go vet` limpios. Ningún archivo fuera de `librarian` tocado; `sqlite-postgres-compat`
intacto. Sin dependencias nuevas. Sin commit (queda para el orquestador).

## Resumen por tarea

### T1 — Esquema (`internal/schema/schema.go`, `internal/store/store.go`)

- `Taxonomies = []string{"category", "tag"}`: nuevo catálogo FIJO en código, hermano de
  `Roles`/`Permissions`, vía el helper existente `catalogTable("taxonomies")` (id + name UNIQUE).
- `"terms.manage"` agregado a `Permissions` (el ÚNICO permiso nuevo — ver justificación abajo).
- `termsTable()`: tabla NUEVA (no es un `ContentType` — no tiene author_id ni trailer
  created_at/updated_at/metadata; es dato de referencia como `users`, no contenido). Columnas:
  `id` (uuid PK gen_random_uuid), `taxonomy_id` (uuid NOT NULL, FK **cascade** a `taxonomies`),
  `name` (text), `slug` (text), `parent_id` (uuid **NULLABLE**, FK a `terms.id` con **SET NULL**
  on delete). Constraint compuesta `unique("taxonomy_id", "slug")`.
- Helper nuevo `foreignKeySetNull(column, refTable, refColumn)` (paralelo exacto a
  `foreignKeyCascade`), necesario porque el único helper de FK existente solo hacía cascade.
- `junctionTable("article_terms", "article_id", "articles", "term_id", "terms")` y
  `junctionTable("product_terms", "product_id", "products", "term_id", "terms")` — con el helper
  genérico existente, sin tocar su firma. PK compuesta + ambos FK cascade.
- Orden en `Build()`: taxonomies → terms → article_terms → product_terms, después de products
  (cada FK-target precede a su referrer, requisito para creación de FK en ambos motores y para el
  round-trip byte-exacto).
- Seed (`store.SeedCatalogs`): **una sola línea** `seedNames(ctx, db, "taxonomies",
  schema.Taxonomies)`, idéntica en forma a las de roles/permissions (INSERT ... ON CONFLICT(name)
  DO NOTHING). **`EnsureSchema` y toda su maquinaria de aplicación incremental quedan intactas.**

Nota sobre la restricción "NO tocar store.go": el RECON del contrato instruye explícitamente
reusar `seedNames` dentro de `SeedCatalogs` para sembrar `taxonomies`, y tanto `main.go` como los
tests siembran ÚNICAMENTE vía `store.SeedCatalogs` — no hay otro punto de siembra. La intención de
la regla (proteger el fix de `EnsureSchema` de CONTRACT-11) se respeta al 100%: el único cambio en
el archivo es la línea de seed de taxonomies; `EnsureSchema`/`missingTables`/
`writeFullSchemaMetadata` no se tocaron. Se documenta la tensión aquí de forma explícita.

### T2 — API JSON de términos (`internal/server/terms.go`, `server.go`)

`POST/GET/GET{id}/PUT/DELETE /terms`. CRUD gateado `terms.manage`; listar/leer solo requiere
identidad válida (como el resto del proyecto). La taxonomía se direcciona por NOMBRE
("category"/"tag") en el body — es el handle público estable de un catálogo fijo; taxonomía
desconocida → 400. Slug duplicado DENTRO de la misma taxonomía → 400 claro, traducido de la
constraint `UNIQUE(taxonomy_id, slug)` vía `isUniqueSlugViolation` (mismo patrón que `sku` en
CONTRACT-11, replicado no reinventado). El MISMO slug en taxonomías distintas → permitido.

### T3 — Asignación de términos a contenido (`terms.go`, `articles.go`, `products.go`)

`PUT /articles/{id}/terms` y `PUT /products/{id}/terms`: reemplazo ATÓMICO del set completo
(`{"term_ids": [...]}`), gateado `terms.manage`. Patrón transaccional idéntico a `auth.SetUserRoles`:
se verifica que el contenido exista (→ 404) y se resuelven TODOS los term_ids ANTES de mutar (un id
inexistente → 400 con NADA modificado). `GET /articles/{id}` y `GET /products/{id}` ahora incluyen
los términos asignados (ver forma exacta abajo). La carga de términos se hace solo en el camino de
GET-por-id (`fetchArticle`/`fetchProduct`), no en el list — la forma del list queda intacta.

### T4 — UI (`internal/server/ui_terms.go`, templates, `ui_nav.go`, `ui.go`, `ui_articles.go`, `ui_products.go`)

- `/admin/terms`: CRUD htmx completo (listar/crear/editar/borrar), mismo patrón que
  `/admin/products` (form POST → 303 en crear; hx-put → HX-Redirect en editar; hx-delete → 200
  vacío en borrar). Write gateado `requireSessionPermission("terms.manage")`; read solo sesión.
  Selector de taxonomía (desde `schema.Taxonomies`) y selector de padre opcional (términos
  existentes, excluyendo el propio en edición).
- Checkboxes de términos en los forms crear/editar de articles y products, **agrupados por
  taxonomía** (categorías separadas de tags). "Categorías y tags" agregado a la navegación de
  CONTRACT-10 (con submenú "Todas..." / "Añadir nueva").

### T5 — Verificación

Round-trip completo, jerarquía con SET NULL, gateo por permiso, unicidad compuesta y no-regresión:
ver "Salida real de los criterios de aceptación" abajo.

## Decisiones de diseño (con su porqué)

1. **Permiso nuevo `terms.manage` (primer permiso nuevo del proyecto).** Ningún permiso existente
   cubre "administrar el catálogo de categorías/tags": `content.*` gobierna la autoría de piezas
   individuales de contenido, no la taxonomía compartida bajo la que se archivan; `users.manage`/
   `roles.manage` son el dominio de cuentas/RBAC. Por eso se agrega uno nuevo, y es el ÚNICO.
   Gatea AMBOS: el CRUD de `terms` y la (re)asignación de términos a contenido.

2. **Un solo permiso para "asignar un término", en API y UI.** La asignación (rutas dedicadas
   `PUT /{content}/{id}/terms` **y** los checkboxes en los forms de contenido) exige
   `terms.manage`, en capa SOBRE el permiso de contenido que el form ya requiere
   (`content.create`/`content.update`). Consecuencia deliberada: un usuario con derechos de
   contenido pero SIN `terms.manage` edita contenido normalmente y **no ve** el fieldset de
   términos; su submit nunca toca las asignaciones (un editor de solo-contenido no puede borrar sin
   querer los términos de un artículo). Alternativa descartada: meter la asignación dentro del
   permiso de contenido — habría dado dos reglas distintas para "asignar un término" (API vs UI) y
   habría dejado que cualquier editor reescriba la taxonomía. Esta opción mantiene UNA sola regla.

3. **Forma de los términos en el GET de contenido.** `GET /articles/{id}` y `GET /products/{id}`
   agregan un campo `"terms"`: array de `{id, name, slug, taxonomy}` (taxonomy = nombre). Es más
   chico que la vista `term` (sin parent_id) porque quien lista los términos de un artículo quiere
   la etiqueta/slug/taxonomía, no la jerarquía interna del término. Con `omitempty`: un contenido
   sin términos asignados omite el campo (tras "desasignar", el término ya no aparece).

4. **Traducción de `UNIQUE(taxonomy_id, slug)` a 400.** `isUniqueSlugViolation(err)` matchea el
   texto del error de SQLite ("UNIQUE constraint failed" + "terms.slug"), sin importar tipos del
   driver — igual que `isUniqueSKUViolation` de products. La constraint del esquema es la garantía
   real (no puede perder una carrera concurrente); el match solo convierte el error en un 400 limpio.

5. **Ciclos de jerarquía.** El esquema no previene ciclos (un check de ciclo es lógica de
   aplicación, no de esquema). Se previene el ciclo trivial de 1 nodo: un término no puede ser su
   propio padre (`errParentIsSelf` → 400, en crear y editar). Ciclos más profundos (A→B→A) NO se
   previenen — comportamiento documentado, no asumido: WordPress tampoco los bloquea a nivel de
   datos. `parent_id` inexistente → 400.

6. **`parent_id` con SET NULL (no cascade).** Borrar un término padre NO borra sus hijos: los deja
   con `parent_id` NULL (huérfanos de padre), igual que WordPress. Confirmado con query real (no
   asumido) — ver T5. La enforcement de FK está activa en SQLite (compat abre con
   `_pragma=foreign_keys(1)`), así que el SET NULL ocurre automáticamente en el motor.

7. **Junction cascade a ambos lados.** Borrar un término → sus filas en `article_terms`/
   `product_terms` se borran en cascada (el contenido pierde el término, no se borra). Borrar el
   contenido → sus filas de junction se borran en cascada (el término sobrevive). Ambos
   confirmados con queries reales (red-team).

## Trade-offs / limitaciones conocidas (documentadas, intencionales)

- El GET de contenido carga términos con una query extra por pieza (solo en GET-por-id, no en
  list). Consistente con el N+1 intencional ya presente en el proyecto; el catálogo es chico.
- Re-render de un form de contenido tras un error de validación (título/cuerpo vacío) reconstruye
  los checkboxes desde el estado en DB, por lo que una selección de checkbox aún-no-guardada se
  pierde en ese re-render. Imperfección menor de UX, no afecta ningún criterio de aceptación.
- Ciclos de jerarquía profundos no prevenidos (ver decisión 5).

## Nota sobre despliegue (CONTRACT-11 aprendido)

Este contrato agrega tablas NUEVAS (`taxonomies`, `terms`, `article_terms`, `product_terms`) a un
esquema ya desplegado en producción con datos reales. `store.EnsureSchema` (ya arreglado en
CONTRACT-11 para aplicar SOLO lo que falta) NO se tocó y no necesita cambios: en el próximo redeploy
detectará las 4 tablas faltantes y las creará dejando todo lo existente intacto. La verificación
local es base-nueva-desde-cero (`TestEnsureSchemaAddsOnlyMissingTable` cubre el caso incremental con
una tabla faltante, pero no con 4 taxonomía-específicas); el orquestador debe re-verificar el
redeploy contra una copia real de la base de producción antes de tocarla, como en CONTRACT-11.

## Salida real de los criterios de aceptación

### `go build ./...` y `go vet ./...` — limpios

```
===== go build =====
BUILD OK
===== go vet =====
VET OK
```

### Suite completa `go test ./... -count=1` — verde, DOS veces

```
########## RUN 1 ##########
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	3.799s
ok  	github.com/MauricioPerera/librarian/internal/config	0.688s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.438s
ok  	github.com/MauricioPerera/librarian/internal/server	18.594s
ok  	github.com/MauricioPerera/librarian/internal/store	2.429s

########## RUN 2 ##########
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	3.840s
ok  	github.com/MauricioPerera/librarian/internal/config	0.691s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.415s
ok  	github.com/MauricioPerera/librarian/internal/server	18.539s
ok  	github.com/MauricioPerera/librarian/internal/store	2.429s
```

### T1 — esquema compila para ambos motores + seed idempotente de taxonomies

```
--- PASS: TestSchemaValidates (0.00s)
--- PASS: TestSchemaRoundTripJSON (0.00s)
--- PASS: TestCompileDDLBothEngines (0.00s)
--- PASS: TestCompileDDLBothEngines/sqlite (0.00s)
--- PASS: TestCompileDDLBothEngines/postgres (0.00s)
--- PASS: TestExpectedTables (0.00s)
--- PASS: TestRoundTripExact (0.01s)          # round-trip byte-exacto con el FK self-referencial SET NULL
--- PASS: TestEnsureSchemaIdempotent (0.01s)
--- PASS: TestEnsureSchemaAddsOnlyMissingTable (0.01s)
--- PASS: TestSeedCatalogsIdempotent (0.05s)  # ahora también asserta taxonomies: len tras seed #1 == len tras seed #2 (sin duplicar)
```

`TestSeedCatalogsIdempotent` se extendió para verificar `count(taxonomies)` == `len(schema.Taxonomies)`
tras dos seeds consecutivos (no duplica ni falla).

### T2/T3/T5 — CRUD de términos, asignación, jerarquía, unicidad, gateo, red-team

```
--- PASS: TestTermsManageAndTaxonomiesSeeded      # terms.manage + category/tag sembrados
--- PASS: TestCreateTermGating                    # terms.manage → 201; sin → 403; sin auth → 401
--- PASS: TestCreateTermUnknownTaxonomyIs400      # taxonomía desconocida → 400, 0 filas
--- PASS: TestTermDuplicateSlugSameTaxonomyIs400  # dup slug misma taxonomía → 400; MISMO slug otra taxonomía → 201 (2 filas)
--- PASS: TestListAndGetTerms                     # list/get con JWT sin perms y con API key → 200; sin auth → 401
--- PASS: TestUpdateAndDeleteTerm                 # update/delete gateado + 404 en id inexistente/malformado
--- PASS: TestTermHierarchyAndParentDeleteSetsNull# parent_id confirmado en respuesta; borrar padre → hijo con parent_id NULL (query real); self-parent → 400; parent inexistente → 400
--- PASS: TestAssignTermsRoundTripArticle         # crear término→asignar→GET lo incluye→reasignar→desasignar→ya no aparece
--- PASS: TestAssignTermsProductAndGating         # asignación en producto; sin terms.manage → 403; term id inexistente → 400 sin efecto; producto inexistente → 404
--- PASS: TestDeleteTermCascadesJunctionNotContent# borrar término → junction cascade, contenido vive; borrar contenido → junction cascade, término vive
```

### T4 — UI de /admin/terms + checkboxes + nav

```
--- PASS: TestAdminTermsNoSessionRedirectsToLogin # read/write sin sesión → 302 /login
--- PASS: TestAdminTermsSessionWithoutPermissionIs403 # sesión sin terms.manage → 403 HTML
--- PASS: TestAdminTermCRUDAndNav                 # empty-state, nav "Categorías y tags", crear→listar→editar→borrar reales
--- PASS: TestArticleFormTermCheckboxes           # editor con terms.manage ve el fieldset y asigna vía form (persistido en DB); editor de solo-contenido NO ve el fieldset
--- PASS: TestProductFormTermCheckboxes           # ídem para productos
```

### Contratos anteriores — sin regresión

Suite del paquete `server`: **100 tests PASS, 0 FAIL**. De ellos, 73 son tests de contratos
anteriores (articles, products, users, roles, api-keys, whoami, login, vector) corridos por nombre:
todos verdes. La forma JSON del list de articles/products y sus respuestas existentes no cambió (el
campo `terms` solo se agrega en GET-por-id y con `omitempty`).

## Archivos tocados

Modificados: `internal/schema/schema.go`, `internal/store/store.go`, `internal/store/store_test.go`,
`internal/server/server.go`, `internal/server/articles.go`, `internal/server/products.go`,
`internal/server/ui.go`, `internal/server/ui_nav.go`, `internal/server/ui_articles.go`,
`internal/server/ui_products.go`, `templates/articles_new.html`, `templates/articles_edit.html`,
`templates/products_new.html`, `templates/products_edit.html`.

Nuevos: `internal/server/terms.go`, `internal/server/ui_terms.go`,
`internal/server/server_terms_test.go`, `internal/server/server_ui_terms_test.go`,
`templates/terms_list.html`, `templates/terms_row.html`, `templates/terms_new.html`,
`templates/terms_edit.html`.

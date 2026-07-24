# Contrato 13 — CPT dinámicos: registro de definiciones y esquema canónico compuesto

Prerrequisitos: `CONTRACT-01`..`CONTRACT-12` completos (`822f5a2`, producción real en
`librarian.ardf.dev`). Primer contrato de la fase 3 (`DEFINITION-CPT-DINAMICOS.md`).

Este contrato NO agrega UI ni CRUD de contenido dinámico (son CONTRACT-14/15). Entrega la
fundación: persistir definiciones de tipos de contenido, validarlas con seguridad, crear su tabla
real en runtime, y — lo más importante — que el esquema canónico del sistema pase a ser
**código + dinámico** en TODOS los lugares que hoy asumen que es solo código.

## RECON ya resuelto (crítico — no re-investigar, pero leelo entero)

- **`schema.Build()` es tratado hoy como EL esquema canónico completo en tres lugares, y los tres
  romperían silenciosamente con tablas dinámicas:**
  1. `store.EnsureSchema` (`internal/store/store.go`): `want := schema.Build()` calcula qué tablas
     faltan Y **reescribe la metadata `__compat_schema`** con ese `want` (fix de CONTRACT-11). Con
     tablas dinámicas existentes en la DB pero ausentes de `Build()`, cada arranque BORRARÍA esas
     tablas del esquema registrado en la metadata.
  2. `schema.JSON()` (`internal/schema/dump.go`, usado por `librarian --dump-schema`): es el
     `schema_ref` que consume `compat copy` para exportar a PostgreSQL. Los CPT dinámicos y sus
     datos quedarían **silenciosamente fuera del export** — exactamente la garantía que motiva el
     proyecto entero (`DEFINITION.md`).
  3. `compat.InferFeatures(schema.Build())` (en `internal/server/export_fixture_test.go`): el
     contrato del `compat audit`.
  Los tres tienen que pasar a componer código + definiciones persistidas. Esta es la parte más
  importante del contrato, no un detalle de implementación.
- **`compat` NO valida identificadores.** `Schema.Validate()` solo chequea nombre no vacío, no
  reservado (`__compat_schema`/`__compat_applied_changes`/etc.) y no duplicado — cualquier string
  pasa (espacios, unicode, 500 caracteres, empezar con dígito). `quoteIdentifier` sí escapa
  comillas dobles correctamente, así que el DDL que emite `compat` está protegido — PERO el CRUD
  genérico de CONTRACT-14 va a tener que **interpolar el nombre de tabla en sus propias queries**
  (un identificador no se puede parametrizar con `?`), y ahí no hay ninguna red. La validación
  estricta es responsabilidad de `librarian`, no de `compat`.
- **Divergencia real entre motores ya documentada en este proyecto:** SQLite pliega mayúsculas
  incluso en identificadores CITADOS; PostgreSQL no. Un CPT llamado `MiTipo` y otro `mitipo`
  colisionarían en SQLite pero no en Postgres — divergencia silenciosa que rompe la
  exportabilidad. Por eso el validador debe forzar minúsculas, no solo "rechazar duplicados".
- Nombres a reservar (además de los `__compat_*` que ya reserva compat): TODAS las tablas que hoy
  produce `schema.Build()` — `users`, `roles`, `permissions`, `role_permissions`, `user_roles`,
  `api_keys`, `articles`, `products`, `taxonomies`, `terms`, `article_terms`, `product_terms` —
  y el nombre de la tabla de registro que agrega este contrato. Derivá la lista de
  `schema.Build()` en vez de hardcodearla (si mañana se agrega una tabla de código, la reserva se
  actualiza sola).
- El permiso `content_types.manage` es NUEVO (segundo del proyecto; el primero fue `terms.manage`
  en CONTRACT-12) — agregalo a `schema.Permissions`, es el catálogo fijo data-driven, el seed
  idempotente lo recoge solo.
- Tipos de campo permitidos en v1 (`DEFINITION-CPT-DINAMICOS.md`): texto, entero, decimal,
  booleano, fecha. Mapean a `compat.TextType`, `IntegerType`, `DecimalType`, `BooleanType`,
  `DateType`. NADA de JSON/vector/dominios/generadas, y NADA de relaciones (FK).
- Las definiciones son DATOS de runtime (como `terms`), no un catálogo fijo. `ContentType()`
  (`internal/schema/content_type.go`) sigue siendo el helper que arma la tabla — un CPT dinámico
  debe producir exactamente la misma forma que uno de código (`id`, `author_id` FK a users,
  columnas propias, `created_at`/`updated_at`/`metadata`), reusando ese helper sin tocar su firma.
- `EnsureSchema` ya es incremental y seguro (CONTRACT-11) — crear una tabla nueva en runtime debe
  ir por ese mismo camino, no por un `ApplySchema` suelto.

## T1 — Registro de definiciones (esquema + validación)

FIX/OBJETIVO: dos tablas nuevas de código en `schema.Build()` para persistir las definiciones
(nombre a tu criterio, ej. `content_types` y `content_type_fields`; una fila por tipo y una por
campo, con su orden, nombre y tipo). Son datos de runtime, NO usan `ContentType()` (no son
contenido). Más el permiso `content_types.manage` en `schema.Permissions`.

Y el validador de identificadores, que es la pieza de seguridad de este contrato: una función
que acepta un nombre propuesto (de tipo o de campo) y lo rechaza salvo que sea `[a-z][a-z0-9_]*`,
con un largo máximo razonable (elegí uno y justificalo), no esté en la lista de reservados
(derivada de `schema.Build()` + los `__compat_*` + los nombres de columna que `ContentType()`
inyecta: `id`, `author_id`, `created_at`, `updated_at`, `metadata`), y no colisione con una
definición ya existente. Test específico con una batería de nombres hostiles: con comillas
dobles, con `;`, con espacios, con mayúsculas, unicode, vacío, solo dígitos, empezando con
dígito, larguísimo, `users`, `id`, `__compat_schema`.

## T2 — Esquema canónico compuesto (la parte crítica)

FIX/OBJETIVO: que el esquema canónico del sistema pase a ser `schema.Build()` **+** las tablas
derivadas de las definiciones persistidas, en los TRES lugares del RECON. La forma exacta la
elegís vos (probablemente una función que recibe las definiciones leídas de la DB y devuelve el
`compat.Schema` completo, y que `EnsureSchema`/`--dump-schema` usen esa en vez de `Build()`
pelado) — pero el resultado tiene que ser:
- `EnsureSchema` crea las tablas dinámicas faltantes y escribe en `__compat_schema` el esquema
  COMPLETO (código + dinámico), nunca uno que omita las dinámicas.
- `librarian --dump-schema` emite el esquema COMPLETO — un `compat copy` con ese `schema_ref`
  incluye las tablas dinámicas y sus datos.
- `--dump-schema` necesita leer la DB para conocer las definiciones. Hoy es un modo OFFLINE
  (`cmd/librarian/main.go` lo maneja ANTES de `config.Load()`, sin DB ni JWT secret) — eso cambia
  necesariamente. Resolvelo de la forma más honesta posible (probablemente: si hay DB disponible
  la usa y emite el esquema completo; documentá el comportamiento exacto y por qué). NO dejes que
  emita silenciosamente un esquema incompleto: si por alguna razón no puede leer las definiciones,
  tiene que fallar ruidosamente, no producir un export que omita datos.

## T3 — API de definiciones + creación de la tabla real

FIX/OBJETIVO: `POST /content-types` (crear una definición: nombre + campos, gateado
`content_types.manage`) que valide (T1), persista la definición y aplique la tabla real vía el
camino de `EnsureSchema` — todo o nada: si la creación de la tabla falla, la definición NO queda
persistida (un tipo registrado sin su tabla es un estado corrupto que rompería todo lo demás).
`GET /content-types` (listar, solo identidad válida) y `GET /content-types/{name}` (detalle con
sus campos). NO hay `PUT` ni `DELETE` — `DEFINITION-CPT-DINAMICOS.md` los deja explícitamente
fuera de alcance (crear-solamente); no los implementes.

## T4 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces):
- T1: batería de nombres hostiles rechazados, uno por uno, con el resultado real de cada uno.
- T2 (lo más importante): un test que cree un CPT dinámico y luego confirme que aparece en el
  esquema que emite el dump — es decir, que un export lo incluiría. Y un test que simule el ciclo
  de reinicio: crear el CPT, correr `EnsureSchema` de nuevo (como haría un restart) y confirmar
  que (a) no falla, (b) la tabla dinámica sigue existiendo, (c) la metadata `__compat_schema`
  sigue conteniéndola. Este es exactamente el bug de dos capas que casi tumba producción en
  CONTRACT-11 — probalo de verdad, no lo asumas.
- T3: crear un CPT real vía la API, confirmar con una query directa que la tabla existe con las
  columnas esperadas (incluidas las que inyecta `ContentType()`). Nombre inválido → 400, nada
  persistido ni creado. Sin `content_types.manage` → 403.
- Confirmá explícitamente que TODO lo de contratos anteriores (JSON y UI de
  articles/products/users/roles/api-keys/terms) sigue funcionando exactamente igual.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: tablas de registro + permiso nuevo; validador rechaza toda la batería hostil.
- [ ] T2: esquema canónico compuesto en los tres lugares; dump incluye tablas dinámicas; ciclo de
  reinicio verificado (tabla y metadata sobreviven).
- [ ] T3: creación real vía API (definición + tabla, atómico), 400 en nombre inválido sin efecto
  parcial, 403 sin permiso.
- [ ] T4: contratos anteriores confirmados sin cambios.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias nuevas.
- NO commitear (el orquestador commitea y despliega tras verificar).
- `content_types.manage` es el ÚNICO permiso agregado.
- NO implementes editar ni borrar tipos (fuera de alcance por definición, y editar NO tiene
  camino limpio: `compat` no soporta `ALTER TABLE`).
- NO uses el nombre de tabla ni de columna sin pasar por el validador de T1, en ningún lado.
- El contrato público existente (rutas JSON y UI de contratos 01-12) no cambia.

## Checklist antes de delegar

- [ ] RECON corrido: los tres call sites de `schema.Build()` identificados, ausencia de
  validación de identificadores en compat confirmada leyendo su código, divergencia de
  case-folding entre motores ya documentada en el proyecto.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿qué pasa si dos requests concurrentes crean el MISMO nombre de tipo? (la unicidad
  real tiene que ser una constraint de esquema, no un chequeo de aplicación que puede perder la
  carrera — igual que `products.sku`). ¿Qué pasa si la definición se persiste pero el
  `CREATE TABLE` falla (disco lleno, nombre que compat rechaza pese a la validación)? — el estado
  parcial es el peor resultado posible, debe ser atómico. ¿Un CPT llamado igual que uno de código
  futuro? (la reserva se deriva de `Build()`, así que hoy está cubierto; documentá qué pasa si
  mañana se agrega una tabla de código con el nombre de un CPT dinámico ya existente — no lo
  resuelvas, pero decilo).
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación EN NAVEGADOR/HTTP y el DEPLOY (con el protocolo de copia-real-de-producción de
  CONTRACT-11) los hace el orquestador después de integrar.

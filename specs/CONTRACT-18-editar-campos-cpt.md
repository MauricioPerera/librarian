# Contrato 18 — Editar los campos de un tipo de contenido dinámico (cierra el hueco 2)

Prerrequisitos: `CONTRACT-01`..`CONTRACT-17` completos y desplegados. Requiere
`sqlite-postgres-compat` **v0.2.0** (la que trae `DROP TABLE`; hoy `go.mod` pide `v0.1.0` —
subirla es parte de este contrato).

Cierra el hueco 2 de `docs/PENDIENTES.md`: hoy un tipo de contenido dinámico es
**crear-solamente**. Una vez aplicada la tabla, sus campos quedan congelados y cambiar uno exige
crear un tipo nuevo y abandonar los datos. La causa era real: `compat` no tenía forma de borrar
una tabla, así que el patrón de reconstrucción dejaba una tabla huérfana por cada edición. Eso ya
no es cierto.

## Decisión YA TOMADA (no la reabras)

**Reconstrucción completa de la tabla, dentro de UNA sola transacción.** No se agrega `ALTER
TABLE` a `compat` ni se construye un mecanismo de migración por fuera de él: la edición se
implementa componiendo las operaciones que `compat` ya expresa (crear, copiar, borrar).

## RECON ya resuelto (no re-investigar)

- **`compat` no tiene `ALTER TABLE` NI `RENAME`** (verificado en v0.2.0: cero resultados para
  ambos). Por eso la tabla reconstruida no puede simplemente renombrarse sobre la original, y por
  eso hace falta una tabla de paso.
- **El DDL es transaccional en los dos motores**, y `CreateContentType`
  (`internal/store/contenttypes.go`) ya lo explota: `CREATE TABLE` + INSERTs en el registro +
  escritura de la metadata completa, todo en UNA transacción que commitea o revierte junta. Este
  contrato extiende ese mismo patrón. Leelo entero antes de escribir una línea: es el modelo a
  seguir, incluidos sus comentarios sobre por qué NO llama a `store.ApplySchema`.
- **`compat.Store.DropTable` abre su PROPIA transacción**, así que NO sirve acá (y `compat` fija
  el pool de SQLite a una sola conexión: una transacción anidada haría deadlock). Usá la función
  PURA `compat.CompileDropTable(target, table)` y ejecutá la sentencia dentro de tu transacción —
  exactamente el mismo trato que `CreateContentType` le da a `compat.CompileDDL`. La separación
  pura/ejecutora de v0.2.0 existe para esto.
  - **Ojo**: `Store.DropTable` mantiene veraz la metadata `__compat_schema` reescribiéndola en su
    propia transacción. Si usás la función pura, **esa responsabilidad pasa a ser tuya**: tu
    transacción debe dejar escrita la metadata compuesta COMPLETA al final, como ya hace
    `CreateContentType`. Que la metadata quede mintiendo es la trampa que más veces mordió en este
    proyecto — `InspectSchema` la PREFIERE por sobre el catálogo físico.
- **`schema.DynamicTableName(typeName)` (`internal/schema/identifier.go`) es el ÚNICO punto** que
  deriva el nombre real de tabla, y `DynamicTable`/`TableName` delegan en él. El nombre real de un
  tipo NO cambia con este contrato: `eventos` sigue viviendo en `cpt_eventos` antes y después de
  cada edición. Lo único nuevo es una tabla de paso transitoria.
- **La tabla de paso necesita un nombre que ningún tipo público pueda producir.** `cpt_` +
  cualquier nombre válido nunca produce un nombre que empiece con `cptmp_` (el `_` del prefijo lo
  impide: un tipo `mp_eventos` da `cpt_mp_eventos`). Elegí el prefijo, documentá por qué es
  disjunto, y protegelo con el MISMO tipo de test que CONTRACT-17 le puso a `cpt_`: uno que
  recorra `schema.Build()` y falle si alguna tabla de código lo usa. Sin ese test es una
  convención, no una garantía.
- **`schema.FieldDefinition` no tiene identidad** (`{Name, Type}`). Es el tipo de COMPILACIÓN y
  debe quedar así. La identidad de un campo existe en la base: `content_type_fields.id`. Un
  renombre solo es distinguible de "borrar uno y agregar otro" si el llamador refiere al campo por
  esa identidad — decidí la forma de la API con eso en mente y documentá la decisión.
- El permiso es el que ya existe: `content_types.manage`. **NINGÚN permiso nuevo.**
- `content_types` **no tiene `updated_at`**, y su comentario en `internal/schema/schema.go`
  explica por qué: "una columna que nunca podría cambiar sería una mentira". Con este contrato ya
  no lo sería. Agregarla o no es tu decisión; si la agregás, es un cambio de esquema de una tabla
  de código, o sea que `EnsureSchema` **no** lo aplica solo (solo agrega tablas faltantes, nunca
  toca las existentes) → el orquestador tendría que migrar producción a mano. Evaluá si vale ese
  costo y **decí explícitamente en el reporte qué elegiste y por qué**; si la agregás, avisá en el
  reporte que requiere migración manual.

## Semántica de la edición

- **Agregar un campo**: la columna nueva queda en NULL para las filas existentes.
- **Renombrar un campo**: los datos se PRESERVAN (se copian de la columna vieja a la nueva).
- **Quitar un campo**: los datos de esa columna **se pierden, irreversiblemente**. Esto no puede
  ocurrir por accidente: exigí una confirmación explícita del llamador y decí en la respuesta/UI
  qué se va a perder ANTES de hacerlo.
- **Cambiar el TIPO de un campo: FUERA DE ALCANCE.** Castear entre familias diverge fuerte entre
  SQLite y PostgreSQL (`'abc'` a entero es 0 en uno y un error en el otro), y este proyecto existe
  para no tener divergencias así. Quien necesite cambiar un tipo, quita el campo y agrega otro,
  asumiendo la pérdida. Rechazalo con un error que EXPLIQUE esto, no con un 400 mudo.
- Las columnas que inyecta `ContentType()` (`id`, `author_id`, `created_at`, `updated_at`,
  `metadata`) no son campos editables y deben conservarse intactas, con sus valores, en la
  reconstrucción. Esto incluye los `id` de las filas: **una edición de campos no cambia la
  identidad de ningún contenido existente** — una URL a un contenido concreto sigue funcionando.

## T1 — La reconstrucción

FIX/OBJETIVO: la operación de store que, dada la definición nueva de un tipo existente, deja la
tabla real con la forma nueva y los datos preservados. En UNA transacción, en este orden: crear la
tabla de paso con la forma NUEVA, copiar los datos desde la original (mapeando las columnas que
sobreviven, por identidad de campo, no por nombre), borrar la original, crear la definitiva con el
nombre de siempre, copiar de vuelta, borrar la de paso, actualizar las filas del registro, y
escribir la metadata compuesta completa. Si algo falla, la transacción revierte y **la base queda
exactamente como estaba**: ese es el criterio, y hay que probarlo, no afirmarlo.

Toda la SQL de copia se arma con el mismo `quoteIdentifier` que ya usa
`internal/server/content.go` y re-valida los identificadores; ningún nombre llega crudo a una
sentencia.

## T2 — La vía de escritura (API)

FIX/OBJETIVO: la ruta JSON que expone T1, gateada por `content_types.manage`, con la confirmación
explícita para la pérdida de datos. Validá la definición nueva con la MISMA `Validate()` de
siempre (el gate de identificadores no se relaja por ser una edición) y rechazá con mensajes
accionables: tipo inexistente, nombre de campo inválido, campo duplicado, cambio de tipo. Los
errores de validación no deben tocar la base.

Concurrencia: `CreateContentType` se serializa con un mutex en la capa HTTP precisamente porque
dos operaciones de esquema simultáneas se pisan al hacer el diff. **Esta operación entra en el
mismo régimen** — encontralo y usalo, no inventes otro.

## T3 — La vía de escritura (UI)

FIX/OBJETIVO: editar los campos de un tipo desde la UI de administración, respetando el patrón de
la sección de tipos de contenido que ya existe (`ui_contenttypes.go`). La pérdida de datos por
quitar un campo tiene que ser visible ANTES de confirmar, nombrando el campo concreto. Respetá el
guardián de CONTRACT-15: `h.page(r, title)`, nunca un literal `pageData{`.

## T4 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces):

- Round-trip con datos REALES: crear un tipo, cargar varias filas con valores en todos los campos,
  editar (agregar + renombrar + quitar en una sola operación), y confirmar fila por fila que los
  datos de los campos que sobreviven siguen ahí, que el renombrado conserva su valor, que el nuevo
  quedó NULL, y que los `id` de las filas NO cambiaron.
- Confirmar por consulta directa al catálogo que la tabla se llama igual que antes y que **no
  quedó ninguna tabla de paso**.
- Fallo a mitad de camino: forzar un error después de haber copiado datos y confirmar que la tabla
  original quedó intacta con todos sus datos y que el registro no cambió. Sin esta prueba, la
  atomicidad es una afirmación.
- Ciclo de reinicio: editar, reiniciar (`EnsureSchema`), confirmar que arranca limpio y que no
  intenta crear nada. Dos arranques seguidos, no uno — el segundo es el que atrapó el bug de
  CONTRACT-11.
- `InspectSchema` y `--dump-schema` reflejan la forma NUEVA (o sea: el export a Postgres lleva las
  columnas nuevas y no las viejas).
- Editar un tipo que tiene CERO filas, y editar uno sin quitar ningún campo: los dos caminos
  fáciles también tienen que andar.
- El CRUD genérico (`/content/{tipo}` y `/admin/content/{tipo}`) funciona con la forma nueva
  inmediatamente después de editar, sin reiniciar.
- Confirmá explícitamente que TODO lo de contratos anteriores sigue funcionando igual.

## Criterios de aceptación

- [ ] `go.mod` pide `sqlite-postgres-compat` v0.2.0 y `go build ./...`/`go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: reconstrucción atómica; nombre de tabla estable; `id` de filas preservados.
- [ ] T1: la metadata `__compat_schema` queda coincidiendo con el catálogo real tras la edición.
- [ ] T2: ruta gateada por `content_types.manage`, con confirmación explícita para la pérdida.
- [ ] T3: UI con la advertencia de pérdida nombrando el campo concreto.
- [ ] T4: todos los puntos de arriba con salida real, incluida la prueba de rollback y el reinicio.
- [ ] Test que impide que una tabla de código use el prefijo de la tabla de paso.
- [ ] Decisión sobre `updated_at` en `content_types` tomada y justificada en el reporte.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. **NO toques `sqlite-postgres-compat`** — ya tiene lo
  que hace falta; si creés que le falta algo, PARÁ y reportalo en vez de trabajar alrededor.
- Sin dependencias nuevas más allá del bump de `compat` a v0.2.0. NINGÚN permiso nuevo.
- NO commitear (el orquestador commitea y despliega tras verificar).
- NO agregues cambio de tipo de campo, ni borrado de un tipo completo: fuera de alcance.
- El contrato público de las rutas de contratos 01-17 no cambia.
- La tabla de paso es un detalle interno: no puede aparecer en ninguna respuesta ni en la UI.

## Checklist antes de delegar

- [ ] RECON corrido: ausencia de `ALTER`/`RENAME` confirmada en v0.2.0, DDL transaccional
  confirmado como el mecanismo, `CompileDropTable` (pura) identificada como la única utilizable
  dentro de una transacción, responsabilidad de la metadata entendida.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿qué pasa si el tipo tiene MUCHAS filas (la copia doble es O(2n) — aceptable acá,
  pero decilo)? ¿Y si el nombre de un campo nuevo colisiona con una columna inyectada por
  `ContentType()`? ¿Y si la edición no cambia NADA (definición idéntica)? ¿Y si dos campos se
  renombran cruzados (`a`→`b` y `b`→`a`)? — ese último es el que rompe una implementación
  ingenua que renombra de a uno. ¿Qué pasa si el tipo no existe, o si existe pero su tabla no?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación HTTP/UI real y el DEPLOY los hace el orquestador.

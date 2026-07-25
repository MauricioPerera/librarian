# CONTRACT-18 — Editar los campos de un tipo de contenido dinámico (cierra el hueco 2)

Base: `d2f9398` (CONTRACT-01..17 completos y desplegados). Árbol SIN commitear, como pide el
contrato: el orquestador commitea y despliega tras verificar.

**Resultado: LISTO.** Un tipo dinámico ya no es crear-solamente. Agregar, renombrar y quitar
campos funciona por API (`PUT /content-types/{nombre}`) y por UI
(`/admin/content-types/{nombre}/edit`), gateado por el permiso que ya existía
(`content_types.manage`, **ningún permiso nuevo**). La tabla real se RECONSTRUYE dentro de UNA
transacción componiendo solo operaciones que `compat` ya expresa (`CompileDDL` +
`CompileDropTable` de v0.2.0): no se agregó `ALTER TABLE` a `compat` ni ningún mecanismo de
migración por fuera de él. El nombre real de la tabla no cambia (`eventos` sigue en `cpt_eventos`),
los `id` de las filas se preservan, y quitar un campo exige confirmación explícita campo por campo.

---

## Decisiones de diseño (con su porqué)

### 1. La forma de la API para expresar un RENOMBRE: identidad de base, lista completa

**Decisión:** el cuerpo de la edición manda la **lista COMPLETA de campos nueva, en el orden
deseado**, y cada entrada lleva opcionalmente el `id` del campo existente
(`content_type_fields.id`):

```json
PUT /content-types/eventos
{"fields": [{"id":"ff4523b3-…","name":"encabezado","type":"text"},
            {"id":"4785c0b4-…","name":"asistentes","type":"integer"},
            {"name":"resumen","type":"text"}],
 "confirm_remove": ["lugar"]}
```

- entrada **con `id`** = "esta posición ES ese campo guardado". Sus datos se copian; si el nombre
  difiere, eso ES el renombre.
- entrada **sin `id`** = campo NUEVO (queda NULL en todas las filas existentes).
- campo guardado que **ninguna entrada menciona** = se QUITA y sus datos se pierden.

**Por qué la identidad de base y no el nombre:** el contrato lo señala y es correcto —
`schema.FieldDefinition` es el tipo de COMPILACIÓN (`{Name, Type}`) y debe quedar así; la
identidad solo existe una vez persistido el campo. Sin referir por `id`, "renombrar `a` a `b`" es
literalmente indistinguible de "borrar `a` y agregar `b`", y la diferencia entre esas dos cosas es
*perder o no perder los datos de una columna*. No es una diferencia cosmética: es la diferencia
entre las dos semánticas que el contrato define.

**Por qué la lista completa y no un lenguaje de parches** ("renombrá a→b", "quitá c"): la lista de
campos es ORDENADA (`content_type_fields.ordinal` decide el orden físico de columnas), el
resultado hay que validarlo como un TODO (nombres duplicados, reservados, el gate de
identificadores) y la reconstrucción consume exactamente una forma final. Un parche habría que
reducirlo a esta misma lista igual, y convertiría el renombre cruzado en un rompecabezas de orden
para el llamador. Con la lista completa, `a`→`b` y `b`→`a` **no es un caso especial**: es el mismo
mapeo de conjunto que cualquier otro.

**Consecuencia de API (aditiva):** los `id` hay que poder leerlos, así que
`GET /content-types/{nombre}` ahora los devuelve. El campo es `json:"id,omitempty"`, de modo que
**la respuesta de creación y la del listado son byte-idénticas a las de CONTRACT-13..17** (test
`TestCreateContentTypeResponseShapeUnchanged`). Ninguna ruta de 01-17 cambió de contrato.

**La confirmación es una LISTA, no un booleano** (`confirm_remove: ["lugar"]`): el llamador tiene
que NOMBRAR cada campo que acepta perder. Un flag `confirm: true` dejaría que un cliente con una
vista vieja del tipo aceptara perder un campo que nunca vio. Por eso también se rechaza confirmar
un campo que NO se está quitando (`UnexpectedConfirmationError`) — eso delata exactamente esa
situación. Y el 400 por falta de confirmación **dice qué se va a perder**, en prosa y en una lista
legible por máquina (`removes_data_of`), que es literalmente lo que hay que reenviar.

### 2. `updated_at` en `content_types`: **NO se agrega** (y el comentario stale se actualizó)

El motivo viejo ("una columna que nunca podría cambiar sería una mentira") **caducó** con este
contrato. Aun así se decide NO agregarla, por tres razones concretas:

1. **Costo operativo real e irreversible.** `EnsureSchema` solo crea tablas FALTANTES y nunca toca
   una existente (restricción deliberada, es lo que hace seguro un reinicio). Agregar la columna
   exigiría una migración a mano en cada base desplegada ANTES de poder confiar en el binario
   nuevo. El contrato pedía evaluar ese costo: no lo vale.
2. **Nadie la consume.** Ninguna respuesta, ninguna vista de UI y ninguna decisión de export
   depende de cuándo se editó un tipo. Sería una columna agregada "porque se acostumbra".
3. **No se pierde información necesaria.** La edición es una acción administrativa gateada por
   permiso, visible en el log del servicio, y la forma ACTUAL del tipo siempre se lee del registro.

Si un contrato futuro necesita "cuándo se editó por última vez" (auditoría, token de concurrencia
optimista para el editor), que la agregue **deliberadamente y con su paso de migración**, no
heredada de acá. Todo esto quedó escrito en el comentario de `contentTypesTable()`
(`internal/schema/schema.go`), reemplazando la justificación vencida.

**Nada que migrar a mano en producción por este contrato.** El esquema de las tablas de CÓDIGO no
cambió: `go.mod` sube a compat v0.2.0 y se agregan rutas + una tabla TRANSITORIA, nada más.

### 3. La tabla de paso: prefijo `cptmp_` y **nombre FIJO** `cptmp_rebuild`

`schema.StagingTablePrefix = "cptmp_"`, `schema.StagingTableName = "cptmp_rebuild"`.

**Por qué el prefijo es disjunto (argumento estructural, no convención):**

- **Contra las tablas dinámicas:** una tabla dinámica es siempre `cpt_` + nombre, o sea que su 4º
  carácter es SIEMPRE `_`; el de una de paso es `m`. El guion bajo de `cpt_` es lo que hace la
  disjunción demostrable: un tipo llamado `mp_eventos` da `cpt_mp_eventos`, jamás `cptmp_eventos`.
  Testeado con ese caso exacto (`TestStagingPrefixIsDisjointFromDynamicTables`).
- **Contra las tablas de código:** garantizado por un test sobre `Build()`
  (`TestNoCodeTableUsesStagingPrefix`), igual que CONTRACT-17 hizo con `cpt_`. Las tablas de código
  son literales de Go y no pasan por ningún validador en runtime, así que solo un test puede
  garantizarlo.

**Por qué el nombre es una CONSTANTE y no se deriva del tipo:** `cptmp_` + un nombre de tipo legal
de 28 caracteres da 34 bytes y **fallaría `ValidateIdentifier`** (presupuesto de 32). O sea: la
edición sería imposible justo para los tipos con los nombres más largos permitidos — una
restricción silenciosa y arbitraria. Y no se gana nada con un nombre por tipo: la tabla vive dentro
de UNA transacción, la capa HTTP serializa las operaciones de esquema con `h.schemaMu`, y compat
fija el pool de SQLite a una sola conexión, así que dos reconstrucciones no pueden solaparse.
Testeado (`TestStagingTableNameIsAlwaysALegalIdentifier`).

Si la tabla de paso ya existiera (solo posible por un crash a mitad de DDL — que ambos motores
revierten — o por un operador que la creó a mano), la operación **falla ruidosamente**
(`ErrStagingTableExists`) en vez de pisarla.

### 4. `quoteIdentifier` se MUEVE a `schema`, no se duplica

El contrato exige que la SQL de copia use "el mismo `quoteIdentifier` que ya usa
`internal/server/content.go`". `store` no puede importar `server` (ciclo), así que el cuerpo se
movió a `schema.QuoteIdentifier` y `server/content.go` ahora delega en él. Sigue habiendo **una
sola implementación, un solo gate y un solo mensaje de error**.

Se agregó además `schema.QuoteInternalIdentifier` para los nombres que el propio proyecto inyecta
(`id`, `author_id`, `created_at`, `updated_at`, `metadata`, y la tabla de paso). Esos nombres están
en `ReservedNames()` justamente para que ningún CAMPO dinámico se llame así, con lo cual el gate
DINÁMICO los rechaza — correctamente. Tener dos puertas nombradas hace que cada call site diga qué
clase de nombre está manejando (`content.go` ya hacía la misma distinción interpolando sus
constantes).

### 5. Las filas del registro se ACTUALIZAN (con parqueo en dos fases), no se borran y re-insertan

Un campo que sobrevive conserva su `content_type_fields.id`, así que un cliente que leyó los ids
puede editar dos veces seguidas sin re-leerlos.

Eso obliga al parqueo en dos fases: `content_type_fields` tiene `UNIQUE(content_type_id, name)` y
`UNIQUE(content_type_id, ordinal)`, y SQLite los verifica **por sentencia**. Un renombre cruzado, o
cualquier reordenamiento, violaría uno de los dos a mitad de camino si se actualizara fila por fila
a su valor final. Por eso cada fila superviviente se parquea primero en un valor que no puede
colisionar (nombre derivado de su propio uuid, ordinal NEGATIVO — ningún campo real tiene uno) y
recién después se fija su valor definitivo. Los campos nuevos se insertan al final, en los ordinales
que ningún superviviente ocupa.

### 6. Edición idéntica = NO-OP que no toca la base

Si la lista pedida es idéntica a la guardada (mismos campos, mismos nombres, mismos tipos, MISMO
ORDEN), no se reconstruye nada: copiar todas las filas dos veces para obtener la tabla que ya
existe es riesgo puro a cambio de nada. Reordenar columnas SÍ cuenta como cambio (el orden físico
es parte del esquema y del export).

---

## Qué se implementó, por tarea

### T1 — La reconstrucción (`internal/store/contenttypes_edit.go`, nuevo)

`EditContentType(ctx, store, typeName, edits, confirmedRemovals) (ContentTypeEdit, error)`, y las
piezas puras que la sostienen:

- `LoadContentTypeFields` — lee `(typeID, []PersistedField)` con identidad, en orden de ordinal.
- `PlanContentTypeEdit` — **pura, sin base**: aplica el mismo `ContentTypeDefinition.Validate()` de
  siempre, resuelve cada entrada contra lo guardado y devuelve el plan (`Carried`/`Added`/
  `Removed`/`Renamed`/`NoOp`). Que sea pura es lo que hace que "un error de validación no toca la
  base" sea verdad **por construcción**, no por rollback.
- `compileRebuild` — **pura**: target + plan → la lista completa de sentencias. Todo identificador
  sale por `schema.QuoteIdentifier` (o `QuoteInternalIdentifier` para los nombres del proyecto);
  ningún nombre llega crudo a una sentencia.
- `applyRegistryEdit` — el ajuste de `content_type_fields` con el parqueo en dos fases.

El orden dentro de la ÚNICA transacción es exactamente el del contrato: crear la tabla de paso con
la forma NUEVA → copiar mapeando por identidad → borrar la original → crearla otra vez con el
nombre de siempre → copiar de vuelta → borrar la de paso → actualizar el registro → **escribir la
metadata compuesta COMPLETA**.

Ese último paso es la responsabilidad que se hereda por usar la función PURA
`compat.CompileDropTable` en vez de `Store.DropTable` (que abre su propia transacción y haría
deadlock con el pool de una sola conexión de compat): `Store.DropTable` mantendría `__compat_schema`
veraz por su cuenta; acá lo hace `writeFullSchemaMetadata` con el esquema compuesto entero, igual
que `CreateContentType`.

Dos precondiciones se consultan contra el catálogo propio de SQLite (nunca contra la metadata, que
es justamente lo que la operación reescribe): la tabla real del tipo TIENE que existir
(`ErrMissingTable`) y la de paso NO (`ErrStagingTableExists`).

Costo: la copia es **O(2n)** — cada fila se escribe dos veces. Se acepta explícitamente: editar un
tipo es una acción administrativa rara, y la alternativa (dejar la tabla reconstruida con otro
nombre) cambiaría el nombre real de la tabla, que CONTRACT-17 fijó en `schema.DynamicTableName` y
del que depende toda la capa CRUD genérica. Medido con 2000 filas en el test
`TestEditWithManyRowsIsLinearAndComplete`.

### T2 — La vía de escritura (API)

- `PUT /content-types/{name}` en `server.go`, gateado por `requirePermission("content_types.manage")`
  — **el mismo permiso de la creación**, ningún permiso nuevo.
- `handleEditContentType` (`internal/server/contenttypes.go`): decodifica, rechaza un cambio de
  NOMBRE del tipo (fuera de alcance, y silenciarlo sería peor que rechazarlo), hace el pre-vuelo
  read-only (404 tipo inexistente / 400 todo lo que el plan rechaza) y recién entonces toma
  `h.schemaMu` — **el mutex que ya existía** para serializar las operaciones de esquema — y ejecuta.
  `store.EditContentType` re-planifica por dentro como guarda fail-closed para llamadores no-HTTP,
  igual que `CreateContentType` re-corre `Validate()`.
- Mapeo de errores: 404 tipo inexistente; 400 con mensaje accionable para nombre inválido, campo
  duplicado, campo reservado, tipo desconocido, id desconocido, campo referido dos veces, CAMBIO DE
  TIPO (con la explicación completa de por qué no se soporta) y falta/exceso de confirmación; 500
  genérico solo para fallas de base.
- `GET /content-types/{name}` ahora emite los `id` (aditivo, ver decisión 1).

### T3 — La vía de escritura (UI)

- `GET /admin/content-types/{name}/edit` (sesión), `GET /admin/content-types/edit/field` (fragmento
  htmx de una fila nueva, sesión), `POST /admin/content-types/{name}` (`content_types.manage`).
- Plantillas nuevas `content_types_edit.html` + `content_type_edit_field_row.html`, agregadas al
  `//go:embed` de `ui.go`. La fila de edición lleva un `field_id` oculto (la identidad) y, para un
  campo existente, el selector de tipo va **deshabilitado** con un gemelo oculto que manda el tipo
  GUARDADO: la vía honesta de la UI no puede ni siquiera intentar un cambio de tipo.
- **La advertencia de pérdida, que es el criterio de T3:** el primer submit que implique quitar
  campos NO llega al store. Vuelve la misma página con el aviso
  «Atención: esta edición BORRA datos y no se puede deshacer», la lista de los campos concretos
  (`<code class="removed-field">lugar</code>`), los `confirm_remove` ocultos correspondientes y el
  botón cambiado a «Confirmar y aplicar». Solo el segundo submit aplica — y el store re-verifica la
  confirmación igual, así que la UI no es lo único que separa al admin de perder datos.
- Vaciar el nombre de una fila existente = quitar ese campo (coherente con el formulario de
  creación, donde una fila con nombre vacío se ignora), y por eso pide confirmación.
- Se respetó el guardián de CONTRACT-15: todas las páginas se construyen con `h.page(r, title)`;
  no hay ningún literal `pageData{` fuera de `ui.go` (test `TestPageDataIsBuiltOnlyByTheConstructor`,
  verde).
- `content_types_list.html`: se agregó el link «Editar campos» y se corrigió el texto que decía que
  los campos quedaban congelados (ya era mentira con este contrato).

### T4 — Verificación

Ver la sección siguiente: todo con salida real.

### Documentación

- `docs/PENDIENTES.md` — hueco 2 marcado RESUELTO, con el texto original conservado.
- `DEFINITION-CPT-DINAMICOS.md` — el ítem "fuera de alcance" queda marcado como implementado,
  aclarando que la objeción original (no construir migraciones fuera de `compat`) se **respetó**,
  no se salteó.

---

## Verificación — salida REAL

### `go.mod`, `go build`, `go vet`, `gofmt`

```
$ go get github.com/MauricioPerera/sqlite-postgres-compat@v0.2.0
go: upgraded github.com/MauricioPerera/sqlite-postgres-compat v0.1.0 => v0.2.0
$ grep compat go.mod
	github.com/MauricioPerera/sqlite-postgres-compat v0.2.0

$ go build ./...     ; echo OK
OK
$ go vet ./...       ; echo OK
OK
$ gofmt -l .
(vacío)
```

### `go test ./... -count=1`, dos veces

```
== corrida 1 ==
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.263s
ok  	github.com/MauricioPerera/librarian/internal/auth	3.996s
ok  	github.com/MauricioPerera/librarian/internal/config	0.633s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.283s
ok  	github.com/MauricioPerera/librarian/internal/server	31.053s
ok  	github.com/MauricioPerera/librarian/internal/store	3.348s

== corrida 2 ==
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.389s
ok  	github.com/MauricioPerera/librarian/internal/auth	4.121s
ok  	github.com/MauricioPerera/librarian/internal/config	0.696s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.317s
ok  	github.com/MauricioPerera/librarian/internal/server	31.607s
ok  	github.com/MauricioPerera/librarian/internal/store	3.374s
```

### Test que impide que una tabla de código use el prefijo de la tabla de paso

```
$ go test ./internal/schema/ -run Staging -v
=== RUN   TestNoCodeTableUsesStagingPrefix
    ENFORCEMENT OK: none of the 14 code tables uses the "cptmp_" prefix
--- PASS: TestNoCodeTableUsesStagingPrefix (0.00s)
=== RUN   TestStagingPrefixIsDisjointFromDynamicTables
    DISJOINT OK: dynamic tables are "cpt_"+name, staging is "cptmp_"; "cptmp_rebuild" is always a legal identifier (13 bytes)
--- PASS: TestStagingPrefixIsDisjointFromDynamicTables (0.00s)
=== RUN   TestStagingTableNameIsAlwaysALegalIdentifier
    CONSTANT OK: "cptmp_rebuild" fits (13 bytes); a derived name for the longest legal type would be 34 bytes (> 32)
--- PASS: TestStagingTableNameIsAlwaysALegalIdentifier (0.00s)
```

### T1 — reconstrucción, datos, rollback, reinicio (paquete `store`)

```
$ go test ./internal/store/ -run "Edit|ManyRows" -count=1 -v
=== RUN   TestEditContentTypeRoundTripWithRealData
    plan: added=[resumen] removed=[lugar] renamed=[[titulo encabezado]]
    ROUND-TRIP OK: 3 rows kept their ids and data; columns [id author_id encabezado asistentes gratis resumen created_at updated_at metadata]; metadata agrees with the catalog
--- PASS: TestEditContentTypeRoundTripWithRealData (0.07s)
=== RUN   TestEditContentTypeCrossRename
    CROSS-RENAME OK: values follow identity (titulo="EL LUGAR", lugar="EL TITULO") and both registry rows kept their ids
--- PASS: TestEditContentTypeCrossRename (0.07s)
=== RUN   TestEditContentTypeRollbackMidwayFK
    rebuild failed as expected: rebuild table for content type "eventos": constraint failed: FOREIGN KEY constraint failed (787)
    ROLLBACK OK: shape [id author_id titulo lugar asistentes gratis created_at updated_at metadata], row intact, registry [titulo:text lugar:text asistentes:integer gratis:boolean], metadata intact, no staging leftovers
--- PASS: TestEditContentTypeRollbackMidwayFK (0.07s)
=== RUN   TestEditContentTypeRollbackAtMetadataStep
    rebuild failed as expected at the metadata step: record full schema metadata: SQL logic error: no such table: __compat_schema (1)
    ROLLBACK-AT-LAST-STEP OK: table [id author_id titulo lugar asistentes gratis created_at updated_at metadata], registry [titulo:text lugar:text asistentes:integer gratis:boolean], no staging leftovers
--- PASS: TestEditContentTypeRollbackAtMetadataStep (0.07s)
=== RUN   TestEditContentTypeSurvivesTwoRestarts
    boot #1 OK: nothing missing, columns [id author_id encabezado asistentes resumen created_at updated_at metadata], data intact
    boot #2 OK: nothing missing, columns [id author_id encabezado asistentes resumen created_at updated_at metadata], data intact
--- PASS: TestEditContentTypeSurvivesTwoRestarts (0.07s)
=== RUN   TestEditContentTypeDumpSchemaReflectsNewShape
    DUMP OK: the export describes the NEW shape and compiles for PostgreSQL
--- PASS: TestEditContentTypeDumpSchemaReflectsNewShape (0.06s)
=== RUN   TestEditContentTypeEasyPaths
    EASY PATHS OK: zero rows, no-removal edit and identical definition all behave
--- PASS: TestEditContentTypeEasyPaths (0.08s)
=== RUN   TestEditContentTypeRejections
    unknown content type                               rejected: content type not found
    unknown field id                                   rejected: unknown field: no field with id "0000…0000" belongs to content type "eventos"
    same field twice                                   rejected: unknown field: field "titulo" is referenced twice in the same edit
    type change                                        rejected: changing the type of an existing field is not supported: field "titulo" is "text" and cannot become "integer". Casting between type families diverges between SQLite and PostgreSQL, which this project exists to prevent; remove the field and add a new one instead, accepting that its data is lost
    field name collides with an injected column        rejected: field name "updated_at" is reserved and cannot be used
    duplicate field name                               rejected: field "titulo" is declared more than once
    invalid field name                                 rejected: field name "Mal Nombre" is invalid: must match [a-z][a-z0-9_]* …
    unknown field type                                 rejected: field "raro" has unknown type "json" (allowed: [text integer decimal boolean date])
    removal without confirmation                       rejected: removing the field(s) lugar, asistentes, gratis destroys their stored data irreversibly; confirm each one explicitly to proceed
    confirmation of a field that is not being removed  rejected: confirmed the removal of the field(s) lugar, which are not being removed; reload the content type and try again
--- PASS: TestEditContentTypeRejections (0.06s)
=== RUN   TestEditContentTypeWithoutItsTable
    MISSING TABLE OK: the real table of this content type does not exist: "cpt_eventos"
--- PASS: TestEditContentTypeWithoutItsTable (0.07s)
=== RUN   TestEditContentTypeRefusesALeftoverStagingTable
    STAGING PRECONDITION OK: the rebuild staging table already exists: "cptmp_rebuild" must be dropped by hand before retrying
--- PASS: TestEditContentTypeRefusesALeftoverStagingTable (0.07s)
=== RUN   TestEditWithManyRowsIsLinearAndComplete
    MANY ROWS OK: 2000 rows copied twice (O(2n), accepted), all ids distinct, all values in correspondence
--- PASS: TestEditWithManyRowsIsLinearAndComplete (0.10s)
```

Notas sobre las dos pruebas de atomicidad (el contrato pedía UNA; se hicieron dos, en puntos
distintos del camino):

- **A mitad** (`RollbackMidwayFK`): otra tabla con una FK hacia `cpt_eventos` hace que el motor
  rechace el `DROP TABLE` original — es decir, falla en el paso 3, **después** de que las filas ya
  se copiaron a la tabla de paso. Tras el rollback: forma de tabla idéntica, fila intacta con todos
  sus valores, registro idéntico (nombres, tipos e **ids**), metadata idéntica, cero tablas de paso.
- **Al final** (`RollbackAtMetadataStep`): se elimina `__compat_schema` antes de llamar, así que la
  falla ocurre en el paso 8, **después de las dos copias y de haber tocado ya las filas del
  registro**. Mismo resultado: todo revertido.

### T2 — API real (paquete `server`)

```
$ go test ./internal/server/ -run "EditContentType|CreateContentTypeResponseShape" -count=1 -v
=== RUN   TestEditContentTypeRequiresPermission
    GATE OK: 403 for content.* only, 401 anonymous, 200 with content_types.manage; columns now [id author_id headline score price_paid verified read_on summary created_at updated_at metadata]
--- PASS: TestEditContentTypeRequiresPermission (0.26s)
=== RUN   TestEditContentTypeDataLossNeedsItemisedConfirmation
    CONFIRMATION OK: refused unconfirmed and partially confirmed, accepted the exact list; columns now [id author_id headline score created_at updated_at metadata]
--- PASS: TestEditContentTypeDataLossNeedsItemisedConfirmation (0.17s)
=== RUN   TestEditContentTypeHTTPRejections
    unknown content type         → 404 content type not found
    invalid field name           → 400 field name "Mal Nombre" is invalid: …
    duplicate field              → 400 field "headline" is declared more than once
    reserved field name          → 400 field name "metadata" is reserved and cannot be used
    unknown field type           → 400 field "raro" has unknown type "json" …
    unknown field id             → 400 unknown field: no field with id … belongs to content type "reviews"
    type change                  → 400 changing the type of an existing field is not supported …
    renaming the type itself     → 400 the name of a content type cannot be changed
--- PASS: TestEditContentTypeHTTPRejections (0.15s)
=== RUN   TestEditContentTypeThenGenericCRUD
    CRUD-AFTER-EDIT OK: row b7c69cdf-… kept its id, its renamed value and accepts the new shape without a restart
--- PASS: TestEditContentTypeThenGenericCRUD (0.17s)
=== RUN   TestEditContentTypeNeverLeaksTheStagingTable
    NO LEAK OK: no response mentions the staging table and none survives in the catalog
--- PASS: TestEditContentTypeNeverLeaksTheStagingTable (0.15s)
=== RUN   TestCreateContentTypeResponseShapeUnchanged
    SHAPE OK: create/list unchanged, single GET carries ids
--- PASS: TestCreateContentTypeResponseShapeUnchanged (0.15s)
```

### T3 — UI real (formularios y cookie de sesión reales)

```
$ go test ./internal/server/ -run AdminContentTypeEdit -count=1 -v
=== RUN   TestAdminContentTypeEditShowsDataLossBeforeDestroyingIt
    UI OK: warning named 'lugar' and changed nothing; the confirmed submit rebuilt the type keeping "Charla Go"
--- PASS: TestAdminContentTypeEditShowsDataLossBeforeDestroyingIt (0.18s)
=== RUN   TestAdminContentTypeEditWithoutRemovalNeedsNoConfirmation
    NO-REMOVAL OK: applied on the first submit; columns id,author_id,encabezado,cuerpo,created_at,updated_at,metadata
--- PASS: TestAdminContentTypeEditWithoutRemovalNeedsNoConfirmation (0.16s)
=== RUN   TestAdminContentTypeEditWithoutPermissionIs403
    UI GATE OK: form readable with a session, write refused with 403 and nothing changed
--- PASS: TestAdminContentTypeEditWithoutPermissionIs403 (0.25s)
=== RUN   TestAdminContentTypeEditUnknownTypeIs404HTML
    404 OK: HTML page for both the form and the submit
--- PASS: TestAdminContentTypeEditUnknownTypeIs404HTML (0.14s)
=== RUN   TestAdminContentTypeEditCrossRenameThroughTheUI
    UI CROSS-RENAME OK: titulo="EL LUGAR" lugar="EL TITULO", row id unchanged (e2c2d13a-…)
--- PASS: TestAdminContentTypeEditCrossRenameThroughTheUI (0.16s)
```

El primero es el criterio de aceptación de T3 y hace la secuencia completa: submit con una
remoción → **200 con la advertencia que nombra `lugar`** + comprobación por SQL de que la tabla y
los datos NO se tocaron → segundo submit con `confirm_remove` → 303 y la tabla reconstruida
conservando `"Charla Go"` en el campo renombrado.

### T4 — END-TO-END con el binario REAL

Se compiló `librarian.exe`, se sembró una base con un admin real (usuario + los 8 permisos del
catálogo) y se levantó el servidor real (`LIBRARIAN_ADDR=127.0.0.1:8931`). Todo lo de abajo es
`curl` contra ese proceso.

```
== POST /content-types
{"name":"eventos","fields":[{"name":"titulo","type":"text"},{"name":"lugar","type":"text"},{"name":"asistentes","type":"integer"}]}
== POST /content/eventos x2
ID1=839795f2-2f4f-41fc-bef3-3425253f9474
ID2=bb36cf06-f34a-4614-8a5f-133a7d0a5bda
== GET /content-types/eventos (ahora con ids)
{"name":"eventos","fields":[{"id":"ff4523b3-…","name":"titulo","type":"text"},{"id":"aa32bb55-…","name":"lugar","type":"text"},{"id":"4785c0b4-…","name":"asistentes","type":"integer"}]}

== PUT sin confirmación  -> status=400
{"confirm_remove":["asistentes","resumen"],
 "error":"removing the field(s) asistentes, resumen destroys their stored data irreversibly; confirm each one explicitly to proceed",
 "nothing_was_done":true,
 "removes_data_of":["asistentes","resumen"]}

== PUT con confirmación exacta (renombra titulo→encabezado, agrega resumen, quita lugar) -> status=200

== GET /content/eventos/839795f2-…   (MISMO id, dato renombrado, campo nuevo NULL)
{"asistentes":120,"author_id":"9a8127b7-…","created_at":"2026-07-25 07:30:51",
 "encabezado":"Feria del libro","id":"839795f2-2f4f-41fc-bef3-3425253f9474",
 "metadata":null,"resumen":null,"updated_at":"2026-07-25 07:30:51"}

== PUT /content/eventos/bb36cf06-… con la forma NUEVA, sin reiniciar
{"asistentes":41,…,"encabezado":"Charla Go 2","resumen":"resumen nuevo","updated_at":"2026-07-25 07:31:11"}

== PUT cambiando el TIPO de un campo
{"error":"changing the type of an existing field is not supported: field \"encabezado\" is \"text\" and cannot become \"integer\". Casting between type families diverges between SQLite and PostgreSQL, which this project exists to prevent; remove the field and add a new one instead, accepting that its data is lost"}

== PUT renombre CRUZADO encabezado<->resumen
{"added":[],"removed":[],
 "renamed":[{"from":"encabezado","to":"resumen"},{"from":"resumen","to":"encabezado"}],
 "fields":[{"id":"ff4523b3-…","name":"resumen","type":"text"},
           {"id":"4785c0b4-…","name":"asistentes","type":"integer"},
           {"id":"db8b6db2-…","name":"encabezado","type":"text"}],"name":"eventos"}
== filas tras el cruce: los valores SIGUEN A LA IDENTIDAD
{"items":[{"asistentes":120,…,"encabezado":null,"resumen":"Feria del libro",…},
          {"asistentes":41,…,"encabezado":"resumen nuevo","resumen":"Charla Go 2",…}],"type":"eventos"}
```

**Catálogo real: la tabla conserva su nombre y NO quedó ninguna tabla de paso.**

```
TABLE __compat_schema
TABLE api_keys
TABLE article_terms
TABLE articles
TABLE content_type_fields
TABLE content_types
TABLE cpt_eventos          <-- misma tabla de siempre, forma nueva
TABLE permissions
TABLE product_terms
TABLE products
TABLE role_permissions
TABLE roles
TABLE taxonomies
TABLE terms
TABLE user_roles
TABLE users
(ninguna cptmp_*)

forma física de cpt_eventos:
id author_id resumen asistentes encabezado created_at updated_at metadata
```

**Ciclo de reinicio (dos `EnsureSchema` seguidos + reinicio real del binario):**

```
== EnsureSchema de nuevo (x2) ==
ENSURE OK, definitions: eventos[{resumen text} {asistentes integer} {encabezado text}]
ENSURE OK, definitions: eventos[{resumen text} {asistentes integer} {encabezado text}]

== reinicio real del binario (puerto 8932) ==
2026/07/25 01:32:11 librarian: schema ready on …/c18.db, listening on 127.0.0.1:8932
GET /content-types -> {"content_types":[{"name":"eventos","fields":[{"name":"resumen","type":"text"},{"name":"asistentes","type":"integer"},{"name":"encabezado","type":"text"}]}]}
GET /content/eventos -> las dos filas, con sus ids originales y sus datos
```

**`--dump-schema` refleja la forma NUEVA (o sea, el export a Postgres lleva las columnas nuevas y
no las viejas):**

```
$ librarian.exe --dump-schema dump.json --db c18.db
"name": "cpt_eventos" → columnas: id, author_id, resumen, asistentes, encabezado, created_at, updated_at, metadata
$ grep -c cptmp dump.json          → 0
$ grep -o '"titulo"|"lugar"' dump.json → (nada: los campos viejos no están)
```

### Contratos anteriores: confirmación explícita

- La suite completa (6 paquetes, **dos corridas**) está verde, incluidos todos los tests de auth,
  API keys, artículos, productos, términos, roles, permisos, CPT dinámicos y toda la UI de admin.
- El guardián de CONTRACT-15 (`h.page(r, title)`, nunca un literal `pageData{`) sigue verde con las
  dos páginas nuevas.
- Las respuestas de `POST /content-types` y `GET /content-types` son byte-idénticas a las de antes
  (test dedicado). Ninguna ruta de 01-17 cambió de método, forma ni gating.
- Contra el binario real, después del reinicio: `GET /health`, `/articles`, `/products`, `/terms`
  → 200; login → 200.

---

## Red-team: las preguntas del contrato, respondidas con tests

| Caso | Respuesta | Dónde |
|---|---|---|
| Tipo con MUCHAS filas (copia O(2n)) | Aceptado y dicho explícitamente; 2000 filas se copian dos veces sin perder ni duplicar ninguna | `TestEditWithManyRowsIsLinearAndComplete` |
| Campo nuevo con el nombre de una columna inyectada (`updated_at`, `id`, …) | 400 "is reserved and cannot be used" — `ReservedNames()` ya lo cubría; nada toca la base | `TestEditContentTypeRejections`, `TestEditContentTypeHTTPRejections` |
| Edición que no cambia NADA | NO-OP: no se reconstruye, no se toca ni la tabla ni los ids del registro | `TestEditContentTypeEasyPaths` (caso 3) |
| Renombre CRUZADO `a`→`b` y `b`→`a` | Correcto: los valores siguen a la IDENTIDAD, y el registro hace el swap conservando ambos ids. Dos mecanismos lo sostienen: el mapeo se aplica como un único `INSERT … SELECT`, y las filas del registro se parquean antes de fijarse | `TestEditContentTypeCrossRename`, `TestAdminContentTypeEditCrossRenameThroughTheUI`, y el E2E con el binario real |
| El tipo no existe | `ErrContentTypeNotFound` → 404 JSON / 404 HTML | `TestEditContentTypeRejections`, `TestAdminContentTypeEditUnknownTypeIs404HTML` |
| Existe pero su tabla no | `ErrMissingTable`, falla ruidosa; NO se crea la tabla por las dudas | `TestEditContentTypeWithoutItsTable` |
| Tabla de paso ya presente | `ErrStagingTableExists`, y no la borra | `TestEditContentTypeRefusesALeftoverStagingTable` |
| Falla a mitad de camino | Rollback total, probado en DOS puntos distintos del camino | `TestEditContentTypeRollbackMidwayFK`, `TestEditContentTypeRollbackAtMetadataStep` |

---

## Cambios de comportamiento que el orquestador debe conocer

1. **`go.mod` sube `sqlite-postgres-compat` a v0.2.0.** Es el único cambio de dependencia.
2. **Rutas nuevas:** `PUT /content-types/{name}` (JSON, `content_types.manage`),
   `GET /admin/content-types/{name}/edit`, `GET /admin/content-types/edit/field` (sesión) y
   `POST /admin/content-types/{name}` (`content_types.manage`).
3. **`GET /content-types/{name}` ahora incluye `"id"` en cada campo.** Aditivo; el listado y la
   respuesta de creación no cambiaron.
4. **NO hay migración manual de producción.** No se agregó ninguna columna a ninguna tabla de
   código (ver la decisión sobre `updated_at`).
5. **Nombre reservado nuevo a nivel de base:** `cptmp_rebuild`. Es transitorio y solo existe dentro
   de la transacción de una edición. Si alguna vez aparece en el catálogo de una base, significa un
   crash muy raro; la operación se negará a seguir hasta que se la borre a mano.
6. **La edición es destructiva por definición cuando se quita un campo.** El backup previo al
   deploy sigue siendo la red de seguridad de siempre: el rollback cubre una edición FALLIDA, no
   una edición exitosa de la que el admin se arrepiente.

---

## Archivos tocados

**Nuevos**

- `internal/store/contenttypes_edit.go` — T1: el plan puro + la reconstrucción atómica.
- `internal/store/contenttypes_edit_test.go` — T1/T4.
- `internal/schema/staging_contract18_test.go` — el enforcement del prefijo de la tabla de paso.
- `internal/server/server_contract18_test.go` — T2 (API real).
- `internal/server/server_ui_contract18_test.go` — T3 (UI real).
- `internal/server/templates/content_types_edit.html`,
  `internal/server/templates/content_type_edit_field_row.html`.

**Modificados**

- `go.mod` / `go.sum` — compat v0.2.0.
- `internal/schema/identifier.go` — `StagingTablePrefix`, `StagingTableName`, `QuoteIdentifier`
  (movida desde `server`), `QuoteInternalIdentifier`.
- `internal/schema/schema.go` — solo COMENTARIO: la decisión sobre `updated_at`.
- `internal/server/content.go` — `quoteIdentifier` delega en `schema.QuoteIdentifier`.
- `internal/server/contenttypes.go` — `id` en la vista, `PUT` handler.
- `internal/server/server.go` — la ruta `PUT /content-types/{name}`.
- `internal/server/ui_contenttypes.go` — T3.
- `internal/server/ui.go` — las dos plantillas nuevas en el `//go:embed`.
- `internal/server/templates/content_types_list.html` — link «Editar campos» + texto corregido.
- `docs/PENDIENTES.md`, `DEFINITION-CPT-DINAMICOS.md` — hueco 2 cerrado.

**NO se tocó `sqlite-postgres-compat`.** Todo lo que hacía falta ya estaba en v0.2.0
(`CompileDropTable` pura + DDL transaccional); no faltó nada.

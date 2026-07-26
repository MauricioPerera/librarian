# CONTRACT-27 — Relaciones (FK reales) entre tipos de contenido dinámicos

Base: `88059f8`. Árbol SIN commitear, como pide el contrato: el orquestador commitea y despliega
tras verificar.

**Resultado: LISTO.** Un tipo dinámico ya puede declarar una **relación** a otro tipo dinámico, y
esa relación es una **clave foránea real** en la base — verificada contra el catálogo propio de los
DOS motores, no contra el esquema que compusimos nosotros. La acción referencial es
**`ON DELETE RESTRICT`** (nunca `CASCADE`). El destino de la relación vive en una **tabla nueva**
(`content_type_references`), no en una columna nueva de `content_type_fields`. La guarda de T3
rechaza **editar** y **borrar** un tipo referenciado **antes de tocar nada**, nombrando quién lo
referencia y qué hay que borrar para liberarlo. **NO se tocó `sqlite-postgres-compat`.**

El precio ya aceptado se cumple tal cual: **mientras exista la relación, el tipo destino no se
puede editar ni borrar**. Lo que este contrato garantiza es que eso se encuentre bien.

---

## Resumen de los gates

| Gate | Resultado |
|---|---|
| `go build ./...` | limpio |
| `go vet ./...` y `go vet -tags dualengine ./...` | limpio |
| `gofmt -l .` | vacío |
| `go test ./... -count=1` (1.ª vez) | verde (7/7 paquetes) |
| `go test ./... -count=1` (2.ª vez) | verde (7/7 paquetes) |
| `go test -tags dualengine -count=1 ./...` contra PostgreSQL 17 + pgvector real | verde (7/7 paquetes) |
| Tests existentes modificados | **ninguno** |

DSN usado: `postgres://postgres:***@31.220.22.176:5453/postgres?sslmode=disable`.

---

## Decisiones de diseño (con su porqué)

### 1. `RESTRICT`, no `NO ACTION` — y no son sinónimos

El contrato pide justificar la diferencia REAL en ambos motores, no asumir que da igual.

**Qué hace cada uno:**

- **PostgreSQL:** `NO ACTION` difiere su chequeo al **final de la sentencia**, y es la única de las
  dos que una constraint `DEFERRABLE` puede posponer hasta el `COMMIT`. `RESTRICT` se chequea
  **inmediatamente** al borrar la fila padre y **no se puede diferir nunca**.
- **SQLite:** el mismo corte, documentado con las mismas palabras — `RESTRICT` dispara apenas se
  borra la fila padre; `NO ACTION` significa "no hacer nada ahora" y la violación aparece cuando
  corre el chequeo de FK al final de la sentencia (o en el `COMMIT` si es diferida).

**Por qué elegí `RESTRICT`** (`internal/schema/schema.go`, `foreignKeyRestrict`):

1. **Es EXPLÍCITO en el DDL compilado.** `compat` no emite NINGUNA cláusula para `NoAction`
   (`compileForeignKey` la saltea cuando la acción está vacía o es `no_action`, porque es el default
   del motor). Un esquema exportado quedaría indistinguible de uno donde nadie pensó el problema.
   La regla de este contrato es deliberada y tiene que poder leerse en el catálogo y en
   `--dump-schema`. Se ve en la salida real: `on_delete=restrict`.
2. **Falla EN EL DELETE, no al final de la sentencia.** El error nombra la operación que lo causó,
   que es justo lo que hace útil el caso hermano de la guarda (borrar una FILA referenciada).
3. **No se puede diferir en silencio.** `compat` no tiene forma de declarar ni manipular
   deferibilidad, así que una constraint que PUDIERA posponerse sería pospuesta por algo fuera del
   modelo de este proyecto, y el rechazo aparecería lejos de la sentencia que lo provocó.

No es `CASCADE` en ninguna dirección, por decisión del contrato: borrar una fila referenciada nunca
puede destruir en silencio las filas que la apuntan.

### 2. El destino va en una TABLA NUEVA: `content_type_references`

Confirmado el RECON del contrato, y por eso no se intentó la variante "un `field_type` más":

- `content_type_fields.field_type` tiene un `CHECK` fijado exactamente a `schema.FieldTypes`.
  Agregar un valor al vocabulario exige **ALTERAR ese CHECK** en una tabla que ya existe, y
  `EnsureSchema` solo crea tablas FALTANTES (restricción deliberada: es lo que hace segura una
  reinicio). En una instalación desplegada el CHECK viejo sobreviviría al upgrade y rechazaría el
  valor nuevo **al INSERTAR** — un 500 en runtime, no un fallo al arrancar donde se encontraría.
- Agregarle una columna `target_type_id` choca contra el mismo muro.

La tabla nueva es puramente **aditiva** y `EnsureSchema` la crea sola. Su forma
(`internal/schema/schema.go`, `contentTypeReferencesTable`):

```
content_type_references
  id             uuid PK   DEFAULT gen_random_uuid()
  content_type_id uuid NOT NULL  FK → content_types(id) ON DELETE CASCADE
  target_type_id  uuid NOT NULL  FK → content_types(id) ON DELETE RESTRICT
  name           text NOT NULL
  ordinal        integer NOT NULL
  UNIQUE(content_type_id, name)
  UNIQUE(content_type_id, ordinal)
```

Las dos FK tienen acciones distintas **a propósito**:

- `content_type_id` **CASCADE**, idéntico a `content_type_fields.content_type_id` y por la misma
  razón: es el enlace padre→hijo del REGISTRO, así que borrar un tipo se lleva sus propias
  declaraciones en una sola sentencia. **No es** una relación entre tipos dinámicos, que es lo que
  este contrato prohíbe cascadear.
- `target_type_id` **RESTRICT**, para que "un tipo que es destino de una referencia no se puede
  borrar" sea también una garantía **de la base** y no solo del chequeo de aplicación. El chequeo de
  aplicación existe para producir un rechazo legible ANTES de tocar nada; este existe para que
  ningún camino (incluido un `DELETE` a mano) lo esquive.

No tiene `CHECK`: su vocabulario son uuids e identificadores, no un conjunto cerrado, así que nunca
va a necesitar que se altere nada — que es exactamente la propiedad que hizo correcta la tabla
nueva.

**El esquema de `content_types` y de `content_type_fields` NO se tocó.** Testeado leyendo el DDL del
propio motor (`TestReferencesRegistryTableIsCreatedAndFieldsTableIsUntouched`).

### 3. Una relación es HERMANA de un campo, no un tipo de campo

`ContentTypeDefinition` gana `References []ReferenceDefinition` (con `omitempty`, así que todo
artefacto JSON previo a este contrato es byte-idéntico). `DynamicTable` compone la tabla real como
**columnas escalares de los campos + columnas uuid nullable de las referencias**, en ese orden, y
agrega las FK después de que `ContentType()` devuelve — igual que `productsTable` agrega su
`UNIQUE(sku)`. **La firma de `ContentType()` no se tocó.**

El orden `(campos, después referencias)` está escrito UNA vez y lo comparten todos los que zipean la
definición contra un slice de valores (`ownColumnNames` en `internal/server/content.go`,
`dynamicContentColumns` en `internal/schema/server_dual.go`, `DynamicTable`). Una segunda ortografía
independiente de ese orden es exactamente cómo un valor termina escrito en la columna equivocada.

### 4. Una relación se declara AL CREAR y no se puede desprender después (v1)

No es omisión, es lo que hace que el grafo sea **acíclico por construcción**:

- Crear exige que el destino ya exista (T2), así que `A→B` requiere `B` primero.
- El `PUT` de edición cambia la lista de CAMPOS y **conserva** las relaciones tal cual; no hay forma
  de agregar `B→A` después.

Por eso la pregunta de red-team "¿dos tipos que se referencien mutuamente (ciclo)?" no tiene
respuesta afirmativa alcanzable por la API. Igual `sortDefinitionsByDependency` **detecta ciclos y
falla fuerte**, porque el registro es una tabla y una fila escrita a mano no puede colgar esa
función ni emitir un esquema inaplicable.

Consecuencia honesta: para liberar un tipo destino hay que **borrar el tipo que lo referencia**. El
mensaje de la guarda lo dice literalmente.

### 5. Sin autorreferencia en v1

Rechazada en `Validate()` con un mensaje que dice que es una **limitación conocida**, no un error
del usuario: la FK tendría que crearse antes de que exista su propia tabla, y este proyecto crea una
tabla dinámica en UNA sentencia dentro de UNA transacción (no hay `ALTER TABLE` en `compat` para
agregar la constraint después).

### 6. El orden de las tablas en `--dump-schema` — la pregunta que rompe el export en silencio

**Era un bug real y está arreglado.** `BuildWithFor` ordenaba las definiciones dinámicas **por
nombre**. Eso era correcto mientras la única FK de una tabla dinámica era `author_id → users` (tabla
de CÓDIGO, siempre emitida antes). Con una relación entre dinámicos deja de serlo: `alpha` que
referencia a `zeta` ordena ANTES que su propio destino, y entonces

- `--dump-schema` emite las tablas en ese orden y **aplicar el esquema exportado en el destino falla
  en `cpt_alpha`** (`relation "cpt_zeta" does not exist`). El dump compila, valida y parece
  completo; solo el destino se entera.
- el mismo orden maneja `missingTables → ApplySchema`, así que recrear una instancia desde cero
  fallaría en el mismo lugar.

Además verifiqué que **`compat` NO valida que el destino de una FK exista** (leído
`compat/schema.go`: solo chequea que la acción referencial sea válida), o sea que no hay red abajo.

**Solución** (`internal/schema/relations.go`, `sortDefinitionsByDependency`): orden topológico con
desempate **alfabético** — la entrada se ordena por nombre y, en cada paso, se emite en ese orden
toda definición cuyos destinos ya estén emitidos. Dos consecuencias que importan:

- **Sin relaciones el resultado es byte-idéntico al orden por nombre viejo**, así que ningún dump
  existente cambia (los tests de determinismo de CONTRACT-13/20 siguen verdes sin tocarlos).
- Un destino ausente es ERROR DURO (no una tabla salteada) y un ciclo también.

**Probado de punta a punta**, no por inspección del orden:
`TestDualEngineDumpedSchemaAppliesOnPostgres` dumpea una instancia SQLite con relación y **aplica el
dump en un schema PostgreSQL vacío**, y después confirma la FK en el catálogo del destino.

### 7. Por qué el 400 de "referencia a un id inexistente" es un chequeo previo y no un clasificador de error

La FK es la garantía real y no se reemplaza. Pero cuando es la FK la que rechaza la escritura, lo
que vuelve es un error del driver (`SQLSTATE 23503` en PostgreSQL, `FOREIGN KEY constraint failed`
en SQLite), que este paquete reportaría como 500 porque es indistinguible de cualquier otra
sentencia fallida sin clasificar por códigos específicos del motor.

**`compat` expone `IsUniqueViolation` pero NO tiene equivalente para foreign key** (verificado en
`compat/portability.go` de v0.4.0). Como este contrato no toca `compat` — y la regla dice que si le
falta algo hay que reportarlo, no editarlo —, la respuesta portátil y chequeada es **preguntar antes
de escribir**:

- `checkReferenceTargets` (crear/actualizar fila): valida el formato uuid y consulta que la fila
  destino exista → 400 con una frase.
- `checkNoIncomingReferences` (borrar fila): pregunta al registro quién referencia a este tipo y
  cuenta las filas que apuntan a ese id → 400 nombrando quién apunta y cuántos.

**Ventana residual, declarada y no escondida:** la fila destino podría borrarse entre el chequeo y
el `INSERT` (o crearse una fila que apunta entre el conteo y el `DELETE`). Es angosta y **no es
silenciosa**: la FK real sigue rechazando la escritura, no se corrompe nada, y solo el código de
estado se degrada a 500 para ese entrelazado. Cerrarla requiere el clasificador de FK que `compat`
no ofrece. **→ ver "Lo que le falta a `compat`" abajo.**

El formato uuid se valida en minúsculas y forma 8-4-4-4-12 por la misma razón que
`schema.identifierPattern` fuerza minúsculas: PostgreSQL normaliza `AB…`/`ab…` al mismo valor `uuid`
y SQLite guarda el TEXT verbatim y compara byte a byte; aceptar ambos casos haría que la MISMA
petición encuentre una fila en un motor y no en el otro.

### 8. `ReadSchemaFor`: por qué la lectura genérica no carga todo el registro

`internal/server/content.go` compone, por lectura, el esquema que DECLARA las dos rutinas de un
tipo. Un esquema compuesto tiene que contener el destino de toda FK que declara (si no,
`sortDefinitionsByDependency` lo rechaza, y hace bien). Pero cargar todas las definiciones de la
instancia en cada listado y cada detalle pondría una consulta al registro en el camino caliente de
toda la API de contenido.

`schema.ReadSchemaFor` agrega los destinos como **definiciones placeholder** (el nombre del destino,
sin campos). Alcanza para satisfacer la FK (una FK nombra una TABLA, no un conjunto de columnas) y
es honesto sobre lo que es: ese esquema **nunca se aplica, nunca se escribe en `__compat_schema` y
nunca se dumpea**; es el argumento de `QueryRoutine`, cuyo único trabajo es resolver la rutina que
se ejecuta y la relación que lee. Todo camino que SÍ aplica o exporta (`CanonicalSchemaFor`, los
tres escritores del store, `--dump-schema`) compone desde el conjunto COMPLETO de definiciones
persistidas.

### 9. El camino de upgrade — el fallo que habría tirado abajo TODA instalación desplegada

`LoadDefinitions` corre **DENTRO de `EnsureSchema`** (vía `CanonicalSchemaFor`), o sea **antes** de
que se creen las tablas faltantes. En el primer arranque de este binario contra una base ya
desplegada, `content_type_references` todavía no existe: una consulta a esa tabla habría hecho que
el servicio se negara a arrancar en todas las instalaciones del mundo.

Resuelto con `referencesTablePresent` (`internal/store/relations.go`), que pregunta al catálogo del
motor por `compat.Store.TableExists` — nunca a `__compat_schema`, que es justo lo que `EnsureSchema`
está por reescribir — y responde "cero relaciones, demostrablemente" cuando la tabla no está, por la
misma razón que `registryPresent` responde "cero tipos dinámicos": una relación solo puede vivir
ahí.

Testeado simulando **fielmente** una base pre-CONTRACT-27 (tabla ausente **y** metadata sin ella):
`TestLoadDefinitionsToleratesAnAbsentReferencesTable`.

### 10. Las relaciones se leen en una SEGUNDA consulta, no en un tercer LEFT JOIN

`LoadContentTypeDefinitions` hacía un `LEFT JOIN` a `content_type_fields`. Sumar un segundo
`LEFT JOIN` a `content_type_references` multiplicaría las filas de los dos hijos entre sí (campos ×
referencias) y deduplicar eso en Go es justo el tipo de reconstrucción implícita que pierde una fila
cuando uno de los dos lados está vacío.

### 11. La forma de la API

Aditiva y con `omitempty`, así que **toda respuesta de los contratos 13-26 para un tipo sin
relaciones es byte-idéntica**:

```json
POST /content-types
{"name":"alpha","fields":[{"name":"titulo","type":"text"}],
 "references":[{"name":"destino","target":"zeta"}]}
```

El rechazo de T3 usa la MISMA forma "rechazado + qué hacer" que ya usan CONTRACT-18
(`removes_data_of`/`confirm_remove`) y CONTRACT-26 (`confirm_name`/`confirm_rows`):

```json
400 {"error":"…","referenced_by":[{"content_type":"alpha","reference":"destino"}],
     "nothing_was_done":true}
```

**Es 400 y deliberadamente NO 409**: todas las respuestas "rechazado, y acá está qué hacer" de este
proyecto son 400 con cuerpo accionable; agregar una segunda convención las volvería inconsistentes.

### 12. La UI (T4), respetando el guardián de CONTRACT-15

- **Crear:** el formulario tiene un fieldset «Relaciones» con filas `reference_name` /
  `reference_target`, el selector se llena con **los tipos existentes leídos del registro** (así el
  orden de T2 es imposible de romper desde el panel, no meramente chequeado después), y una ruta
  htmx `GET /admin/content-types/new/reference` agrega una fila más. Cuando **no hay ningún tipo
  todavía** el formulario lo EXPLICA en vez de renderizar un `<select>` vacío inusable.
- **Templates:** archivo FIJO nuevo (`content_type_reference_row.html`) en la lista `//go:embed` y
  parseado en `init`. **Nada se genera en runtime** — la regla de CONTRACT-15 sigue intacta. Se usa
  el mismo mecanismo de claves repetidas que zipean posicionalmente por el orden de `r.PostForm`,
  así que el fragmento sigue siendo stateless.
- **La guarda de T3 se ve igual de clara:** las páginas `/admin/content-types/{name}/edit` y
  `/admin/content-types/{name}/delete` de un tipo referenciado muestran un bloque `referenced-block`
  que nombra al referente y su columna, explica por qué (reconstrucción/DROP + nunca CASCADE), dice
  qué borrar para liberarlo con un link directo, y **no ofrecen su botón de submit**. Un POST
  fabricado a mano igual se rechaza con la misma explicación.
- El listado muestra una columna «Relaciones».

**Fuera de alcance, dicho explícitamente:** el formulario de CONTENIDO (`/admin/content/{type}/new`)
**no** tiene todavía un selector para elegir la fila destino de una relación; el valor se carga por
la API JSON. T4 pide dos cosas ("declarar una referencia al crear un tipo" y "que el mensaje de la
guarda se vea igual de claro en el panel") y ambas están; el selector de filas destino sería
ampliar el alcance por mi cuenta.

---

## Respuestas al red-team del contrato

| Pregunta | Respuesta |
|---|---|
| ¿Crear dos tipos que se referencien mutuamente (ciclo)? | **Inalcanzable por la API**: una relación se declara al crear y el destino debe existir antes; la edición no toca relaciones. Igual `sortDefinitionsByDependency` detecta ciclos y falla fuerte por si el registro se escribe a mano. |
| ¿Borrar el destino mientras alguien crea una fila que lo referencia? | El `DROP` está dentro de la transacción; la FK real lo rechaza y todo hace rollback. La guarda previa serializa además por `h.schemaMu` en la capa HTTP, así que dentro de un proceso la ventana no existe. |
| ¿Una referencia a un tipo que se borró entre la validación y la creación? | El `CREATE TABLE` fallaría por su FK **y** el `INSERT` de la fila de referencia fallaría por la FK de `target_type_id`: la transacción entera hace rollback, nada queda a medias. |
| **¿El orden de las tablas en `--dump-schema` respeta las dependencias?** | **Era un bug y está arreglado** (decisión 6). Probado aplicando el dump real en un PostgreSQL 17 vacío. |
| ¿`missingTables` sigue esperando exactamente UNA tabla faltante? | **Sí, y por una razón chequeada, no por suerte**: el destino ya debe existir (paso 2b de `CreateContentType`), así que su tabla está en el catálogo y el diff no puede reportarla como faltante. Comentado en el código. |
| ¿La FK se ENFORCEA en SQLite (que la trae apagada por defecto)? | Sí: `compat` fija `_pragma=foreign_keys(1)` en el DSN. La batería dual-motor **lo mide** en vez de confiar. |

---

## Verificación — salida REAL

### Gates

```
$ go build ./...      -> (sin salida)
$ go vet ./...        -> (sin salida)
$ go vet -tags dualengine ./...  -> (sin salida)
$ gofmt -l .          -> (vacío)

$ go test ./... -count=1      # 1.ª vez
ok  github.com/MauricioPerera/librarian/cmd/librarian      2.695s
ok  github.com/MauricioPerera/librarian/internal/auth      6.837s
ok  github.com/MauricioPerera/librarian/internal/config    1.175s
ok  github.com/MauricioPerera/librarian/internal/dual      1.121s
ok  github.com/MauricioPerera/librarian/internal/schema    1.252s
ok  github.com/MauricioPerera/librarian/internal/server    47.216s
ok  github.com/MauricioPerera/librarian/internal/store     6.380s

$ go test ./... -count=1      # 2.ª vez
ok  github.com/MauricioPerera/librarian/cmd/librarian      2.641s
ok  github.com/MauricioPerera/librarian/internal/auth      6.783s
ok  github.com/MauricioPerera/librarian/internal/config    1.124s
ok  github.com/MauricioPerera/librarian/internal/dual      1.114s
ok  github.com/MauricioPerera/librarian/internal/schema    1.203s
ok  github.com/MauricioPerera/librarian/internal/server    46.727s
ok  github.com/MauricioPerera/librarian/internal/store     6.442s
```

### Baterías dual-motor completas (PostgreSQL 17 + pgvector REAL)

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5453/postgres?sslmode=disable' \
    go test -tags dualengine -count=1 ./...
ok  github.com/MauricioPerera/librarian/cmd/librarian      2.596s
ok  github.com/MauricioPerera/librarian/internal/auth      48.124s
ok  github.com/MauricioPerera/librarian/internal/config    1.017s
ok  github.com/MauricioPerera/librarian/internal/dual      1.020s
ok  github.com/MauricioPerera/librarian/internal/schema    1.068s
ok  github.com/MauricioPerera/librarian/internal/server    171.788s
ok  github.com/MauricioPerera/librarian/internal/store     91.094s
```

Es decir: TODO lo de los contratos anteriores (19, 20, 20B, 20C, 21, 22, 23, 26) sigue verde contra
los dos motores.

### `TestDualEngineRelations` — 23 observaciones IDÉNTICAS en SQLite y PostgreSQL 17

```
references registry table present=true
create alpha before zeta: err=yes unknown_target=true
cpt_alpha exists after the refusal=false
create zeta: err=none
create alpha: err=none
catalog: cpt_alpha.destino → cpt_zeta(id) on_delete=restrict
insert a row WITH the relation: err=none
insert a row WITHOUT the relation (NULL): err=none
insert a row referencing an id that does not exist: err=yes
delete the referenced row: err=yes
the referenced row survives=true
the referring rows survive=2
edit the referenced type: err=yes guarded=true referrers=edit:alpha.destino
cpt_zeta survives the refused edit=true
delete the referenced type: err=yes guarded=true referrers=delete:alpha.destino
cpt_zeta survives the refused deletion=true
delete the referrer: err=none
edit the target once freed: err=none
delete the target once freed: err=none
cpt_zeta exists at the end=false
dump order: cpt_zeta at 16, cpt_alpha at 17, target first=true
after a restart: 2 definitions, alpha references=destino→zeta
after a restart, catalog: cpt_alpha.destino → cpt_zeta(id) on_delete=restrict
--- PASS: TestDualEngineRelations (22.27s)
```

`alpha` referencia a `zeta` **a propósito**: el orden por dependencia y el alfabético están
enfrentados, así que el `dump order` de la línea 21 es la prueba directa del red-team.

### `TestDualEngineRelationsHTTP` — el ciclo completo por HTTP, 21 observaciones idénticas

```
POST /content-types libros (target missing) -> 400 catalog-has-table=false
POST /content-types autores                 -> 201 catalog-has-table=true
POST /content-types libros                  -> 201 references=1 catalog-has-table=true
catalog: cpt_libros.autor -> cpt_autores(id) on_delete=restrict
POST /content/autores                       -> 201 id-present=true
POST /content/libros WITH the relation      -> 201 round-tripped=true
POST /content/libros WITHOUT the relation   -> 201 value-is-null=true
POST /content/libros -> id that does not exist -> 400
POST /content/libros -> malformed id           -> 400
GET  /content/libros                        -> 200 rows=2
DELETE /content/autores/{referenced}        -> 400
GET  /content/autores/{referenced}          -> 200 (it survived)
POST /content/autores (second)              -> 201
DELETE /content/autores/{unreferenced}      -> 204
PUT    /content-types/autores (referenced)  -> 400 referenced_by=libros.autor nothing_was_done=true
DELETE /content-types/autores (referenced)  -> 400 referenced_by=libros.autor nothing_was_done=true
after both refusals: catalog-has-table=true field-name=nombre
DELETE /content-types/libros (the referrer) -> 200 catalog-has-table=false
PUT    /content-types/autores (freed)       -> 200
DELETE /content/autores/{was referenced}    -> 204
DELETE /content-types/autores (freed)       -> 200 catalog-has-table=false
--- PASS: TestDualEngineRelationsHTTP (29.74s)
```

Ahí está todo lo que pide T5, en los dos motores: FK real en el catálogo, filas con y sin relación,
FK cumplida como **400 y no 500**, borrado de fila referenciada rechazado, la guarda en las DOS
operaciones nombrando al referente, y el destino **editable y borrable de nuevo** tras eliminar al
referente.

### El export exportado se puede APLICAR en el destino

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@…' \
    go test -tags dualengine -run TestDualEngineDumpedSchemaAppliesOnPostgres -v ./internal/store
    dump: 18 tables, cpt_zeta at 16, cpt_alpha at 17
    OK: the dumped schema applied cleanly on PostgreSQL 17 and cpt_alpha.destino → cpt_zeta(id) on_delete=restrict
--- PASS: TestDualEngineDumpedSchemaAppliesOnPostgres (4.13s)
```

### Ciclo manual con el binario real (SQLite): crear, cargar, reiniciar, `--dump-schema`

```
$ librarian --bootstrap --email admin@example.com
librarian: bootstrap complete on …/c27.db (sqlite)
  identity created: admin@example.com (id 1ae47f26-…), status active, role "administrator"

$ curl -X POST /content-types -d '{"name":"zeta","fields":[{"name":"nombre","type":"text"}]}'
201

$ curl -X POST /content-types \
  -d '{"name":"alpha","fields":[{"name":"titulo","type":"text"}],
       "references":[{"name":"destino","target":"zeta"}]}'
{"name":"alpha","fields":[{"name":"titulo","type":"text"}],
 "references":[{"name":"destino","target":"zeta"}]}

$ curl -X POST /content/alpha -d '{"titulo":"con","destino":"b52c4bbf-…"}'
{"author_id":"1ae47f26-…","created_at":"2026-07-26T06:45:35.7143795Z",
 "destino":"b52c4bbf-d48b-4847-91a6-eb5b8dcd2977","id":"0f9fcfed-…","metadata":null,
 "titulo":"con","updated_at":"2026-07-26T06:45:35.7143795Z"}

$ curl -X POST /content/alpha -d '{"titulo":"sin"}'
{… "destino":null, "titulo":"sin" …}

$ curl -X POST /content/alpha -d '{"titulo":"fantasma","destino":"99999999-9999-4999-8999-999999999999"}'
{"error":"reference \"destino\" points at \"zeta\", and no \"zeta\" row has the id
 \"99999999-9999-4999-8999-999999999999\""} [400]

$ curl -X DELETE /content/zeta/b52c4bbf-…
{"error":"this \"zeta\" row cannot be deleted: 1 \"alpha\" row(s) reference it through
 \"destino\". This project never emits ON DELETE CASCADE, so those rows are not destroyed
 silently — clear or delete them first"} [400]

$ curl -X DELETE /content-types/zeta -d '{"confirm_name":"zeta","confirm_rows":1}'
{"error":"the content type \"zeta\" cannot be deleted because another content type references
 it: \"alpha\" (through its reference \"destino\"). Nothing was done. Deleting it rebuilds or
 drops the real table \"cpt_zeta\", and a foreign key exists precisely to forbid that while a
 referring table lives — this project never emits ON DELETE CASCADE, so the reference is not
 removed silently. A reference is declared when a content type is created and cannot be detached
 afterwards, so to free \"zeta\" you must DELETE the content type(s) \"alpha\" and then retry",
 "nothing_was_done":true,
 "referenced_by":[{"content_type":"alpha","reference":"destino"}]} [400]
```

Reinicio del proceso sobre el MISMO archivo:

```
2026/07/26 00:45:50 librarian: schema ready on …/c27.db (sqlite, vector enabled), listening on 127.0.0.1:8127

$ curl /content-types
{"content_types":[
  {"name":"alpha","fields":[{"name":"titulo","type":"text"}],
   "references":[{"name":"destino","target":"zeta"}]},
  {"name":"zeta","fields":[{"name":"nombre","type":"text"}]}]}

$ curl /content/alpha
{"items":[{… "destino":null, "titulo":"sin" …},
          {… "destino":"b52c4bbf-d48b-4847-91a6-eb5b8dcd2977", "titulo":"con" …}],"type":"alpha"}
```

`--dump-schema` incluyendo la FK y con el orden correcto:

```
$ librarian --dump-schema schema.json --db c27.db
tables: ['users','roles','permissions','role_permissions','user_roles','api_keys','articles',
         'products','taxonomies','terms','article_terms','product_terms','content_types',
         'content_type_fields','content_type_references','bootstrap','cpt_zeta','cpt_alpha']
cpt_zeta idx 16 | cpt_alpha idx 17

cpt_alpha constraints (foreign_key):
 {"kind":"foreign_key","columns":["author_id"],
  "references":{"table":"users","columns":["id"],"on_delete":"cascade"}}
 {"kind":"foreign_key","columns":["destino"],
  "references":{"table":"cpt_zeta","columns":["id"],"on_delete":"restrict"}}
```

### Batería SQLite del store (`internal/store`, suite por defecto)

```
--- PASS: TestReferencesRegistryTableIsCreatedAndFieldsTableIsUntouched
    OK: "content_type_references" created; "content_type_fields" DDL unchanged (642 bytes)
--- PASS: TestCreateWithReferenceProducesARealForeignKey
    OK: cpt_libros.autor → cpt_autores(id) ON DELETE RESTRICT, read from the engine catalog
--- PASS: TestCreateRejectsAReferenceToAMissingTarget
--- PASS: TestSelfReferenceIsRejectedAsAKnownLimitation
--- PASS: TestReferenceNameCannotCollideWithAField
--- PASS: TestReferencedTypeCannotBeEditedOrDeletedAndIsFreedAfterwards
    OK: after deleting the referrer, the target is editable AND deletable again
--- PASS: TestEditKeepsTheReferenceColumnAndItsData
    OK: the rebuild preserved the reference column, its value and its foreign key
--- PASS: TestForeignKeyIsEnforcedOnSQLite
    OK: the engine refused it (constraint failed: FOREIGN KEY constraint failed (787))
--- PASS: TestDumpSchemaOrdersTargetsBeforeReferrers
    OK: cpt_zeta at 16 precedes its referrer cpt_alpha at 17 (alphabetical order would have been the opposite)
    OK sqlite: 25 statements, cpt_zeta created at 16 before cpt_alpha at 17
    OK postgres: 25 statements, cpt_zeta created at 16 before cpt_alpha at 17
--- PASS: TestRelationSurvivesARestartCycle
    boot #2 OK: FK present, metadata has 18 tables, the reference is in the registry
    boot #3 OK: FK present, metadata has 18 tables, the reference is in the registry
--- PASS: TestLoadDefinitionsToleratesAnAbsentReferencesTable
    OK: an installation without the references table boots and gains it
```

### UI (T4)

```
--- PASS: TestAdminCanDeclareARelationFromThePanel
    OK: a relation was declared entirely from the panel and is listed
    OK: an impossible relation is a form error, not a 500
--- PASS: TestAdminSeesTheGuardOnBothPages
    OK /admin/content-types/autores/edit: blocked, names libros, "Guardar cambios" is gone
    OK /admin/content-types/autores/delete: blocked, names libros, "id=\"confirm-delete\"" is gone
    OK: a direct POST is refused with the same explanation and nothing was destroyed
```

---

## Criterios de aceptación

- [x] build/vet/gofmt limpios; `go test ./... -count=1` verde **dos veces**.
- [x] **T1**: tabla nueva aditiva (`content_type_references`); `content_type_fields` **sin cambios
      de esquema** (verificado leyendo el DDL del propio motor).
- [x] **T2**: creación con referencia; orden validado con mensaje claro
      (`ErrUnknownReferenceTarget` → 400 diciendo qué crear primero).
- [x] **T3**: editar y borrar un tipo referenciado fallan **ANTES de tocar nada**, nombrando al
      referente (y su columna), en API y en UI, en los dos motores.
- [x] **T5**: FK real verificada contra el catálogo de los **dos** motores
      (`cpt_x.y → cpt_z(id) on_delete=restrict`), y su cumplimiento probado (insert a id inexistente
      → 400; borrar fila referenciada → 400 legible; borrar fila no referenciada → 204).
- [x] Tras eliminar la referencia, el destino vuelve a ser **editable y borrable**.
- [x] `--dump-schema` incluye la FK y ordena los destinos antes de sus referentes; el dump **se
      aplica limpio** en un PostgreSQL 17 vacío.
- [x] Ciclo de reinicio: definición, relación, FK y metadata sobreviven (2 reinicios).
- [x] Todo lo de contratos anteriores sigue funcionando (suite completa dual-motor verde).

---

## Lo que le falta a `compat` (NO lo edité, como manda la regla 6)

**`compat` no tiene clasificador de violación de clave foránea.** Tiene
`Store.IsUniqueViolation(err) bool`, que existe exactamente por el mismo motivo (detectar duplicados
por texto de mensaje es la forma SQLite y falla en silencio al migrar a PostgreSQL, que reporta
`23505`), pero no hay un `IsForeignKeyViolation`. Los códigos existirían:

- SQLite (`modernc.org/sqlite`): `SQLITE_CONSTRAINT_FOREIGNKEY` (787) — se ve en la salida real de
  `TestForeignKeyIsEnforcedOnSQLite`.
- PostgreSQL (`pgx`): `SQLSTATE 23503` (`foreign_key_violation`).

**Consecuencia concreta en `librarian` hoy:** los dos 400 de este contrato se producen con un
chequeo previo portátil, que gana en el caso normal pero deja una ventana de carrera angosta en la
que el rechazo llega como 500 en vez de 400 (nada se corrompe; la FK real sigue rechazando la
escritura). Con `IsForeignKeyViolation` en `compat` esa ventana se cerraría y los chequeos previos
pasarían a ser una cortesía en vez de la única defensa. **No toqué `compat`; queda reportado.**

---

## Archivos tocados

**Nuevos:**

- `internal/schema/relations.go` — el modelo de la relación, su validación, el orden topológico
  (`sortDefinitionsByDependency`) y `ReadSchemaFor`.
- `internal/store/relations.go` — lectura de relaciones, `ReferencesTo`, `ReferencedTypeError` y la
  guarda de T3; inserción de las filas de referencia.
- `internal/server/templates/content_type_reference_row.html` — la fila de relación del formulario.
- `internal/store/contenttypes_relations_test.go`, `internal/store/dualengine_contract27_test.go`,
  `internal/server/server_contract27_test.go`,
  `internal/server/server_ui_contract27_test.go`, `internal/server/dualengine_contract27_test.go`.

**Modificados:**

- `internal/schema/schema.go` — `foreignKeyRestrict`, `contentTypeReferencesTable`, su entrada en
  `Build()`.
- `internal/schema/dynamic.go` — `ContentTypeDefinition.References`, validación, columnas + FK en
  `DynamicTable`, orden topológico en `BuildWithFor`.
- `internal/schema/server_dual.go` — las columnas de relación en las rutinas de lectura.
- `internal/store/contenttypes.go` — carga de relaciones, validación de orden (T2), inserción en la
  transacción de creación.
- `internal/store/contenttypes_edit.go` — las relaciones viajan en el plan, se copian en la
  reconstrucción, y la guarda de T3.
- `internal/store/contenttypes_delete.go` — la guarda de T3 y `Referrers` en el plan.
- `internal/server/contenttypes.go` — `references` en el payload, mapeo de los rechazos a 400.
- `internal/server/content.go` — lectura/escritura del valor de la relación, los dos chequeos
  previos, `ownColumnNames`.
- `internal/server/ui_contenttypes.go`, `internal/server/ui.go` (lista `//go:embed`) y los cuatro
  templates de tipos de contenido.

**NO se tocó:** `sqlite-postgres-compat`, ningún test existente, ningún permiso, `go.mod`.

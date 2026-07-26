# CONTRACT-26 — Borrar un tipo de contenido dinámico (cierra el ciclo de vida)

Base: `88059f8`. Árbol SIN commitear, como pide el contrato: el orquestador commitea y despliega
tras verificar.

**Resultado: LISTO.** Un tipo dinámico ya se puede crear (CONTRACT-13), editar (CONTRACT-18) y
ahora **borrar**: su definición, sus campos y **su tabla real con todas sus filas**, dentro de UNA
transacción, por API (`DELETE /content-types/{nombre}`) y por UI
(`/admin/content-types/{nombre}/delete`), gateado por el permiso que ya existía
(`content_types.manage`, **ningún permiso nuevo**). Se usa la función PURA
`compat.CompileDropTableIfExists` dentro de la transacción propia — nunca `compat.Store.DropTable`,
que abre la suya — y por eso la transacción termina reescribiendo la metadata compuesta COMPLETA de
`__compat_schema`. **NO se tocó `sqlite-postgres-compat`.**

La guarda es el contrato, y quedó así: para borrar hay que mandar **el nombre exacto del tipo Y el
número exacto de filas que se van a destruir**. No hay booleano.

---

## Decisiones de diseño (con su porqué)

### 1. La forma de la confirmación: nombre exacto + **recuento exacto de filas**

**Decisión:**

```json
DELETE /content-types/eventos
{"confirm_name": "eventos", "confirm_rows": 3}
```

Ambos son obligatorios y ambos se comparan contra lo que hay **en ese momento**.

**Por qué no un booleano.** `confirm: true` se manda igual de fácil por error que a propósito, y no
prueba nada sobre lo que el cliente vio. CONTRACT-18 ya lo había rechazado para la pérdida PARCIAL
(quitar campos) y exigió la LISTA DE NOMBRES; esto es la pérdida TOTAL, así que el criterio se
**extiende**, no se afloja. En la salida real de más abajo se ve `{"confirm":true}` rechazado con
400 y `nothing_was_done: true`.

**Por qué el NOMBRE.** Impide que un cliente que tenía OTRO tipo en pantalla acierte por accidente.
Un `DELETE /content-types/eventos` con `confirm_name: "articles"` se rechaza.

**Por qué el RECUENTO DE FILAS — y por qué es la pieza central.** El contrato lo pide explícitamente
("un tipo con cero filas y uno con diez mil no son la misma decisión") y además resuelve tres cosas
a la vez con un solo mecanismo:

1. **Es un valor que nadie puede producir sin habérselo preguntado al servidor.** Ahí está
   literalmente la propiedad "una confirmación que no se puede mandar sin haberla leído": no hay
   forma de adivinar 2.000.
2. **Hace visible la magnitud ANTES.** El primer `DELETE` sin confirmar **es** el canal de lectura:
   devuelve 400 con `rows`, `table` y los `confirm_name`/`confirm_rows` exactos a reenviar — el
   mismo patrón "eco de lo que hay que confirmar" que CONTRACT-18 usa con
   `removes_data_of`/`confirm_remove`. Ninguna ruta existente cambió su respuesta para llevar el
   recuento.
3. **Es un token de frescura.** Si otro cliente carga contenido entre que se leyó el número y que se
   confirma, los números ya no coinciden y el borrado se **rechaza** en vez de llevarse filas que
   quien confirmó nunca vio. Testeado (`TestDeleteContentTypeRowsChangedWhileConfirming`,
   `TestAdminContentTypeDeleteReflectsAConcurrentWrite`).

**`confirm_rows` es un puntero (`*int64`) en el JSON y `RowsStated bool` en el store.** "Confirmo
cero filas" tiene que ser distinguible de "no mandé recuento": sin esa distinción, un cuerpo vacío
borraría cualquier tipo que casualmente esté vacío — exactamente el accidente que la guarda existe
para evitar. Testeado (`TestDeleteContentTypeEmptyType`).

### 2. La tabla ausente: **se limpia**, con `CompileDropTableIfExists` (y por qué NO se rechaza)

El contrato pide decidir y justificar. **Decisión: se limpia igual.**

- `EditContentType` **rechaza** ese estado (`ErrMissingTable`) y hace bien: una reconstrucción no
  tiene de dónde copiar, y crear la tabla al pasar inventaría un estado que el registro solo
  afirmaba.
- Un **borrado no necesita NADA de la tabla**. Rechazarlo dejaría al admin con un tipo que no se
  puede usar (la capa CRUD genérica consulta una tabla inexistente), no se puede editar
  (`ErrMissingTable`) y tampoco se podría borrar: una fila de registro sin salida del producto,
  justo en la operación cuyo propósito entero es sacarla. Cerrar el ciclo de vida significa que el
  camino de limpieza tiene que funcionar también sobre el estado roto. El test lo prueba en ese
  orden: primero comprueba que la edición se niega, y recién después que el borrado limpia.
- **El costo documentado de `IF EXISTS` no aplica acá.** `compat` advierte que "un typo en el nombre
  se vuelve un no-op silencioso"; el nombre no lo tipea nadie: lo deriva `schema.DynamicTableName`
  de la definición PERSISTIDA, la misma función que produjo el `CREATE TABLE`. La única forma de que
  la tabla falte es que realmente falte.
- **Y no es silencioso.** La operación PRUEBA el catálogo físico primero y reporta `TableMissing` /
  `table_was_missing` en el plan, en la respuesta JSON y en la página de confirmación de la UI. La
  guarda sigue aplicando: hay que confirmar `confirm_rows: 0` explícitamente.

### 3. El orden dentro de la transacción — y por qué el `DELETE` del registro va PRIMERO

```
1. DELETE FROM content_types WHERE id = ?     (los campos caen por ON DELETE CASCADE)
2. SELECT count(*) FROM "cpt_x"  → comparar con confirm_rows
3. DROP TABLE IF EXISTS "cpt_x"
4. escribir la metadata compuesta COMPLETA (sin este tipo) en __compat_schema
```

- **(1) primero porque su `RowsAffected` es lo que resuelve la carrera.** Dos borrados simultáneos
  del mismo tipo: el perdedor ve 0 filas afectadas y recibe `ErrContentTypeNotFound` (→ 404) con
  TODA su transacción revertida, el `DROP` incluido. Es una garantía de la base, no un chequeo de
  aplicación, así que vale incluso entre procesos.
  **Esto se corrigió durante el desarrollo:** con la comparación de recuento ANTES del `DELETE`, dos
  competidores se rechazaban mutuamente con "confirmaste 1 y hay 0 filas" — cierto y completamente
  inútil. Está escrito en el comentario del código para que no vuelva.
- **(2) la comparación de recuento vive DENTRO de la transacción**, sobre un `count(*)` leído
  inmediatamente antes del `DROP`. Es lo único que hace del número confirmado un hecho sobre la base
  que se está modificando y no sobre una foto de hace unos segundos. Cuando la tabla ya no está, el
  recuento vivo es 0 por construcción y la comparación **igual corre** (una definición huérfana
  tampoco se borra confirmando un número inventado); la CONSULTA se saltea porque en PostgreSQL una
  sentencia contra una relación inexistente ABORTA la transacción entera (25P02) y se llevaría
  puesta la limpieza que esa rama existe para hacer.
- **(4) es la responsabilidad heredada** de usar el compilador PURO en vez de
  `compat.Store.DropTable`: `InspectSchema` PREFIERE `__compat_schema` por sobre el catálogo físico,
  así que una fila de metadata que siga listando una tabla borrada es el estado más dañino
  alcanzable en este proyecto (todo `EnsureSchema` posterior creería que la tabla existe y todo
  `--dump-schema` exportaría una tabla que no está). Se escribe el esquema compuesto COMPLETO, sin
  este tipo, exactamente como `CreateContentType`/`EditContentType` lo escriben CON él.

### 4. La ventana entre la prueba de catálogo y el `count(*)` del plan (bug encontrado y cerrado)

`PlanContentTypeDeletion` hace dos sentencias sobre el pool (existe la tabla / cuántas filas tiene).
Un borrado que commitea en el medio dejaba al `count(*)` consultando una tabla que ya no está, y eso
salía como un error crudo. **No es un fallo de nada**: es la respuesta "no queda nada que contar"
llegando con forma de error. Se **clasifica volviendo a preguntarle al catálogo**, nunca por el
texto del driver (que difiere por motor y es exactamente la clase de bug que CONTRACT-21 sacó del
proyecto). Si la tabla sigue ahí, el error era real y se propaga tal cual. Lo detectó
`TestDeleteContentTypeConcurrent`, que ahora corre estable con `-count=8`.

### 5. La UI: dos pasos, y el recuento en la pantalla

`GET /admin/content-types/{name}/delete` (sesión) muestra qué se destruye —la definición, la lista
de campos, la tabla real y **cuántas filas**— y lleva el nombre y el recuento en dos `input`
ocultos. `POST /admin/content-types/{name}/delete` (`content_types.manage`) aplica solo si el envío
trae exactamente eso; cualquier otra cosa **vuelve a la misma página con 400, con las cifras VIVAS**
(se re-lee el plan: si el número se movió, pedir de nuevo el número viejo sería pedir un dato ya
equivocado) y sin haber destruido nada. El store re-verifica igual, así que la UI no es lo único que
separa al admin de la pérdida.

Se respetó el guardián de CONTRACT-15: la página se construye con `h.page(r, title)`; el test
`TestPageDataIsBuiltOnlyByTheConstructor` sigue verde.

### 6. Lo que NO se hizo, a propósito

- **No se agregó borrado de tipos de CÓDIGO.** `articles`, `products` y sus tablas de unión no son
  tipos dinámicos: la ruta simplemente no los encuentra (404) y el test lo comprueba mirando que las
  tablas sigan ahí.
- **No se tocó el borrado de FILAS de contenido**, que ya existía.
- **No cambió el contrato público de ninguna ruta existente.** El recuento de filas NO se agregó a
  `GET /content-types/{nombre}`; se aprende del propio 400 del `DELETE`.
- **Ningún permiso nuevo, ninguna dependencia nueva, ninguna migración manual de producción**
  (no se agregó ni se quitó ninguna columna a ninguna tabla de código).

---

## Qué se implementó, por tarea

### T1 — La operación (`internal/store/contenttypes_delete.go`, nuevo)

- `PlanContentTypeDeletion(ctx, store, name) (ContentTypeDeletion, error)` — **read-only**: responde
  "¿qué destruiría esto?" sin destruir nada. Devuelve `TypeName`, `TypeID`, `TableName`, `Fields`,
  `Rows` y `TableMissing`. Nombre desconocido → `ErrContentTypeNotFound` (→ 404); el nombre se BINDEA
  como parámetro, nunca se interpola.
- `DeleteContentType(ctx, store, name, DeleteConfirmation) (ContentTypeDeletion, error)` — la
  transacción única descrita arriba; devuelve el recibo de lo que se fue.
- `checkDeleteConfirmationShape` — **pura**: la mitad de la guarda que no puede correr carreras (se
  mandó un nombre, es el de este tipo, se declaró un recuento). La comparación del recuento vive
  dentro de la transacción, ver decisión 3.
- `DeleteConfirmationError` — el rechazo, que lleva los datos VIVOS (nombre, tabla, filas,
  `TableMissing`) y por eso es el canal por el que un llamador aprende qué tiene que confirmar.
- El único identificador interpolado (el nombre real de la tabla en el `count(*)`) sale por
  `schema.QuoteIdentifier`, el MISMO gate que usa `compileRebuild` para ese mismo nombre.

Los campos caen por la cascada que ya existía (`foreignKeyCascade("content_type_id")`); no se
escribe una segunda sentencia, y el test cuenta las filas de `content_type_fields` de TODA la base
para probar que la cascada se llevó las de este tipo y las de nadie más.

### T2 — La vía de escritura (API) — `internal/server/contenttypes.go`, `server.go`

- `DELETE /content-types/{name}`, gateado por `requirePermission("content_types.manage")`.
- Cuerpo AUSENTE = petición legítima (es como un cliente pregunta "¿qué destruiría esto?"), y se
  distingue de un cuerpo MALFORMADO: se lee primero y solo se decodifica si hay algo.
- 200 con el recibo (`deleted`, `rows_deleted`, `table`, `fields_deleted`, `table_was_missing`);
  400 con `error`, `rows`, `confirm_name`, `confirm_rows`, `table_was_missing` y
  `nothing_was_done: true`; 404 para tipo inexistente.
- Toma `h.schemaMu`, el mutex que YA serializaba las operaciones de esquema.

### T3 — La vía de escritura (UI) — `internal/server/ui_contenttypes.go` + plantilla nueva

- `GET /admin/content-types/{name}/delete` (sesión) y
  `POST /admin/content-types/{name}/delete` (`content_types.manage`).
- Plantilla nueva `content_types_delete.html`, agregada al `//go:embed` de `ui.go`.
- `content_types_list.html`: link «Borrar» por fila y el texto explicativo actualizado.

### T4 — Verificación

Ver la sección siguiente: todo con salida real.

### Documentación

- `DEFINITION-CPT-DINAMICOS.md` — el ítem "Borrar un CPT dinámico completo … fuera de alcance salvo
  que se decida explícitamente en un contrato futuro" queda marcado como implementado por este
  contrato (que ES ese contrato futuro), conservando el texto original.

---

## Verificación — salida REAL

### `go build`, `go vet`, `gofmt`

```
$ go build ./...   ; echo OK
OK
$ go vet ./...     ; echo OK
OK
$ gofmt -l .
(vacío)
```

### `go test ./... -count=1`, DOS veces

```
== corrida 1 ==
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.797s
ok  	github.com/MauricioPerera/librarian/internal/auth	6.374s
ok  	github.com/MauricioPerera/librarian/internal/config	1.192s
ok  	github.com/MauricioPerera/librarian/internal/dual	1.177s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.258s
ok  	github.com/MauricioPerera/librarian/internal/server	39.193s
ok  	github.com/MauricioPerera/librarian/internal/store	5.136s

== corrida 2 ==
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.807s
ok  	github.com/MauricioPerera/librarian/internal/auth	6.523s
ok  	github.com/MauricioPerera/librarian/internal/config	1.249s
ok  	github.com/MauricioPerera/librarian/internal/dual	1.255s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.386s
ok  	github.com/MauricioPerera/librarian/internal/server	39.469s
ok  	github.com/MauricioPerera/librarian/internal/store	5.293s
```

**Ningún test existente fue modificado.** Los únicos archivos de test tocados son los cuatro nuevos
de este contrato.

### T1/T2 — el store (`internal/store`, SQLite)

```
$ go test ./internal/store/ -run Delete -count=1 -v
=== RUN   TestDeleteContentTypeRoundTripWithRealData
    DELETE OK: cpt_eventos y sus 3 filas fuera del catálogo, registro types=1 fields=1,
    metadata y esquema canónico coinciden, `notas` intacto
--- PASS
=== RUN   TestDeleteContentTypeGuard
    no confirmation at all (an empty body)        rejected: refusing to delete the content type "eventos": no confirmation was sent. Deleting it drops the table "cpt_eventos" and destroys its 2 stored row(s) irreversibly; to proceed, send confirm_name="eventos" and confirm_rows=2 exactly
    a boolean-style confirmation: the name only   rejected: … the number of rows to be destroyed was not confirmed. …
    the row count only, no name                   rejected: … the name of the content type was not confirmed. …
    the name of a DIFFERENT type                  rejected: … the confirmed name "notas" is not the name of this content type. …
    a stale/invented row count                    rejected: … the confirmed row count 3 does not match the 2 row(s) this content type holds. …
    zero rows on a type that has two              rejected: … the confirmed row count 0 does not match the 2 row(s) this content type holds. …
    GUARD OK: 6 confirmaciones equivocadas rechazadas con el recuento vivo visible y nada tocado;
    la exacta (name="eventos" rows=2) se aplicó
--- PASS
=== RUN   TestDeleteContentTypeRowsChangedWhileConfirming
    STALE COUNT OK: rechazado ("the confirmed row count 1 does not match the 2 row(s) …");
    las 2 filas recién contadas están intactas
--- PASS
=== RUN   TestDeleteContentTypeRollbackMidwayFK
    deletion failed as expected: drop table for content type "eventos": constraint failed: FOREIGN KEY constraint failed (787)
    ROLLBACK OK: definición [titulo:text lugar:text asistentes:integer gratis:boolean],
    tabla [id author_id titulo lugar asistentes gratis created_at updated_at metadata], fila y metadata intactas
--- PASS
=== RUN   TestDeleteContentTypeRollbackAtMetadataStep
    deletion failed as expected at the metadata step: record full schema metadata: SQL logic error: no such table: __compat_schema (1)
    ROLLBACK-AT-LAST-STEP OK: la tabla DROPEADA volvió, registro intacto, fila intacta
--- PASS
=== RUN   TestDeleteContentTypeWithoutItsTable
    MISSING TABLE OK: la definición huérfana y sus 4 campos se limpiaron, reportado table_missing=true
--- PASS
=== RUN   TestDeleteContentTypeTwice
    DOUBLE DELETE OK: el segundo intento es ErrContentTypeNotFound (→ 404)
--- PASS
=== RUN   TestDeleteContentTypeConcurrent
    CONCURRENCY OK: 6 competidores → 1 éxito, 5 ErrContentTypeNotFound, catálogo y metadata consistentes
--- PASS
=== RUN   TestDeleteContentTypeNameIsReusable
    REUSE OK: `eventos` recreado con la forma nueva [id author_id encabezado cupos created_at updated_at metadata] y 0 filas
--- PASS
=== RUN   TestDeleteContentTypeSurvivesTwoRestarts
    boot #1 OK: nada recreado, 0 tipos dinámicos, metadata y esquema canónico limpios
    boot #2 OK: nada recreado, 0 tipos dinámicos, metadata y esquema canónico limpios
--- PASS
=== RUN   TestDeleteContentTypeEmptyType
    EMPTY TYPE OK: rows=0 igual hay que declararlo; el cuerpo vacío se rechaza, `confirm_rows: 0` funciona
--- PASS
=== RUN   TestDeleteContentTypeUnknownName
    HOSTILE NAMES OK: 5 nombres (uno con comillas y punto y coma) → ErrContentTypeNotFound, nada tocado
--- PASS
=== RUN   TestDeleteContentTypeManyRows
    MANY ROWS OK: 2000 filas contadas exactamente, una confirmación de cero filas rechazada,
    la exacta destruyó la tabla
--- PASS
```

Las **DOS pruebas de atomicidad** (el contrato pedía UNA; se hicieron dos, en puntos distintos):

- **A mitad** (`RollbackMidwayFK`): otra tabla con una FK hacia `cpt_eventos` hace que el motor
  rechace el `DROP` — o sea, falla **después** de que las filas del registro ya se borraron en esa
  transacción. Tras el rollback: la definición y sus campos vuelven, la tabla sigue, la fila intacta,
  la metadata idéntica.
- **Al final** (`RollbackAtMetadataStep`): se elimina `__compat_schema` antes de llamar, así que la
  falla ocurre en el paso 4, **después del `DROP TABLE`**. La tabla dropeada VUELVE — que es
  exactamente la afirmación "la atomicidad es real porque ambos motores ejecutan DDL
  transaccionalmente", probada y no afirmada.

Estabilidad de la carrera:

```
$ go test ./internal/store/ -run Delete -count=8
ok  	github.com/MauricioPerera/librarian/internal/store	11.323s
```

### T2/T3 — API y UI reales (`internal/server`, SQLite)

```
$ go test ./internal/server/ -run TestDeleteContentType -count=1 -v
=== RUN   TestDeleteContentTypeRequiresPermission
    GATE OK: 403 con content.* solamente, 401 anónimo, 200 con content_types.manage
--- PASS
=== RUN   TestDeleteContentTypeGuardOverHTTP
    unconfirmed DELETE  -> 400 rows=3 confirm_name=eventos confirm_rows=3
    a boolean, the shape this contract refuses -> 400 refusing to delete the content type "eventos": no confirmation was sent. …
    the name only                              -> 400 … the number of rows to be destroyed was not confirmed. …
    the count only                             -> 400 … the name of the content type was not confirmed. …
    another type's name                        -> 400 … the confirmed name "articles" is not the name of this content type. …
    a wrong count                              -> 400 … the confirmed row count 2 does not match the 3 row(s) …
    zero on a type with three rows             -> 400 … the confirmed row count 0 does not match the 3 row(s) …
    confirmed DELETE    -> 200 map[deleted:true fields_deleted:[titulo lugar asistentes] name:eventos rows_deleted:3 table:cpt_eventos table_was_missing:false]
    AFTER OK: /content-types/eventos y /content/eventos son 404, el listado está vacío, un segundo DELETE es 404
--- PASS
=== RUN   TestDeleteContentTypeNameIsReusableOverHTTP
    REUSE OK: el nombre quedó libre inmediatamente, la tabla nueva está vacía y acepta la forma nueva sin reiniciar
--- PASS
=== RUN   TestDeleteContentTypeUnknownIs404
    404 OK: nombres desconocidos, nombres de tablas de CÓDIGO y el nombre real de la tabla → 404 sin tocar nada
--- PASS

$ go test ./internal/server/ -run TestAdminContentTypeDelete -count=1 -v
=== RUN   TestAdminContentTypeDeleteShowsTheRowCountBeforeDestroyingIt
    STEP ONE OK: la página muestra 3 filas, nombra cpt_eventos y lleva confirm_name/confirm_rows; nada destruido
    BOUNCE OK: 5 envíos desalineados rechazados con 400, el recuento vivo re-mostrado, la tabla intacta
    STEP TWO OK: 303, la tabla y las filas del registro se fueron y la lista ya no muestra el tipo
--- PASS
=== RUN   TestAdminContentTypeDeleteWithoutPermissionIs403
    UI GATE OK: página legible con sesión, el borrado rechazado con 403 y nada destruido
--- PASS
=== RUN   TestAdminContentTypeDeleteUnknownTypeIs404HTML
    404 OK: página HTML tanto para la confirmación como para el envío
--- PASS
=== RUN   TestAdminContentTypeDeleteReflectsAConcurrentWrite
    STALE COUNT OK: el envío que llevaba 1 fue rechazado y la página volvió mostrando 3
--- PASS
```

### T4 — CICLO COMPLETO POR HTTP CONTRA **AMBOS MOTORES** (PostgreSQL 17 real con pgvector)

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5452/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineDeleteContentType -count=1 -v ./internal/server

transcript (22 lines, identical on both engines):
  POST /content-types                       -> 201 catalog-has-table=true
  POST /content/eventos #1                  -> 201
  POST /content/eventos #2                  -> 201
  POST /content/eventos #3                  -> 201
  GET  /content/eventos                     -> 3 rows
  DELETE unconfirmed                        -> 400 rows=3 confirm_name=eventos confirm_rows=3 nothing_was_done=true
  DELETE a boolean confirm:true             -> 400 rows=3 nothing_was_done=true
  DELETE the name only                      -> 400 rows=3 nothing_was_done=true
  DELETE the count only                     -> 400 rows=3 nothing_was_done=true
  DELETE another type's name                -> 400 rows=3 nothing_was_done=true
  DELETE a wrong count                      -> 400 rows=3 nothing_was_done=true
  after 6 refusals: catalog-has-table=true rows=3
  DELETE confirmed                          -> 200 deleted=true rows_deleted=3 table=cpt_eventos table_was_missing=false
  catalog: cpt_eventos=false articles=true users=true
  registry: types=0 fields=0
  GET  /content-types/eventos               -> 404
  GET  /content/eventos                     -> 404
  GET  /content-types                       -> 200 0 definitions
  DELETE again                              -> 404
  POST /content-types (same name again)     -> 201 catalog-has-table=true
  GET  /content/eventos (re-created)        -> 0 rows
  POST /content/eventos (new shape)         -> 201 rows=1
OK: 22 observations identical on SQLite and PostgreSQL 17
--- PASS: TestDualEngineDeleteContentType (25.19s)
```

`catalog-has-table` no es la respuesta de la aplicación: es `compat.Store.TableExists` preguntándole
al **catálogo propio de cada motor** (`sqlite_master` / `pg_class` acotado a `current_schema()`).

Y la misma batería a nivel store, con la ATOMICIDAD y la metadata medidas en los dos motores:

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5452/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineDeleteContentType -count=1 -v ./internal/store

transcript (22 lines, identical on both engines):
  create eventos err=none
  before: table=true rows=3 types=1 fields=3 metadata-has-table=true
  plan rows=3 table=cpt_eventos table-missing=false fields=[titulo lugar asistentes]
  delete unconfirmed        refused rows=3
  delete name only          refused rows=3
  delete wrong name         refused rows=3
  delete wrong count        refused rows=3
  after 4 refusals: table=true rows=3 types=1 fields=3
  delete with a dependent FK err=yes
  ATOMIC: table=true rows=3 types=1 fields=3 metadata-has-table=true
  delete confirmed err=none rows-deleted=3 table-was-missing=false
  after: table=false types=0 fields=0 metadata-has-table=false
  canonical schema has the deleted table=false dynamic=[]
  delete again not-found=true
  ensureSchema after delete #1 err=none recreated=false
  ensureSchema after delete #2 err=none recreated=false
  re-create with the same name err=none
  re-created: table=true rows=0 columns=[id author_id encabezado cupos created_at updated_at metadata]
  orphan plan err=none table-missing=true rows=0
  orphan delete unconfirmed refused rows=0
  orphan delete confirmed err=none table-was-missing=true types=0 fields=0
  orphan cleanup: metadata-has-table=false
OK: 22 observations identical on SQLite and PostgreSQL 17
--- PASS: TestDualEngineDeleteContentType (51.27s)
```

La línea `ATOMIC:` es la prueba pedida: **con una FK dependiente el `DROP` falla y NADA se fue** —
ni la definición, ni los campos, ni la tabla, ni la metadata — y eso vale **en los dos motores**,
que llegan al mismo estado final por rutas internas distintas (PostgreSQL envenena la transacción
con 25P02, SQLite no).

`metadata-has-table` se lee de la fila `canonical_schema` de `__compat_schema`, o sea **la caché que
`InspectSchema` prefiere**: `true` antes, `true` tras el fallo, `false` después del borrado, en
ambos motores. Criterio de aceptación «la metadata coincide con el catálogo» cumplido y medido.

### TODAS las baterías dual-engine de los contratos anteriores, verdes

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5452/postgres?sslmode=disable' \
    go test -tags dualengine -count=1 ./...
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.833s
ok  	github.com/MauricioPerera/librarian/internal/auth	45.518s
ok  	github.com/MauricioPerera/librarian/internal/config	1.244s
ok  	github.com/MauricioPerera/librarian/internal/dual	1.222s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.314s
ok  	github.com/MauricioPerera/librarian/internal/server	144.722s
ok  	github.com/MauricioPerera/librarian/internal/store	55.442s
```

### T4 — END-TO-END con el BINARIO REAL (`curl`, `--dump-schema`, reinicio)

Se compiló `librarian.exe`, se hizo `--bootstrap` real y se levantó el servidor real
(`LIBRARIAN_ADDR=127.0.0.1:8961`). Todo lo de abajo es `curl` contra ese proceso.

```
== POST /content-types
{"name":"eventos","fields":[{"name":"titulo","type":"text"},{"name":"lugar","type":"text"},{"name":"asistentes","type":"integer"}]}
== 3 filas cargadas por POST /content/eventos
GET /content/eventos -> 3 filas

== DELETE SIN confirmar -> 400
{
    "confirm_name": "eventos",
    "confirm_rows": 3,
    "error": "refusing to delete the content type \"eventos\": no confirmation was sent. Deleting it drops the table \"cpt_eventos\" and destroys its 3 stored row(s) irreversibly; to proceed, send confirm_name=\"eventos\" and confirm_rows=3 exactly",
    "name": "eventos",
    "nothing_was_done": true,
    "rows": 3,
    "table": "cpt_eventos",
    "table_was_missing": false
}

== DELETE {"confirm":true}  (el booleano que este contrato rechaza) -> 400
refusing to delete the content type "eventos": no confirmation was sent. …destroys its 3 stored row(s)…

== DELETE {"confirm_name":"eventos","confirm_rows":2} -> 400
refusing to delete the content type "eventos": the confirmed row count 2 does not match the 3 row(s) this content type holds. …

== tras los tres rechazos: GET /content/eventos -> 3 filas   (SIN EFECTOS)

== dump ANTES del borrado
cpt_eventos en el dump: True

== DELETE {"confirm_name":"eventos","confirm_rows":3} -> 200
{
    "deleted": true,
    "fields_deleted": ["titulo", "lugar", "asistentes"],
    "name": "eventos",
    "rows_deleted": 3,
    "table": "cpt_eventos",
    "table_was_missing": false
}

== GET  /content/eventos        -> 404
== GET  /content-types/eventos  -> 404
== DELETE otra vez              -> 404

== dump DESPUÉS  (librarian.exe --dump-schema dump-despues.json --db c26.db)
cpt_eventos en el dump: False
tablas: 15

== rutas de contratos anteriores
/health          200
/articles        200
/products        200
/terms           200
/content-types   200
```

**Catálogo físico REAL de SQLite tras el borrado (consulta directa, no la aplicación):**

```
__compat_schema
api_keys
article_terms
articles
bootstrap
content_type_fields
content_types
permissions
product_terms
products
role_permissions
roles
taxonomies
terms
user_roles
users
(ninguna cpt_*)

registro: types=0 fields=0
metadata __compat_schema menciona cpt_eventos: False
metadata tablas=15
```

**Ciclo de reinicio (reinicio REAL del binario, puerto 8962) y reutilización del nombre:**

```
2026/07/25 23:41:15 librarian: schema ready on …/c26.db (sqlite, vector enabled), listening on 127.0.0.1:8962
GET /content-types -> {"content_types":[]}          <- arranca limpio, no recrea nada

== recrear el MISMO nombre
{"name":"eventos","fields":[{"name":"encabezado","type":"text"},{"name":"cupos","type":"integer"}]}
GET /content/eventos -> {"items":[],"type":"eventos"}   <- tabla VACÍA, no las filas del anterior
```

---

## Red-team: las preguntas del contrato, respondidas con tests

| Caso | Respuesta | Dónde |
|---|---|---|
| Un tipo con filas y otro sin filas | Los dos se borran, y **ninguno con la confirmación del otro**: 0 filas hay que declararlo explícitamente y una confirmación de 0 sobre 2000 filas se rechaza | `TestDeleteContentTypeEmptyType`, `TestDeleteContentTypeManyRows` |
| Borrar dos veces | El segundo intento es `ErrContentTypeNotFound` → 404 JSON / 404 HTML | `TestDeleteContentTypeTwice`, `TestDeleteContentTypeGuardOverHTTP`, `TestAdminContentTypeDeleteUnknownTypeIs404HTML` |
| **Dos borrados concurrentes del mismo tipo** | 6 competidores → **exactamente 1 éxito**, 5 × 404. Lo decide `RowsAffected` del `DELETE` del registro dentro de cada transacción, no un chequeo de aplicación; el perdedor revierte TODO, el `DROP` incluido. Aquí se encontró y se cerró un bug real (ver decisión 3) | `TestDeleteContentTypeConcurrent` (estable con `-count=8`) |
| **Borrar mientras otro cliente escribe contenido de ese tipo** | Rechazado: el recuento se re-lee DENTRO de la transacción, justo antes del `DROP`, así que una confirmación con el número viejo no pasa y no se destruye ninguna fila que quien confirmó no haya visto | `TestDeleteContentTypeRowsChangedWhileConfirming`, `TestAdminContentTypeDeleteReflectsAConcurrentWrite` |
| Un nombre que necesita comillas | Estructuralmente imposible: el gate `ValidateIdentifier` acepta solo `[a-z][a-z0-9_]*`, así que un nombre hostil ni siquiera puede nombrar un tipo guardado; la búsqueda lo BINDEA y no encuentra nada → 404. Probado con `eventos"; DROP TABLE users; --` | `TestDeleteContentTypeUnknownName`, `TestDeleteContentTypeUnknownIs404` |
| Un token de sesión de alguien parado en la pantalla de ese tipo | Sigue siendo una identidad válida (el borrado de un tipo no toca la autenticación); lo que ve es que **el tipo ya no está**: `GET /content-types/{t}` y `GET /content/{t}` responden 404, y `/admin/content-types` ya no lo lista. Un envío suyo del formulario de borrado da 404 HTML, no un 500 | `TestDeleteContentTypeGuardOverHTTP`, `TestAdminContentTypeDeleteUnknownTypeIs404HTML` |
| ¿Deja tablas de paso `cptmp_` si falla a mitad? | **Este contrato no crea ninguna**: la tabla de paso es de la RECONSTRUCCIÓN (CONTRACT-18); un borrado no copia nada. Verificado igual tras el borrado con éxito | `TestDeleteContentTypeRoundTripWithRealData` (`stagingTables` vacío) |
| La definición existe pero la tabla no | Se LIMPIA (decisión 2), reportado como `table_missing` / `table_was_missing`, y la guarda sigue exigiendo `confirm_rows: 0` | `TestDeleteContentTypeWithoutItsTable`, transcript dual `orphan …` |
| Falla a mitad de camino | Rollback total, probado en DOS puntos distintos y **en los dos motores** | `TestDeleteContentTypeRollbackMidwayFK`, `TestDeleteContentTypeRollbackAtMetadataStep`, línea `ATOMIC:` del transcript dual |
| ¿Se puede borrar un tipo de CÓDIGO por esta ruta? | No: `articles`/`products` no son tipos dinámicos, la ruta responde 404 y las tablas siguen ahí | `TestDeleteContentTypeUnknownIs404` |

---

## Cambios de comportamiento que el orquestador debe conocer

1. **Rutas nuevas:** `DELETE /content-types/{name}` (JSON, `content_types.manage`),
   `GET /admin/content-types/{name}/delete` (sesión) y
   `POST /admin/content-types/{name}/delete` (`content_types.manage`).
2. **Ningún permiso nuevo, ninguna dependencia nueva, ninguna migración manual.** `go.mod` no cambió.
3. **Ninguna ruta existente cambió** de método, forma de respuesta ni gating. El recuento de filas NO
   se agregó a `GET /content-types/{nombre}`.
4. **La operación es irreversible por definición.** El rollback cubre un borrado FALLIDO, no un
   borrado exitoso del que el admin se arrepiente: el backup previo al deploy sigue siendo la red de
   seguridad de siempre.
5. **Un `DELETE` con confirmación equivocada abre y revierte una transacción.** Es deliberado: el
   recuento tiene que compararse contra el número vivo dentro de la transacción. No deja nada.
6. **`DELETE` con cuerpo.** El endpoint lee la confirmación del CUERPO de la petición. Un cliente o
   proxy que descarte el cuerpo de un `DELETE` recibirá siempre el 400 de "no confirmation was
   sent"; nunca borrará nada por accidente, pero tampoco podrá borrar.

---

## Archivos tocados

**Nuevos**

- `internal/store/contenttypes_delete.go` — T1/T2: el plan read-only, la guarda y el borrado atómico.
- `internal/store/contenttypes_delete_test.go` — T1/T2/T4 (13 tests).
- `internal/store/dualengine_contract26_test.go` — T4 en los dos motores (store, tag `dualengine`).
- `internal/server/server_contract26_test.go` — T2 (API real).
- `internal/server/server_ui_contract26_test.go` — T3 (UI real).
- `internal/server/dualengine_contract26_test.go` — T4: ciclo HTTP completo en los dos motores.
- `internal/server/templates/content_types_delete.html` — la página de confirmación con el recuento.

**Modificados**

- `internal/server/contenttypes.go` — el handler `DELETE` y la documentación del payload de la guarda.
- `internal/server/server.go` — la ruta `DELETE /content-types/{name}`.
- `internal/server/ui_contenttypes.go` — T3 (las dos rutas y sus handlers).
- `internal/server/ui.go` — la plantilla nueva en el `//go:embed`.
- `internal/server/templates/content_types_list.html` — link «Borrar» + texto actualizado.
- `DEFINITION-CPT-DINAMICOS.md` — el ítem "fuera de alcance" queda cerrado por este contrato.

**NO se tocó `sqlite-postgres-compat`.** Todo lo necesario ya estaba (`CompileDropTable` /
`CompileDropTableIfExists` puras + DDL transaccional); no faltó nada.

---

## Honestidad sobre el alcance de la verificación

- El ciclo **por HTTP** está verificado contra **los dos motores** (batería `dualengine` en
  `internal/server`, PostgreSQL 17 real con pgvector en un esquema efímero, verificando contra el
  catálogo propio del motor).
- El ciclo con el **BINARIO real** (`--dump-schema`, reinicio del proceso, catálogo consultado
  directamente) se corrió sobre **SQLite**. La infraestructura PostgreSQL provista no tiene `psql` ni
  ninguna forma de crear un esquema aislado desde la línea de comandos para apuntar el binario ahí,
  y correrlo contra `public` habría dejado las tablas de librarian en una base compartida. El
  equivalente en PostgreSQL está cubierto por la batería dual del store, que ejecuta los MISMOS
  caminos de producción: `EnsureSchema` dos veces tras el borrado sin recrear nada, y
  `CanonicalSchema` —que es exactamente lo que `--dump-schema` emite— sin la tabla borrada.
- La comparación de recuento es un guardián contra una **confirmación humana desactualizada**, no una
  garantía de serializabilidad. Un escritor cuya transacción sigue ABIERTA cuando corre el borrado
  bloqueará el `DROP TABLE` en PostgreSQL (ACCESS EXCLUSIVE) y sus filas se irán con la tabla cuando
  commitee. Eso es inherente a `DROP TABLE` y no lo resuelve ningún recuento; queda dicho acá en vez
  de prometer algo que el test no prueba. Lo que SÍ está probado es el caso realista: un escritor que
  **commiteó** entre la lectura del número y la confirmación hace que el borrado se rechace.

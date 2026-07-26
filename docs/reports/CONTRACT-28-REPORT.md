# CONTRACT-28 — Cerrar la ventana de carrera de las referencias

Base: `55b0e1c`. Árbol SIN commitear, como pide el contrato: el orquestador commitea y despliega
tras verificar.

**Resultado: LISTO CON UN HUECO REAL, MEDIDO Y DECLARADO.** `go.mod` está en `compat v0.5.0`. Los
tres sitios donde una escritura de contenido dinámico puede fallar por clave foránea consumen
`Store.IsForeignKeyViolation` y traducen el rechazo del motor al mismo `400` que habría dado la
comprobación previa. Las dos comprobaciones previas **se quedan intactas** y siguen siendo el camino
normal con el mensaje bueno. La carrera se provoca **de verdad** contra los dos motores, no se
simula.

**El hueco:** de los tres interleavings × dos motores, **cinco cierran y uno no**: la carrera del
**DELETE en SQLite**. La causa es estructural y está medida abajo — el `ON DELETE RESTRICT` que
eligió CONTRACT-27 hace que SQLite reporte `SQLITE_CONSTRAINT_TRIGGER (1811)` en vez de
`SQLITE_CONSTRAINT_FOREIGNKEY (787)`, y `compat` v0.5.0 excluye 1811 **a propósito y por
documentación**. Cerrarlo desde `librarian` exigiría una de las tres cosas que el contrato prohíbe.
Lo dejé abierto, asertado tal cual se comporta hoy, para que se borre el día que `compat` amplíe el
predicado en vez de pudrirse. **NO se tocó `sqlite-postgres-compat`.**

---

## Resumen de los gates

| Gate | Resultado |
|---|---|
| `go.mod` en `sqlite-postgres-compat v0.5.0` | sí |
| `go build ./...` | limpio |
| `go vet ./...` y `go vet -tags dualengine ./...` | limpio |
| `gofmt -l .` | vacío |
| `go test ./... -count=1` (1.ª vez) | verde (7/7 paquetes) |
| `go test ./... -count=1` (2.ª vez) | verde (7/7 paquetes) |
| `go test -tags dualengine -count=1 ./...` contra PostgreSQL 17 + pgvector real | verde (7/7 paquetes) |
| Tests existentes modificados | **ninguno** |
| Dependencias nuevas | **ninguna** más allá del bump |
| Permisos nuevos | **ninguno** |

DSN usado: `postgres://postgres:***@31.220.22.176:5455/postgres?sslmode=disable`.

Archivos tocados: `go.mod`, `go.sum`, `internal/server/content.go`, `internal/server/server.go`, y
un archivo de test nuevo `internal/server/dualengine_contract28_test.go`. Nada fuera de `librarian`.

---

## T1 — El clasificador consumido, y las comprobaciones previas conservadas

### La división del trabajo, tal como la fija el contrato

No hay reemplazo. Hay dos cosas que responden lo mismo (`400`) por caminos distintos:

- **La comprobación previa es el camino normal y dueña del mensaje.** `checkReferenceTargets` sabe
  QUÉ relación falló y QUÉ id no existía; `checkNoIncomingReferences` sabe QUIÉN apunta a la fila y
  CUÁNTOS son. Esos son los mensajes que ve un cliente normal, y no se degradaron (evidencia en T2).
- **El clasificador es la RED bajo la carrera.** Solo sabe "una clave foránea rechazó esto". Su
  mensaje es necesariamente genérico, y ese precio se paga **únicamente** en el interleaving.

Borrar la comprobación previa cambiaría todos los mensajes buenos por el genérico; borrar el
clasificador devolvería el 500. Quedan las dos.

### La dirección sale del sitio de llamada, nunca del texto

`compat` documenta explícitamente que **no puede** distinguir las dos direcciones (los dos motores
reportan el mismo código para "referencia inexistente" y para "fila todavía referenciada"). No hace
falta: la sentencia que falló se conoce en el sitio. Por eso hay **dos funciones, una por sentencia**
(`internal/server/content.go`):

- `foreignKeyRaceOnWrite` — se consulta tras `INSERT` (crear) y tras `UPDATE` (actualizar). Un
  `INSERT`/`UPDATE` que falla por FK solo puede ser una referencia a algo que ya no está.
- `foreignKeyRaceOnDelete` — se consulta tras `DELETE`. Un `DELETE` que falla por FK solo puede ser
  una fila que alguien todavía referencia.

**Ninguna de las dos mira el mensaje.** Ambas son un `if err == nil || !h.store.IsForeignKeyViolation(err)`
y nada más. Si el error no es una violación de clave foránea devuelven `nil` y el sitio cae en su
manejo de siempre — así una caída de base conserva su `500` y, vía `writeOperationFailure`, su `503`
cuando el pool está muerto (CONTRACT-24/25 intactos).

### Los comentarios viejos, actualizados

Los dos comentarios que decían textualmente que `compat` no ofrecía el clasificador quedaron viejos y
se reescribieron para explicar la división nueva, no para contradecirla:

- `checkReferenceTargets`: la ventana residual **sigue existiendo** (no se puede eliminar), pero ya
  no degrada a 500. El comentario ahora dice por qué la comprobación se queda: es la única de las dos
  que sabe qué referencia y qué id fallaron.
- `checkNoIncomingReferences`: lo mismo en espejo, más la advertencia del hueco de SQLite.
- El comentario del borrado en `handleDeleteContent`: reescrito igual.

### Alcance, dicho porque tiene consecuencia real

- `author_id` de una tabla de contenido **también** es una clave foránea (a `users`). Un `INSERT`
  cuyo autor fue borrado entre el login y la escritura cae en el mismo `400` con la misma frase
  genérica. Es la respuesta honesta para ese caso también: la petición nombró un usuario que ya no
  existe. Está dicho en el comentario de la función.
- `articles`, `products` y `terms` **no** se tocaron: siguen con su comportamiento previo. El
  contrato acota T1 a "los sitios donde hoy una escritura **de contenido** puede fallar por clave
  foránea", que son los tres de `content.go`, y ampliar más habría sido alcance inventado.
- **¿El clasificador confunde una violación única con una foránea?** No. Los códigos son disjuntos
  por construcción en `compat` (`23505`/`2067`/`1555` para única; `23503`/`787` para foránea) y las
  tres rutas donde se consulta no tienen ninguna constraint única propia más allá de la PK, cuyo
  valor genera el proceso (`dual.NewUUID`).

---

## T2 — La carrera provocada DE VERDAD

### El punto de sincronización, y por qué existe

Nada acá fabrica un error. Cada rechazo asertado lo produce **el motor** sobre una tabla real con una
clave foránea real, y lo clasifica `compat`. No hay store falso, ni error inyectado, ni driver
stubbeado.

Lo único que el test controla es **cuándo** ocurre la escritura que interfiere. Añadí un campo
`referenceRaceHook func(ctx, op string)` en `handlers` (`internal/server/server.go`):

- Es **nil en producción**. `NewMux` nunca lo setea, ninguna configuración lo alcanza, y el campo es
  no exportado en un paquete interno: solo un test de este paquete puede tocarlo. Con nil el costo es
  una comparación por escritura de contenido.
- **No decide nada.** Solo corre. No se consulta para ningún resultado y no puede hacer que una
  escritura tenga éxito o falle por sí mismo.
- `op` vale `"create"`, `"update"` o `"delete"`, porque la interferencia del test **es una petición
  HTTP real por el mismo mux**, y sin ese filtro el hook se re-entraría sobre la escritura anidada.

Se justifica solo: la ventana es, por construcción, el hueco entre la comprobación previa y la
sentencia que protege — unos microsegundos. Nada fuera del proceso puede apuntarle. La batería
necesita que el proceso la mantenga abierta exactamente una vez.

### Los tres interleavings

`internal/server/dualengine_contract28_test.go`, `TestDualEngineForeignKeyRaceHTTP`:

1. **CREATE** — se crea un autor, la comprobación previa lo ve, la ventana se abre, el autor se borra
   **por la ruta DELETE real**, y recién entonces corre el `INSERT`, que choca contra la FK real.
2. **UPDATE** — igual, pero con un `PUT` que mueve la referencia a la fila borrada en el medio.
3. **DELETE** — el espejo: `checkNoIncomingReferences` cuenta cero, la ventana se abre, se crea un
   libro que referencia la fila **por la ruta POST real**, y recién entonces corre el `DELETE`.

### Salida real

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5455/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineForeignKeyRaceHTTP -count=1 -v ./internal/server

=== RUN   TestDualEngineForeignKeyRaceHTTP
    dualengine_contract28_test.go:78: [sqlite] RACE delete: 1 interference(s) -> status 500, message class OTHER(could not delete content)
    dualengine_contract28_test.go:79: [postgres] RACE delete: 1 interference(s) -> status 400, message class classifier-race-on-delete
    dualengine_contract28_test.go:81: transcript (11 lines, identical on both engines):
        POST /content-types autores -> 201
        POST /content-types libros  -> 201
        RACE create: pre-check passed, target deleted in the window (1 interference) -> 400 msg=classifier-race-on-write
        RACE create: nothing was stored=true, target really gone=true
        RACE update: pre-check passed, target deleted in the window (1 interference) -> 400 msg=classifier-race-on-write
        RACE update: reference stayed null=true (the UPDATE did not land)
        RACE delete: pre-check counted zero, referrer created in the window (1 interference) -> refused=true row-survived=true
        NORMAL create with a missing reference -> 400 msg=precheck-missing-target names-the-id=true
        NORMAL delete of a referenced row    -> 400 msg=precheck-incoming-references names-the-referrer=true
        NORMAL delete of an unreferenced row -> 204
        NORMAL create with a live reference  -> 201 round-tripped=true
    dualengine_contract28_test.go:95: OK: 11 observations identical on SQLite and PostgreSQL 17
--- PASS: TestDualEngineForeignKeyRaceHTTP (33.25s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/server	37.057s
```

Lo que prueba, línea por línea:

- **`msg=classifier-race-on-write` en las carreras de create y update, en LOS DOS motores.** No es el
  mensaje de la comprobación previa: es el genérico del clasificador. O sea que la comprobación
  previa **pasó** y quien rechazó fue la clave foránea real, clasificada. Status `400`, no `500`.
- **`nothing was stored=true` / `reference stayed null=true`.** La escritura no aterrizó: la FK hizo
  su trabajo, no se corrompió nada.
- **`msg=precheck-missing-target` / `precheck-incoming-references` con la ventana cerrada**, y
  `names-the-id=true` / `names-the-referrer=true`: **el camino normal no se degradó**. Los mensajes
  buenos siguen siendo los que ve un cliente normal, con el id concreto y el referente concreto.
- **Las 11 líneas son idénticas en SQLite y PostgreSQL 17.**

La aserción es explícita, no decorativa: un `>= 500` en cualquiera de las carreras de create/update, o
un status distinto de `400`, hace fallar el test (`mustNot500`).

### Salida real del hueco (medición directa, DDL idéntico)

Programa de diagnóstico corrido contra `compat.OpenSQLite` (ya borrado del árbol; era temporal):

```
PRAGMA foreign_keys = 1
[noaction]   CREATE TABLE c (id TEXT PRIMARY KEY, p TEXT REFERENCES p(id))
  DELETE parent : constraint failed: FOREIGN KEY constraint failed (787)  | IsForeignKeyViolation=true
  INSERT child  : constraint failed: FOREIGN KEY constraint failed (787)  | IsForeignKeyViolation=true
[restrict]   CREATE TABLE c (id TEXT PRIMARY KEY, p TEXT REFERENCES p(id) ON DELETE RESTRICT)
  DELETE parent : constraint failed: FOREIGN KEY constraint failed (1811) | IsForeignKeyViolation=false
  INSERT child  : constraint failed: FOREIGN KEY constraint failed (787)  | IsForeignKeyViolation=true
```

Y el error real que ve el handler en la carrera del DELETE, instrumentado temporalmente:

```
C28DIAG type=*sqlite.Error   err=constraint failed: FOREIGN KEY constraint failed (1811)  fk=false
C28DIAG type=*pgconn.PgError err=ERROR: update or delete on table "cpt_autores" violates foreign key
        constraint "cpt_libros_autor_fkey" on table "cpt_libros" (SQLSTATE 23503)          fk=true
```

---

## El hueco: la carrera del DELETE en SQLite

**Qué pasa.** CONTRACT-27 declara **toda** relación `ON DELETE RESTRICT` (`schema.foreignKeyRestrict`,
con tres razones argumentadas). SQLite implementa el rechazo del lado padre de un `RESTRICT` a través
de su programa interno de triggers de clave foránea, así que reporta el código extendido
`SQLITE_CONSTRAINT_TRIGGER (1811)`, **no** `SQLITE_CONSTRAINT_FOREIGNKEY (787)`. `compat` v0.5.0
acepta únicamente 787 — deliberadamente, y con 1811 nombrado en su propia documentación como *"sibling
constraint codes [that] are different rejections and must not be folded in"*. El predicado devuelve
`false` y esa interleaving conserva su `500` en SQLite y solo ahí.

Nótese la asimetría, que es exactamente la que se mide arriba: el `INSERT` del lado hijo da `787` con
las dos acciones referenciales, y por eso **las carreras de create y update SÍ cierran en los dos
motores**.

**Por qué no lo cerré.** Las tres salidas disponibles desde `librarian` están prohibidas por este
contrato:

1. Leer el texto del error — prohibido explícitamente, y es justo lo que la clasificación por código
   estructurado existe para evitar.
2. Re-implementar acá una tabla de códigos específica por motor (importar `modernc.org/sqlite`
   directo) — dependencia nueva más allá del bump, y recrea el branching por motor que el proyecto
   existe para borrar.
3. Cambiar `RESTRICT` por `NO ACTION` — revierte una decisión deliberada y argumentada de
   CONTRACT-27, fuera del alcance de este contrato.

La cuarta salida real es **ampliar el predicado en `compat`** para que acepte 1811, y este contrato
dice explícitamente NO tocar `compat`.

**Qué NO se rompe mientras tanto.** La clave foránea sigue rechazando el borrado y la fila sobrevive:
el test lo asserta y esa línea **sí es idéntica en los dos motores** (`refused=true row-survived=true`).
Lo único que se degrada es el código de estado, para ese interleaving, en ese motor.

**Cómo queda vigilado.** El test asserta el comportamiento de hoy **exacto** por motor
(`sqlite → 500`, `postgres → 400`). El día que `compat` amplíe el predicado, ese test **falla ruidosamente**
y obliga a borrar el hueco en vez de olvidarlo. El comentario de `foreignKeyRaceOnDelete` lleva la
misma medición.

---

## Red-team del contrato, respondido

| Pregunta | Respuesta |
|---|---|
| ¿Una violación de FK que NO es de contenido dinámico —la de `users`— cae en el camino nuevo y devuelve algo razonable? | Sí para `author_id` de una tabla dinámica: `400` con la frase genérica, que es honesta (la petición nombró un usuario que ya no existe). `articles`/`products`/`terms` no se tocaron. |
| ¿Un `UPDATE` que cambia la referencia a una fila borrada en el medio? | Cubierto y probado como interleaving propio (`RACE update`), `400` en los dos motores. |
| ¿El clasificador confunde una violación única con una foránea en algún camino compartido? | No: los códigos son disjuntos en `compat`, y las tres rutas no tienen constraint única propia salvo la PK generada por el proceso. |
| ¿La comprobación previa se degradó? | No. Sección "NORMAL" del transcript: mismo mensaje, con el id y el referente concretos. |
| ¿Un fallo de infraestructura sigue dando 503? | Sí: el clasificador devuelve `nil` para cualquier error que no sea FK, y el sitio cae en `writeOperationFailure` sin cambios. Baterías de CONTRACT-24/25 verdes. |

---

## Criterios de aceptación

- [x] `go.mod` en v0.5.0; build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [x] T1: clasificación consumida; comprobaciones previas conservadas; comentarios actualizados.
- [~] T2: la carrera provocada de verdad devuelve el código correcto, con salida real — **en 5 de los
      6 casos (3 interleavings × 2 motores)**. El sexto, la carrera del DELETE en SQLite, está medido,
      argumentado, asertado y declarado arriba.
- [x] Los mensajes del camino normal no se degradan.

---

## Cierre posterior — el hueco se cerró en `compat` v0.5.1

Todo lo anterior es el registro de lo que se sabía en su momento y no se reescribe. Esta sección es
lo único que cambió después.

**El hueco declarado arriba —la carrera del `DELETE` en SQLite— ya no existe.** `compat` **v0.5.1**
amplió `Store.IsForeignKeyViolation` para aceptar también `SQLITE_CONSTRAINT_TRIGGER (1811)`, que es
como SQLite reporta el rechazo del lado del padre de un `ON DELETE RESTRICT`. Era exactamente la
cuarta salida que este reporte identificaba como la real: ampliar el predicado en `compat`, no
trabajarlo alrededor desde `librarian`.

**No hizo falta tocar código de producción.** `foreignKeyRaceOnDelete` ya llamaba al predicado; solo
subió `go.mod` a `v0.5.1`. Los cambios de este lado son: subir la dependencia, borrar la aserción por
motor que fijaba el hueco en `internal/server/dualengine_contract28_test.go` —puesta ahí a propósito
para fallar el día que `compat` lo arreglara, y falló— y actualizar la prosa del test y de
`internal/server/content.go` que lo describía como abierto.

Los **seis** casos (3 interleavings × 2 motores) cierran ahora, y el caso del `DELETE` se compara
línea por línea en el transcript como los otros cinco en vez de tratarse aparte:

```
COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5456/postgres?sslmode=disable' \
  go test -tags dualengine -run TestDualEngineForeignKeyRaceHTTP -count=1 -v ./internal/server

=== RUN   TestDualEngineForeignKeyRaceHTTP
    dualengine_contract28_test.go:85: transcript (12 lines, identical on both engines):
        POST /content-types autores -> 201
        POST /content-types libros  -> 201
        RACE create: pre-check passed, target deleted in the window (1 interference) -> 400 msg=classifier-race-on-write
        RACE create: nothing was stored=true, target really gone=true
        RACE update: pre-check passed, target deleted in the window (1 interference) -> 400 msg=classifier-race-on-write
        RACE update: reference stayed null=true (the UPDATE did not land)
        RACE delete: pre-check counted zero, referrer created in the window (1 interference) -> 400 msg=classifier-race-on-delete
        RACE delete: the delete was refused=true, row survived=true
        NORMAL create with a missing reference -> 400 msg=precheck-missing-target names-the-id=true
        NORMAL delete of a referenced row    -> 400 msg=precheck-incoming-references names-the-referrer=true
        NORMAL delete of an unreferenced row -> 204
        NORMAL create with a live reference  -> 201 round-tripped=true
    dualengine_contract28_test.go:99: OK: 12 observations identical on SQLite and PostgreSQL 17
--- PASS: TestDualEngineForeignKeyRaceHTTP (32.95s)
PASS
ok      github.com/MauricioPerera/librarian/internal/server      36.883s
```

El `T2` de los criterios de aceptación queda por tanto en **6 de 6**, y con esto el contrato cierra
sin excepciones.

# CONTRACT-13 — CPT dinámicos: registro de definiciones y esquema canónico compuesto

Base: `822f5a2` (CONTRACT-01..12 completos, producción real en `librarian.ardf.dev`).
Sin dependencias nuevas. Sin cambios en `sqlite-postgres-compat`. Sin commits (el árbol queda
con los cambios sin commitear, como pide el contrato).

Estado: **COMPLETO**. Los tres call sites de `schema.Build()` pasaron a componer código +
dinámico; el ciclo de reinicio (tabla **y** metadata) está probado de verdad, no asumido.

---

## Resumen por tarea

### T1 — Registro de definiciones (esquema + validación)

**Tablas de registro nuevas, de CÓDIGO, en `schema.Build()`** (`internal/schema/schema.go`):

| tabla | columnas | constraints |
|---|---|---|
| `content_types` | `id` (uuid PK, `gen_random_uuid()`), `name` (text), `created_at` | PK(`id`), **UNIQUE(`name`)** |
| `content_type_fields` | `id`, `content_type_id` (uuid), `name`, `field_type` (text), `ordinal` (integer) | PK(`id`), UNIQUE(`content_type_id`,`name`), UNIQUE(`content_type_id`,`ordinal`), FK `content_type_id`→`content_types.id` ON DELETE CASCADE, **CHECK `field_type IN ('text','integer','decimal','boolean','date')`** |

Ninguna de las dos usa `ContentType()`: una *definición* no es contenido (no tiene autor, ni
workflow de publicación, ni columna `metadata`); es dato de administración, como `terms`.
`ContentType()` **no fue tocado** — su firma es exactamente la misma.

**Permiso nuevo**: `content_types.manage` agregado a `schema.Permissions`. Es el ÚNICO permiso
agregado. El seed (`store.SeedCatalogs`) es idempotente y data-driven, así que no necesitó ningún
cambio de lógica — agregar el string fue todo.

**Validador de identificadores** (`internal/schema/identifier.go`) — la pieza de seguridad:

```go
const MaxIdentifierLength = 32
var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
func ReservedNames() map[string]struct{}
func ValidateIdentifier(name string) error
```

`ValidateIdentifier` es **una sola función** aplicada tanto a nombres de tipo como de campo, con
**una sola lista de reservados** (unión), tal como lo describe el contrato. No hay ninguna ruta de
"escapado" o "saneado": un nombre pasa entero o no llega nunca a ser un identificador.

`ReservedNames()` es **derivada, nunca hardcodeada**, de tres fuentes:
1. todas las tablas de `Build()` (si mañana se agrega una tabla de código, la reserva se actualiza
   sola — incluidas `content_types`/`content_type_fields`, que están en `Build()`),
2. las cuatro tablas internas `__compat_*`,
3. las columnas que inyecta `ContentType()`, obtenidas llamando a `ContentType("x", nil)` y leyendo
   lo que devolvió (`id`, `author_id`, `created_at`, `updated_at`, `metadata`).

Resultado real: **23 nombres reservados**
`[__compat_applied_changes __compat_capture_state __compat_change_journal __compat_schema api_keys
article_terms articles author_id content_type_fields content_types created_at id metadata
permissions product_terms products role_permissions roles taxonomies terms updated_at user_roles
users]`

**Modelo dinámico** (`internal/schema/dynamic.go`): `FieldType` cerrado a los cinco tipos de v1
(`text`/`integer`/`decimal`/`boolean`/`date` → `compat.TextType`/`IntegerType`/`DecimalType`/
`BooleanType`/`DateType`). Nada de JSON/vector/dominios/generadas ni FK. `DynamicTable(def)`
construye la tabla **a través de `ContentType()`**, así que un CPT dinámico produce exactamente la
misma forma que uno de código.

### T2 — Esquema canónico compuesto (la parte crítica)

`schema.BuildWith(defs) (compat.Schema, error)` = `Build()` + una tabla por definición persistida,
ordenadas por nombre (determinismo) y siempre DESPUÉS de las de código (su FK `author_id` apunta a
`users`, que `Build()` declara primero). Valida el compuesto entero antes de devolverlo.

`store.CanonicalSchema(ctx, store)` es la respuesta única a "¿cuál es el esquema de esta
instancia?". Los **tres** call sites del RECON ahora la usan:

| # | lugar | antes | ahora |
|---|---|---|---|
| 1 | `store.EnsureSchema` | `want := schema.Build()` | `want, err := CanonicalSchema(ctx, store)` |
| 2 | `schema.JSON()` / `--dump-schema` | `json.MarshalIndent(Build())` | `schema.JSONWith(defs)`, con `defs` leídas de la DB |
| 3 | `compat.InferFeatures(schema.Build())` (fixture de export) | `Build()` | `store.CanonicalSchema(ctx, sdb)` |

`schema.JSON()` **fue eliminada** deliberadamente y reemplazada por `JSONWith(defs)`. Dejar una
función sin parámetros que emite medio esquema es exactamente la trampa que este contrato viene a
sacar del código: seguiría compilando, seguiría pareciendo correcta, y silenciosamente dejaría los
CPT dinámicos y sus filas fuera de todo `compat copy`. Ahora no existe una sobrecarga que "solo
tira el esquema de código".

### T3 — API de definiciones + creación de la tabla real

`internal/server/contenttypes.go` + rutas en `internal/server/server.go`:

```
POST   /content-types          content_types.manage   201 + definición + tabla real (atómico)
GET    /content-types          identidad válida       200 {"content_types":[...]}
GET    /content-types/{name}   identidad válida       200 definición | 404
```

**No hay PUT ni DELETE** (fuera de alcance por definición; `compat` no soporta `ALTER TABLE`).
Verificado: ambos devuelven 405.

El contrato público de las rutas 01-12 no cambió en absoluto.

---

## Decisiones de diseño (con su porqué)

### 1. Nombres y forma de las tablas de registro

`content_types` + `content_type_fields`. Dos tablas y no una con JSON: un array JSON de campos no
puede llevar un `UNIQUE` ni un `CHECK`, y las garantías de este contrato tienen que ser de
esquema, no de aplicación. Además, guardar los campos como JSON reintroduciría un formato propio
por fuera del modelo declarativo de `compat` — la segunda fuente de verdad que el proyecto evita.

Detalles con motivo:
- **`UNIQUE(name)` en `content_types`**: es la unicidad REAL del nombre de tipo. Un chequeo de
  aplicación ("¿ya existe?") puede perder la carrera entre dos admins concurrentes; la constraint
  no. Mismo razonamiento que `products.sku` y `terms(taxonomy_id, slug)`. El servidor la traduce a
  400 limpio (`isDuplicateContentTypeViolation`), nunca a un 500 con SQL crudo.
- **`CHECK field_type IN (...)`** construido a partir de `schema.FieldTypes`: el vocabulario del
  API, el valor persistido y la constraint son **la misma lista**, no pueden divergir. Una fila
  escrita por fuera del API no puede colar un tipo no soportado.
- **`ordinal`** (no `position`): el orden de los campos define el orden de las columnas de la tabla
  producida, y ese orden tiene que ser reproducible para el round-trip byte-exacto del esquema.
  `POSITION` es palabra reservada en PostgreSQL — `compat` cita identificadores así que compilaría
  igual, pero un nombre no reservado mantiene legible cualquier SQL de diagnóstico a mano.
- **Sin `updated_at` en `content_types`**: v1 es crear-solamente. Una columna que nunca podría
  cambiar sería una mentira en el esquema.
- **FK `ON DELETE CASCADE`** de los campos al tipo: borrar tipos está fuera de alcance, pero la
  regla de integridad se declara honestamente (y hace que limpiar un tipo sea un solo DELETE).

### 2. Largo máximo de identificador: **32**

Derivado de la restricción real más ajustada de la pila, no elegido por estética:

- PostgreSQL **trunca** identificadores en `NAMEDATALEN-1 = 63` bytes. Truncar es el peor modo de
  falla disponible: dos nombres distintos podrían volverse el mismo objeto en Postgres y seguir
  distintos en SQLite (que no tiene límite práctico) — justo la divergencia dual-motor prohibida.
- `compat` **deriva** nombres más largos a partir del nombre de tabla. La derivación más larga del
  paquete es el trigger de captura de cambios (`capture.go:105`):
  `"__compat_capture_" + <tabla> + "_" + <kind>` = 17 + len + 1 + 6 (`insert`/`update`/`delete`)
  = **24 + len(tabla)**. Quedar bajo 63 exige `len(tabla) ≤ 39`.
- **32** queda cómodo bajo ese techo duro (32 + 24 = 56), deja 7 bytes de aire para cualquier
  prefijo derivado futuro de `compat`, y es más que suficiente para un nombre humano.

El límite se aplica igual a campos (simetría; una sola función de validación).

### 3. Minúsculas obligatorias (no solo "rechazar duplicados")

SQLite pliega mayúsculas **incluso en identificadores citados**; PostgreSQL no. Un tipo `MiTipo` y
otro `mitipo` colisionarían en SQLite y coexistirían en Postgres: divergencia silenciosa que rompe
la exportabilidad. Restringir el alfabeto a minúsculas elimina la divergencia en el origen en vez
de intentar detectarla después.

### 4. `--dump-schema`: cómo pasa de OFFLINE a necesitar la DB

Condición innegociable: **nunca emitir en silencio un esquema incompleto**. Resolución, ordenada
por qué tan ruidosamente falla:

1. **Sigue sin requerir `LIBRARIAN_JWT_SECRET`** (`config.Load()` sigue sin llamarse): volcar un
   esquema no es servir tráfico. Lo único nuevo que necesita es la base.
2. La base se localiza por `--db <path>` / `--db=<path>`, si no `LIBRARIAN_DB`, si no
   `librarian.db` (el mismo default del servidor).
3. **Si ese archivo NO existe, el comando FALLA con exit≠0** y un mensaje explícito. No cae de
   vuelta al esquema de solo-código, y **no deja que SQLite cree la base vacía** — un path mal
   escrito produciría si no un dump verosímil pero incompleto, exactamente la falla silenciosa que
   se está diseñando para afuera.
4. **Si la base existe pero no tiene la tabla `content_types`**, cero tipos dinámicos es una
   **prueba**, no una suposición: una definición solo puede vivir en esa tabla. Se emite el esquema
   de código y ese esquema **es completo para esa base**. Esto es lo que mantiene el comando
   funcionando contra una base anterior a CONTRACT-13.
5. Cualquier otra falla (archivo ilegible, fila de definición corrupta, motor inesperado) propaga
   como exit≠0. **No hay camino que imprima un esquema parcial.**

Documentado en `docs/OPERATIONS.md` (paso 2 del procedimiento de export).

### 5. Atomicidad de "persistir definición + crear tabla"

Un tipo registrado sin su tabla es estado corrupto: todo `EnsureSchema` posterior compondría un
esquema canónico con una tabla que no existe, el CRUD genérico de CONTRACT-14 consultaría una tabla
faltante, y `compat copy` recibiría un `schema_ref` que describe algo que no puede leer. El estado
espejo (tabla sin definición) es igual de malo en el otro sentido: la tabla sería invisible al
esquema compuesto y quedaría silenciosamente fuera de todo export, con sus datos.

**Solución: todo ocurre dentro de UNA transacción** (`store.CreateContentType`). SQLite ejecuta DDL
transaccionalmente, así que el `CREATE TABLE`, los `INSERT` de la definición y el upsert de
`__compat_schema` commitean o revierten juntos.

Los pasos son **la misma maquinaria de `EnsureSchema`**, no un `ApplySchema` suelto:

1. `def.Validate()` — el portón T1, antes de que el nombre toque nada.
2. Componer el esquema que la base VA a tener (`BuildWith(existentes + nueva)`) y dejar que
   `compat` valide todo (FK, familias de tipo, columnas duplicadas) por adelantado.
3. `missingTables(...)` — el mismo diff incremental de `EnsureSchema`. Debe resolver **exactamente**
   a esa única tabla nueva; cualquier otra cosa significa que la base no está en el estado que
   creemos y crear tablas a ciegas ahí sería inseguro → error.
4. `compat.CompileDDL(...)` **antes** de abrir la transacción, para que una falla de compilación no
   cueste nada ni deje nada a medias.
5. Una transacción: INSERTs + statements DDL + `writeFullSchemaMetadata(tx, want)` con el esquema
   compuesto COMPLETO (nunca el reducido).

No se llama a `compat.Store.ApplySchema` porque abre su propia transacción, y `compat` fija el pool
SQLite a **una sola conexión** (`db.SetMaxOpenConns(1)` en `OpenSQLite`), con lo cual una
transacción anidada haría deadlock. Su cuerpo se reproduce statement por statement en su lugar.

`compat.Store` es un struct de dos campos exportados (`{Target, DB}`), así que
`store.FromDB(*sql.DB)` lo envuelve sin abrir una segunda conexión — por eso **`server.Deps` y la
firma de `NewMux` no cambiaron** (el contrato público de 01-12 queda intacto).

**Concurrencia**: dos admins con el MISMO nombre los decide `UNIQUE(name)` (el perdedor revierte
toda su transacción, tabla incluida, y recibe 400). Dos admins con nombres DISTINTOS los serializa
un `sync.Mutex` en `handlers` — el paso diff-y-crear no debe intercalarse. Serializar es gratis en
la práctica (una sola conexión SQLite de todos modos, y definir un tipo es una acción
administrativa rara).

### 6. Columnas dinámicas NULLABLE

DEFINITION-CPT-DINAMICOS.md lista solo *nombre + tipo* por campo: no hay concepto de "requerido" en
v1. Una columna `NOT NULL` sin default haría fallar con un error opaco todo insert que la omita, y
v1 **no puede** `ALTER TABLE` para arreglarlo después. Nullable es el default honesto.

---

## Trade-offs asumidos

- **Un solo conjunto de reservados para tipos y campos.** Es levemente más estricto de lo
  estrictamente necesario (un campo llamado `users` sería inofensivo), pero un conjunto único es
  mucho más fácil de auditar que dos solapados, y ningún modelo de contenido legítimo necesita esas
  palabras. El contrato lo describe así explícitamente.
- **`schema.JSON()` eliminada** en vez de conservada por compatibilidad. Rompe llamadores internos
  (se actualizó `schema_test.go`), pero conservarla habría dejado el pie de la trampa puesto.
- **`registryPresent` consulta `sqlite_master`**, no `InspectSchema`. `InspectSchema` prefiere la
  fila `__compat_schema`, que es justo lo que `EnsureSchema` está por reescribir: preguntarle sería
  circular. El catálogo físico es la única respuesta no circular. Eso ata esta función al motor
  SQLite — que es el motor de runtime de librarian por construcción (`store.Open` →
  `compat.OpenSQLite`); cualquier otro motor **falla ruidosamente** en vez de reportar "no hay
  registro" en silencio (que produciría un esquema canónico incompleto).
- **`--dump-schema` sin escotilla `--code-only`.** Se evaluó y se descartó: cualquier bandera que
  produzca un esquema de solo-código contra una base con tipos dinámicos es un pie de bala, y el
  caso legítimo (base sin registro) ya está cubierto y es correcto por prueba.
- **Ventana residual conocida (preexistente, no introducida acá):** si el `INSERT` en
  `__compat_schema` de `EnsureSchema` fallara DESPUÉS de un `ApplySchema` exitoso (falla de disco),
  la metadata quedaría reducida y el próximo arranque intentaría re-crear tablas existentes. Es el
  mismo riesgo que dejó CONTRACT-11 y está fuera de alcance; en `CreateContentType` **no existe**,
  porque ahí la escritura de metadata está dentro de la misma transacción.

## Red-team del contrato — respuestas

- **Dos requests concurrentes con el MISMO nombre de tipo** → `UNIQUE(name)` a nivel de esquema
  (garantía real, no chequeable por aplicación). Probado en dos niveles:
  `TestCreateContentTypeDuplicateName` verifica el sentinel Y que un `INSERT` directo duplicado sea
  rechazado por la base; `TestCreateContentTypeDuplicateIs400` verifica el 400 limpio en HTTP y que
  quede exactamente 1 definición.
- **Definición persistida pero `CREATE TABLE` falla** → imposible: una sola transacción. Probado en
  `TestCreateContentTypeIsAtomic` induciendo la falla de forma realista (una tabla física con ese
  nombre que la metadata de compat desconoce) y verificando `content_types=0`,
  `content_type_fields=0` y metadata intacta.
- **Un CPT llamado igual que una tabla de código FUTURA.** Hoy está cubierto: la reserva se deriva
  de `Build()`, así que ninguna tabla de código actual puede ser tomada. **Qué pasa si mañana se
  agrega una tabla de código con el nombre de un CPT dinámico YA existente (documentado, no
  resuelto):** `BuildWith` compondría dos tablas con el mismo nombre y `compat.Schema.Validate()`
  fallaría con `duplicate table` — es decir, el servicio **no arrancaría**. Falla ruidosa, no
  corrupción silenciosa, que es el comportamiento correcto; pero es un arranque roto que exigiría
  intervención manual. Resolverlo (p. ej. prefijar tablas dinámicas, o chequear en el arranque
  contra los CPT existentes antes de aplicar) queda para un contrato futuro.

---

## Verificación — salida REAL

### `go build ./...` y `go vet ./...` limpios

```
=== go build ./... ===
exit=0
=== go vet ./... ===
exit=0
=== go vet -tags exportfixture ./internal/server ===
exit=0
```

### `go test ./... -count=1` verde, DOS VECES

```
########## RUN 1 ##########
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.415s
ok  	github.com/MauricioPerera/librarian/internal/auth	3.870s
ok  	github.com/MauricioPerera/librarian/internal/config	0.678s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.385s
ok  	github.com/MauricioPerera/librarian/internal/server	20.848s
ok  	github.com/MauricioPerera/librarian/internal/store	2.498s
run1_exit=0
########## RUN 2 ##########
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.246s
ok  	github.com/MauricioPerera/librarian/internal/auth	3.659s
ok  	github.com/MauricioPerera/librarian/internal/config	0.639s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.329s
ok  	github.com/MauricioPerera/librarian/internal/server	20.445s
ok  	github.com/MauricioPerera/librarian/internal/store	2.405s
run2_exit=0
```

### T1 — batería de nombres hostiles, uno por uno (nivel validador)

`go test ./internal/schema/ -run TestValidateIdentifierHostileBattery -v`

```
=== RUN   TestValidateIdentifierHostileBattery
    REJECTED [empty                       ] ""                      -> name must not be empty
    REJECTED [double quotes               ] "re\"views"             -> name "re\"views" is invalid: must match [a-z][a-z0-9_]* (...)
    REJECTED [quote-escape injection      ] "x\" ; DROP TABLE \"users" -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [semicolon                   ] "reviews; DROP TABLE users" -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [spaces                      ] "my reviews"            -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [leading/trailing space      ] " reviews "             -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [uppercase                   ] "MiTipo"                -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [single uppercase letter     ] "Reviews"               -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [unicode                     ] "reseñas"               -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [unicode homoglyph           ] "revіews"               -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [only digits                 ] "123"                   -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [starts with digit           ] "1abc"                  -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [starts with underscore      ] "_reviews"              -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [hyphen                      ] "my-reviews"            -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [dot-qualified               ] "public.reviews"        -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [newline                     ] "reviews\nDROP TABLE users" -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [null byte                   ] "reviews\x00"           -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [too long (33)               ] "aaaa...a" (33)         -> name "aaa..." is invalid: longer than 32 characters
    REJECTED [way too long (200)          ] "aaaa...a" (200)        -> name "aaa..." is invalid: longer than 32 characters
    REJECTED [reserved code table: users  ] "users"                 -> name "users" is reserved and cannot be used
    REJECTED [reserved code table: articles] "articles"             -> name "articles" is reserved and cannot be used
    REJECTED [reserved code table: terms  ] "terms"                 -> name "terms" is reserved and cannot be used
    REJECTED [reserved registry table     ] "content_types"         -> name "content_types" is reserved and cannot be used
    REJECTED [reserved injected column: id] "id"                    -> name "id" is reserved and cannot be used
    REJECTED [reserved injected column: author_id] "author_id"      -> name "author_id" is reserved and cannot be used
    REJECTED [reserved injected column: metadata] "metadata"        -> name "metadata" is reserved and cannot be used
    REJECTED [reserved injected column: created_at] "created_at"    -> name "created_at" is reserved and cannot be used
    REJECTED [compat internal             ] "__compat_schema"       -> ... must match [a-z][a-z0-9_]* (...)
    REJECTED [compat internal             ] "__compat_applied_changes" -> ... must match [a-z][a-z0-9_]* (...)
--- PASS: TestValidateIdentifierHostileBattery (0.00s)

=== RUN   TestValidateIdentifierAcceptsLegitimateNames
    ACCEPTED "reviews" / "a" / "book_reviews" / "x1" / "my_type_2026" / "aaaa...a" (exactamente 32)
    (33 caracteres → rechazado: el límite es exacto, no aproximado)
--- PASS: TestValidateIdentifierAcceptsLegitimateNames (0.00s)

=== RUN   TestReservedNamesDerivedFromBuild
    reserved names (23): [__compat_applied_changes __compat_capture_state __compat_change_journal
    __compat_schema api_keys article_terms articles author_id content_type_fields content_types
    created_at id metadata permissions product_terms products role_permissions roles taxonomies
    terms updated_at user_roles users]
--- PASS: TestReservedNamesDerivedFromBuild (0.00s)

=== RUN   TestDefinitionValidateRejectsBadFields
    REJECTED [hostile type name               ] -> content type name "x\"; --" is invalid: ...
    REJECTED [reserved type name              ] -> content type name "products" is reserved ...
    REJECTED [hostile field name              ] -> field name "sc ore" is invalid: ...
    REJECTED [reserved field name             ] -> field name "metadata" is reserved ...
    REJECTED [unknown field type              ] -> field "score" has unknown type "bigint" (allowed: [text integer decimal boolean date])
    REJECTED [empty field type                ] -> field "score" has unknown type "" (allowed: ...)
    REJECTED [json field type (excluded in v1)] -> field "extra" has unknown type "json" (allowed: ...)
    REJECTED [vector field type (excluded in v1)] -> field "emb" has unknown type "vector" (allowed: ...)
    REJECTED [duplicate field name            ] -> field "score" is declared more than once
--- PASS: TestDefinitionValidateRejectsBadFields (0.00s)
```

### T1 — la misma batería a través del API HTTP real, con 400 y sin efecto

`go test ./internal/server/ -run TestCreateContentTypeHostileNamesAre400 -v`

```
=== RUN   TestCreateContentTypeHostileNamesAre400
    400 [empty                   ] ""                                 -> content type name must not be empty
    400 [double quote            ] "re\"views"                        -> content type name "re\"views" is invalid: must match [a-z][a-z0-9_]* (...)
    400 [quote-escape injection  ] "x\" ; DROP TABLE \"users"         -> content type name ... is invalid: must match [a-z][a-z0-9_]* (...)
    400 [semicolon               ] "reviews; DROP TABLE users"        -> content type name ... is invalid: must match [a-z][a-z0-9_]* (...)
    400 [spaces                  ] "my reviews"                       -> content type name ... is invalid: must match [a-z][a-z0-9_]* (...)
    400 [uppercase               ] "MiTipo"                           -> content type name ... is invalid: must match [a-z][a-z0-9_]* (...)
    400 [unicode                 ] "reseñas"                          -> content type name ... is invalid: must match [a-z][a-z0-9_]* (...)
    400 [only digits             ] "123"                              -> content type name ... is invalid: must match [a-z][a-z0-9_]* (...)
    400 [starts with digit       ] "1abc"                             -> content type name ... is invalid: must match [a-z][a-z0-9_]* (...)
    400 [too long                ] "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" -> content type name ... is invalid: longer than 32 characters
    400 [reserved code table     ] "users"                            -> content type name "users" is reserved and cannot be used
    400 [reserved injected column] "id"                               -> content type name "id" is reserved and cannot be used
    400 [compat internal         ] "__compat_schema"                  -> content type name ... is invalid: must match [a-z][a-z0-9_]* (...)
    400 [registry table          ] "content_types"                    -> content type name "content_types" is reserved and cannot be used
    400 [hostile field         ] "sc\"ore"                            -> field name "sc\"ore" is invalid: ...
    400 [hostile field         ] "score; DROP TABLE users"            -> field name ... is invalid: ...
    400 [hostile field         ] "Score"                              -> field name "Score" is invalid: ...
    400 [hostile field         ] "metadata"                           -> field name "metadata" is reserved and cannot be used
    400 [hostile field         ] "1st"                                -> field name "1st" is invalid: ...
    400 [unsupported field type] "json"                               -> field "x" has unknown type "json" (allowed: [text integer decimal boolean date])
    400 [unsupported field type] "vector"                             -> field "x" has unknown type "vector" (allowed: ...)
    400 [unsupported field type] "bigint"                             -> field "x" has unknown type "bigint" (allowed: ...)
    400 [unsupported field type] ""                                   -> field "x" has unknown type "" (allowed: ...)
    NO EFFECT OK: 0 definitions, 0 fields, table count unchanged at 15, 'users' intact
--- PASS: TestCreateContentTypeHostileNamesAre400 (0.16s)
```

### T2 — el dump incluye el CPT dinámico (un export lo incluiría)

`go test ./internal/store/ ./cmd/librarian/ -run 'Dump' -v`

```
=== RUN   TestDumpSchemaIncludesDynamicType        (internal/store)
    DUMP OK: 15 tables including dynamic 'reviews'
    [id author_id headline score price_paid verified read_on created_at updated_at metadata];
    compiles for PostgreSQL
--- PASS: TestDumpSchemaIncludesDynamicType (0.02s)

=== RUN   TestDumpSchemaIncludesDynamicTypes       (cmd/librarian — el código real de --dump-schema)
    DUMP OK: 15 tables, dynamic 'reviews' included, compiles for PostgreSQL.
    tables=[users roles permissions role_permissions user_roles api_keys articles products
            taxonomies terms article_terms product_terms content_types content_type_fields reviews]
--- PASS: TestDumpSchemaIncludesDynamicTypes (0.04s)

=== RUN   TestDumpSchemaFailsLoudlyWhenDatabaseMissing
    FAILS LOUDLY: --dump-schema must read the database to include the dynamic content types
    (a dump without them would export an incomplete schema): cannot access "...\does-not-exist.db":
    ... El sistema no puede encontrar el archivo especificado. — pass --db <path> or set LIBRARIAN_DB
--- PASS: TestDumpSchemaFailsLoudlyWhenDatabaseMissing (0.00s)

=== RUN   TestDumpSchemaOnPreContract13Database
    LEGACY DB OK: dumped the complete code schema (14 tables), no dynamic types to include
--- PASS: TestDumpSchemaOnPreContract13Database (0.03s)
```

### T2 — CICLO DE REINICIO (el bug de dos capas de CONTRACT-11), probado de verdad

`go test ./internal/store/ -run 'Restart|Upgrade|Atomic|Duplicate|Hostile|Canonical' -v`

```
=== RUN   TestDynamicTypeSurvivesRestartCycle
    boot #1 OK: table exists=true, metadata tables=15 (reviews present)
    restart #1 OK: table exists, metadata still contains 'reviews' (15 tables)
    restart #2 OK: table exists, metadata contains 'reviews' (15 tables), definition reloaded with 5 fields
--- PASS: TestDynamicTypeSurvivesRestartCycle (0.07s)

=== RUN   TestUpgradeFromPreContract13Database
    UPGRADE OK: registry added, legacy row intact, dynamic type created and surviving a restart
--- PASS: TestUpgradeFromPreContract13Database (0.06s)

=== RUN   TestCreateContentTypeIsAtomic
    create failed as expected: create table for content type "reviews": SQL logic error: table "reviews" already exists (1)
    ATOMIC OK: 0 definition rows, 0 field rows, metadata untouched after the failed creation
--- PASS: TestCreateContentTypeIsAtomic (0.01s)

=== RUN   TestCreateContentTypeDuplicateName
    DUPLICATE OK: sentinel=content type already exists; schema-level UNIQUE also rejects a direct
    insert: constraint failed: UNIQUE constraint failed: content_types.name (2067)
--- PASS: TestCreateContentTypeDuplicateName (0.02s)

=== RUN   TestCreateContentTypeRejectsHostileNames
    HOSTILE OK: 0 definitions persisted; database still has 15 tables (code schema only)
--- PASS: TestCreateContentTypeRejectsHostileNames (0.01s)

=== RUN   TestCanonicalSchemaWithoutRegistry
    registry-less database composes to the code schema exactly (14 tables)
--- PASS: TestCanonicalSchemaWithoutRegistry (0.00s)
```

Las tres capas se verifican por separado y con oráculos independientes:
- **física**: `sqlite_master` (nunca la metadata — la metadata es lo que está bajo prueba),
- **metadata**: se lee y decodifica la fila `__compat_schema.canonical_schema` y se confirma que
  contiene `reviews` Y las tablas de código,
- **definición**: se recargan las definiciones y se confirma que siguen componiendo.

El reinicio se prueba **dos veces**, porque la corrupción de metadata clase CONTRACT-11 solo se
manifiesta en el SEGUNDO arranque (cuando la fila corrupta se lee de vuelta).

### T2 — composición y round-trip del esquema compuesto

```
=== RUN   TestBuildWithComposesCodePlusDynamic
    COMPOSED OK: 15 tables (14 code + 1 dynamic); sqlite stmts=15, postgres stmts=15
--- PASS
=== RUN   TestDynamicSchemaRoundTripJSON
    COMPOSED ROUND_TRIP OK: sqlite stmts=15, postgres stmts=15, DIFF=none
--- PASS
=== RUN   TestBuildWithIsDeterministic                                      --- PASS
=== RUN   TestBuildWithRejectsHostilePersistedDefinition
    BuildWith rejected hostile persisted definition: dynamic content type "evil\"; DROP TABLE users; --": ...
--- PASS
=== RUN   TestContentTypesRegistryInBuild
    registry OK: content_types(UNIQUE name), content_type_fields(CHECK field_type IN [text integer decimal boolean date])
--- PASS
```

### T3 — CPT real vía API + query directa + 400 + 403

`go test ./internal/server/ -run 'ContentType|ListAndGetContentTypes|DynamicType' -v`

```
=== RUN   TestContentTypesManagePermissionSeeded
    content_types.manage seeded (id=a8b1c509-2560-4be1-a4d1-49c9f4b7a0e4)
--- PASS: TestContentTypesManagePermissionSeeded (0.05s)

=== RUN   TestCreateContentTypeCreatesRealTable
    T3 OK: POST /content-types created the real table;
    PRAGMA table_info(reviews)=[id author_id headline score price_paid verified read_on
                                created_at updated_at metadata]; 1 row inserted
--- PASS: TestCreateContentTypeCreatesRealTable (0.15s)

=== RUN   TestCreateContentTypeWithoutPermissionIs403
    GATING OK: 403 without content_types.manage (with every other permission granted),
    401 unauthenticated, nothing created
--- PASS: TestCreateContentTypeWithoutPermissionIs403 (0.16s)

=== RUN   TestCreateContentTypeDuplicateIs400
    DUPLICATE OK: 400 "a content type with this name already exists", still exactly 1 definition
--- PASS: TestCreateContentTypeDuplicateIs400 (0.16s)

=== RUN   TestListAndGetContentTypes
    READ OK: list=1, detail fields in order
    "headline:text,score:integer,price_paid:decimal,verified:boolean,read_on:date",
    unknown→404, unauthenticated→401
--- PASS: TestListAndGetContentTypes (0.25s)

=== RUN   TestCreateContentTypeIsCreateOnly
    CREATE-ONLY OK: PUT /content-types/reviews -> 405
    CREATE-ONLY OK: DELETE /content-types/reviews -> 405
--- PASS: TestCreateContentTypeIsCreateOnly (0.15s)
```

Nota sobre el 403: el rol de prueba recibe **todos los demás permisos**
(`content.create/update/publish/delete`, `users.manage`, `roles.manage`, `terms.manage`) y aun así
recibe 403 — o sea, el gate es el permiso nuevo y dedicado, no "cualquier grant de admin".

La columna `author_id` inyectada por `ContentType()` es FK real: el test inserta una fila en
`reviews` referenciando un usuario real y la cuenta.

### T4 — CONFIRMACIÓN EXPLÍCITA: todo lo de contratos anteriores sigue igual

Suite completa, 158 tests, todos PASS. Lista completa (JSON **y** UI de
articles/products/users/roles/api-keys/terms incluidos):

```
--- PASS: TestAPIKeysTable
--- PASS: TestAdminAPIKeyCreateShowsSecretOnce
--- PASS: TestAdminAPIKeyCreateUnknownRoleRejected
--- PASS: TestAdminAPIKeyRevokeIdempotentAndMissing
--- PASS: TestAdminAPIKeyRoundTrip
--- PASS: TestAdminAPIKeysNoSessionRedirects
--- PASS: TestAdminAPIKeysWriteWithoutPermission
--- PASS: TestAdminCreateAppearsInList
--- PASS: TestAdminCreateValidationReRendersForm
--- PASS: TestAdminDelete
--- PASS: TestAdminDeleteWithoutPermissionServerSide
--- PASS: TestAdminEditForm
--- PASS: TestAdminMalformedIDIsNotFound
--- PASS: TestAdminNavShowsProductos
--- PASS: TestAdminNoSessionRedirectsToLogin
--- PASS: TestAdminProductCreateAppearsInList
--- PASS: TestAdminProductCreateValidationReRendersForm
--- PASS: TestAdminProductDelete
--- PASS: TestAdminProductDeleteWithoutPermissionServerSide
--- PASS: TestAdminProductDuplicateSKUReRendersForm
--- PASS: TestAdminProductEditForm
--- PASS: TestAdminProductMalformedIDIsNotFound
--- PASS: TestAdminProductRoundTrip
--- PASS: TestAdminProductUpdate
--- PASS: TestAdminProductsNoSessionRedirectsToLogin
--- PASS: TestAdminProductsSessionWithoutPermissionIs403
--- PASS: TestAdminPublish
--- PASS: TestAdminRolesViewReflectsRealGrants
--- PASS: TestAdminRoundTrip
--- PASS: TestAdminSessionWithPermissionPasses
--- PASS: TestAdminSessionWithoutPermissionIs403
--- PASS: TestAdminTermCRUDAndNav
--- PASS: TestAdminTermsNoSessionRedirectsToLogin
--- PASS: TestAdminTermsSessionWithoutPermissionIs403
--- PASS: TestAdminUpdate
--- PASS: TestAdminUserCreateAppearsInListAndDetail
--- PASS: TestAdminUserCreateUnknownRoleRejected
--- PASS: TestAdminUserDetailMissingIs404
--- PASS: TestAdminUserRolesChange
--- PASS: TestAdminUserRoundTripLoginRejection
--- PASS: TestAdminUserStatusChange
--- PASS: TestAdminUsersWriteWithoutPermissionServerSide
--- PASS: TestArticleFormTermCheckboxes
--- PASS: TestArticlesEmbeddingVectorColumn
--- PASS: TestAssignTermsProductAndGating
--- PASS: TestAssignTermsRoundTripArticle
--- PASS: TestBuildWithComposesCodePlusDynamic
--- PASS: TestBuildWithIsDeterministic
--- PASS: TestBuildWithRejectsHostilePersistedDefinition
--- PASS: TestCanonicalSchemaWithoutRegistry
--- PASS: TestCompileDDLBothEngines
--- PASS: TestContentTypeHelper
--- PASS: TestContentTypesManagePermissionInCatalog
--- PASS: TestContentTypesManagePermissionSeeded
--- PASS: TestContentTypesRegistryInBuild
--- PASS: TestContentUpdatePermissionSeeded
--- PASS: TestCreateArticle
--- PASS: TestCreateArticleAPIKeyRejected
--- PASS: TestCreateArticleEmbeddingOmittedIsNull
--- PASS: TestCreateArticleWithEmbedding
--- PASS: TestCreateArticleWithMetadata
--- PASS: TestCreateContentTypeCreatesRealTable
--- PASS: TestCreateContentTypeDuplicateIs400
--- PASS: TestCreateContentTypeDuplicateName
--- PASS: TestCreateContentTypeHostileNamesAre400
--- PASS: TestCreateContentTypeIsAtomic
--- PASS: TestCreateContentTypeIsCreateOnly
--- PASS: TestCreateContentTypeRejectsHostileNames
--- PASS: TestCreateContentTypeWithoutPermissionIs403
--- PASS: TestCreateProduct
--- PASS: TestCreateProductAPIKeyRejected
--- PASS: TestCreateProductAcceptsJSONNumberPrice
--- PASS: TestCreateTermGating
--- PASS: TestCreateTermUnknownTaxonomyIs400
--- PASS: TestCreateUserAndVerify
--- PASS: TestDbPathFlag
--- PASS: TestDefinitionValidateRejectsBadFields
--- PASS: TestDeleteArticle
--- PASS: TestDeleteProduct
--- PASS: TestDeleteTermCascadesJunctionNotContent
--- PASS: TestDumpSchemaFailsLoudlyWhenDatabaseMissing
--- PASS: TestDumpSchemaIncludesDynamicType
--- PASS: TestDumpSchemaIncludesDynamicTypes
--- PASS: TestDumpSchemaOnPreContract13Database
--- PASS: TestDynamicSchemaRoundTripJSON
--- PASS: TestDynamicTableShapeMatchesCodeContentType
--- PASS: TestDynamicTypeCoexistsWithCodeContentTypes
--- PASS: TestDynamicTypeSurvivesRestartCycle
--- PASS: TestEmbeddingInvalidDimension
--- PASS: TestEmbeddingNonNumericComponent
--- PASS: TestEmbeddingNotArray
--- PASS: TestEnsureSchemaAddsOnlyMissingTable
--- PASS: TestEnsureSchemaIdempotent
--- PASS: TestExpectedTables
--- PASS: TestGetAPIKey
--- PASS: TestGetUser
--- PASS: TestHealth
--- PASS: TestIssueAndVerifyJWT
--- PASS: TestIssueJWTRejectsEmptySecret
--- PASS: TestListAPIKeysResolvesRoleName
--- PASS: TestListAndGetArticles
--- PASS: TestListAndGetContentTypes
--- PASS: TestListAndGetProducts
--- PASS: TestListAndGetTerms
--- PASS: TestListUsers
--- PASS: TestLoadAcceptsSecret
--- PASS: TestLoadRejectsAbsentSecret
--- PASS: TestLoadRejectsEmptySecret
--- PASS: TestLoginInvalidCredentials
--- PASS: TestLoginSuccess
--- PASS: TestMalformedIDIsNotFound
--- PASS: TestMintAndVerifyAPIKey
--- PASS: TestNewMuxRejectsEmptySecret
--- PASS: TestNotFound
--- PASS: TestProductDuplicateSKUIs400
--- PASS: TestProductFormTermCheckboxes
--- PASS: TestProductMissingFields
--- PASS: TestProductNotFoundAndMalformedID
--- PASS: TestProductPriceNotNumericIs400
--- PASS: TestProductRoundTrip
--- PASS: TestProductsTable
--- PASS: TestPublishArticle
--- PASS: TestReservedNamesDerivedFromBuild
--- PASS: TestRevokeAPIKeyByID
--- PASS: TestRevokedAPIKeyRejected
--- PASS: TestRoundTripExact
--- PASS: TestSchemaRoundTripJSON
--- PASS: TestSchemaValidates
--- PASS: TestSeedCatalogsIdempotent
--- PASS: TestSetUserRoles
--- PASS: TestTermDuplicateSlugSameTaxonomyIs400
--- PASS: TestTermHierarchyAndParentDeleteSetsNull
--- PASS: TestTermsManageAndTaxonomiesSeeded
--- PASS: TestUIExpiredJWTCookieRejected
--- PASS: TestUIForgedJWTCookieRejected
--- PASS: TestUIHomeInvalidCookieRedirects
--- PASS: TestUIHomeNoCookieRedirects
--- PASS: TestUIJSONRoutesUnaffected
--- PASS: TestUILoginInvalidGenericError
--- PASS: TestUILoginSuccessSetsCookie
--- PASS: TestUILogoutClearsCookie
--- PASS: TestUIRoundTrip
--- PASS: TestUIStaticAssetsEmbedded
--- PASS: TestUpdateAndDeleteTerm
--- PASS: TestUpdateArticle
--- PASS: TestUpdateArticleEmbedding
--- PASS: TestUpdateProduct
--- PASS: TestUpdateUserStatus
--- PASS: TestUpgradeFromPreContract13Database
--- PASS: TestValidateIdentifierAcceptsLegitimateNames
--- PASS: TestValidateIdentifierHostileBattery
--- PASS: TestVectorFormatConvergesWithCompat
--- PASS: TestVerifyCredentialsIdenticalMessage
--- PASS: TestVerifyCredentialsSuspendedRejected
--- PASS: TestWhoamiAPIKey
--- PASS: TestWhoamiGarbageToken
--- PASS: TestWhoamiJWT
--- PASS: TestWhoamiNoCredentials
--- PASS: TestWhoamiRevokedAPIKeyRejected
```

Además, `TestDynamicTypeCoexistsWithCodeContentTypes` prueba explícitamente la convivencia: con un
CPT dinámico ya creado, `POST /articles`, `POST /products`, `POST /terms` y
`PUT /articles/{id}/terms` siguen respondiendo exactamente igual.

```
=== RUN   TestDynamicTypeCoexistsWithCodeContentTypes
    COEXISTENCE OK: articles, products and terms all behave unchanged with a dynamic type present
--- PASS: TestDynamicTypeCoexistsWithCodeContentTypes (0.19s)
```

Y `TestUpgradeFromPreContract13Database` prueba el camino REAL de upgrade de producción: una base
construida con el esquema anterior (sin las tablas de registro) + una fila de usuario preexistente,
reiniciada con este binario → se agregan solo las dos tablas nuevas, la fila sobrevive, la metadata
queda completa, y sobre esa base se puede crear un CPT dinámico que a su vez sobrevive otro
reinicio.

---

## Archivos tocados

Nuevos:
- `internal/schema/identifier.go` — el validador T1 + reservados derivados.
- `internal/schema/dynamic.go` — modelo de definición, `DynamicTable`, `BuildWith`.
- `internal/store/contenttypes.go` — `CanonicalSchema`, `LoadDefinitions`, `CreateContentType`
  (atómico), `FromDB`.
- `internal/server/contenttypes.go` — handlers T3.
- `internal/schema/dynamic_test.go`, `internal/store/contenttypes_test.go`,
  `internal/server/server_contenttypes_test.go`, `cmd/librarian/main_test.go`.

Modificados:
- `internal/schema/schema.go` — `content_types` + `content_type_fields` en `Build()`, permiso
  `content_types.manage`, helpers `integerColumn`/`checkIn`.
- `internal/schema/dump.go` — `JSON()` → `JSONWith(defs)`.
- `internal/store/store.go` — `EnsureSchema` usa `CanonicalSchema`; `writeFullSchemaMetadata` acepta
  `*sql.DB` o `*sql.Tx`.
- `internal/server/server.go` — tres rutas nuevas + `schemaMu`.
- `cmd/librarian/main.go` — `--dump-schema` lee la DB y falla ruidosamente; flag `--db`.
- `internal/schema/schema_test.go` — `schema.JSON()` → `schema.JSONWith(nil)`.
- `internal/server/export_fixture_test.go` — `InferFeatures` sobre el esquema compuesto.
- `docs/OPERATIONS.md` — nuevo comportamiento de `--dump-schema`.

## Pendiente para el orquestador

- Verificación EN NAVEGADOR/HTTP y el DEPLOY con el protocolo de copia-real-de-producción de
  CONTRACT-11 (esta fase no agrega UI; CONTRACT-14/15 la agregan).
- El comando de export documentado cambió: `librarian --dump-schema <out.json> --db <ruta.db>`.
  El paso 2 de `docs/OPERATIONS.md` está actualizado; el runbook de deploy debe pasar `--db`.

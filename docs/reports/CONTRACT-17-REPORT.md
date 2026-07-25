# CONTRACT-17 — Prefijo en tablas dinámicas (cierra el hueco 3 de PENDIENTES.md)

Base: `dc85fa2` (CONTRACT-01..16 completos y desplegados). Árbol SIN commitear, como pide el
contrato: el orquestador commitea y despliega tras verificar.

**Resultado: LISTO.** Un tipo dinámico llamado `eventos` crea ahora la tabla real `cpt_eventos`.
El prefijo está vedado para las tablas de código (garantizado por test sobre `Build()`), así que
la colisión del hueco 3 pasó de "falla ruidosa que impide arrancar el servicio" a **imposible**.
Ninguna superficie pública cambió: `/content/{tipo}`, `/admin/content/{tipo}`, la API de
definiciones y la sidebar siguen usando el nombre que el admin eligió.

---

## Decisiones de diseño (con su porqué)

### 1. El prefijo exacto: `cpt_`

`schema.DynamicTablePrefix = "cpt_"` (`internal/schema/identifier.go`).

- **Vocabulario ya existente:** "CPT" es el término que usa `DEFINITION-CPT-DINAMICOS.md` en todo
  el proyecto. No introduce jerga nueva.
- **Corto:** 4 bytes del presupuesto de identificador. Cualquier prefijo más largo (`dynamic_`,
  `content_type_`) recortaría más el nombre que el admin puede elegir sin ganar nada.
- **Es un identificador legal:** matchea `[a-z][a-z0-9_]*`, así que el nombre prefijado pasa por
  el mismo validador que cualquier otro identificador, sin casos especiales en ninguna capa.
- **Un solo punto de derivación:** `schema.DynamicTableName(typeName)` es la ÚNICA concatenación
  del proyecto. `ContentTypeDefinition.TableName()` delega en ella, y todo lo demás
  (`DynamicTable`, `store.CreateContentType`, la capa CRUD genérica) llama a `TableName()`. No hay
  una segunda derivación que pueda quedar desincronizada.
- **Trade-off aceptado:** el prefijo queda grabado en las bases existentes. Cambiarlo más adelante
  sería una migración de datos, no un refactor. Se documenta explícitamente en el comentario de la
  constante ("nunca 'resuelvas' este test cambiando el prefijo").

### 2. `ReservedNames()`: se RELAJA la reserva de nombres de tablas de código para los NOMBRES DE TIPO

Antes, `ReservedNames()` derivaba de tres fuentes: internos de compat + columnas inyectadas por
`ContentType()` + **todas las tablas de `Build()`**. Se eliminó la tercera.

**Por qué se relaja:**

- Su ÚNICO propósito era evitar que un tipo dinámico pisara una tabla de código. Con el prefijo
  eso es estructuralmente imposible (`users` → `cpt_users`). Conservarla sería mantener una regla
  cuya justificación ya no existe — el tipo de deuda que este proyecto documenta en vez de
  arrastrar.
- Tenía un costo real y concreto: `articles`, `products`, `terms`, `media`, `comments` son
  exactamente los nombres que un admin elegiría para su propio tipo, y el rechazo le hablaba de un
  detalle de implementación que él no puede ver.
- El argumento a favor de conservarla (un tipo `users` conviviendo con `/users` confunde a un
  humano) es real pero **cosmético**, y es una decisión del admin: son dos superficies distintas
  (`/content/users` vs `/users`) y dos tablas distintas (`cpt_users` vs `users`). Una
  imposibilidad estructural le gana a un tabú de nombres.

**Lo que NO cambia:** las columnas que inyecta `ContentType()` (`id`, `author_id`, `created_at`,
`updated_at`, `metadata`) siguen reservadas — esa reserva sí conserva su motivo (un CAMPO con ese
nombre produciría un error opaco de columna duplicada al aplicar el esquema, en vez de un 400
limpio). Los cuatro internos de compat también siguen reservados.

Testeado en los dos sentidos: `TestReservedNamesDerivedFromContentType` falla si alguien vuelve a
meter las tablas de código en el set (fuerza a re-argumentar la decisión en vez de revertirla en
silencio), y `TestTypeNamedLikeCodeTableDoesNotCollide` prueba que un tipo `users`/`articles`/
`products`/`content_types` produce una tabla prefijada que convive con la de código y que el
esquema compuesto sigue validando y compilando para PostgreSQL.

### 3. El largo máximo (32) aplica a la tabla YA PREFIJADA, no al nombre del tipo

`MaxTypeNameLength = MaxIdentifierLength - len(DynamicTablePrefix)` = **28**.

Toda la justificación del 32 es *aguas abajo*: el `NAMEDATALEN`=63 de PostgreSQL y los nombres que
**compat deriva de la tabla** (`__compat_capture_<tabla>_<kind>` = 24 + len(tabla)). Esas
derivaciones se aplican a la tabla REAL, que ahora lleva el prefijo. Si el límite se aplicara al
nombre del tipo, un tipo de 32 caracteres produciría una tabla de 36 y rompería exactamente el
invariante que la constante existe para proteger. Los nombres de CAMPO no llevan prefijo y
conservan los 32 completos.

Consecuencia visible: un nombre de tipo de 29..32 caracteres, que antes era válido, ahora se
rechaza con un mensaje que explica el porqué y muestra la tabla que habría generado.

### 4. Un tipo que EMPIEZA con el prefijo (`cpt_algo`) se permite

`name → prefix+name` es inyectiva: `cpt_algo` produce `cpt_cpt_algo` y `algo` produce `cpt_algo`;
nunca chocan. Y la colisión que este contrato elimina es entre tablas dinámicas y de CÓDIGO — la
unicidad entre dinámicas ya la decide el `UNIQUE(name)` de `content_types`. Prohibirlo sería una
regla sin ninguna falla que prevenir. Testeado (`TestTypeNameStartingWithThePrefixIsAllowed`).

---

## Qué se implementó, por tarea

### T1 — El prefijo en la capa de datos

- `internal/schema/identifier.go`: `DynamicTablePrefix`, `MaxTypeNameLength`, `DynamicTableName()`
  (el único lugar donde se compone el nombre) y `ValidateTypeName()` (el gate del nombre público =
  `ValidateIdentifier` + presupuesto de largo).
- `internal/schema/dynamic.go`: `ContentTypeDefinition.TableName()` y `DynamicTable()` pasando
  `d.TableName()` a `ContentType()`. **La firma de `ContentType()` no se tocó**: simplemente
  recibe el nombre ya prefijado. `Validate()` ahora usa `ValidateTypeName` para el nombre del tipo
  y sigue usando `ValidateIdentifier` para los campos.
- `internal/store/contenttypes.go`: el paso de diff de `CreateContentType` compara contra
  `def.TableName()` (antes `def.Name`), y el mensaje de error nombra las dos cosas.

### T2 — Enforcement y validador

- `TestNoCodeTableUsesDynamicPrefix` (`internal/schema/dynamic_test.go`): recorre
  `schema.Build().Tables` y falla si alguna empieza con el prefijo, con un mensaje que explica la
  consecuencia (esquema compuesto con tabla duplicada → `Schema.Validate()` falla → **el servicio
  no arranca**) y el arreglo correcto (renombrar la tabla de código, nunca cambiar el prefijo).
  Como las tablas de código son literales de Go y no pasan por ningún validador en runtime, este
  test ES la garantía; sin él, el prefijo sería una convención.
- `ReservedNames()` relajado según la decisión 2, con tests en ambos sentidos.
- `TestTypeNameBudgetAccountsForThePrefix` fija la decisión 3 (incluye que la tabla del nombre más
  largo legal mide exactamente 32).

### T3 — El nombre público no cambia

Revisión sitio por sitio de todo lo que podía usar el nombre como tabla o filtrar el prefijo hacia
arriba:

- `internal/server/content.go` (CRUD genérico de CONTRACT-14): los **cuatro** sitios que armaban
  el nombre de tabla (`selectClause`, `insertContentRow`, `updateContentRow`, `deleteContentRow`)
  pasan por el nuevo `quoteTable(def)` → `quoteIdentifier(def.TableName())`. Los nombres de CAMPO
  siguen yendo por `quoteIdentifier` tal cual. El segmento `{type}` de la URL sigue usándose solo
  como valor ligado para resolver la definición.
- `internal/server/contenttypes.go` (API de definiciones), `ui_content.go`, `ui_contenttypes.go`,
  `ui_nav.go`: **no requirieron cambios** — ya usaban `def.Name` únicamente como nombre público
  (títulos, rutas, etiquetas de la sidebar) y delegaban todo el acceso a datos en los helpers de
  `content.go`. Eso es lo que hace que el cambio no sea disruptivo, y está verificado con
  aserciones que escanean cada byte de respuesta buscando el prefijo.
- El registro (`content_types.name`) sigue guardando el nombre PÚBLICO; el prefijo se deriva, no
  se persiste.
- Guardián de CONTRACT-15 respetado: no se tocó ninguna plantilla ni handler de UI, no se
  introdujo ningún literal `pageData{` (el test guardián sigue verde).

### T4 — Verificación

Ver la sección siguiente: salida real de cada criterio.

---

## Verificación — salida REAL

### `go build ./...` y `go vet ./...`

```
$ go build ./... && go vet ./... && echo "BUILD+VET CLEAN"
BUILD+VET CLEAN
```

### `go test ./... -count=1`, dos veces

```
=== RUN 1 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.325s
ok  	github.com/MauricioPerera/librarian/internal/auth	3.996s
ok  	github.com/MauricioPerera/librarian/internal/config	0.672s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.323s
ok  	github.com/MauricioPerera/librarian/internal/server	40.303s
ok  	github.com/MauricioPerera/librarian/internal/store	2.379s
=== RUN 2 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.181s
ok  	github.com/MauricioPerera/librarian/internal/auth	3.846s
ok  	github.com/MauricioPerera/librarian/internal/config	0.608s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.253s
ok  	github.com/MauricioPerera/librarian/internal/server	38.953s
ok  	github.com/MauricioPerera/librarian/internal/store	2.285s
```

### T2 — enforcement y decisión sobre `ReservedNames()`

```
$ go test ./internal/schema/ -count=1 -v -run 'Prefix|TypeName|Reserved|DynamicTableIsPrefixed|CodeTable'
=== RUN   TestReservedNamesDerivedFromContentType
    dynamic_test.go:114: reserved names (9): [__compat_applied_changes __compat_capture_state __compat_change_journal __compat_schema author_id created_at id metadata updated_at]
--- PASS: TestReservedNamesDerivedFromContentType (0.00s)
=== RUN   TestNoCodeTableUsesDynamicPrefix
    dynamic_test.go:139: ENFORCEMENT OK: none of the 14 code tables uses the "cpt_" prefix
--- PASS: TestNoCodeTableUsesDynamicPrefix (0.00s)
=== RUN   TestDynamicTableIsPrefixed
    dynamic_test.go:159: PREFIX OK: public name "eventos" -> real table "cpt_eventos"
--- PASS: TestDynamicTableIsPrefixed (0.00s)
=== RUN   TestTypeNamedLikeCodeTableDoesNotCollide
    dynamic_test.go:194: NO COLLISION: code table "users" and dynamic table "cpt_users" coexist
    dynamic_test.go:194: NO COLLISION: code table "articles" and dynamic table "cpt_articles" coexist
    dynamic_test.go:194: NO COLLISION: code table "products" and dynamic table "cpt_products" coexist
    dynamic_test.go:194: NO COLLISION: code table "content_types" and dynamic table "cpt_content_types" coexist
--- PASS: TestTypeNamedLikeCodeTableDoesNotCollide (0.00s)
=== RUN   TestTypeNameBudgetAccountsForThePrefix
    dynamic_test.go:218: REJECTED type name of length 29 -> name "aaaaaaaaaaaaaaaaaaaaaaaaaaaaa" is invalid: longer than 28 characters (a content type name is limited to 28 so its real table "cpt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaa" fits the 32-character identifier budget)
--- PASS: TestTypeNameBudgetAccountsForThePrefix (0.00s)
=== RUN   TestTypeNameStartingWithThePrefixIsAllowed
    dynamic_test.go:247: INJECTIVE OK: types "cpt_algo" and "algo" -> tables "cpt_cpt_algo" and "cpt_algo"
--- PASS: TestTypeNameStartingWithThePrefixIsAllowed (0.00s)
ok  	github.com/MauricioPerera/librarian/internal/schema	1.437s
```

### T1/T3/T4 — tests de aceptación nuevos (HTTP real, cookie real)

`internal/server/server_contract17_test.go`:

```
$ go test ./internal/server/ -count=1 -run Contract17 -v
=== RUN   TestContract17JSONRoundTripUsesThePublicName
    server_contract17_test.go:193: T3 OK: full round-trip on /content/eventos; data in cpt_eventos; no response ever mentioned "cpt_"
--- PASS: TestContract17JSONRoundTripUsesThePublicName (0.18s)
=== RUN   TestContract17TypeNamedLikeCodeTableIsHarmless
    server_contract17_test.go:253: T4 OK: dynamic type 'users' -> table cpt_users (1 row); real users table intact (1 rows) and login still 200
--- PASS: TestContract17TypeNamedLikeCodeTableIsHarmless (0.20s)
=== RUN   TestContract17AdminUIUsesThePublicName
    server_contract17_test.go:320: T3 UI OK: sidebar + listing + forms use "eventos"; real table is cpt_eventos; no rendered byte contains "cpt_"
--- PASS: TestContract17AdminUIUsesThePublicName (0.18s)
=== RUN   TestContract17EnsureSchemaIsIdempotentWithPrefixedTables
    server_contract17_test.go:353: RESTART OK: EnsureSchema twice after creating 'boletines'; cpt_boletines intact, 16 tables unchanged
--- PASS: TestContract17EnsureSchemaIsIdempotentWithPrefixedTables (0.15s)
ok  	github.com/MauricioPerera/librarian/internal/server	4.530s
```

El test de UI usa `openUITLS` (`httptest.NewTLSServer`) + `loginUI`, o sea cookie de sesión REAL:
una cookie `Secure` se descarta sobre HTTP plano.

### T4 — Verificación END-TO-END con el binario real

Se compiló `librarian.exe`, se sembró una base con un admin real y se levantó el servidor real
(`LIBRARIAN_ADDR=127.0.0.1:8917`). Todo lo de abajo es `curl` contra ese proceso.

**Crear el tipo por la API real y round-trip completo, SIEMPRE con el nombre sin prefijo:**

```
== POST /content-types (nombre público 'eventos')
{"name":"eventos","fields":[{"name":"titulo","type":"text"},{"name":"asistentes","type":"integer"}]}
== GET /content-types
{"content_types":[{"name":"eventos","fields":[{"name":"titulo","type":"text"},{"name":"asistentes","type":"integer"}]}]}
== POST /content/eventos
{"asistentes":120,"author_id":"3c30b8e8-...","created_at":"2026-07-25 05:33:12","id":"d8e709f7-e2af-4847-b368-d7104a9105e3","metadata":null,"titulo":"Feria del libro","updated_at":"2026-07-25 05:33:12"}
== GET /content/eventos  (listar)
{"items":[{"asistentes":120,...,"titulo":"Feria del libro",...}],"type":"eventos"}
== GET /content/eventos/{id}  (leer)
{"asistentes":120,...,"titulo":"Feria del libro",...}
== PUT /content/eventos/{id}  (editar)
{"asistentes":300,...,"titulo":"Feria del libro 2027","updated_at":"2026-07-25 05:33:26"}
== DELETE /content/eventos/{id}
status=204
== GET /content/eventos tras borrar
{"items":[],"type":"eventos"}
== el nombre prefijado NO es una ruta pública
GET /content/cpt_eventos -> 404
GET /content-types/cpt_eventos -> 404
```

**Consulta directa a la base: la tabla REAL lleva el prefijo y la pelada NO existe.**

```
TABLE __compat_schema
TABLE api_keys
TABLE article_terms
TABLE articles
TABLE content_type_fields
TABLE content_types
TABLE cpt_eventos          <-- la tabla real del tipo 'eventos'
TABLE cpt_users            <-- la tabla real del tipo 'users'
TABLE permissions
TABLE product_terms
TABLE products
TABLE role_permissions
TABLE roles
TABLE taxonomies
TABLE terms
TABLE user_roles
TABLE users                <-- la tabla de CÓDIGO, intacta

== filas reales ==
SELECT titulo, asistentes FROM cpt_eventos  -> ROW [Feria del libro 2027 300]
SELECT apodo FROM cpt_users                 -> ROW [el bibliotecario]
SELECT email FROM users                     -> ROW [admin@example.com]
SELECT count(*) FROM eventos                -> QUERY ERROR: SQL logic error: no such table: eventos (1)
```

**Tipo con el nombre de una tabla de código (`users`) — creado, con una fila, y ni la tabla
`users` real ni el login se ven afectados:**

```
POST /content-types {"name":"users",...}  -> {"name":"users","fields":[{"name":"apodo","type":"text"}]}
POST /content/users {"apodo":"el bibliotecario"} -> 201, id 009e19b8-...
SELECT email FROM users -> ROW [admin@example.com]        (la tabla de código, intacta)
POST /auth/login (admin@example.com)      -> login status=200
```

**`--dump-schema` incluye la tabla prefijada (o sea, el export a Postgres la lleva):**

```
$ librarian.exe --dump-schema --db c17.db
TABLAS EN EL DUMP: ['users', 'roles', 'permissions', 'role_permissions', 'user_roles', 'api_keys',
 'articles', 'products', 'taxonomies', 'terms', 'article_terms', 'product_terms', 'content_types',
 'content_type_fields', 'cpt_eventos', 'cpt_users']
cpt_eventos presente: True
cpt_users presente: True
eventos pelado presente: False
```

Y el test del binario (`cmd/librarian`) se endureció para exigir la tabla prefijada y prohibir la
pelada:

```
main_test.go:113: DUMP OK: 15 tables, dynamic 'reviews' included, compiles for PostgreSQL.
tables=[users roles permissions role_permissions user_roles api_keys articles products taxonomies
terms article_terms product_terms content_types content_type_fields cpt_reviews]
```

**Ciclo de reinicio (dos capas, como CONTRACT-11/13):** crear el tipo, correr `EnsureSchema` de
nuevo (dos veces), y reiniciar el binario real:

```
== EnsureSchema de nuevo (ciclo de reinicio) ==
ENSURE OK, definitions: 2
  public="eventos" table="cpt_eventos"
  public="users" table="cpt_users"
ENSURE OK, definitions: 2
  public="eventos" table="cpt_eventos"
  public="users" table="cpt_users"
SELECT count(*) FROM cpt_users -> ROW [1]      (la fila sobrevivió)
TABLE cpt_eventos
TABLE cpt_users

== tras reinicio real del binario (puerto 8918) ==
librarian: schema ready on .../c17.db, listening on 127.0.0.1:8918
GET /content-types -> {"content_types":[{"name":"eventos",...},{"name":"users",...}]}
GET /content/users -> {"items":[{"apodo":"el bibliotecario",...}],"type":"users"}
GET /articles      -> {"articles":[]}
```

No falla, la tabla prefijada sigue ahí con sus datos, y las definiciones se recargan con el nombre
PÚBLICO.

### Contratos anteriores: confirmación explícita

Todo lo de CONTRACT-01..16 sigue funcionando igual:

- La suite completa (6 paquetes, dos corridas) está verde, incluidos todos los tests de auth,
  API keys, artículos, productos, términos, roles, permisos y toda la UI de admin.
- El guardián de CONTRACT-15 (`h.page(r, title)`, nunca un literal `pageData{`) sigue verde: no se
  tocó ninguna plantilla ni handler de UI.
- Login, `/articles`, `/admin/*` verificados también contra el binario real después del reinicio.
- Ninguna ruta pública cambió: `/content/{tipo}` y `/admin/content/{tipo}` siguen usando el nombre
  SIN prefijo, y ninguna respuesta (JSON o HTML) contiene la cadena `cpt_` — hay aserciones
  automáticas que escanean cada byte de respuesta para eso.
- Sin dependencias nuevas, sin permisos nuevos, sin cambios en `sqlite-postgres-compat`.

---

## Cambios de comportamiento que el orquestador debe conocer

1. **Migración manual pendiente (asignada al orquestador, como dice el contrato).** Producción
   tiene el tipo `eventos` con su tabla SIN prefijo. El código quedó con **un solo camino**: no
   hay compatibilidad con tablas dinámicas sin prefijo ni migración automática. Al desplegar hay
   que renombrar la tabla (`ALTER TABLE eventos RENAME TO cpt_eventos`) y reescribir el metadato
   `__compat_schema` — lo que hace `EnsureSchema` al arrancar si la tabla ya está renombrada. Con
   backup previo. Si NO se hace, el arranque verá `cpt_eventos` como tabla faltante y la creará
   vacía, dejando la vieja `eventos` huérfana con sus datos.
2. **Nombres de tipo de 29 a 32 caracteres pasan a ser inválidos** (decisión 3). Ninguno existe
   hoy en producción.
3. **Un tipo puede llamarse como una tabla de código** (`users`, `articles`, …) — decisión 2. Es
   una relajación, no una restricción: nada que ya funcionara deja de funcionar.

## Archivos tocados

Producción:

- `internal/schema/identifier.go` — prefijo, presupuesto de largo, `DynamicTableName`,
  `ValidateTypeName`, `ReservedNames` relajado.
- `internal/schema/dynamic.go` — `TableName()`, `DynamicTable` prefijando, `Validate` usando
  `ValidateTypeName`.
- `internal/store/contenttypes.go` — el diff compara contra la tabla prefijada.
- `internal/server/content.go` — `quoteTable()` y sus 4 usos.

Tests:

- `internal/server/server_contract17_test.go` (**nuevo**), `internal/schema/dynamic_test.go`,
  `internal/store/contenttypes_test.go`, `internal/server/server_content_test.go`,
  `internal/server/server_contenttypes_test.go`, `internal/server/server_ui_content_test.go`,
  `cmd/librarian/main_test.go`.

Documentación:

- `docs/PENDIENTES.md` (hueco 3 → RESUELTO), `DEFINITION-CPT-DINAMICOS.md` (espacio de nombres de
  tablas), este reporte.

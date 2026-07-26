# CONTRACT-23 — La capacidad vectorial, opcional

Base: `b62afe0` (CONTRACT-22 en `main`). Árbol **SIN commitear**, como pide el contrato.

**Resultado: LISTO. `librarian` arranca y sirve sobre un PostgreSQL 17 que no tiene `pgvector` —
y en el que la extensión ni siquiera se puede instalar.** El binario real, sobre una base limpia:
creó el esquema, hizo el bootstrap de la primera identidad, autenticó por HTTP y corrió el CRUD
completo de artículos. Transcripción abajo. Eso era imposible antes de este contrato, y el mismo
binario contra el mismo servidor con la capacidad habilitada **sigue fallando** con el mensaje de
CONTRACT-21, en la misma corrida, para que la diferencia sea atribuible a la capacidad y a nada más.

La elección es **IRREVERSIBLE después del primer arranque** y eso está fijado: cambiarla sobre una
instalación ya creada no arranca, en las dos direcciones, en los dos motores, con un mensaje que
dice qué pasó y qué se puede hacer. La incoherencia se detecta **contra la BASE** (la columna
física en el catálogo del motor), y hay un test que lo prueba envenenando `__compat_schema` con el
esquema de la instalación contraria: la sonda sigue contestando la verdad.

Con la capacidad **habilitada** no cambió nada: `git diff --stat -- '*_test.go'` está **vacío** —
ni un solo test preexistente tocado — y las tres baterías dual-motor de CONTRACT-19/20/21 más la
tagged `exportfixture` pasan contra un PostgreSQL 17 con `pgvector`.

`sqlite-postgres-compat` **no se tocó**: su `git status` quedó exactamente como estaba.
Ninguna dependencia nueva, ningún permiso nuevo, ningún cambio en el contrato público de las rutas
HTTP cuando la capacidad está habilitada. **La canonicalización del texto del vector no se tocó**:
en `internal/server/vector.go` no cambió ni una línea de `FormatVector`, `ParseVector`,
`formatVectorComponent` ni `validateEmbedding` — lo único que se agregó a ese archivo es la negativa
de T2. `TestVectorFormatConvergesWithCompat` (round-trip real contra `compat` sobre SQLite) sigue
verde en la suite por defecto.

---

## Resumen de qué se tocó

| Archivo | Qué |
|---|---|
| `internal/schema/capabilities.go` (**nuevo**) | **T1**: el tipo `Capabilities` y por qué su valor cero es el statu quo |
| `internal/schema/schema.go` | **T1**: `BuildFor(caps)`; `articleOwnColumns(caps)`; `Build()` = `BuildFor(Capabilities{})` |
| `internal/schema/server_dual.go` | **T1**: `articleColumns(caps)` y `serverRoutines(caps)` — la rutina `server_article_embedding` deja de declararse |
| `internal/schema/dynamic.go` | **T1**: `BuildWithFor(caps, defs)` |
| `internal/schema/dump.go` | **T4**: `JSONWithFor(caps, defs)` — `--dump-schema` refleja la elección |
| `internal/config/config.go` | **T1**: `LIBRARIAN_VECTOR` + `ResolveCapabilities` + `Config.Capabilities` |
| `internal/store/store.go` | **T3**: `EnsureSchemaFor`, `InstalledCapabilities`, `columnExists`, la guarda de coherencia; `requireVectorType` pasa a ser condicional |
| `internal/store/contenttypes.go` | **T3**: `CanonicalSchemaFor`; `CanonicalSchema` deriva de la BASE; `CreateContentType` compone para la instalación real |
| `internal/store/contenttypes_edit.go` | ídem para `EditContentType` |
| `internal/server/server.go` | `Deps.Capabilities` → `handlers.caps` |
| `internal/server/dual.go` | `h.codeSchema()`: de qué esquema se compilan las rutinas de esta instalación |
| `internal/server/vector.go` | **T2**: `refuseEmbeddingIfDisabled` + el mensaje. **La canonicalización no se tocó** |
| `internal/server/articles.go` | **T2**: la negativa en POST y PUT; la lectura consulta si la columna fue DECLARADA |
| `internal/server/content.go` | `dynamicSchema` pasa a método para componer con las capacidades de la instancia |
| `cmd/librarian/main.go` | arranque, `--bootstrap` y `--dump-schema` por capacidad; la línea de arranque la dice |
| `docs/OPERATIONS.md`, `docs/PENDIENTES.md` | la variable, la irreversibilidad, y el hueco 5 cerrado |

**Tests nuevos** (5 archivos, ninguno preexistente modificado):
`internal/schema/vector_optional_contract23_test.go`, `internal/config/capabilities_contract23_test.go`,
`internal/store/vector_optional_contract23_test.go`, `internal/server/vector_optional_contract23_test.go`,
`internal/server/dualengine_contract23_test.go` (tag `dualengine`).

---

## T1 — La declaración condicional: la forma, y el valor por defecto

**Decisión: una variable explícita `LIBRARIAN_VECTOR`, con la MISMA forma que `LIBRARIAN_ENGINE` de
CONTRACT-21** — valores nombrados, un default documentado, y un valor no reconocido que es un error
de arranque en vez de un silencio.

| Variable | Valores | Qué hace |
|---|---|---|
| `LIBRARIAN_VECTOR` | `enabled` (por defecto; también `on`/`true`/`1`), `disabled` (también `off`/`false`/`0`) | declara si esta instalación tiene `articles.embedding vector(1536)` |

No se infiere de nada — ni de si `pgvector` está disponible, ni de si alguna fila trae un embedding.
Una capacidad inferida del entorno cambia con el entorno, y esta es irreversible.

### Por qué el default es ENABLED

Los dos candidatos **no son simétricos**:

- **Default ENABLED es lo que TODA instalación desplegada ya tiene**: `articles.embedding` existe en
  todas. Actualizar el binario no cambia nada, que es la única propiedad bajo la cual una instancia
  existente reinicia igual. Con el default contrario, cada instancia desplegada arrancaría
  declarando un esquema SIN una columna que su tabla sí tiene, y la guarda de T3 —correctamente—
  se negaría a arrancar **todo el parque** en el primer reinicio después de la actualización. Un
  flag de capacidad se habría convertido en una caída.
- **Default DISABLED** optimizaría para la instalación liviana nueva, que es justamente la que **ya
  está tomando una decisión explícita de despliegue** (eligió un PostgreSQL sin `pgvector`). Que lo
  diga en voz alta cuesta una variable; que lo sufra quien no decidió nada cuesta un incidente.

La regla general que sigue: **lo que un llamador obtiene por no decir nada tiene que ser lo que
preserva lo que ya existe.** Quitar una columna no es algo que una instalación deba recibir por
omisión.

Esa misma regla está codificada en el TIPO, no solo en la variable. `schema.Capabilities` tiene un
único campo y está escrito **en negativo**:

```go
type Capabilities struct{ VectorDisabled bool }
```

Así, `Capabilities{}` —lo que produce cada call site preexistente, cada test y cada parámetro
olvidado— es EXACTAMENTE el esquema que ya corre. Una struct de capacidades cuyo valor cero
*quitara* una columna convertiría cada call site no actualizado en una divergencia de esquema, que
es el fallo que este contrato existe para prevenir, no para introducir. (Y si aun así alguien no
propaga la declaración, la guarda de T3 lo detiene ruidosamente: es el respaldo exacto de ese error.)

### Qué desaparece cuando no se declara

No alcanza con sacar la columna: una rutina de lectura DECLARA sus columnas de salida y compat las
compila en la lista del SELECT, así que declarar una columna que la tabla no tiene no es un
desprolijo cosmético, es una sentencia que falla en cada lectura. Desaparecen las tres cosas:

```
$ go test ./internal/schema -run 'Disabled|Enabled|JSONWithFor|InferFeatures' -v
=== RUN   TestEnabledIsByteIdenticalToBeforeTheContract
    enabled schema bytes=26267, declares the vector family: yes
=== RUN   TestDisabledDeclaresNoVectorAnywhere
    articles enabled=[id author_id title body published_at embedding created_at updated_at metadata]
    articles disabled=[id author_id title body published_at created_at updated_at metadata]
=== RUN   TestDisabledCompilesPostgresDDLWithoutTheVectorType
    enabled: 22 postgres DDL statements, 1 mention vector(
    disabled: 22 postgres DDL statements, 0 mention vector(
=== RUN   TestJSONWithForReflectsTheChoice
    dump enabled bytes=65911 (embedding: yes), disabled bytes=64406 (embedding: no)
=== RUN   TestInferFeaturesDropsTheVectorFamily
    features enabled : canonical_routines json canonical_foreign_keys tables uuid primary_keys canonical_check_constraints canonical_vectors canonical_full_text canonical_views
    features disabled: canonical_check_constraints canonical_routines json canonical_foreign_keys tables canonical_full_text canonical_views uuid primary_keys
PASS
```

`TestEnabledIsByteIdenticalToBeforeTheContract` es el criterio "con la capacidad habilitada nada
cambia" comprobado como comparación de BYTES del esquema serializado, que cubre tablas, columnas,
vistas y rutinas de una sola vez.

---

## T3 — La guarda de coherencia, y por qué mira la BASE

`EnsureSchema` crea solo las tablas FALTANTES y jamás altera una existente. De ahí sale toda la
restricción central: sobre una instalación cuyo `articles` ya existe, cambiar la declaración no
cambia **nada físico** — habilitarla no agrega la columna, deshabilitarla no la quita. Lo que sí
cambia es el esquema que el proceso CREE tener, y esa creencia no es decorativa: es de donde compat
compila las rutinas de lectura, lo que `EnsureSchema` escribe en `__compat_schema`, y lo que
`--dump-schema` le entrega a `compat copy` como `schema_ref` de un export. Negarse a arrancar es el
único desenlace que deja el fallo donde se puede entender.

`store.InstalledCapabilities` pregunta por la **columna física**: `compat.Store.TableExists` para la
tabla, y el catálogo propio de cada motor para la columna (`information_schema.columns` restringido
a `current_schema()` en PostgreSQL — el mismo alcance que usa `TableExists` y donde aterriza el
`CREATE TABLE` sin calificar de este código; `pragma_table_info` en SQLite). `installed=false` (la
tabla no existe) es el único estado en que la declaración decide algo.

**No se confía en `__compat_schema`**, y eso está probado, no afirmado:

```
$ go test ./internal/store -run 'GuardReads' -v
=== RUN   TestTheGuardReadsTheBaseNotTheMetadata
    metadata says vector enabled, the physical table says disabled — the probe answers disabled
    guard still REFUSES: this installation was created WITHOUT the vector capability ...
--- PASS
```

El test **envenena** la fila `canonical_schema` con el esquema de la instalación contraria. Esa fila
la escribe el mismo camino cuya creencia está en duda, y `InspectSchema` de compat la PREFIERE por
sobre el catálogo vivo — así que una sonda que le creyera coincidiría con cualquier error que
alguna vez hubiera arrancado, que es exactamente cuando la guarda tiene que disentir.

Las dos direcciones, con salida real de la guarda (`go test ./internal/store -run Irreversible -v`):

```
=== RUN   TestTheChoiceIsIrreversible/created_WITH,_now_declared_without
    REFUSED as required: this installation was created WITH the vector capability (its articles
    table has the "embedding" column) but the configuration now declares it DISABLED
    (LIBRARIAN_VECTOR=disabled): refusing to start. The choice is made at the first boot and is
    IRREVERSIBLE — the schema is applied by creating MISSING tables, never by altering an existing
    one, so disabling it now would NOT drop the column; the service would run believing in a schema
    its own tables do not match, which breaks the export and the writes silently. Set
    LIBRARIAN_VECTOR=enabled to keep serving this installation, or create a NEW installation with
    the capability disabled and move the data into it with `compat copy`
    table untouched: [id author_id title body published_at embedding created_at updated_at metadata]
=== RUN   TestTheChoiceIsIrreversible/created_WITHOUT,_now_declared_with
    REFUSED as required: this installation was created WITHOUT the vector capability (its articles
    table has no "embedding" column) but the configuration now declares it ENABLED
    (LIBRARIAN_VECTOR=enabled): refusing to start. ... Set LIBRARIAN_VECTOR=disabled to keep serving
    this installation, or create a NEW installation with the capability enabled and move the data
    into it with `compat copy`
    table untouched: [id author_id title body published_at created_at updated_at metadata]
```

`table untouched` no es decoración: la corrida rechazada **no escribió nada**.

### El agujero que la guarda tapa y no es obvio: los tipos de contenido dinámicos

`CreateContentType` y `EditContentType` reescriben `__compat_schema` con el esquema compuesto. Si lo
compusieran con las capacidades por DEFECTO, una instalación sin la capacidad terminaría con una
fila de metadata que declara una columna que su tabla no tiene — sin que nada fallara en el momento.
Por eso `CanonicalSchema` (y esas dos escrituras) derivan las capacidades **de la tabla física**, no
de un parámetro que alguien pueda olvidar:

```
=== RUN   TestContentTypesComposeForTheInstalledChoice
    metadata after CreateContentType: 27635 bytes, embedding declared: no, cpt_eventos declared: yes
    restart after CreateContentType: clean
```

---

## T2 — `embedding` se rechaza, no se ignora

Un pedido que trae `embedding` sobre una instalación sin la capacidad recibe **400** con una
explicación. Se rechaza **cualquier valor presente, incluido el `null` explícito**: un `null`
significa "borrá el embedding", que es una afirmación sobre una columna que no existe, y contestarle
200 sería la misma mentira silenciosa que aceptar un array. Un campo **ausente** no es una
afirmación sobre el embedding, así que pasa — y eso es lo que mantiene funcionando sin cambios a
todo cliente que nunca usó la capacidad.

```
$ go test ./internal/server -run 'RefusedNotIgnored|WithoutTheVectorCapability|AbsentEmbedding|StillWorksWhenEnabled' -v
=== RUN   TestArticlesCRUDWithoutTheVectorCapability
    POST /articles -> 201 id issued: true
    GET /articles/{id} -> 200 title=Sin vector, embedding field present: false
    GET list / PUT / publish / DELETE / DELETE again -> 200 200 200 204 404
=== RUN   TestEmbeddingIsRefusedNotIgnored
    POST and PUT /articles with a full-dimension array -> 400, explained
    POST and PUT /articles with a wrong-dimension array -> 400, explained
    POST and PUT /articles with an empty array -> 400, explained
    POST and PUT /articles with an explicit null -> 400, explained
    POST and PUT /articles with a non-array -> 400, explained
    after every refusal: rows=1, the row is untouched (title="base")
=== RUN   TestAbsentEmbeddingIsNotARefusal
    a request with NO embedding field -> 201, unchanged
=== RUN   TestEmbeddingStillWorksWhenEnabled
    capability enabled: 1536 components written and read back
PASS
```

El mensaje, literal, es el que se ve en la transcripción HTTP de más abajo.

Las **lecturas** simplemente no traen el campo (es `omitempty` y la rutina no declara la columna).
`articleFromRow` lo comprueba explícitamente (`if _, declared := row["embedding"]; declared && …`)
en vez de apoyarse en el valor cero de una entrada faltante del mapa: una columna ausente del
resultado y una columna presente y NULL son hechos distintos, y solo el segundo es un embedding
nulo.

---

## T4 — LA PRUEBA QUE CIERRA EL HUECO

### El destino: un PostgreSQL 17 donde `pgvector` no se puede ni instalar

```
$ pgtool "$PG_NOVEC" "SELECT version()" "SELECT to_regtype('vector') IS NOT NULL" \
         "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'"
PostgreSQL 17.10 on x86_64-pc-linux-musl, compiled by gcc (Alpine 15.2.0) 15.2.0, 64-bit
to_regtype('vector') IS NOT NULL = false
public tables = 0

$ pgtool "$PG_NOVEC" "CREATE EXTENSION IF NOT EXISTS vector"
ERR ERROR: extension "vector" is not available (SQLSTATE 0A000)

$ pgtool "$PG_VEC" "SELECT to_regtype('vector') IS NOT NULL"
true
```

`$PG_NOVEC = postgres://postgres:***@31.220.22.176:5446/postgres?sslmode=disable`
`$PG_VEC   = postgres://postgres:***@31.220.22.176:5445/postgres?sslmode=disable`

No es un destino simulado: la extensión **no está en el sistema**, así que ni un superusuario puede
crearla. Es el PostgreSQL administrado del hueco 5.

### A. El hueco, reproducido — con la capacidad por defecto, sigue fallando

```
$ LIBRARIAN_ENGINE=postgres LIBRARIAN_DB=<sin pgvector> LIBRARIAN_JWT_SECRET=… ./librarian.exe
librarian: the pgvector extension is required on PostgreSQL and its `vector` type is not resolvable
by this connection: librarian's canonical schema declares articles.embedding as vector(1536)
(CONTRACT-05), so the schema cannot be created without it. Run `CREATE EXTENSION IF NOT EXISTS
vector;` in the target database as a superuser, and if it is installed into a schema other than the
one this connection uses, make that schema visible on the connection's search_path
exit=1
```

### B. La misma base, la capacidad no declarada — arranca

```
$ LIBRARIAN_VECTOR=disabled ./librarian.exe --bootstrap --email admin@example.com < password
librarian: bootstrap complete on postgres://postgres:***@31.220.22.176:5446/postgres?sslmode=disable (postgres)
  identity created: admin@example.com (id 1df1cda8-7a44-4b77-8563-c27c0793365a), status active, role "administrator"
  role "administrator" now holds 8 permission(s): content.create, content.delete, content.publish, content.update, content_types.manage, roles.manage, terms.manage, users.manage
  roles editor/author/contributor were left with NO permissions on purpose; grant them from the admin UI
  this installation is now claimed and can never be bootstrapped again

$ LIBRARIAN_VECTOR=disabled ./librarian.exe
librarian: schema ready on postgres://postgres:***@31.220.22.176:5446/postgres?sslmode=disable (postgres, vector disabled), listening on 127.0.0.1:8123
```

La línea de arranque dice la elección (`vector disabled`), como dice el motor.

### LA TRANSCRIPCIÓN HTTP CONTRA EL POSTGRESQL SIN `pgvector`

Salida real de `curl`, sin editar:

```
== 1. health ==
GET /health -> 200 {"status":"ok"}
== 2. login (the bootstrapped admin) ==
POST /auth/login -> token issued: yes
GET /whoami -> {"auth":"jwt","email":"admin@example.com","roles":["administrator"],"user_id":"1df1cda8-7a44-4b77-8563-c27c0793365a"}
POST /auth/login wrong password -> {"error":"invalid credentials"}
== 3. article CRUD over HTTP, on a PostgreSQL WITHOUT pgvector ==
POST /articles -> {"author_id":"1df1cda8-…","body":"cuerpo","id":"42e4cb0c-7ebd-4516-9ffc-4b12aa4193bd","title":"nota sin pgvector"}
GET /articles/{id} -> {"id":"42e4cb0c-…","author_id":"1df1cda8-…","title":"nota sin pgvector","body":"cuerpo","created_at":"2026-07-26T01:42:41.4894107Z","updated_at":"2026-07-26T01:42:41.4894107Z"}
PUT /articles/{id} -> {"body":"cuerpo 2","id":"42e4cb0c-…","title":"nota editada"}
POST /articles/{id}/publish -> {"id":"42e4cb0c-…","published_at":"2026-07-26T01:42:42.6814141Z"}
GET /articles (list) -> {"articles":[{"id":"42e4cb0c-…","author_id":"1df1cda8-…","title":"nota editada","body":"cuerpo 2","published_at":"2026-07-26T01:42:42.6814141Z","created_at":"2026-07-26T01:42:41.4894107Z","updated_at":"2026-07-26T01:42:42.6814141Z"}]}
GET /articles/{unknown} -> {"error":"article not found"}
== 4. T2: an embedding is REFUSED, not ignored ==
POST /articles (1536 components) -> 400 {"error":"this installation does not have the vector capability: its articles table has no embedding column, so an embedding cannot be stored. The field is refused rather than ignored, because accepting it and discarding it would report a success that stored nothing. The choice is made when the installation is created (LIBRARIAN_VECTOR) and is irreversible; a new installation with the capability enabled is the only way to store embeddings"}
PUT /articles/{id} embedding:null -> 400 {"error":"this installation does not have the vector capability: … refused rather than ignored …"}
GET /articles after the refusals -> {"articles":[{"id":"42e4cb0c-…","title":"nota editada", …
== 5. content types and generic content, unaffected ==
POST /content-types -> {"name":"resenas","fields":[{"name":"titular","type":"text"},{"name":"puntaje","type":"integer"}]}
POST /content/resenas -> {"author_id":"1df1cda8-…","created_at":"2026-07-26T01:43:02.6064667Z","id":"8d5dd027-…","metadata":null,"puntaje":9,"titular":"primera","updated_at":"2026-07-26T01:43:02.6064667Z"}
GET /content/resenas -> {"items":[{"author_id":"1df1cda8-…","created_at":"2026-07-26T01:43:02.6064667Z","id":"8d5dd027-…","metadata":null,"puntaje":9,"titular":"primera","updated_at":"2026-07-26T01:43:02.6064667Z"}],"type":"resenas"}
DELETE /articles/{id} -> 204
DELETE /articles/{id} again -> 404
```

Punto por punto, lo que el contrato exige de esta prueba:

| Exigencia | Dónde |
|---|---|
| arrancar en limpio, capacidad deshabilitada, PostgreSQL SIN `pgvector` | `librarian: schema ready on … (postgres, vector disabled)` |
| bootstrap | `bootstrap complete … identity created … 8 permission(s)` |
| login | `POST /auth/login -> token issued: yes`, `GET /whoami -> 200`, y el 401 del password equivocado |
| CRUD de artículos por HTTP | `POST` 201 → `GET` → `PUT` → `publish` → `GET` lista → `DELETE` 204 → repetido 404 |
| de yapa: tipos dinámicos y contenido genérico | `POST /content-types`, `POST/GET /content/resenas` — una tabla real creada en ese PostgreSQL |

### El ciclo de reinicio y la guarda, con el binario real

```
== 6. RESTART with the SAME declaration (boot #2) ==
librarian: schema ready on postgres://postgres:***@31.220.22.176:5446/postgres?sslmode=disable (postgres, vector disabled), listening on 127.0.0.1:8123

== 7. RESTART with the declaration CHANGED — must refuse (T3) ==
librarian: this installation was created WITHOUT the vector capability (its articles table has no
"embedding" column) but the configuration now declares it ENABLED (LIBRARIAN_VECTOR=enabled):
refusing to start. … Set LIBRARIAN_VECTOR=disabled to keep serving this installation, or create a
NEW installation with the capability enabled and move the data into it with `compat copy`
exit=1

== 8. RESTART with LIBRARIAN_VECTOR UNSET (the default) — must refuse the same way ==
librarian: this installation was created WITHOUT the vector capability … exit=1
```

El caso 8 importa: **olvidarse de la variable no revierte nada en silencio**. El default es la
declaración habilitada, y sobre esta instalación eso es una contradicción, así que también se
detiene.

### Lo mismo sobre SQLite, y la dirección contraria de la guarda

```
== 9. SQLite, capability DISABLED: clean install + bootstrap ==
librarian: bootstrap complete on …/sq-off.db (sqlite)
  identity created: admin@example.com (id c061ca22-…), status active, role "administrator"
-- restart with the declaration flipped (created WITHOUT, now enabled) --
librarian: this installation was created WITHOUT the vector capability … exit=1

== 10. SQLite, capability ENABLED: the mirror direction of T3 ==
librarian: bootstrap complete on …/sq-on.db (sqlite)
librarian: this installation was created WITH the vector capability (its articles table has the
"embedding" column) but the configuration now declares it DISABLED (LIBRARIAN_VECTOR=disabled):
refusing to start. … exit=1
```

### `--dump-schema` refleja la elección

```
== 11. --dump-schema reflects the installation's choice ==
sqlite disabled   exit=0 bytes=64406 embedding-mentions=0
sqlite enabled    exit=0 bytes=65911 embedding-mentions=4
postgres disabled exit=0 bytes=69578 embedding-mentions=0
-- the dump ignores LIBRARIAN_VECTOR when the installation already decided --
exit=0 identical to the honest dump: YES
-- vector( in the postgres DDL of the disabled installation --
0 (no vector anywhere)
```

La última línea del bloque es la que importa de verdad y no es cosmética: el dump es el `schema_ref`
que consume `compat copy`, así que un dump que declarara `vector(1536)` **crearía esa columna en el
DESTINO** y le devolvería el requisito de `pgvector` a la instalación que existe justamente para no
tenerlo. Por eso el dump toma la elección de la **tabla física** cuando la instalación existe, y
solo cae en la declaración cuando todavía no existe — comprobado arriba: con
`LIBRARIAN_VECTOR=enabled` sobre una instalación deshabilitada, el dump sale **idéntico** al honesto.

### La batería dual-motor de este contrato

`internal/server/dualengine_contract23_test.go` (tag `dualengine`), contra los DOS PostgreSQL:

```
$ LIBRARIAN_PG_NO_VECTOR_DSN='postgres://postgres:***@31.220.22.176:5446/postgres?sslmode=disable' \
  COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5445/postgres?sslmode=disable' \
  go test -tags dualengine -run TestVectorOptional -count=1 -v ./internal/server

=== RUN   TestVectorOptionalPostgresWithoutPgvector
    target: PostgreSQL 17.10
    to_regtype('vector') IS NULL — pgvector is NOT available here
    capability ENABLED  -> REFUSED as before: the pgvector extension is required on PostgreSQL …
    capability DISABLED -> schema applied, twice, on a PostgreSQL WITHOUT pgvector
    InstalledCapabilities (read from information_schema, not from __compat_schema): vector disabled
    GET /health -> 200 map[status:ok]
    GET /whoami -> 200 roles=[administrator]
    POST /articles -> 201 id issued: true
    GET /articles/{id} -> 200 title=sin pgvector, embedding field: absent
    GET /articles -> 200
    PUT /articles/{id} -> 200 title=editado
    POST /articles/{id}/publish -> 200 published_at set: true
    POST /articles with an embedding -> 400 "this installation does not have the vector capability: …"
    PUT /articles/{id} with embedding:null -> 400 (refused, not ignored)
    DELETE /articles/{id} -> 204, again -> 404
    T3 (enable later)  -> REFUSED: this installation was created WITHOUT the vector capability …
    T3 (disable later) -> REFUSED: this installation was created WITH the vector capability …
--- PASS: TestVectorOptionalPostgresWithoutPgvector (29.68s)
```

La batería **verifica primero que el destino no puede resolver el tipo `vector`** y aborta si puede:
sin eso, toda la batería podría pasar contra un servidor que sí tiene la extensión, y no probaría
nada. Y arranca reproduciendo el hueco en la MISMA corrida, así que la diferencia entre las dos
mitades es atribuible a la capacidad y a nada más.

### Las baterías preexistentes, con `pgvector`, verdes

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5445/postgres?sslmode=disable' …
=== C19 === ok  github.com/MauricioPerera/librarian/internal/auth    48.617s
=== C20 === ok  github.com/MauricioPerera/librarian/internal/server  67.308s
=== C21 === ok  github.com/MauricioPerera/librarian/internal/store   30.260s
$ go test -tags exportfixture -count=1 ./internal/server
ok  github.com/MauricioPerera/librarian/internal/server  52.154s
```

C20 incluye `TestDualEngineVectorPrecision` y el round-trip del embedding de 1536 componentes contra
`pgvector` real: **con la capacidad habilitada, el camino del vector es el de siempre**.

### build / vet / gofmt / suite

```
$ go build ./...        (sin salida = OK)
$ go vet ./...          VET-OK
$ gofmt -l .            GOFMT-CLEAN  (sin salida)
$ go vet -tags exportfixture ./internal/server && go vet -tags dualengine ./...   VET-TAGS-OK

=== RUN 1 ===
ok  github.com/MauricioPerera/librarian/cmd/librarian   3.072s
ok  github.com/MauricioPerera/librarian/internal/auth   6.725s
ok  github.com/MauricioPerera/librarian/internal/config 1.349s
ok  github.com/MauricioPerera/librarian/internal/dual   1.359s
ok  github.com/MauricioPerera/librarian/internal/schema 1.441s
ok  github.com/MauricioPerera/librarian/internal/server 35.242s
ok  github.com/MauricioPerera/librarian/internal/store  4.139s
=== RUN 2 ===
ok  github.com/MauricioPerera/librarian/cmd/librarian   2.951s
ok  github.com/MauricioPerera/librarian/internal/auth   6.476s
ok  github.com/MauricioPerera/librarian/internal/config 1.296s
ok  github.com/MauricioPerera/librarian/internal/dual   1.305s
ok  github.com/MauricioPerera/librarian/internal/schema 1.416s
ok  github.com/MauricioPerera/librarian/internal/server 34.488s
ok  github.com/MauricioPerera/librarian/internal/store  3.953s
```

---

## Red-team: las preguntas del contrato, respondidas con evidencia

| Pregunta | Respuesta | Evidencia |
|---|---|---|
| **¿Qué pasa con una instalación que YA existe con la columna, que son todas las de hoy?** (la más importante) | **Nada.** El default de la variable es `enabled`, la columna existe, la guarda coincide y arranca igual. Y es lo que decidió el default: bajo el default contrario, la guarda —correctamente— habría frenado todo el parque en el primer reinicio después de actualizar el binario. Además el valor cero del TIPO es "habilitado", así que ni un call site olvidado puede degradar una instalación existente. | `git diff --stat -- '*_test.go'` **vacío**; suite completa verde dos veces sin tocar un test; las tres baterías dual-motor con `pgvector` verdes; `TestEnabledIsByteIdenticalToBeforeTheContract` |
| **¿Y si la base tiene la columna pero la config dice que no, y al revés?** | No arranca, en las dos direcciones, en los dos motores, con un mensaje que dice qué se creó, qué dice la config, que la elección es irreversible y cuáles son las dos salidas reales (volver al valor correcto, o instalación nueva + `compat copy`). Y la corrida rechazada **no escribe nada**. | `TestTheChoiceIsIrreversible` (`table untouched`); bloques 7, 8, 9 y 10 con el binario real; `T3 (enable later)` / `T3 (disable later)` en la batería dual-motor contra PostgreSQL real |
| **¿El export de una instalación sin la capacidad sigue siendo válido y auditable?** | Sí, y es el punto más delicado: `--dump-schema` toma la elección de la TABLA FÍSICA, no de la variable, así que el `schema_ref` describe la instalación real. Sin eso, el export crearía un `vector(1536)` en el destino y le devolvería el requisito de `pgvector` a quien migró para no tenerlo. El dump sigue siendo el mismo artefacto, con una columna menos. | bloque 11: `postgres disabled … embedding-mentions=0`, `vector( → 0`, y `identical to the honest dump: YES` con la variable contradictoria |
| **¿`InferFeatures` deja de reportar la familia vectorial? ¿Cambia eso el contrato de equivalencia?** | Deja de reportarla, y es la respuesta CORRECTA, no una regresión: `InferFeatures` describe lo que un esquema USA, y este no usa ningún vector. El contrato de equivalencia es entre una fuente y su export, y **las dos puntas se construyen desde ESTE esquema** — el dump que consume `compat copy` lleva la misma elección. Una instalación CON la capacidad la reporta exactamente igual que antes. | `TestInferFeaturesDropsTheVectorFamily`: `canonical_vectors` presente con la capacidad, ausente sin ella |
| **¿Un artículo creado con embedding antes de deshabilitar?** | **No es alcanzable, y eso es lo que había que comprobar.** Para que existiera haría falta una instalación creada CON la capacidad y después deshabilitada, y ese es exactamente el arranque que la guarda rechaza. Sobre una instalación creada SIN la capacidad no hay columna donde escribirlo, y la API rechaza el campo antes de cualquier SQL. No hay ningún camino que produzca el estado. | `TestTheChoiceIsIrreversible/created_WITH,_now_declared_without` (no arranca); `TestEmbeddingIsRefusedNotIgnored` (`rows=1`, fila intacta después de todos los rechazos) |
| **Extra: ¿y si alguien miente en `__compat_schema`?** | La sonda contesta desde la columna física igual, y la guarda sigue disintiendo. Es la razón explícita por la que el contrato dice "detectalo contra la BASE". | `TestTheGuardReadsTheBaseNotTheMetadata` |
| **Extra: ¿crear/editar un tipo dinámico corrompe la metadata de una instalación sin la capacidad?** | No: esas dos escrituras componen con las capacidades leídas de la tabla física, no con el default. Era el agujero menos obvio del contrato. | `TestContentTypesComposeForTheInstalledChoice`: `embedding declared: no, cpt_eventos declared: yes` |
| **Extra: ¿un typo en la variable deshabilita en silencio?** | No: cualquier valor no reconocido es un error de arranque que nombra los dos valores válidos y dice que la elección es irreversible. | `TestResolveCapabilities` (13 casos), `typo is refused` / `no is refused` |

---

## Lo que NO se hizo, y por qué

1. **No se construyó reconstrucción de tablas de código.** El contrato lo prohíbe explícitamente y
   la irreversibilidad se documenta en vez de resolverse. La vía real para cambiar de opinión es
   una instalación nueva + `compat copy`, que es lo que dice el mensaje de error.
2. **No se agregó búsqueda vectorial ni se cambió el formato canónico del vector.** `vector.go`
   conserva `FormatVector`/`ParseVector`/`validateEmbedding` sin una línea de diferencia.
3. **No se tocó `sqlite-postgres-compat`.** `columnExists` se escribió acá (compat expone
   `TableExists` pero no un equivalente a nivel de columna) en el mismo archivo que ya tiene la
   precondición por motor `requireVectorType`, y es la ÚNICA rama por motor que este contrato
   agrega. Si algún día compat expusiera `ColumnExists`, esta función desaparecería sin cambiar
   ninguna semántica.
4. **`store.EnsureSchema(ctx, store)` sigue existiendo** como "todas las capacidades habilitadas",
   igual que `schema.Build()`, `BuildWith` y `JSONWith`. Es deliberado: es lo que significan los 39
   call sites de test y lo que tiene toda instalación desplegada, y así el contrato no obligó a
   tocar un solo test preexistente. El camino de producción usa siempre la variante `…For(caps)`, y
   si alguien lo olvidara, la guarda de T3 lo detiene ruidosamente en el arranque.
5. **`docs/DEPLOY.md` no se reescribió.** Describe el despliegue SQLite; la variable nueva está
   documentada donde vive la operación de PostgreSQL (`docs/OPERATIONS.md`).

---

## Cómo reproducir T4 (el orquestador va a correrlo)

```powershell
go build -o librarian.exe ./cmd/librarian

# 1. El hueco, reproducido: capacidad por defecto contra el PostgreSQL SIN pgvector -> exit 1
$env:LIBRARIAN_ENGINE = "postgres"
$env:LIBRARIAN_DB     = "postgres://postgres:***@31.220.22.176:5446/postgres?sslmode=disable"
$env:LIBRARIAN_JWT_SECRET = "c23-secret"
.\librarian.exe

# 2. La misma base, capacidad no declarada: bootstrap + arranque
$env:LIBRARIAN_VECTOR = "disabled"
"admin-pass-c23" | .\librarian.exe --bootstrap --email admin@example.com
$env:LIBRARIAN_ADDR = "127.0.0.1:8123"
.\librarian.exe          # -> "schema ready on … (postgres, vector disabled)"

# 3. El guion HTTP de arriba (login + CRUD + el 400 del embedding)
# 4. Reiniciar con $env:LIBRARIAN_VECTOR = "enabled" (o sin la variable) -> exit 1, T3
# 5. --dump-schema y comprobar que no menciona embedding
```

Las baterías:

```powershell
$env:LIBRARIAN_PG_NO_VECTOR_DSN = "postgres://postgres:***@31.220.22.176:5446/postgres?sslmode=disable"
$env:COMPAT_POSTGRES_DSN        = "postgres://postgres:***@31.220.22.176:5445/postgres?sslmode=disable"
go test -tags dualengine -run TestVectorOptional    -count=1 -v ./internal/server
go test -tags dualengine -run TestDualEngineAuth    -count=1 ./internal/auth
go test -tags dualengine -run TestDualEngine        -count=1 ./internal/server
go test -tags dualengine -run TestDualEngineStore   -count=1 ./internal/store
go test ./... -count=1
```

---

## Criterios de aceptación

- [x] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [x] T4: **instalación funcionando sobre un PostgreSQL sin `pgvector`**, con salida real (bootstrap,
      login y CRUD de artículos por HTTP, § "LA TRANSCRIPCIÓN HTTP").
- [x] Con la capacidad habilitada nada cambia respecto de hoy (`git diff --stat -- '*_test.go'`
      vacío; las tres baterías dual-motor y la `exportfixture` verdes; comparación por bytes del
      esquema).
- [x] T2: `embedding` rechazado con explicación cuando está deshabilitada, nunca ignorado (incluido
      el `null` explícito), y sin escribir nada.
- [x] T3: cambiar la decisión sobre una instalación existente falla de forma ruidosa y explicada, en
      las dos direcciones, en los dos motores, detectado contra la BASE.
- [x] La canonicalización del texto del vector no cambia.

---

## Archivos tocados

**Nuevos**

- `internal/schema/capabilities.go` — el tipo `Capabilities` y el razonamiento del valor cero
- `internal/schema/vector_optional_contract23_test.go`
- `internal/config/capabilities_contract23_test.go`
- `internal/store/vector_optional_contract23_test.go`
- `internal/server/vector_optional_contract23_test.go`
- `internal/server/dualengine_contract23_test.go` (tag `dualengine`)
- `docs/reports/CONTRACT-23-REPORT.md` — este informe

**Modificados (producción)**

`internal/schema/{schema,server_dual,dynamic,dump}.go`, `internal/config/config.go`,
`internal/store/{store,contenttypes,contenttypes_edit}.go`,
`internal/server/{server,dual,articles,vector,content}.go`, `cmd/librarian/main.go`

**Modificados (docs)**

`docs/OPERATIONS.md` (§ "La capacidad vectorial es opcional"), `docs/PENDIENTES.md` (hueco 5 cerrado)

**NO tocados**

- `sqlite-postgres-compat` (todo el repo)
- **Ningún archivo `_test.go` preexistente** — `git diff --stat -- '*_test.go'` está vacío
- La canonicalización de `internal/server/vector.go`
- El contrato público de las rutas HTTP con la capacidad habilitada

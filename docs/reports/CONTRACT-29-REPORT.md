# CONTRACT-29 — Aislar las baterías dual-motor

Base: `ec18209`. Árbol SIN commitear, como pide el contrato.

**Resultado: LISTO, por la vía preferida.** El aislamiento es **una BASE DE DATOS por corrida**, no
un esquema. `CREATE DATABASE` copia `template1`, así que el `public` de la base nueva **nace vacío**;
`pgvector` se instala **dentro de esa base**, así que el tipo `vector` resuelve desde su propio
`public` y el `search_path` de la corrida es `public` **a secas, sin segunda entrada**. La propiedad
que pedía el contrato — *que el aislamiento no dependa de que `public` esté limpio* — se cumple **por
construcción**: lo que haya en el `public` del servidor está en **otra base de datos** y no es
alcanzable.

El escenario que da sentido al contrato está probado **de verdad, con salida real**: se ensució
`public` con una **instalación completa de librarian** (17 tablas + 7 vistas + `__compat_schema` +
datos, plantada con `librarian --bootstrap`), y **las baterías dual-motor pasaron enteras, dos
veces**, dejando esa suciedad intacta y sin dejar bases huérfanas.

**Cero cambios en código de producción.** Solo infraestructura de pruebas.

---

## Resumen de los gates

| Gate | Resultado |
|---|---|
| `go build ./...` | limpio |
| `go vet ./...` | limpio |
| `go vet -tags dualengine ./...` | limpio |
| `gofmt -l .` | vacío |
| `go test ./... -count=1` (1.ª vez) | verde (7/7 paquetes) |
| `go test ./... -count=1` (2.ª vez) | verde (7/7 paquetes) |
| `go test -tags dualengine -count=1 ./...` con `public` LIMPIO | verde (7/7 paquetes) |
| `go test -tags dualengine -count=1 ./...` con `public` SUCIO (instalación completa, ×2) | verde (7/7 paquetes) |
| `go test -tags dualengine -count=1 ./...` con `public` SUCIO (instalación PARCIAL + metadata mentirosa) | verde (7/7 paquetes) |
| Lo que las baterías VERIFICAN | **sin cambios** (transcripciones idénticas, mismas observaciones) |
| Código de producción tocado | **ninguno** |
| Dependencias nuevas | **ninguna** |

DSN usado: `postgres://postgres:***@31.220.22.176:5457/postgres?sslmode=disable` (contenedor
`pg-iso`, PostgreSQL 17, pgvector 0.8.5).

Archivos tocados:

| Archivo | Qué |
|---|---|
| `internal/pgtestdb/doc.go` | NUEVO — doc del paquete, sin tag (ver más abajo por qué) |
| `internal/pgtestdb/pgtestdb.go` | NUEVO — `Provision`, el mecanismo, tras el tag `dualengine` |
| `internal/store/dualengine_contract29_test.go` | NUEVO — la prueba de contaminación (T2) |
| `internal/auth/dualengine_contract19_test.go` | `openPostgresEngine` delega en `pgtestdb` |
| `internal/server/dualengine_contract20_test.go` | `openPostgresEngine` delega en `pgtestdb` |
| `internal/server/dualengine_contract23_test.go` | `openScopedPostgres` delega en `pgtestdb` |
| `internal/store/dualengine_contract21_test.go` | `openPostgresForStore` delega en `pgtestdb` |

Neto en los cuatro archivos existentes: **-165 / +46 líneas**. El patrón duplicado desaparece.

---

## RECON — dónde estaba el `,public`, y por qué

Cuatro sitios, no tres. El contrato nombra `internal/auth`, `internal/server` e `internal/store`;
además `internal/server/dualengine_contract23_test.go` tenía su propio `openScopedPostgres` que
llamaba al `withSearchPath` de CONTRACT-20. Los cuatro construían
`search_path=<esquema>,public` con tres copias casi idénticas de la misma función
(`withSearchPath`, `withSearchPath`, `withSearchPathC21`).

El `,public` estaba ahí por lo que dice el contrato y está medido: el tipo `vector` vive en `public`
y se resuelve por `search_path`. **Quitarlo a secas rompe.** Eso no se re-investigó.

**El vector real de la fuga, medido en esta sesión** (esto sí aporta algo nuevo, y corrige una
suposición fácil): **no** es `compat.Store.TableExists`. Ese método filtra por `current_schema()`
explícitamente y **no** ve `public`; lo comprobé plantando tablas en `public` y consultándolo desde
una conexión legacy — respondió `false` para las 11. Quien fuga es
**`store.missingTables` → `compat.InspectSchema`**, que prefiere la metadata de `__compat_schema` y
resuelve por `search_path`. Por eso el incidente necesitaba un `__compat_schema` **de verdad** en
`public`, no un residuo cualquiera: es esa tabla la que hizo el daño. La prueba T2 planta la
suciedad ejecutando el camino de arranque de producción, precisamente para que el `__compat_schema`
sea real.

---

## T1 — El aislamiento

### El mecanismo

`internal/pgtestdb.Provision(t, dsn, label) (*compat.Store, func())`:

1. Abre una conexión *admin* al DSN (pool limitado a 1 conexión).
2. **Barre** bases huérfanas de corridas interrumpidas (ver más abajo).
3. `CREATE DATABASE "lbtest_<unixnano>_<pid>_<8 hex>_<label>"`.
4. Reescribe el DSN para apuntar a esa base, con `search_path=public` **y nada más**.
5. **Asegura** el aislamiento en vez de suponerlo: `current_database()` es la base nueva,
   `current_schema()` es `public`, y `pg_tables WHERE schemaname='public'` devuelve **0**. Si
   `template1` estuviera contaminado, la corrida se detiene ahí con un mensaje que lo dice.
6. `CREATE EXTENSION IF NOT EXISTS vector` **en esa base**, y solo si el servidor la ofrece.
7. Devuelve el store y un `func()` que cierra y hace `DROP DATABASE ... WITH (FORCE)`.

`Provision` **no aplica ningún esquema**: para CONTRACT-23 el aplicar es justamente lo que se mide.
Cada batería sigue haciendo su `EnsureSchema`/`SeedCatalogs` como antes.

### Por qué el paquete tiene dos archivos

`pgtestdb.go` lleva `//go:build dualengine`, para que el build por defecto no enlace nunca un paquete
que importa `testing`. Un paquete cuyos archivos están TODOS excluidos por build constraints hace
fallar `go build ./...` con *"build constraints exclude all Go files"*; `doc.go` existe sin tag
únicamente para que el directorio siempre tenga un archivo compilable. Sin el tag, el paquete es
literalmente vacío.

### Consolidar entre paquetes: sí, y sin mover nada de sitio

El contrato pide evaluar si consolidar obliga a mover código entre paquetes de prueba. **No obliga.**
`internal/server/dualengine_contract20_test.go` es `package server` (test interno), los otros son
`_test` externos; un paquete normal importable desde ambos resuelve el caso sin ciclos, porque
`pgtestdb` solo importa `compat`, `internal/schema` e `internal/store`, y ninguno de esos importa
`internal/server`. Las cuatro funciones fixture **siguen viviendo donde vivían**, con sus nombres y
firmas intactos: lo único que cambió es su cuerpo. Ninguna batería tuvo que tocarse más allá de eso.

### Lo que las baterías VERIFICAN: sin tocar

Ni una línea de escenario, ni una observación, ni un `tr.add`. Las transcripciones comparadas siguen
dando lo mismo — la evidencia es que las tres baterías de comparación línea por línea
(CONTRACT-19/20/21) y las de 20C/22/23/26/27/28 pasan sin cambios, y su criterio de éxito **es** que
las dos transcripciones sean idénticas.

---

## T2 — Verificación

### La prueba que da sentido al contrato, automatizada

`internal/store/dualengine_contract29_test.go` —
`TestContract29IsolationSurvivesADirtyPublic`. Cuatro partes:

1. **Se asegura de que `public` esté SUCIO, sea cual sea su estado previo** (`ensureDirtyPublic`).
2. **Comprueba que esa suciedad es alcanzable** por el fallback `,public` (`provePublicLeaks`),
   para que la parte 4 no sea vacua.
3. **Reproduce el mecanismo VIEJO** — esquema + `search_path=<esquema>,public` — sobre un `public`
   sucio, y mide cuánto del esquema canónico acaba en el espacio de nombres propio de la corrida.
4. **Mide el mecanismo NUEVO** con `openPostgresForStore`, el mismísimo fixture de las baterías
   CONTRACT-21/26/27, y luego comprueba que el camino de arranque de producción **sí** construye y
   **sí** escribe en las tablas de la corrida.

#### El test NO puede exigir un `public` limpio para arrancar — y ya no lo hace

Mi primera versión plantaba su propia instalación **incondicionalmente**. El orquestador la rompió
con dos líneas de suciedad propia: un `public` con una instalación **a medias** (un `articles` sin la
columna `embedding`) hace que el plantado choque contra la guarda de coherencia de CONTRACT-23:

```
plant a librarian installation in public: this installation was created WITHOUT the vector
capability (its articles table has no "embedding" column) but the configuration now declares
it ENABLED ... refusing to start
cleanup: removed 0 planted relations from public, leaving the 8 that were there before
```

Es decir: **el test que prueba que el aislamiento sobrevive a un `public` sucio se murió de un
`public` sucio**, y con un mensaje de otra capa que no nombraba la causa. Exactamente la clase de
fallo desconcertante que este contrato existe para abolir, reproducida dentro de su propia prueba.

La regla que salió de ahí, escrita en el código:

> Un test que asevera "el aislamiento sobrevive a un `public` sucio" **nunca** puede exigir un
> `public` limpio para arrancar.

`ensureDirtyPublic` tiene ahora tres desenlaces, y los tres son legibles:

- **`public` YA tiene relaciones con nombres de la aplicación** → se usan **esas** como suciedad y
  **no se planta nada**. La porquería ajena sirve igual de bien, y así el test no impone ninguna
  precondición sobre la forma del desastre. Es el caso normal en un servidor donde alguien sondeó.
- **`public` no tiene nada reconocible** → planta una instalación real por el camino de arranque de
  producción (la única forma de tener un `__compat_schema` verídico).
- **el plantado es rechazado** → **SKIP** nombrando el rechazo, no FAIL. Lo que no se puede medir es
  la *suciedad*; el aislamiento en sí no está en duda, y un test rojo apuntaría al sitio equivocado.

La limpieza sigue quitando **solo lo que este test añadió**, así que una suciedad ajena queda
exactamente como se encontró.

#### Salida real — `public` LIMPIO (el test planta su propia suciedad)

```
=== RUN   TestContract29IsolationSurvivesADirtyPublic
    public was clean, so this test planted the pollution itself through the production startup
        path: 24 relations, 17 application-named, __compat_schema among them: true
    the pollution IS reachable the old way: 17/17 application-named relations resolve from a
        `search_path=contract29_probe_1785092724287516000,public` session
    OLD mechanism (search_path=contract29_legacy,public over a dirty public):
        EnsureSchema err=<nil>, tables created in the run's OWN schema: 0 of 16 wanted
    OLD mechanism: the dirty public was seen through the `,public` fallback, so EnsureSchema
        created (almost) nothing and every later write would land in public — this IS the incident
    NEW mechanism (database per run): 0 of the 24 polluting relations visible,
        InspectSchema reports 0 tables
    production path on the isolated database: schema built twice, 4 roles seeded,
        reads land in the run's own tables
    cleanup: removed 24 planted relations from public, leaving the 0 that were there before
--- PASS: TestContract29IsolationSurvivesADirtyPublic (51.91s)
```

#### Salida real — `public` SUCIO con la forma exacta que rompió la versión anterior

Suciedad plantada a mano, la del orquestador: 8 relaciones —`users`, `roles`, `permissions`,
`role_permissions`, `content_types`, `content_type_fields`, un `articles` **sin** `embedding`, y un
`__compat_schema` con metadata mentirosa `{"tables":[]}`—.

```
=== RUN   TestContract29IsolationSurvivesADirtyPublic
    public was ALREADY dirty when this test started: 8 relations, 8 of them application-named
        (__compat_schema, articles, content_type_fields, content_types, permissions,
        role_permissions, roles, users). Using THAT as the pollution — nothing is planted,
        so this test imposes no precondition on the shape of the mess.
    the pollution IS reachable the old way: 8/8 application-named relations resolve from a
        `search_path=contract29_probe_1785093091397661800,public` session
    OLD mechanism (search_path=contract29_legacy,public over a dirty public):
        EnsureSchema err=<nil>, tables created in the run's OWN schema: 0 of 16 wanted
    OLD mechanism: the dirty public was seen through the `,public` fallback, so EnsureSchema
        created (almost) nothing and every later write would land in public — this IS the incident
    NEW mechanism (database per run): 0 of the 8 polluting relations visible,
        InspectSchema reports 0 tables
    production path on the isolated database: schema built twice, 4 roles seeded,
        reads land in the run's own tables
    cleanup: removed 0 planted relations from public, leaving the 8 that were there before
--- PASS: TestContract29IsolationSurvivesADirtyPublic (25.24s)
```

Verificado con `psql` después: las 8 relaciones y el `{"tables":[]}` siguen ahí, byte por byte.

**`EnsureSchema err=<nil>` con `0 of 16` tablas creadas es el incidente entero en una línea**: no
falla, no avisa, simplemente no crea nada, y todo lo que se escriba después se va a `public`. De ahí
el 404 sobre un tipo de contenido que "existía".

La parte 3 corre **dentro de una base desechable propia**, no contra el `public` del servidor, y por
una razón que descubrí a golpes: el mecanismo viejo es **destructivo** a través del fallback que se
le está midiendo — `applyViews` emite un `DROP VIEW` sin calificar y el `,public` lo resuelve a las
vistas de otro. Mi primera versión del test se llevó por delante las 7 vistas del `public` sucio.
Medir el bug no puede ser cometerlo. La parte 2 (`provePublicLeaks`) es por eso **estrictamente de
lectura** sobre `public`: un esquema vacío de nombre único, unos `to_regclass`, y el esquema
borrado.

### Las dos corridas completas que pidió el orquestador

Suite dual-motor entera, con la suciedad del orquestador puesta (8 relaciones, `articles` sin
`embedding`, `__compat_schema` con `{"tables":[]}`):

```
=== FULL DUALENGINE SUITE — public DIRTY (8 relations, lying __compat_schema) ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.860s
ok  	github.com/MauricioPerera/librarian/internal/auth	63.615s
ok  	github.com/MauricioPerera/librarian/internal/config	1.290s
ok  	github.com/MauricioPerera/librarian/internal/dual	1.296s
?   	github.com/MauricioPerera/librarian/internal/pgtestdb	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/schema	1.382s
ok  	github.com/MauricioPerera/librarian/internal/server	260.648s
ok  	github.com/MauricioPerera/librarian/internal/store	163.454s
```

Estado de `public` inmediatamente después — intacto, y sin bases ni esquemas huérfanos:

```
       relname                      key        |     value        datname      nspname
---------------------            ------------------+---------------   -----------   ---------
 __compat_schema                  canonical_schema | {"tables":[]}     postgres      public
 articles                                                              template0
 content_type_fields                                                   template1
 content_types
 permissions
 role_permissions
 roles
 users
(8 rows)
```

Y la misma suite con `public` limpio:

```
=== FULL DUALENGINE SUITE — public CLEAN ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	3.001s
ok  	github.com/MauricioPerera/librarian/internal/auth	56.564s
ok  	github.com/MauricioPerera/librarian/internal/config	1.307s
ok  	github.com/MauricioPerera/librarian/internal/dual	1.306s
?   	github.com/MauricioPerera/librarian/internal/pgtestdb	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/schema	1.417s
ok  	github.com/MauricioPerera/librarian/internal/server	222.201s
ok  	github.com/MauricioPerera/librarian/internal/store	140.825s
```

### La prueba manual con una instalación COMPLETA en `public`

Ensucié `public` **fuera del test**, con el binario real, para que nadie pueda decir que el test se
ensucia a sí mismo un `public` de mentira:

```
$ LIBRARIAN_ENGINE=postgres LIBRARIAN_DB='postgres://postgres:***@31.220.22.176:5457/postgres?sslmode=disable&search_path=public' \
    librarian --bootstrap --email probe@example.com < password
librarian: bootstrap complete on postgres://postgres:***@31.220.22.176:5457/postgres?sslmode=disable&search_path=public (postgres)
  identity created: probe@example.com (id b57ceb0e-...), status active, role "administrator"
  role "administrator" now holds 8 permission(s): content.create, content.delete, ...
  this installation is now claimed and can never be bootstrapped again
```

Estado de `public` tras eso — una instalación completa, con `__compat_schema` y datos:

```
 relkind | count            tablename
---------+-------          -------------------------
 r       |    17            __compat_schema, api_keys, article_terms, articles, bootstrap,
 v       |     7            content_type_fields, content_type_references, content_types,
                            permissions, product_terms, products, role_permissions, roles,
 users_in_public            taxonomies, terms, user_roles, users
-----------------
               1
```

Con `public` así, la suite dual-motor completa, **dos veces**:

```
=== DIRTY-PUBLIC RUN 1 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.676s
ok  	github.com/MauricioPerera/librarian/internal/auth	56.009s
ok  	github.com/MauricioPerera/librarian/internal/config	1.064s
ok  	github.com/MauricioPerera/librarian/internal/dual	1.058s
?   	github.com/MauricioPerera/librarian/internal/pgtestdb	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/schema	1.107s
ok  	github.com/MauricioPerera/librarian/internal/server	218.561s
ok  	github.com/MauricioPerera/librarian/internal/store	131.745s
=== DIRTY-PUBLIC RUN 2 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.979s
ok  	github.com/MauricioPerera/librarian/internal/auth	72.729s
ok  	github.com/MauricioPerera/librarian/internal/config	1.270s
ok  	github.com/MauricioPerera/librarian/internal/dual	1.273s
?   	github.com/MauricioPerera/librarian/internal/pgtestdb	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/schema	1.405s
ok  	github.com/MauricioPerera/librarian/internal/server	256.398s
ok  	github.com/MauricioPerera/librarian/internal/store	153.372s
```

Y el `public` sucio **intacto** después de las dos corridas, sin bases ni esquemas huérfanos:

```
 relkind | count      users_in_public     datname       nspname
---------+-------    -----------------   -----------   ---------
 r       |    17                   1      postgres      public
 v       |     7                          template0
                                          template1
```

Antes de este contrato esa misma suciedad habría hecho fallar las baterías con un 404 incomprensible.
Ahora ni las roza.

### El entorno queda como se encontró

Tras limpiar la suciedad deliberada y correr los gates finales:

```
  datname       nspname     relations_in_public
-----------    ---------   ---------------------
 postgres       public                        0
 template0
 template1
```

---

## Red-team

### Paquetes concurrentes (la importante)

`go test ./...` corre `internal/auth`, `internal/server` e `internal/store` **a la vez**, y las tres
provisionan contra el mismo servidor. El nombre de cada base lleva reloj de nanosegundos + pid + 4
bytes de `crypto/rand`, así que dos provisiones simultáneas no pueden colisionar. Verificado en la
práctica: **cuatro** corridas completas de `go test -tags dualengine -count=1 ./...` (dos con
`public` limpio, dos con `public` sucio de dos formas distintas), más varias parciales, todas verdes,
con ~18 provisiones por corrida.

### Una corrida interrumpida con Ctrl-C

El `func()` de limpieza va en `defer` en cada batería, así que también corre cuando un test hace
`t.Fatal`. Lo que no cubre es un Ctrl-C o un panic: para eso, **cada** provisión barre primero las
bases de este mismo esquema de nombres cuyo timestamp embebido sea más viejo que **30 minutos**. La
guarda de edad es lo que impide que el barrido se lleve por delante la base viva de un paquete
hermano; 30 minutos es holgadamente más que la batería más larga (~4 min).

Verificado de verdad, plantando una base huérfana a mano con timestamp del año 2001:

```
$ psql -c 'CREATE DATABASE "lbtest_1000000000000000000_1_deadbeef_stale"'
$ go test -tags dualengine -run TestDualEngineStore -count=1 -v ./internal/store
    sweep leftovers: dropped lbtest_1000000000000000000_1_deadbeef_stale, left behind by an interrupted run
--- PASS: TestDualEngineStore (48.66s)
```

`creationTime` devuelve `false` para cualquier nombre que no parsee, así que una base que solo
*empiece* por el prefijo nunca se borra por conjetura.

### ¿Permisos del usuario del DSN?

Confirmado antes de escribir código: `usesuper = t`. `CREATE DATABASE` y
`DROP DATABASE ... WITH (FORCE)` (PostgreSQL ≥ 13; el servidor es 17) funcionan. No hizo falta ningún
permiso nuevo.

### ¿Y si `pgvector` no está disponible en la base nueva?

Ese es exactamente el caso de CONTRACT-23, que apunta a un PostgreSQL que **no debe** poder resolver
`vector`. `Provision` consulta `pg_available_extensions` y solo instala la extensión si el **servidor**
la ofrece; contra `LIBRARIAN_PG_NO_VECTOR_DSN` no la instala y `to_regtype('vector')` sigue siendo
`NULL`, que es lo que `requireNoPgvector` asevera acto seguido. La distinción sobre la que vive esa
batería queda intacta.

### ¿Coste de arranque?

`CREATE DATABASE` copia `template1`. La suite dual-motor completa pasó de ~4:00 a ~4:00–7:00 según la
corrida (la variación entre corridas del mismo código es mayor que la diferencia). No es un
impedimento.

---

## Lo que NO quedó cubierto — dicho claro

1. **`template1` es la nueva premisa.** El aislamiento por base de datos hereda `template1`: si
   alguien contamina *esa* plantilla, todas las bases nuevas nacen sucias. No es hipotético a nivel
   de mecanismo, sí lo es a nivel de práctica. **No lo dejé implícito**: `Provision` cuenta las
   tablas de `public` en la base recién creada y **aborta con un mensaje que nombra `template1`** si
   no es cero. O sea: el fallo desconcertante que este contrato vino a matar no puede reaparecer por
   esta puerta sin decir su nombre.

2. **La suite dual-motor no corrió contra el PostgreSQL SIN pgvector.** `LIBRARIAN_PG_NO_VECTOR_DSN`
   no estaba provisto en esta sesión, así que `TestVectorOptional*` **skipeó** en las cinco corridas.
   El cambio en `openScopedPostgres` de esa batería está razonado y compila y vetea, pero **no está
   ejecutado contra el servidor sin la extensión**. Es el único gate del contrato que no puedo
   declarar medido. Si el orquestador levanta ese segundo servidor, basta una corrida de
   `go test -tags dualengine -run TestVectorOptional ./internal/server` para cerrarlo.

3. **El barrido de restos solo conoce bases, no los esquemas del mecanismo viejo.** Si quedaran
   esquemas `librarian_c19_*` / `librarian_c20_*` / `librarian_c21_*` de antes de este contrato,
   nadie los limpia automáticamente. En el servidor `pg-iso` no quedaba ninguno (verificado), pero en
   otro servidor habría que barrerlos a mano una vez.

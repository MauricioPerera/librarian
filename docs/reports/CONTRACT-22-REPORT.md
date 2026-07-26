# CONTRACT-22 — Bootstrap: dejar administrable una instalación en limpio

Base: `4d3c115` (CONTRACT-21 y la documentación de huecos en `main`). Árbol **SIN commitear**, como
pide el contrato.

**Resultado: LISTO.** Una instalación en limpio se vuelve administrable con un solo comando, sobre
los dos motores, y está probado del modo que el contrato exige: base vacía → `--bootstrap` → entrar
por HTTP con el binario real y hacer **escrituras gateadas por permiso que hoy fallaban con 403**.
La transcripción de las dos corridas (SQLite y PostgreSQL 17 real, con `pgvector`) está abajo.

Lo que cierra no es "falta el primer usuario": es el **bloqueo circular** que el contrato midió.
`EnsureSchema` + `SeedCatalogs` dejan `role_permissions` VACÍA, así que un usuario con rol
`administrator` no tenía ningún permiso, y la única UI capaz de otorgarlos (CONTRACT-16) está
gateada por `roles.manage`, que tampoco tenía. El bootstrap hace **las dos cosas en una sola
transacción**: crea la identidad y otorga los permisos al rol. O las dos, o ninguna.

`SeedCatalogs` no se tocó. Ninguna dependencia nueva, **ningún permiso nuevo**, ningún cambio en el
contrato público de las rutas HTTP existentes. `sqlite-postgres-compat` no se tocó.

> **El password de la infraestructura PostgreSQL está enmascarado como `***` en todo este reporte**,
> incluida la salida real del binario (que lo enmascara él mismo: ver `redactDSN`).

---

## Resumen de qué se tocó

| Archivo | Qué |
|---|---|
| `internal/schema/schema.go` | **+** tabla `bootstrap` (una sola fila representable) y sus constantes |
| `internal/auth/bootstrap.go` | **NUEVO** — `Bootstrap`, `BootstrapStatus`, los sentinelas y las validaciones |
| `internal/auth/users.go` | `CreateUser` partida en `createUserTx` / `createUserWithIDTx` (sin cambio observable) |
| `internal/auth/roles.go` | `SetRolePermissions` partida en `setRolePermissionsTx` (sin cambio observable) |
| `cmd/librarian/main.go` | **+** modo `--bootstrap --email <dir>`; password por entrada estándar |
| `internal/auth/bootstrap_contract22_test.go` | **NUEVO** — 12 tests: atomicidad forzada, una-sola-vez, red-team |
| `internal/server/bootstrap_contract22_test.go` | **NUEVO** — la prueba HTTP end-to-end en la suite por defecto |
| `internal/server/dualengine_contract22_test.go` | **NUEVO** — la misma prueba contra PostgreSQL 17 + carrera concurrente |
| `cmd/librarian/bootstrap_contract22_test.go` | **NUEVO** — el modo del binario, incluido "el password no es un flag" |
| `docs/PENDIENTES.md`, `docs/DEPLOY.md`, `README.md` | hueco 4 cerrado; runbook del bootstrap; el README decía algo hoy falso |

---

## T1 — La operación

### La atomicidad, y por qué obligó a partir dos funciones

El estado prohibido es exactamente el del incidente histórico: **un usuario con rol, y el rol sin
permisos**. Componer la operación con `auth.CreateUser(...)` seguido de
`auth.SetRolePermissions(...)` lo hace ALCANZABLE, porque cada una abre y cierra su propia
transacción: un fallo entre las dos deja el sistema en el estado que el contrato existe para
impedir, producido por el código escrito para impedirlo.

Por eso **el cuerpo** de las dos se extrajo a helpers que corren sobre la transacción del llamador:

- `auth.CreateUser` → `createUserTx` → `createUserWithIDTx`
- `auth.SetRolePermissions` → `setRolePermissionsTx`

Las funciones públicas quedaron como envoltorios finos que abren su transacción, llaman al cuerpo y
comitean: **mismas sentencias, mismo orden, un solo commit**. Nada observable cambió para el
llamador de CONTRACT-16 (hay un test dedicado a eso, `TestSetRolePermissionsStillBehaves`).
`Bootstrap` **reusa** esos cuerpos dentro de la única transacción que él posee. No reimplementa
nada.

La transacción, en orden:

1. leer el marcador (si existe → rechazo legible),
2. contar usuarios (si hay → rechazo legible),
3. **reclamar el marcador** ← la garantía,
4. crear la identidad con rol `administrator`,
5. otorgar al rol los 8 permisos del catálogo,
6. commit.

Los pasos 1 y 2 existen **solo para que el mensaje sea útil**. Si se borraran, la garantía seguiría
en pie: es el paso 3.

### "Una sola vez": la garantía es de la base

La tabla `bootstrap` tiene `PRIMARY KEY (id)` **y** un `CHECK id IN ('bootstrap')`. Hay exactamente
**una clave representable**, así que una segunda fila es imposible — no improbable. Medido sin pasar
por `Bootstrap`, con dos `INSERT` crudos:

```
$ go test ./internal/auth -run TestTheMarkerTableMakesASecondClaimUnrepresentable -v
    UNREPRESENTABLE: same key -> constraint failed: UNIQUE constraint failed: bootstrap.id (1555) ;
                     other key -> constraint failed: CHECK constraint failed: ("id" IN ('bootstrap')) (275)
--- PASS: TestTheMarkerTableMakesASecondClaimUnrepresentable (0.11s)
```

**Se descartaron las dos alternativas obvias**, y el motivo importa:

- *"rechazar si ya hay un usuario"* es una POLÍTICA correcta (el comando también la aplica, para dar
  un mensaje legible) pero **no es una garantía**: dos transacciones simultáneas pueden ver las dos
  cero usuarios. Además es reversible — borrar al administrador reabriría la puerta.
- *"rechazar si `role_permissions` no está vacía"* es peor: ese es precisamente el estado en que
  estuvo producción durante semanas, y no dice nada sobre si alguna vez se creó una identidad.

La tabla **no tiene FK a `users`** a propósito: un `ON DELETE CASCADE` borraría el marcador junto con
el administrador, que es literalmente "una vía de creación de administradores que sobrevive a su
primer uso".

### Qué permisos, y a quién

Los 8 de `schema.Permissions`, al rol `administrator`. `editor`, `author` y `contributor` quedan
**sin permisos**, y hay un test que falla si alguno recibe algo. No existe en ningún lado una
definición de qué debería poder cada uno; inventarla sería fijar política por código.

`SeedCatalogs` quedó **sin tocar**, como fijaba el contrato.

---

## T2 — La forma: un modo del binario, y el password por entrada estándar

### Por qué un modo del binario y no una ruta HTTP

Una ruta HTTP tendría que ser alcanzable **sin autenticación** — no hay contra quién autenticarse,
ese es todo el problema —, o sea: el servicio en marcha expondría permanentemente un endpoint de
creación de administradores cuya única protección sería la fila marcadora. Es superficie de red
grande y permanente delante de una acción irreversible, en un servicio cuya historia de
configuración entera es "fallar cerrado".

El modo del binario **no agrega superficie al servicio en marcha** (requisito explícito del
contrato): exige acceso de shell al host, que ya tiene cualquiera que hoy podía reparar la base a
mano. El precedente de forma es `--dump-schema`, y se siguió: mismo estilo de parseo, misma
resolución de motor, y tampoco requiere `LIBRARIAN_JWT_SECRET` (no sirve tráfico — verificado abajo
con la variable vacía).

### Por qué el password va por entrada estándar

**Un argumento de línea de comandos es el peor canal disponible para un secreto**, por dos razones
independientes y ordinarias:

- queda escrito literal en el archivo de historial del shell, y ahí se queda;
- es visible en la **lista de procesos** para todo usuario de la máquina mientras el proceso corre:
  `/proc/<pid>/cmdline` es legible por cualquiera en Linux, y `ps -ef` /
  `Get-CimInstance Win32_Process` lo muestran. Se filtra a usuarios que no tienen acceso alguno a la
  base.

**También se descartó la variable de entorno**, aunque es mejor: la heredan todos los procesos hijo,
es legible en `/proc/<pid>/environ`, e invita a terminar escrita en una unit de systemd o un
compose, o sea en control de versiones.

La entrada estándar no está en el historial, ni en la lista de procesos, ni en el entorno, ni se
hereda; y es el canal al que cualquier gestor de secretos ya sabe escribir:

```bash
librarian --bootstrap --email admin@example.com < /run/secrets/admin-password
pass show librarian/admin | librarian --bootstrap --email admin@example.com
```

El **email** sigue siendo un flag a propósito: no es un secreto, y tenerlo en la línea de comandos
deja la invocación auto-documentada en el historial.

Y el comando **rechaza explicando** si alguien intenta pasarlo igual (`--password`, `--pass`,
`--pwd`, `--secret`), en vez de ignorarlo en silencio — ignorarlo dejaría el secreto en el historial
y en la lista de procesos **y además** fallaría de forma confusa:

```
$ librarian --bootstrap --email x@y.com --password hunter2
2026/07/25 18:45:31 librarian: --password is not accepted: a password passed on the command line is
written to the shell history and is visible in the process list to every user of this machine.
--bootstrap reads the password from STANDARD INPUT instead — for example:
librarian --bootstrap --email admin@example.com < /run/secrets/admin-password
exit=1
```

### El resultado dice qué quedó hecho

Las **dos** mitades, porque la que faltaba es la que causó el incidente:

```
$ LIBRARIAN_ENGINE=sqlite LIBRARIAN_DB=/tmp/c22sqlite.db \
  ./librarian --bootstrap --email admin@example.com < /tmp/c22pw.txt
librarian: bootstrap complete on C:/Users/…/c22sqlite.db (sqlite)
  identity created: admin@example.com (id 32384a65-f20b-4e22-be40-ac4ada18b264), status active, role "administrator"
  role "administrator" now holds 8 permission(s): content.create, content.delete, content.publish, content.update, content_types.manage, roles.manage, terms.manage, users.manage
  roles editor/author/contributor were left with NO permissions on purpose; grant them from the admin UI
  this installation is now claimed and can never be bootstrapped again
exit=0
```

(La corrida de arriba se hizo con `LIBRARIAN_JWT_SECRET` **vacío**: el bootstrap no lo necesita.)

Y si ya se usó, lo dice — no un error genérico:

```
$ ./librarian --bootstrap --email otro@example.com < /tmp/c22pw.txt
2026/07/25 18:45:31 librarian: this installation has already been bootstrapped: it was claimed on
2026-07-26T00:45:30.876353200Z by "admin@example.com" (user id 32384a65-f20b-4e22-be40-ac4ada18b264),
and a bootstrap can never run twice — to add another administrator, sign in as that user and use the admin UI
exit=1
```

### La resolución de motor se comparte, no se duplica

`bootstrapCommand` llama a la **misma** `config.ResolveEngine()` que usan el servidor y
`--dump-schema`, con el mismo override `--db` y la misma guarda de contradicción motor/DSN. Un
bootstrap capaz de discrepar con la instancia en marcha sobre qué motor es este despliegue
escribiría el administrador en una base que nadie sirve.

---

## T3 — LA PRUEBA QUE CIERRA EL HUECO (los dos motores, binario real)

### El punto de partida, medido

```
$ go test ./internal/server -run TestGatedWriteIsImpossibleWithoutBootstrap -v
    MEASURED: an administrator on an unbootstrapped install READS fine and is 403 on every gated
    write (the production incident)
--- PASS
```

Un usuario creado con rol `administrator` sobre una instalación en limpio **lee bien** (por eso el
incidente quedó invisible semanas: todas las verificaciones eran lecturas) y recibe **403 en
`POST /articles`, `POST /content-types` y `POST /terms`**.

### SQLite — binario real, servidor real, curl real

```
$ LIBRARIAN_ENGINE=sqlite LIBRARIAN_DB=/tmp/c22sqlite.db LIBRARIAN_JWT_SECRET=… ./librarian &
2026/07/25 18:45:44 librarian: schema ready on C:/Users/…/c22sqlite.db (sqlite), listening on 127.0.0.1:8231

$ curl -s http://127.0.0.1:8231/health
{"status":"ok"}

# login por la ruta real, con la identidad que creó el bootstrap
$ curl -s -X POST .../auth/login -d '{"email":"admin@example.com","password":"…"}'   → token OK

### GATED WRITE POST /articles (content.create)
HTTP 201
{"author_id":"32384a65-f20b-4e22-be40-ac4ada18b264","body":"cuerpo","id":"faec7772-4ab7-4d0b-9823-3137a30bc80a","title":"Prueba manual"}

### GATED WRITE POST /content-types (content_types.manage)   ← la ruta que dio 403 en producción
HTTP 201
{"name":"resenas","fields":[{"name":"titular","type":"text"},{"name":"puntaje","type":"integer"}]}
```

### PostgreSQL 17 real (con `pgvector`) — mismo binario, esquema en limpio

```
$ LIBRARIAN_ENGINE=postgres LIBRARIAN_DB='postgres://postgres:***@31.220.22.176:5444/postgres?sslmode=disable&search_path=librarian_c22_manual,public' \
  ./librarian --bootstrap --email admin@example.com < /tmp/c22pw.txt
librarian: bootstrap complete on postgres://postgres:***@31.220.22.176:5444/postgres?sslmode=disable&search_path=librarian_c22_manual,public (postgres)
  identity created: admin@example.com (id c6ea7b13-97c7-43e9-b1aa-c4eb1ad412ac), status active, role "administrator"
  role "administrator" now holds 8 permission(s): content.create, content.delete, content.publish, content.update, content_types.manage, roles.manage, terms.manage, users.manage
  roles editor/author/contributor were left with NO permissions on purpose; grant them from the admin UI
  this installation is now claimed and can never be bootstrapped again
exit=0

$ ./librarian --bootstrap --email otro@example.com < /tmp/c22pw.txt
2026/07/25 18:46:10 librarian: this installation has already been bootstrapped: it was claimed on
2026-07-26T00:46:04.121478400Z by "admin@example.com" (user id c6ea7b13-97c7-43e9-b1aa-c4eb1ad412ac), …
exit=1

$ ./librarian &   # el servicio, sobre el MISMO esquema
2026/07/25 18:46:27 librarian: schema ready on postgres://postgres:***@31.220.22.176:5444/postgres?sslmode=disable&search_path=librarian_c22_manual,public (postgres), listening on 127.0.0.1:8232

$ curl -s http://127.0.0.1:8232/health
{"status":"ok"}

### GATED WRITE POST /articles (content.create)
HTTP 201
{"author_id":"c6ea7b13-97c7-43e9-b1aa-c4eb1ad412ac","body":"cuerpo","id":"bc22b156-f865-4fdd-ac5a-364db53b0461","title":"Prueba manual PG"}

### GATED WRITE POST /content-types (content_types.manage)
HTTP 201
{"name":"resenas","fields":[{"name":"titular","type":"text"},{"name":"puntaje","type":"integer"}]}

### GATED WRITE POST /content/resenas (CRUD genérico, content.create)
HTTP 201
{"author_id":"c6ea7b13-97c7-43e9-b1aa-c4eb1ad412ac","created_at":"2026-07-26T00:46:31.2496381Z","id":"2026f7d2-8e6d-494e-a3df-afc810f0818f","metadata":null,"puntaje":9,"titular":"Primera",…}

### control
POST /articles sin token -> HTTP 401
```

El esquema `librarian_c22_manual` se creó para la prueba y se borró al terminar (`DROP SCHEMA …
CASCADE`). El DSN aparece enmascarado **porque el binario lo enmascara** (`redactDSN`), no porque se
haya editado el reporte.

### La misma prueba, automatizada, con las dos transcripciones idénticas

`TestDualEngineBootstrap` corre el guion completo sobre SQLite y sobre PostgreSQL 17 y exige que las
21 observaciones coincidan línea por línea:

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5444/postgres?sslmode=disable' \
  go test -tags dualengine -run TestDualEngineBootstrap -count=1 -v ./internal/server

transcript (21 lines, identical on both engines):
  clean install users=0 roles=4 permissions=8 role_permissions=0
  before bootstrap: login status=401
  before bootstrap: POST /articles status=401 error=unauthorized
  before bootstrap: marker found=false err=none
  bootstrap err=none role=administrator permissions=[content.create content.delete content.publish content.update content_types.manage roles.manage terms.manage users.manage]
  after bootstrap users=1 role_permissions=8 marker=1
  after bootstrap: marker found=true email=admin@example.com owns-created-user=true err=none
  after bootstrap: role editor found=true permissions=[] err=none
  after bootstrap: role author found=true permissions=[] err=none
  after bootstrap: role contributor found=true permissions=[] err=none
  after bootstrap: login status=200 token=true
  GATED WRITE POST /articles (content.create) status=201 has-id=true error=<none>
  articles rows=1
  GATED WRITE POST /content-types (content_types.manage) status=201 error=<none>
  content_types rows=1
  GATED WRITE POST /content/resenas22 (content.create) status=201 has-id=true error=<none>
  second bootstrap err=yes sentinel=true
  second bootstrap changed-nothing users=true grants=true marker=1
  second bootstrap: refused identity login status=401
  CONTROL roleless POST /articles status=403 error=forbidden
  administrator still writes status=201 articles=2

OK: 21 observations identical on SQLite and PostgreSQL 17
--- PASS: TestDualEngineBootstrap (19.43s)
```

**La última línea del control es la que hace que los 201 signifiquen algo**: un usuario sin rol
recibe 403 en la misma ruta, así que el éxito es una decisión de autorización y no un endpoint sin
puerta.

En la suite por defecto (sin tag), `TestBootstrapUnblocksGatedWritesOverHTTP` hace lo mismo sobre
SQLite **y además ejercita la UI de CONTRACT-16** que estaba fuera de alcance — otorgar
`content.create` al rol `editor` por `POST /admin/roles/editor/permissions` (303) — leyendo el grant
resultante con SQL directo:

```
HOLE CLOSED: clean install -> bootstrap(admin@example.com, 8 grants) -> HTTP login ->
201 POST /articles, 201 POST /content-types, 303 granting a permission through the roles.manage UI;
a roleless user still gets 403
```

---

## Atomicidad: forzada, no afirmada

Dos fallos inducidos en **puntos distintos** de la transacción, ambos posteriores al `INSERT` del
usuario:

| Test | Dónde falla | Qué prueba |
|---|---|---|
| `TestBootstrapIsAtomicWhenTheRoleIsMissing` | resolución del rol, **después** de insertar el usuario | rollback con el usuario ya escrito |
| `TestBootstrapIsAtomicWhenAPermissionIsMissing` | **dentro del paso de grants**, con usuario y rol ya asignados | el estado prohibido no sobrevive |

```
ATOMIC (failure after the user insert): create the administrator identity: unknown role "administrator"
ATOMIC (failure inside the grant step): grant the catalog permissions to role "administrator": unknown permission "roles.manage"
```

El oráculo del rollback verifica las tres cosas: **cero usuarios, cero grants, cero marcador** — y
además que la instalación sigue siendo bootstrappable, que es lo que hace útil al rollback y no
solo prolijo (`TestBootstrapAfterARolledBackAttemptStillWorks`: un intento fallido **no consume** el
uso único). El segundo test recorre además todos los usuarios y falla explícitamente si alguno tiene
un rol sin permisos, con el mensaje `THE HISTORICAL INCIDENT IS REACHABLE`.

---

## Red-team: cada pregunta, con su test

| Pregunta del contrato | Respuesta | Test |
|---|---|---|
| **Hay usuarios pero `role_permissions` vacía (producción HOY)** | **Se niega y no cambia nada.** Acuñar administradores sobre un sistema que ya tiene identidades sería un agujero de autenticación, no una reparación. El mensaje apunta a la UI de CONTRACT-16 | `TestBootstrapRefusesAnInstallationThatAlreadyHasUsers`, `TestBootstrapCommandRefusesAnInstallationWithUsers` |
| Existe el rol pero fue borrado del catálogo | Falla con `ErrUnknownRole` y **rollback completo** | `TestBootstrapIsAtomicWhenTheRoleIsMissing` |
| Email inválido | Rechazado **antes** de abrir transacción (vacío, sin `@`, dos `@`, parte local o dominio vacíos, espacios, >254 bytes) | `TestBootstrapRejectsBadArguments` |
| Email repetido | Imposible: no puede haber usuarios previos; con uno, se niega antes | mismos dos de la primera fila |
| Contraseña vacía | Rechazada. También >72 bytes (límite duro de bcrypt), como error de validación y no como fallo de hashing a mitad | `TestBootstrapRejectsBadArguments`, `TestReadPassword` |
| Dos bootstraps concurrentes | **Exactamente uno pasa.** En SQLite gana el lock del archivo; en **PostgreSQL, donde no hay lock, gana la clave primaria del marcador** — que es la garantía que el contrato pedía | `TestConcurrentBootstrapsExactlyOneWins` (4 racers, SQLite), `TestPostgresConcurrentBootstrap` (6 racers, PG 17) |
| Base sin esquema: ¿lo crea o falla? | El **comando** lo crea (`EnsureSchema` + `SeedCatalogs`, las mismas dos llamadas del arranque). `auth.Bootstrap` por sí sola **falla ruidosamente** en vez de fingir "no bootstrappeada" | `TestBootstrapCommandCreatesTheSchemaAndAdministers`, `TestBootstrapOnADatabaseWithoutSchemaFailsLoudly` |

La carrera en PostgreSQL, donde la clave es lo único que separa a los competidores:

```
$ go test -tags dualengine -run TestPostgresConcurrentBootstrap -count=1 -v ./internal/server
    loser: this installation has already been bootstrapped (a concurrent bootstrap claimed it first):
           it was claimed on 2026-07-26T00:42:30.134046100Z by "racer5@example.com" …     (×5)
    EXACTLY ONE WON on PostgreSQL 17: [racer5@example.com]; users=1 marker=1 grants=8
--- PASS: TestPostgresConcurrentBootstrap (10.70s)
```

Los 5 perdedores reciben **el sentinela correcto**, no un error crudo de driver: cuando el `INSERT`
del marcador falla, el código **no compara el texto del mensaje** (los dos motores lo redactan
distinto — la lección que CONTRACT-21 ya pagó). Hace rollback y **vuelve a leer** el marcador desde
el pool; si está, es `ErrAlreadyBootstrapped`. El rollback es obligatorio antes de releer porque
PostgreSQL envenena la transacción abortada (25P02).

---

## Verificación — salida REAL

### build / vet / gofmt

```
$ go build ./...
(sin salida)
$ go vet ./...
(sin salida)
$ gofmt -l .
(sin salida)
```

`go vet -tags dualengine ./...` también limpio (las baterías con tag compilan).

### `go test ./... -count=1`, dos veces

```
$ go test ./... -count=1   # 1/2
ok  github.com/MauricioPerera/librarian/cmd/librarian     2.738s
ok  github.com/MauricioPerera/librarian/internal/auth     6.508s
ok  github.com/MauricioPerera/librarian/internal/config   1.260s
ok  github.com/MauricioPerera/librarian/internal/dual     1.317s
ok  github.com/MauricioPerera/librarian/internal/schema   1.328s
ok  github.com/MauricioPerera/librarian/internal/server  35.868s
ok  github.com/MauricioPerera/librarian/internal/store    3.713s

$ go test ./... -count=1   # 2/2
ok  github.com/MauricioPerera/librarian/cmd/librarian     3.049s
ok  github.com/MauricioPerera/librarian/internal/auth     6.862s
ok  github.com/MauricioPerera/librarian/internal/config   1.336s
ok  github.com/MauricioPerera/librarian/internal/dual     1.335s
ok  github.com/MauricioPerera/librarian/internal/schema   1.488s
ok  github.com/MauricioPerera/librarian/internal/server  37.367s
ok  github.com/MauricioPerera/librarian/internal/store    3.876s
```

### La batería de `internal/auth`

```
--- PASS: TestCleanInstallIsInadministrableBeforeBootstrap (0.06s)
--- PASS: TestBootstrapMakesTheInstallationAdministrable (0.16s)
--- PASS: TestSecondBootstrapIsRefusedAndChangesNothing (0.16s)
--- PASS: TestBootstrapRefusesAnInstallationThatAlreadyHasUsers (0.11s)
--- PASS: TestBootstrapIsAtomicWhenTheRoleIsMissing (0.11s)
--- PASS: TestBootstrapIsAtomicWhenAPermissionIsMissing (0.12s)
--- PASS: TestTheMarkerTableMakesASecondClaimUnrepresentable (0.11s)
--- PASS: TestBootstrapAfterARolledBackAttemptStillWorks (0.17s)
--- PASS: TestConcurrentBootstrapsExactlyOneWins (0.11s)
--- PASS: TestBootstrapRejectsBadArguments (0.53s)
--- PASS: TestBootstrapOnADatabaseWithoutSchemaFailsLoudly (0.00s)
--- PASS: TestSetRolePermissionsStillBehaves (0.07s)
```

`TestCleanInstallIsInadministrableBeforeBootstrap` es la medición de partida re-tomada: sin ella el
resto de la batería podría estar probando algo contra un fixture que nunca estuvo roto.

### Todo lo de contratos anteriores sigue funcionando

Las **cuatro** baterías dual-motor (CONTRACT-19, 20, 21 y la nueva 22) contra el PostgreSQL 17 real:

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5444/postgres?sslmode=disable' \
  go test -tags dualengine -count=1 ./...
ok  github.com/MauricioPerera/librarian/cmd/librarian      2.747s
ok  github.com/MauricioPerera/librarian/internal/auth     48.312s
ok  github.com/MauricioPerera/librarian/internal/config    1.166s
ok  github.com/MauricioPerera/librarian/internal/dual      1.203s
ok  github.com/MauricioPerera/librarian/internal/schema    1.301s
ok  github.com/MauricioPerera/librarian/internal/server  107.184s
ok  github.com/MauricioPerera/librarian/internal/store    31.672s
```

La tabla nueva no rompió nada de lo existente porque `EnsureSchema` es **incremental**: sobre una
base ya desplegada crea solo `bootstrap` y deja intacto todo lo demás. Los tests que cuentan tablas
lo hacen **relativo** a `schema.Build()`, nunca contra un número fijo, así que siguen midiendo lo que
medían. Y al estar en `Build()`, `ReservedNames()` reserva el nombre automáticamente: ningún tipo de
contenido dinámico puede llamarse `bootstrap`.

---

## Criterios de aceptación

- [x] build/vet/gofmt limpios; `go test ./... -count=1` verde **dos veces**.
- [x] T1: operación atómica; probada **forzando dos fallos distintos**, en puntos distintos de la
      transacción.
- [x] T1: imposible de usar dos veces, **con la garantía en la base** (única clave representable) y
      no en el orden de los chequeos — medido sin pasar por el código de la operación, y con
      carreras concurrentes en los dos motores.
- [x] T2: la contraseña **no viaja por línea de comandos** (entrada estándar; un `--password` se
      rechaza explicando por qué); decisión justificada contra flag y contra variable de entorno.
- [x] T3: **escritura gateada por permiso funcionando tras el bootstrap, en los dos motores**, con
      el binario real y salida real.
- [x] `SeedCatalogs` sin cambios de comportamiento (archivo sin tocar).

## Restricciones

- [x] Solo se tocaron archivos dentro de `librarian`. `sqlite-postgres-compat` **sin tocar**.
- [x] Sin dependencias nuevas (`go.mod` intacto). **Ningún permiso nuevo**.
- [x] **NO commiteado**: todo queda en el working tree.
- [x] No se otorgó nada a `editor`/`author`/`contributor` (hay un test que lo verifica en los dos
      motores).
- [x] El contrato público de las rutas HTTP existentes no cambió: no se agregó ni modificó ninguna
      ruta.

---

## Lo que un operador tiene que saber

**Producción hoy no se arregla con esto, y es deliberado.** Tiene usuarios y `role_permissions` fue
reparada a mano en su momento (hueco 1); el bootstrap se **niega** sobre ella y no la toca. Lo que
este contrato garantiza es que **ninguna instalación nueva puede volver a nacer en ese estado**. Si
alguna vez hiciera falta reparar una instalación existente sin acceso administrativo, eso es otra
capacidad (una reparación explícita, con su propio contrato y sus propias guardas) y **no debe**
colarse dentro de esta vía: mezclarlas convertiría el bootstrap en un mecanismo para acuñar
administradores sobre sistemas poblados.

Segundo: el bootstrap se corre **con el servicio abajo y una sola vez**, antes del primer arranque
público. Está documentado en `docs/DEPLOY.md` → *Bootstrap inicial*.

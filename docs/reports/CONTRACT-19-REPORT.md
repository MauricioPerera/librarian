# CONTRACT-19 — `internal/auth` dual-motor (parte 1 de 3)

Base: `0718746` (CONTRACT-01..18 completos y desplegados). Árbol **SIN commitear**, como pide el
contrato: el orquestador commitea y despliega tras verificar.

**Resultado: LISTO, con UNA salvedad de criterio que hay que leer** (§ "Lo que no se pudo cumplir
al pie de la letra"). `internal/auth` ya no tiene una sola sentencia SQL atada a un motor: sus 23
sentencias pasaron a rutinas canónicas (lectura y escritura) o a SQL crudo compuesto con
`compat.Placeholder`. Las funciones públicas devuelven **exactamente lo mismo** contra SQLite real
y contra PostgreSQL 17 real — 65 observaciones idénticas, línea por línea, incluida la que más
importa: **el ORDEN de los listados**.

La aplicación **sigue corriendo solo en SQLite**, como el contrato establece: elegir motor es
CONTRACT-21 y no se adelantó nada de eso.

---

## Resumen de qué se tocó

| Archivo | Qué |
|---|---|
| `go.mod` / `go.sum` | `sqlite-postgres-compat` v0.2.0 → **v0.4.0** |
| `internal/schema/auth_dual.go` (**nuevo**) | T1: 3 vistas + 14 rutinas canónicas |
| `internal/schema/schema.go` | `Build()` ahora devuelve `Views` + `Routines` |
| `internal/auth/dual.go` (**nuevo**) | plomería neutral: UUID v4, instante canónico, constructores de `compat.Value`, `bind`, `dedupe` |
| `internal/auth/users.go`, `roles.go`, `apikey.go` | T2: las 23 sentencias migradas; firmas `*sql.DB` → `*compat.Store` |
| `internal/store/store.go` | `EnsureSchema` crea/recrea las vistas (camino de upgrade de una base ya desplegada) |
| `internal/server/*.go` (6 archivos) | solo acompañar la firma: `h.authStore` |
| `internal/auth/dualengine_contract19_test.go` (**nuevo**) | T3: batería dual-motor (build tag `dualengine`) |
| `internal/store/store_test.go` | test del camino de upgrade de las vistas |
| `docs/OPERATIONS.md` | cómo se corre la batería |

`sqlite-postgres-compat` **no se tocó**. No hizo falta: los tres caminos que el RECON del contrato
declara alcanzaron para todo. Ver § "¿Le falta algo a `compat`?".

---

## Decisiones de diseño (con su porqué)

### 1. La firma nueva: `*compat.Store`, no el motor suelto

**Decisión:** todas las funciones públicas de `auth` que tocan la base pasan de
`db *sql.DB` a `store *compat.Store`.

```go
// antes
func CreateUser(ctx context.Context, db *sql.DB, email, password string, roleNames []string) (*User, error)
// ahora
func CreateUser(ctx context.Context, store *compat.Store, email, password string, roleNames []string) (*User, error)
```

Aplica a las 13 funciones públicas con base: `CreateUser`, `VerifyCredentials`, `ListUsers`,
`GetUser`, `UpdateUserStatus`, `SetUserRoles`, `SetRolePermissions`, `RolePermissions`,
`MintAPIKey`, `RevokeAPIKey`, `RevokeAPIKeyByID`, `ListAPIKeys`, `GetAPIKey`, `VerifyAPIKey`.
`jwt.go` no toca base y **no cambió en absoluto**.

**Por qué `*compat.Store` y no `(db *sql.DB, engine compat.Engine)`:**

1. `QueryRoutine` y `CallRoutine` **son métodos de `*compat.Store`**. Con el par suelto habría que
   reconstruir un `Store` en cada llamada — es decir, tener el `Store` igual, pero armado adentro y
   sin que el llamador vea que existe.
2. Conexión y motor **no pueden ir desapareados nunca**. Un par de argumentos permite pasar el
   `*sql.DB` de PostgreSQL con `compat.SQLite`: compila, y emite `?` contra PostgreSQL. El `Store`
   hace ese error inexpresable.
3. `Store.IsUniqueViolation` queda disponible sin ampliar la firma otra vez si un contrato
   posterior necesita clasificar duplicados (hoy `auth` no clasifica; no se agregó).

**Por qué NO se derivó el motor del `*sql.DB`:** técnicamente se puede mirar `db.Driver()` y
distinguir `modernc.org/sqlite` de `pgx`. Eso es exactamente la ramificación por motor **en el
consumidor** que esta capa existe para eliminar, y `compat` no expone nada así. Se descartó.

**Costo, dicho sin adornos:** es un cambio de firma que rompe compilación en todo llamador,
incluidos los tests. Ver § "Lo que no se pudo cumplir al pie de la letra".

### 2. La división en tres caminos: la tabla del contrato, sin reabrir

Se siguió la tabla del RECON tal cual. Para el registro, la razón por la que las tres operaciones
de conjunto variable **no pueden** ir por rutina, verificada contra el código de `compat` v0.4.0:

- `CallRoutine` (`compat/runtime.go:15`) abre `store.DB.BeginTx` **adentro** y commitea al final:
  no se puede anidar en una transacción del consumidor, y dos llamadas son dos transacciones.
- Una `Routine` tiene `Actions []RoutineAction` **estática**: no hay forma de expresar "N inserts
  donde N viene de un argumento".

`CreateUser`, `SetUserRoles` y `SetRolePermissions` son cada una un `DELETE`/`INSERT` maestro más N
`INSERT` de junción que **tienen que commitear juntos** — en `SetRolePermissions` esa atomicidad es
una garantía deliberada de CONTRACT-16, con test propio. Van por SQL crudo con `compat.Placeholder`
dentro de la transacción que la función abre. **No se intentó forzarlas a rutina.**

Efecto colateral coherente: `roleIDsForNames` y `permissionIDsForNames` **también** quedaron en SQL
crudo, aunque son lecturas. Existen rutinas equivalentes declaradas
(`auth_role_id_by_name`, `auth_permission_id_by_name`) pero no se usan desde ahí: leer con
`QueryRoutine` abriría **otra** transacción, y la garantía "resolver ANTES de mutar" pasaría a leer
el catálogo fuera de la transacción que está por borrar y reinsertar.

### 3. `RETURNING id` → UUID generado en Go

Resuelto como manda el contrato: `newUUID()` (crypto/rand, RFC 4122 v4, `internal/auth/dual.go`)
genera el id y se pasa como un valor más. El `DEFAULT gen_random_uuid()` de la columna **se
conserva** como red de seguridad para escrituras que no vengan de la app.

**Se extendió a `api_keys.id`, y no fue por gusto:** la acción `insert` de una rutina exige *todas*
las columnas no generadas de la tabla — `compat/store.go:240` responde
`import api_keys: missing column "id"` si falta una. Es decir, `MintAPIKey` no puede apoyarse en el
`DEFAULT` yendo por `CallRoutine`. Se generó el id en Go, igual que el de usuario. `revoked_at` se
declara explícitamente como literal `null` en la rutina.

Sin dependencia nueva: `newUUID` usa `crypto/rand` de la stdlib (`github.com/google/uuid` está en
el grafo como indirecta de `compat`, pero no se promovió a directa).

### 4. `CURRENT_TIMESTAMP` → instante pasado como parámetro… **y también en `created_at`**

El contrato manda pasar el instante como parámetro en los tres `UPDATE ... SET x = CURRENT_TIMESTAMP`
(`users.go:243`, `apikey.go:57`, `apikey.go:80`), en formato canónico RFC3339Nano. Hecho.

**Se hizo además en `api_keys.created_at`, que el contrato no enumeraba porque ahí el
`CURRENT_TIMESTAMP` no está en el SQL de `auth` sino en el `DEFAULT` de la columna.** La razón es la
pregunta de red-team del orden, y está medida, no supuesta. Sonda contra los dos motores reales
(insert apoyándose en los `DEFAULT`, exactamente la forma que `MintAPIKey` tenía antes):

```
[sqlite]   DEFAULT gen_random_uuid()   -> "03d18e78-bc9f-4149-a40e-1d61195228c4"
[sqlite]   DEFAULT CURRENT_TIMESTAMP   -> "2026-07-25 18:38:25"
[postgres] DEFAULT gen_random_uuid()   -> "00d0f63b-15f8-490e-9fba-2448ff92261c"
[postgres] DEFAULT CURRENT_TIMESTAMP   -> "2026-07-25 18:38:26.471192+00"
```

`created_at` es la clave primaria de orden de `ListAPIKeys` (`ORDER BY created_at DESC, label`) y es
`TEXT` en ambos motores. Con esos dos textos, el orden por texto **no es el mismo**: en SQLite
(resolución de segundo) dos claves acuñadas en el mismo segundo empatan y desempata `label`; en
PostgreSQL nunca empatan. Eso es exactamente la divergencia "rompe en producción sin romper ningún
test" que la sección red-team señala. Escribiendo el instante desde la aplicación en RFC3339Nano —
que es la forma que `compat` **declara** para la familia `timestamp`, no un formato inventado — el
texto es idéntico en los dos motores y el orden también. El `DEFAULT` de la columna queda como red
de seguridad.

Consecuencia observable (menor, documentada abajo): `APIKeyRecord.CreatedAt` / `RevokedAt` ahora
son RFC3339Nano. Ya lo eran de hecho al leerlos, porque `QueryRoutine` canonicaliza todo `timestamp`
a RFC3339Nano UTC (`compat/store.go:423`); lo que cambia es también el texto **almacenado**.

### 5. `ON CONFLICT DO NOTHING` → **eliminado**, conflicto imposible por construcción

El contrato pide comprobar, no asumir. Comprobado: los tres `INSERT ... ON CONFLICT DO NOTHING`
vienen inmediatamente después de un `DELETE` que vacía el conjunto destino
(`SetUserRoles`, `SetRolePermissions`) o insertan en una junción de un usuario recién creado
(`CreateUser`). El **único** conflicto posible con la PK compuesta es un nombre repetido en la lista
del llamador — que CONTRACT-16 documenta como inofensivo y tiene test
(`TestSetRolePermissionsIsIdempotentOnRepeats`).

Solución: `dedupe()` sobre los ids resueltos (`internal/auth/dual.go`), preservando el orden de
primera aparición. El conflicto pasa a ser **imposible**, no "tolerado", y desaparece la dependencia
de una cláusula upsert cuya forma aceptada varía por motor y por versión. El comportamiento
observable es idéntico: el test existente sigue verde sin tocarse, y la batería dual-motor lo
confirma en los dos motores (`setUserRoles duplicates` → `roles after duplicates=[author]`,
`setRolePermissions duplicates` → `[content.create]`).

### 6. `UpdateUserStatus`: el `ErrUserNotFound` sin `RowsAffected`

`CallRoutine` devuelve **solo `error`**: no hay `RowsAffected`. La implementación anterior decidía
`ErrUserNotFound` por `n == 0`. Ahora lee primero la fila (`auth_user_by_id`, `QueryRoutine`) y, si
existe, ejecuta el `UPDATE` por `CallRoutine`. El resultado observable es el mismo (la versión
previa reportaba `ErrUserNotFound` exactamente cuando ninguna fila casaba el id) y el `UPDATE` sigue
siendo **una** sentencia en **una** transacción: no se partió nada que antes fuera atómico. La
ventana entre la lectura y el update no es observable por ningún llamador ni por ningún test, y era
igual de inobservable antes.

`RevokeAPIKey` / `RevokeAPIKeyByID` **sí** ignoraban `RowsAffected` (la revocación es idempotente
por diseño), así que ahí no cambió nada más que el camino.

### 7. Las vistas: recrearlas en cada arranque, no "crearlas si faltan"

Este es el punto donde el contrato podía romper una instancia **ya desplegada** y merece explicación.

`EnsureSchema` sólo aplicaba las **tablas** que faltaban, y salía temprano si no faltaba ninguna. En
una base desplegada no falta ninguna tabla ⇒ las vistas nuevas **nunca se crearían** y toda lectura
de `auth` fallaría después del deploy. Cambio hecho:

- el early-return se eliminó; el `ApplySchema` de tablas faltantes queda condicionado a que haya
  alguna;
- `applyViews` hace `DROP VIEW IF EXISTS` + el `CREATE VIEW` que `compat.CompileDDL` compila para el
  motor, todo en una transacción. Una vista es una **definición sin estado**: recrearla no destruye
  datos y es lo que hace que un cambio de definición tenga efecto;
- el `writeFullSchemaMetadata` con el esquema canónico COMPLETO pasa a correr **siempre**, no solo
  cuando se crearon tablas: es lo que mantiene `__compat_schema` (y por lo tanto `--dump-schema` /
  `compat copy`) diciendo la verdad ahora que el esquema incluye vistas y rutinas.

`store.ApplySchema` no se usa para las vistas a propósito: sobrescribiría `__compat_schema` con un
esquema "solo vistas".

**El único SQL escrito a mano fuera de `auth` es `DROP VIEW IF EXISTS "nombre"`**, porque `compat`
no compila esa forma (`CompileDDL` solo crea; `DROP TABLE` tiene entrada propia). Es
byte-idéntico en ambos motores, el nombre es una constante de compilación de `schema.Build()` y se
cita con la misma regla que `compat` usa. Cubierto por `TestEnsureSchemaRecreatesViews`.

### 8. `internal/server`: el mínimo, y nada más

`Deps` **no cambió** (sigue tomando `DB *sql.DB`), así que ningún test que arma un `server.Deps`
tuvo que tocarse. `handlers` gana un campo `authStore *compat.Store` que `NewMux` compone con
`schema.SQLiteTarget` — que hoy es la verdad literal: librarian corre en SQLite. **Esa es la única
línea que CONTRACT-21 tendrá que cambiar.** Las 14 llamadas a `auth.*` pasaron de `h.db` a
`h.authStore`; `resolveIdentity` cambió el tipo de un parámetro. Cero cambios de comportamiento,
cero cambios de rutas, cero cambios en `permissionsForRoles`/`permissionsForRoleID` (siguen con `?`
— son de CONTRACT-20).

---

## Qué se implementó, por tarea

### T1 — Vistas y rutinas en el esquema canónico (`internal/schema/auth_dual.go`)

**3 vistas**, una por cada JOIN escrito a mano que había en `auth` (los de `apikey.go:109` y `:139`
son la MISMA vista con distinto `WHERE`, como el contrato anticipa):

| Vista | Reemplaza | Columnas expuestas |
|---|---|---|
| `user_role_names` | `users.go:336` | `user_id`, `role_name` |
| `role_permission_names` | `roles.go:108` | `role_id`, `permission_name` |
| `api_key_records` | `apikey.go:109` y `:139` | `id`, `label`, `role_name`, `created_at`, `revoked_at` |

`api_key_records` **no proyecta `key_hash`**: el hash no puede llegar a una lectura ni por error.

DDL real emitido, idéntico en los dos motores:

```sql
CREATE VIEW "user_role_names" AS SELECT "ur"."user_id" AS "user_id", "r"."name" AS "role_name" FROM "user_roles" AS "ur" INNER JOIN "roles" AS "r" ON ("r"."id" = "ur"."role_id")
CREATE VIEW "role_permission_names" AS SELECT "rp"."role_id" AS "role_id", "p"."name" AS "permission_name" FROM "role_permissions" AS "rp" INNER JOIN "permissions" AS "p" ON ("p"."id" = "rp"."permission_id")
CREATE VIEW "api_key_records" AS SELECT "k"."id" AS "id", "k"."label" AS "label", "r"."name" AS "role_name", "k"."created_at" AS "created_at", "k"."revoked_at" AS "revoked_at" FROM "api_keys" AS "k" INNER JOIN "roles" AS "r" ON ("r"."id" = "k"."role_id")
```

**14 rutinas** (10 de lectura vía `QueryRoutine`, 4 de escritura vía `CallRoutine`):

| Rutina | Tipo | Relación | Orden declarado |
|---|---|---|---|
| `auth_user_credentials_by_email` | select | `users` | — |
| `auth_user_by_id` | select | `users` | — |
| `auth_list_users` | select | `users` | `email ASC` |
| `auth_user_role_names` | select | vista `user_role_names` | `role_name ASC` |
| `auth_role_id_by_name` | select | `roles` | — |
| `auth_permission_id_by_name` | select | `permissions` | — |
| `auth_role_permission_names` | select | vista `role_permission_names` | `permission_name ASC` |
| `auth_list_api_keys` | select | vista `api_key_records` | `created_at DESC, label ASC` |
| `auth_api_key_by_id` | select | vista `api_key_records` | — |
| `auth_api_key_by_hash` | select | `api_keys` | — |
| `auth_insert_api_key` | insert | `api_keys` | — |
| `auth_revoke_api_key_by_hash` | update | `api_keys` | — |
| `auth_revoke_api_key_by_id` | update | `api_keys` | — |
| `auth_update_user_status` | update | `users` | — |

Cada acción `select` **declara sus columnas de salida con su familia** (`uuid`/`text`/`timestamp`),
que es lo que hace que el escaneo sea idéntico en los dos motores. **Todo listado declara `ORDER BY`
aunque no lleve `LIMIT`/`OFFSET`** (donde `compat` solo lo haría obligatorio): sin orden total los
dos motores pueden devolver las mismas filas en secuencia distinta. No hay `LIMIT`/`OFFSET` en
ninguna rutina de este contrato.

Las tres operaciones atómicas de conjunto variable **no tienen rutina**, a propósito y con el porqué
escrito en el código.

Vistas y rutinas viajan con el esquema canónico: están en `Build()`, así que `--dump-schema` /
`compat copy` las llevan, y el round-trip JSON existente (`TestSchemaRoundTripJSON`) las cubre sin
tocarlo.

### T2 — Migrar `internal/auth`

**Cero `?` literales.** Cada sentencia va por rutina o se compone con `compat.Placeholder`
(envuelto en el helper local `bind`, que es un nombre, no una segunda implementación).

Mapa de las 23 sentencias:

| Función | Sentencias | Camino |
|---|---|---|
| `CreateUser` | INSERT users, SELECT roles ×N, INSERT user_roles ×N | **SQL crudo + Placeholder**, en su transacción |
| `VerifyCredentials` | SELECT users | `QueryRoutine` |
| `ListUsers` | SELECT users | `QueryRoutine` |
| `GetUser` | SELECT users | `QueryRoutine` |
| `rolesForUser` (JOIN) | SELECT user_roles⋈roles | `QueryRoutine` sobre vista |
| `UpdateUserStatus` | UPDATE users (+ SELECT de existencia) | `CallRoutine` (+ `QueryRoutine`) |
| `SetUserRoles` | SELECT users, SELECT roles ×N, DELETE user_roles, INSERT ×N | **SQL crudo + Placeholder**, en su transacción |
| `SetRolePermissions` | SELECT roles, SELECT permissions ×N, DELETE role_permissions, INSERT ×N | **SQL crudo + Placeholder**, en su transacción |
| `RolePermissions` | SELECT roles, SELECT role_permissions⋈permissions | `QueryRoutine` (×2, la 2ª sobre vista) |
| `MintAPIKey` | INSERT api_keys | `CallRoutine` |
| `RevokeAPIKey` | UPDATE api_keys | `CallRoutine` |
| `RevokeAPIKeyByID` | UPDATE api_keys | `CallRoutine` |
| `ListAPIKeys` | SELECT api_keys⋈roles | `QueryRoutine` sobre vista |
| `GetAPIKey` | SELECT api_keys⋈roles | `QueryRoutine` sobre vista |
| `VerifyAPIKey` | SELECT api_keys | `QueryRoutine` |

`jwt.go`: sin cambios (no tiene SQL).

Comportamiento público: mismos valores, mismos errores centinela, mismas garantías de atomicidad.
Los tests existentes lo confirman sin que se les tocara **ni una aserción** (§ siguiente).

### T3 — Verificación

Batería dual-motor en `internal/auth/dualengine_contract19_test.go`, build tag `dualengine`
(mismo patrón que `exportfixture` de CONTRACT-04). Sin `COMPAT_POSTGRES_DSN` **saltea**, no pasa en
falso.

```
COMPAT_POSTGRES_DSN='postgres://usuario:***@host:puerto/db?sslmode=disable' \
  go test -tags dualengine -run TestDualEngineAuth -count=1 -v ./internal/auth
```

Cómo prueba la igualdad: cada motor corre el **mismo** escenario, que va anexando una línea por
OBSERVACIÓN — valores devueltos, centinela de error, y sobre todo el **ORDEN** de cada listado. Los
dos transcripts tienen que ser idénticos línea a línea. Lo intrínsecamente aleatorio (uuids,
secretos, timestamps) no se registra a propósito; lo que se registra es todo lo que un llamador de
este paquete puede observar. Una divergencia de orden, de test de NULL o de qué error volvió **falla
acá**, que es justo la clase de bug que sobrevive a un test que compara conjuntos.

- Lado SQLite: camino de **producción** (`store.Open` → `store.EnsureSchema` → `store.SeedCatalogs`),
  así que la batería también prueba que el arranque real crea las vistas.
- Lado PostgreSQL: **schema propio y único por corrida** (`librarian_c19_<nanos>`), `ApplySchema`
  del esquema canónico completo, `DROP SCHEMA … CASCADE` al terminar. `search_path` con `public`
  en segundo lugar solo para resolver el tipo `vector` de pgvector; se verifica con
  `SELECT current_schema()` que el aislamiento es real antes de crear nada.

---

## Verificación — salida REAL

### `go.mod`, `go build`, `go vet`, `gofmt`

```
=== go.mod ===
	github.com/MauricioPerera/sqlite-postgres-compat v0.4.0
=== go build ./... ===
(sin salida = OK)
=== go vet ./... ===
(sin salida = OK)
=== gofmt -l . ===
(sin salida = OK)
```

Con los build tags también:

```
$ go vet -tags exportfixture ./internal/server && go vet -tags dualengine ./internal/auth && echo VET-OK
VET-OK
```

### Cero `?` literales en `internal/auth`

```
$ grep -n '?' internal/auth/users.go internal/auth/roles.go internal/auth/apikey.go internal/auth/jwt.go internal/auth/dual.go
internal/auth/users.go:9:// `?` or `$n`. A bare *sql.DB cannot answer "which engine is this?", and asking
internal/auth/dual.go:5:// Before this contract every statement in `auth` was hand-written SQL with `?`
internal/auth/dual.go:121:// place in this package that knows about `?` vs `$n` — compat's.
```

Las tres ocurrencias son **comentarios** que explican precisamente esto. Ninguna sentencia.

(Prueba independiente y más fuerte que el grep: la mitad PostgreSQL de la batería pasa. Un `?`
suelto sería error de sintaxis en PostgreSQL, y un `$1` lo sería en SQLite.)

### `go test ./... -count=1`, dos veces

```
=== RUN 1 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.269s
ok  	github.com/MauricioPerera/librarian/internal/auth	4.176s
ok  	github.com/MauricioPerera/librarian/internal/config	0.670s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.300s
ok  	github.com/MauricioPerera/librarian/internal/server	32.403s
ok  	github.com/MauricioPerera/librarian/internal/store	3.399s
=== RUN 2 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.292s
ok  	github.com/MauricioPerera/librarian/internal/auth	4.219s
ok  	github.com/MauricioPerera/librarian/internal/config	0.652s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.330s
ok  	github.com/MauricioPerera/librarian/internal/server	32.527s
ok  	github.com/MauricioPerera/librarian/internal/store	3.487s
```

### La batería dual-motor — SQLite real + PostgreSQL 17 real

Motor destino verificado en vivo antes de empezar:
`PostgreSQL 17.10 (Debian 17.10-1.pgdg12+1) on x86_64-pc-linux-gnu`, extensión `vector` presente.

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5442/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineAuth -count=1 -v ./internal/auth
=== RUN   TestDualEngineAuth
    dualengine_contract19_test.go:66: transcript (65 lines, identical on both engines):
        createUser zeta email=zeta@example.com roles=[author]
        createUser alpha email=alpha@example.com roles=[editor administrator]
        verify alpha correct err=<none> email=alpha@example.com roles=[administrator editor]
        verify alpha wrong err=ErrInvalidCredentials
        verify ghost err=ErrInvalidCredentials
        verify messages-identical=true
        listUsers err=<none> order=[alpha@example.com zeta@example.com]
        listUsers row email=alpha@example.com status=active roles=[administrator editor]
        listUsers row email=zeta@example.com status=active roles=[author]
        getUser alpha found=true err=<none> email=alpha@example.com status=active roles=[administrator editor]
        getUser missing-uuid found=false err=<none>
        getUser malformed found=false err=<none>
        updateStatus suspended err=<none>
        verify suspended-with-correct-password err=ErrInvalidCredentials
        updateStatus banned err=ErrInvalidStatus
        updateStatus missing-id err=ErrUserNotFound
        status after rejected values=suspended
        updateStatus active err=<none>
        verify reactivated err=<none>
        schema CHECK rejects raw banned status=true
        setUserRoles replace err=<none>
        roles after replace=[author contributor]
        setUserRoles unknown err=ErrUnknownRole
        roles after unknown-rejected=[author contributor]
        setUserRoles duplicates err=<none>
        roles after duplicates=[author]
        setUserRoles empty err=<none>
        roles after empty=[] len=0
        setUserRoles missing-user err=ErrUserNotFound
        setRolePermissions grant err=<none>
        rolePermissions editor found=true err=<none> order=[content.create content.update]
        setRolePermissions replace err=<none>
        rolePermissions after replace order=[content.publish terms.manage]
        setRolePermissions atomic-abort err=ErrUnknownPermission set-unchanged=[content.publish terms.manage]
        setRolePermissions unknown-role err=ErrRoleNotFound
        setRolePermissions duplicates err=<none>
        rolePermissions contributor after duplicates=[content.create]
        setRolePermissions empty err=<none>
        rolePermissions editor after empty=[] len=0
        rolePermissions ghost found=false err=<none>
        verifyAPIKey fresh err=<none> label=c-first-minted role-matches=true
        verifyAPIKey bogus err=ErrAPIKeyRejected
        verifyAPIKey empty err=ErrAPIKeyRejected
        listAPIKeys order=[a-third-minted b-second-minted c-first-minted]
        listAPIKeys row label=a-third-minted role=editor revoked=false
        listAPIKeys row label=b-second-minted role=administrator revoked=false
        listAPIKeys row label=c-first-minted role=editor revoked=false
        revokeAPIKey by-secret err=<none>
        verifyAPIKey after-revoke err=ErrAPIKeyRejected
        revokeAPIKey by-secret again err=<none>
        verifyAPIKey after-double-revoke err=ErrAPIKeyRejected
        revoked key still listed=true revoked=true revoked-at-present=true
        revokeAPIKeyByID err=<none>
        verifyAPIKey b after-revoke-by-id err=ErrAPIKeyRejected
        revokeAPIKeyByID again err=<none>
        revokeAPIKeyByID unknown err=<none>
        verifyAPIKey untouched-key err=<none>
        getAPIKey found=true err=<none> label=b-second-minted role=administrator revoked=true revoked-at-present=true
        getAPIKey missing found=false err=<none>
        listAPIKeys final order=[a-third-minted b-second-minted c-first-minted]
        listAPIKeys final row label=a-third-minted role=editor revoked=false
        listAPIKeys final row label=b-second-minted role=administrator revoked=true
        listAPIKeys final row label=c-first-minted role=editor revoked=true
        created_at canonical-rfc3339nano=true
        verify zeta err=<none> roles=[author]
    dualengine_contract19_test.go:80: OK: 65 observations identical on SQLite and PostgreSQL 17
--- PASS: TestDualEngineAuth (23.55s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/auth	25.957s
```

Lo que el contrato pedía explícitamente, señalado en ese transcript:

- **crear usuario y verificar credenciales** → `createUser zeta/alpha`, `verify alpha correct`.
- **asignar roles y reasignar un conjunto distinto (reemplazo, no acumulación)** →
  `createUser alpha roles=[editor administrator]` … `setUserRoles replace` →
  `roles after replace=[author contributor]`. Los dos anteriores desaparecieron.
- **otorgar permisos a un rol y reemplazarlos** →
  `rolePermissions editor … order=[content.create content.update]` →
  `rolePermissions after replace order=[content.publish terms.manage]`.
- **acuñar API key, verificar, revocar y confirmar que deja de verificar** →
  `verifyAPIKey fresh err=<none>` → `revokeAPIKey by-secret` → `verifyAPIKey after-revoke
  err=ErrAPIKeyRejected`. Y por id: `revokeAPIKeyByID` → `verifyAPIKey b after-revoke-by-id
  err=ErrAPIKeyRejected`.
- **listar, que es donde el orden importa** → `listUsers order=[alpha@… zeta@…]` y
  `listAPIKeys order=[a-third-minted b-second-minted c-first-minted]`, idénticos en ambos motores.
- **atomicidad de `SetRolePermissions` con un permiso inexistente** →
  `setRolePermissions atomic-abort err=ErrUnknownPermission
  set-unchanged=[content.publish terms.manage]`: el conjunto anterior quedó intacto, ni el `DELETE`
  ni el insert parcial sobrevivieron. **En los dos motores.**

### Camino de upgrade de una base ya desplegada (las vistas)

```
$ go test ./internal/store -run TestEnsureSchemaRecreatesViews -count=1 -v
=== RUN   TestEnsureSchemaRecreatesViews
--- PASS: TestEnsureSchemaRecreatesViews (0.03s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/store	2.387s
```

Simula exactamente el riesgo: base con TODAS las tablas y SIN las vistas (se borran a mano), se
corre `EnsureSchema` y las vistas tienen que volver; una tercera corrida tiene que ser no-op sin
"view already exists"; y cada vista tiene que quedar consultable.

### Contratos anteriores: siguen funcionando igual

La suite completa (`cmd/librarian`, `internal/{auth,config,schema,server,store}`) pasa verde dos
veces, incluidos los ~30 archivos de test de `internal/server` que ejercitan las rutas HTTP y la UI
de CONTRACT-01..18 de punta a punta. **El contrato público de las rutas HTTP no cambió en absoluto.**
El round-trip JSON del esquema (`TestSchemaRoundTripJSON`) pasa con las vistas y rutinas nuevas
adentro, que es lo que garantiza que `--dump-schema` sigue siendo generable y fiel.

---

## Red-team: las preguntas del contrato, respondidas con tests

| Pregunta | Respuesta | Evidencia |
|---|---|---|
| **¿El orden de `ListUsers`/`ListAPIKeys` es el mismo en ambos motores? ¿Hay `ORDER BY` explícito o se depende del orden natural?** | Hay `ORDER BY` **declarado en la rutina**, no en el código. `ListUsers`: `email ASC` (email es UNIQUE ⇒ orden **total**). `ListAPIKeys`: `created_at DESC, label ASC`, y `created_at` se escribe desde la app en RFC3339Nano justamente para que el orden por texto sea el mismo en los dos motores (§ decisión 4). Además `rolesForUser` y `RolePermissions` ganaron orden explícito, que antes no tenían. | `listUsers order=[…]` y `listAPIKeys order=[…]` idénticos; etiquetas elegidas para que el orden alfabético sea el INVERSO del de creación, y `time.Sleep` entre acuñaciones para que la clave primaria de orden decida sola |
| **¿`revoked_at IS NULL` se comporta igual?** | Sí. El `WHERE` de las rutinas de revocación lleva `is_null`, que `compat` compila a `IS NULL` en ambos; y la lectura decide "revocada" por el **KIND canónico** `NullValue`, no por texto vacío. Probado en las dos direcciones: revocar dos veces es no-op, y una clave NO revocada queda intacta después de revocar otras dos. | `verifyAPIKey after-double-revoke`, `revokeAPIKeyByID unknown err=<none>`, `verifyAPIKey untouched-key err=<none>` |
| **¿El `status` con su `CHECK` sigue rechazando lo mismo?** | Sí, en las dos capas y en los dos motores: `auth` rechaza antes de tocar SQL (`ErrInvalidStatus`) y el `CHECK` del esquema rechaza un `UPDATE` crudo que lo saltee. | `updateStatus banned err=ErrInvalidStatus`, `status after rejected values=suspended`, `schema CHECK rejects raw banned status=true` |
| **¿Qué pasa si `SetUserRoles` recibe lista vacía?** | Lo que hacía HOY: **borra todo y devuelve `nil`**, no es error. Preservado tal cual. | `setUserRoles empty err=<none>` → `roles after empty=[] len=0` (y el test existente `TestSetUserRoles` sin tocar) |
| **¿Y si un nombre de rol no existe?** | `ErrUnknownRole`, con la tabla **intacta** — se resuelve ANTES de mutar, dentro de la transacción. Igual para permisos (`ErrUnknownPermission`) y para rol inexistente en `SetRolePermissions` (`ErrRoleNotFound`). | `setUserRoles unknown err=ErrUnknownRole` → `roles after unknown-rejected=[author contributor]` (sin cambio) |
| **Extra: ¿nombre repetido en la lista, ahora sin `ON CONFLICT`?** | Sigue siendo inofensivo: `dedupe` lo hace imposible en vez de tolerado. | `roles after duplicates=[author]`, `rolePermissions contributor after duplicates=[content.create]`, y `TestSetRolePermissionsIsIdempotentOnRepeats` sin tocar |
| **Extra: anti-enumeración** | El mensaje de "usuario inexistente" y el de "password incorrecto" siguen siendo byte-idénticos, y un usuario suspendido con password correcto devuelve el mismo error. | `verify messages-identical=true`, `verify suspended-with-correct-password err=ErrInvalidCredentials` |

---

## Lo que NO se pudo cumplir al pie de la letra

**Un criterio de aceptación es literalmente inalcanzable, y hay que decirlo:**

> T2: … tests existentes verdes **sin modificar**.

en combinación con el RECON, que decide:

> Las firmas públicas de `auth` toman `db *sql.DB`… **Cambiar esas firmas es parte del contrato**.

Los tests del paquete `auth` **son llamadores**: su helper `openDB` está declarado como
`func openDB(t *testing.T) (*sql.DB, func())` y pasa ese `*sql.DB` a `auth.CreateUser` y compañía.
Cualquier cambio de firma les rompe la **compilación**. No existe una firma que dé el motor a `auth`
y a la vez siga aceptando un `*sql.DB` (la única forma sería mirar el driver, que es la
ramificación por motor prohibida). Los dos requisitos son mutuamente excluyentes.

**Qué se hizo:** ganó el RECON (que el contrato marca como decidido y no reabrible), y se hizo la
adaptación **mínima concebible** en los tests de `auth`: el helper `openDB` devuelve el
`*compat.Store` que ya tenía en la mano, y los tres helpers de SQL directo
(`roleID`, `userID`, `grantedPerms`) cambian el tipo del parámetro y usan `.DB`. Diff exacto:
**31 líneas en 3 archivos**, de las cuales **cero** son aserciones y **cero** son llamadas a
`auth.*` — esas líneas quedaron carácter por carácter iguales, porque la variable sigue llamándose
`db`.

```
 internal/auth/auth_test.go             | 13 +++++++------   (openDB + roleID + import)
 internal/auth/roles_contract16_test.go | 12 ++++++------   (grantedPerms + 3 QueryRow directos)
 internal/auth/users_contract08_test.go |  6 +++---        (userID + import)
```

Ni una sola aserción, mensaje de error esperado, valor esperado o caso de prueba fue tocado. En
otras palabras: **el contrato de comportamiento que esos tests fijan quedó intacto**; lo único que
cambió es el tipo del handle que el andamiaje les pasa. Si el orquestador prefiere otra resolución
(p.ej. mantener wrappers `*sql.DB` que asuman SQLite), es una decisión suya: yo elegí no hacerlo
porque un wrapper que asume el motor es exactamente la mentira que este contrato existe para borrar.

En `internal/server` los tests sí se tocaron un poco más (13 + 11 llamadas envueltas en un helper
`storeFor(db)` de una línea, más el helper mismo): el contrato solo protege los tests del paquete
`auth`, y ahí también las aserciones quedaron intactas.

---

## ¿Le falta algo a `compat`? — No

Se evaluó honestamente durante la implementación. Los tres huecos candidatos y por qué **ninguno**
justifica tocar el paquete:

1. **`CallRoutine` no devuelve `RowsAffected`.** Resuelto en el consumidor sin perder
   comportamiento (§ decisión 6). Agregarlo cambiaría una firma con consumidores en producción para
   un caso que el consumidor resuelve con una lectura.
2. **No hay `CompileDropView`.** `DROP VIEW IF EXISTS "x"` es byte-idéntico en ambos motores y sin
   parámetros: no hay nada que `compat` pueda hacer mejor que el consumidor. Es una línea, en un
   solo lugar (`internal/store/store.go`), con el nombre proveniente de una constante de
   compilación.
3. **La acción `insert` exige todas las columnas.** Es una decisión de diseño de `compat`
   (declarar en vez de adivinar), no una carencia: obliga a que la app conozca el valor, que es
   justo lo que el contrato ya pedía para el `id`.

**`sqlite-postgres-compat` no se modificó. `git status` en ese repo queda como estaba.**

---

## Cambios de comportamiento que el orquestador debe conocer

Ninguno rompe una ruta HTTP ni un test, pero son observables y hay que saberlos antes de desplegar:

1. **Formato de los timestamps que `auth` escribe.** `api_keys.created_at`, `api_keys.revoked_at` y
   `users.updated_at` pasan de `"2026-07-25 18:38:25"` (SQLite `CURRENT_TIMESTAMP`) a
   `"2026-07-25T18:38:25.123456789Z"` (RFC3339Nano, la forma canónica de la familia `timestamp`).
   Filas **preexistentes** conservan su formato viejo; ambos son parseables por `compat`
   (`timestampFormats` acepta los dos), y toda lectura vía rutina los **normaliza** a RFC3339Nano,
   así que la UI ve un formato consistente. Consecuencia a tener presente: en un listado con filas
   viejas y nuevas, el orden por texto de `created_at` pone todas las nuevas antes que todas las
   viejas (`'T'` > `' '`), que en la práctica coincide con "más nuevas primero" pero no está
   garantizado por la fecha. Si eso molesta, se normaliza con un UPDATE de una línea al desplegar.
2. **`APIKeyRecord.CreatedAt` / `RevokedAt`** ahora son siempre RFC3339Nano (antes venían crudos del
   driver). La UI los muestra tal cual; ningún test asertaba el formato.
3. **`EnsureSchema` recrea las 3 vistas en cada arranque** y **siempre** reescribe
   `__compat_schema` con el esquema canónico completo (antes salía temprano si no faltaban tablas).
   Es lo que hace que un deploy sobre una base existente funcione.
4. **Los ids de `users` y `api_keys` los genera la app**, no el `DEFAULT`. El `DEFAULT
   gen_random_uuid()` sigue declarado y sigue funcionando para escrituras externas.

---

## Cómo se corre la batería (también en `docs/OPERATIONS.md`)

```powershell
$env:COMPAT_POSTGRES_DSN = "postgres://usuario:***@host:puerto/db?sslmode=disable"
go test -tags dualengine -run TestDualEngineAuth -count=1 -v ./internal/auth
```

Requiere `pgvector` en el destino (el esquema canónico declara `articles.embedding vector(1536)`).
Crea y destruye su propio schema PostgreSQL por corrida; no deja nada atrás.

---

## Archivos tocados

**Nuevos**

- `internal/schema/auth_dual.go` — vistas y rutinas canónicas (T1)
- `internal/auth/dual.go` — plomería neutral del paquete
- `internal/auth/dualengine_contract19_test.go` — batería dual-motor (tag `dualengine`)
- `docs/reports/CONTRACT-19-REPORT.md` — este informe

**Modificados**

- `go.mod`, `go.sum` — compat v0.4.0
- `internal/schema/schema.go` — `Build()` publica `Views` y `Routines`
- `internal/auth/users.go`, `internal/auth/roles.go`, `internal/auth/apikey.go` — T2
- `internal/store/store.go` — `applyViews` + metadata siempre completa
- `internal/store/store_test.go` — `TestEnsureSchemaRecreatesViews`
- `internal/server/server.go`, `authz.go`, `ui.go`, `ui_apikeys.go`, `ui_roles.go`, `ui_users.go` —
  solo acompañar la firma
- tests de `internal/auth` (3 archivos, 31 líneas de andamiaje) y de `internal/server`
  (7 archivos, llamadas envueltas en `storeFor`)
- `docs/OPERATIONS.md` — sección de la batería dual-motor

**NO tocados**

- `internal/auth/jwt.go` (no tiene SQL)
- `sqlite-postgres-compat` (todo el repo)
- `store.Open` y cualquier elección de motor (CONTRACT-21)
- `server.Deps` y el contrato público de las rutas HTTP

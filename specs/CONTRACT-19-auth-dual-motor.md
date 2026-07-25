# Contrato 19 — `internal/auth` dual-motor (parte 1 de 3)

Prerrequisitos: `CONTRACT-01`..`CONTRACT-18` completos y desplegados.
Requiere `sqlite-postgres-compat` **v0.4.0** (hoy `go.mod` pide v0.2.0 — subirla es parte de esto).

## Por qué existe esta serie

`librarian` promete —heredado de `sqlite-postgres-compat`— arrancar en SQLite/libSQL y **migrar a
PostgreSQL cuando el crecimiento lo exija, sin apagar la aplicación**. Hoy esa promesa **no se
cumple**: exportar el esquema y los datos funciona y está verificado por digest contra un
PostgreSQL real, pero la aplicación no puede servir desde el destino. La causa son 61 consultas
escritas a mano con placeholders `?`, que es la sintaxis de SQLite; el driver de PostgreSQL exige
`$n`. Mover los datos no es migrar si la app no puede leerlos.

La migración se parte en tres contratos para que cada parte se verifique sola. **Este es el 1:
`internal/auth`** (23 sentencias en `users.go`, `roles.go`, `apikey.go`; `jwt.go` no tiene ninguna).
Los siguientes serán `internal/server` y `internal/store` + la elección de motor.

**Al terminar este contrato la aplicación sigue corriendo solo en SQLite.** No es un fallo del
contrato: la elección de motor es del contrato 3. Lo que este entrega es que el paquete `auth` deje
de tener SQL atado a un motor, verificable de forma aislada contra los dos motores reales.

## RECON ya resuelto (no re-investigar)

`compat` ofrece TRES caminos, y elegir mal es el error más caro de este contrato:

1. **`QueryRoutine`** (v0.3.0) — lecturas parametrizadas. Devuelve `[]compat.Row` ya canonicalizadas
   por familia de columna, así que el escaneo es idéntico en ambos motores.
2. **`CallRoutine`** — escrituras (`insert`/`update`/`delete`) declaradas en el esquema canónico.
   Ejecuta TODAS las acciones de la rutina en UNA transacción.
3. **SQL crudo con `compat.Placeholder(engine, position)`** (v0.4.0) — para lo que los dos
   anteriores no pueden expresar.

**`CallRoutine` y `QueryRoutine` abren su propia transacción.** No se pueden usar dentro de una
transacción del consumidor, ni componerse entre sí de forma atómica.

**Una rutina tiene una lista ESTÁTICA de acciones.** No puede insertar N filas donde N depende de un
argumento.

De esas dos restricciones sale la división de este contrato, que **ya está decidida**:

| Operación | Camino | Por qué |
|---|---|---|
| `VerifyCredentials`, `ListUsers`, `GetUser`, `UserRoles`, `RolePermissions`, `ListAPIKeys`, `GetAPIKey`, `VerifyAPIKey` | `QueryRoutine` (+ vista si hay `JOIN`) | Lecturas fuera de transacción |
| `MintAPIKey`, `RevokeAPIKey`, `RevokeAPIKeyByID`, `UpdateUserStatus` | `CallRoutine` | Escritura única |
| `CreateUser`, `SetUserRoles`, `SetRolePermissions` | **SQL crudo con `Placeholder`, en la transacción del consumidor** | Reemplazo atómico de un conjunto de tamaño VARIABLE: un `DELETE` seguido de N `INSERT` que deben commitear juntos. Ninguna rutina lo expresa, y partirlo en varias llamadas perdería la atomicidad — que en `SetRolePermissions` es una garantía deliberada de CONTRACT-16 |

**NO intentes forzar esas tres últimas a rutinas.** Si creés que se puede, PARÁ y explicá cómo en el
reporte antes de escribir nada.

Otros hechos ya establecidos, con su resolución decidida:

- **`RETURNING id` (`users.go:98`)**: la columna `id` tiene default `gen_random_uuid()` en la base,
  por eso se lee de vuelta. **Resolución: `auth` genera el UUID en Go y lo pasa como un valor más.**
  El default de la columna se conserva como red de seguridad para escrituras que no vengan de la
  app. Está justificado en el DEFINIR de compat: evita agregarle `RETURNING` al paquete para algo
  que el consumidor resuelve solo, y deja el identificador conocido ANTES de escribir.
- **`CURRENT_TIMESTAMP` en `UPDATE` (`users.go:243`, `apikey.go:57`, `apikey.go:80`)**: las
  asignaciones de una rutina resuelven a parámetro o literal, no a funciones del motor. **Resolución:
  pasar el instante como parámetro.** Usá el mismo formato canónico que el paquete usa para
  `timestamp` (RFC3339Nano, `TEXT` en ambos motores) — no inventes uno.
- **Los `JOIN`** (`users.go:336`, `roles.go:108`, `apikey.go:109` y `139`) van en **vistas**
  declaradas en `schema.Build()`, que `compat` compila a ambos motores. `apikey.go:109` (listar) y
  `139` (uno por id) son la MISMA vista con distinto `WHERE`.
- **`ON CONFLICT DO NOTHING`** aparece en tres inserciones. Verificá que la forma que uses sea
  aceptada por AMBOS motores; si no lo es, resolvelo sin `ON CONFLICT` (el conjunto se acaba de
  vaciar con el `DELETE`, así que el conflicto puede ser imposible por construcción — comprobalo en
  vez de asumirlo).
- **Las firmas públicas de `auth` toman `db *sql.DB`**, que no sabe el motor. `Placeholder`,
  `QueryRoutine` y `CallRoutine` lo necesitan. Cambiar esas firmas es parte del contrato; elegí
  entre pasar `*compat.Store` o el motor, y justificá la elección. Actualizá todos los llamadores
  (están en `internal/server/`) **sin cambiarles el comportamiento**.
- `compat.Store.IsUniqueViolation(err)` (v0.4.0) clasifica por código del driver y funciona con el
  error envuelto. `auth` hoy no clasifica errores únicos, pero si tu implementación lo necesita,
  usá eso y NUNCA el texto del mensaje.

## T1 — Vistas y rutinas en el esquema canónico

FIX/OBJETIVO: declarar en `schema.Build()` las vistas que reemplazan los `JOIN` y las rutinas
(lectura y escritura) que este contrato necesita. Son parte del esquema canónico, así que viajan
solas al export y quedan cubiertas por el round-trip JSON existente.

Nombralas de forma que se vea a qué operación sirven. Recordá que la acción `select` **declara sus
columnas de salida con su tipo** y que, si lleva `LIMIT`/`OFFSET`, el `ORDER BY` es obligatorio.

## T2 — Migrar `internal/auth`

FIX/OBJETIVO: las 23 sentencias, según la tabla de arriba. **Ni una sola sentencia SQL puede quedar
con un `?` literal escrito a mano**: o va por rutina, o se compone con `compat.Placeholder`.

El comportamiento observable de cada función pública no cambia: mismos valores devueltos, mismos
errores, mismas garantías de atomicidad. Los tests existentes del paquete son el contrato de eso y
tienen que seguir verdes **sin modificarlos** — si necesitás tocar uno, explicá por qué en el
reporte; es señal de que cambiaste comportamiento.

## T3 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- **La prueba que da sentido al contrato**: una batería que ejercite las funciones públicas de
  `auth` contra **SQLite real y PostgreSQL 17 real**, y confirme que devuelven **lo mismo**. Sin
  esto el contrato no está cumplido, porque lo único que importa es que el paquete funcione en los
  dos motores. Si hace falta un tag de build para esa batería (el repo ya usa `exportfixture` para
  algo así), usalo y documentá cómo se corre.
- Dentro de esa batería, explícitamente: crear un usuario y verificar sus credenciales; asignarle
  roles y volver a asignarle un conjunto distinto (que el reemplazo sea completo, no acumulativo);
  otorgar permisos a un rol y reemplazarlos; acuñar una API key, verificarla, revocarla y confirmar
  que deja de verificar; y listar, que es donde el orden importa.
- Un caso que pruebe la **atomicidad** de `SetRolePermissions` en ambos motores: forzar un fallo a
  mitad (un permiso inexistente) y confirmar que el conjunto anterior quedó intacto.
- Confirmá que TODO lo de contratos anteriores sigue funcionando igual.

## Criterios de aceptación

- [ ] `go.mod` pide `sqlite-postgres-compat` v0.4.0; build/vet/gofmt limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: vistas y rutinas declaradas en el esquema canónico.
- [ ] T2: cero `?` literales en `internal/auth`; comportamiento público sin cambios; tests
  existentes verdes **sin modificar**.
- [ ] T3: batería contra AMBOS motores reales con salida real, incluida la de atomicidad.
- [ ] Los llamadores en `internal/server/` actualizados, sin cambio de comportamiento.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. **NO toques `sqlite-postgres-compat`** — ya tiene todo
  lo necesario; si creés que le falta algo, PARÁ y reportalo en vez de trabajar alrededor.
- Sin dependencias nuevas más allá del bump de `compat`.
- NO commitear (el orquestador commitea y despliega tras verificar).
- **NO cambies `internal/server` más allá de lo mínimo** para acompañar las firmas de `auth`. La
  migración de `server` es el contrato 20 y no se adelanta acá.
- NO cambies `store.Open` ni agregues elección de motor: es el contrato 21.
- El contrato público de las rutas HTTP no cambia en absoluto.

## Checklist antes de delegar

- [ ] RECON corrido: los tres caminos de `compat` entendidos, y por qué las tres operaciones
  atómicas de conjunto variable NO pueden ir por rutina.
- [ ] Red-team: ¿el orden de `ListUsers`/`ListAPIKeys` es el mismo en ambos motores (¿hay `ORDER BY`
  explícito, o se está dependiendo del orden natural, que no es el mismo)? ¿`revoked_at IS NULL`
  se comporta igual? ¿El `status` con su `CHECK` sigue rechazando lo mismo? ¿Qué pasa si
  `SetUserRoles` recibe una lista vacía (¿borra todo, o es un error?) — mirá qué hace HOY y
  preservalo. ¿Y si un nombre de rol no existe?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 efímera provista por el orquestador; password enmascarado como `***`.

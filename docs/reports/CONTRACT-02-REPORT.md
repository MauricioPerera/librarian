# CONTRACT-02 — Autenticación dual (API keys + JWT) — Reporte de implementación

Repo: `D:\Repo\librarian` · módulo `github.com/MauricioPerera/librarian`
Base: `CONTRACT-01` (commit `19c80e2`) — esquema, store idempotente, server `/health`.
Dependencias nuevas: `github.com/golang-jwt/jwt/v5@v5.3.1`, `golang.org/x/crypto@v0.54.0`.

---

## 0. RECON (corrido ANTES de escribir código dependiente)

- `compat.Store.DB` confirmado `*sql.DB` exportado (`compat/store.go:23`). Todas las
  queries de aplicación van con `database/sql` estándar, parametrizadas, contra ese
  handle. No se abre ninguna conexión separada.
- `go get github.com/golang-jwt/jwt/v5` → **resuelve** (v5.3.1).
- `go get golang.org/x/crypto/bcrypt` → **resuelve** (x/crypto v0.54.0).
- Ningún `ABORTAR SI` se disparó.

## 1. Qué se implementó por tarea

### T1 — Seed idempotente de roles y permisos
`internal/store/store.go`: `SeedCatalogs(ctx, db *sql.DB)` inserta cada nombre de
`schema.Roles` y `schema.Permissions` con `INSERT INTO <tabla> (name) VALUES (?) ON
CONFLICT(name) DO NOTHING`. El `id` toma su `DEFAULT gen_random_uuid()` (no se generan
UUIDs en Go). Idempotente: la segunda corrida no duplica ni falla. Se invoca desde
`main` después de `EnsureSchema`. El nombre de tabla es una constante interna (no
input de usuario); el `name` siempre va ligado como parámetro.

### T2 — Hashing de contraseñas + creación/verificación de usuarios
`internal/auth/users.go`:
- `CreateUser(ctx, db, email, password, roleNames)` — hashea con `bcrypt.DefaultCost`,
  nunca guarda ni loguea el plaintext; en una tx inserta el usuario (`status='active'`),
  resuelve los `role_id` por nombre, y los asigna vía `user_roles` con `ON CONFLICT DO
  NOTHING`.
- `VerifyCredentials(ctx, db, email, password)` — busca el hash, compara con bcrypt, y
  devuelve el usuario + roles. **Anti-enumeración**: un único error genérico
  `ErrInvalidCredentials` para “usuario no existe” y “contraseña incorrecta” (mismo
  tipo y mensaje). Además, en la rama de usuario inexistente se ejecuta un
  `bcrypt.CompareHashAndPassword` contra un hash dummy (`dummyHash`) para **igualar el
  tiempo** de la rama de contraseña incorrecta — de lo contrario el missing-email
  respondería instantáneamente y el wrong-password tras correr bcrypt, filtrando
  existencia por timing. Es una medida de seguridad alineada con el requerimiento
  explícito de anti-enumeración del contrato (y su checklist red-team).

### T3 — Emisión y verificación de JWT
`internal/auth/jwt.go`:
- `IssueJWT(secret, user, now)` — HS256 con `golang-jwt/jwt/v5`, claims `sub` (user id),
  `email`, `roles` y `exp` = `now + 24h` (constante `jwtTTL`). `now` es parámetro para
  tests determinísticos.
- `VerifyJWT(secret, tokenStr)` — `ParseWithClaims` rechazando todo método que no sea
  HMAC (no permite `none` ni algs asimétricos). Cualquier fallo → error genérico
  `ErrInvalidToken`.
- `POST /auth/login` (`internal/server/server.go`): verifica credenciales, 200 +
  `{"token": ...}` en éxito, 401 + envelope `{"error":"invalid credentials"}` en fallo
  (mismo mensaje genérico que T2).
- **Fail-closed**: el secreto viene **solo** de `LIBRARIAN_JWT_SECRET`.
  `internal/config/config.go::Load()` falla explícitamente si está ausente o vacía (sin
  default hardcodeado); `server.NewMux` además rechaza construir el handler si el
  secreto está vacío. `main` usa `config.Load()` antes de abrir el server, así que el
  proceso no arranca sin secreto.
- Middleware de verificación reusable: `auth.VerifyJWT` + `bearerToken` en el server,
  consumido por `/whoami` (T5).

### T4 — Emisión y verificación de API keys
`internal/schema/schema.go`: tabla `api_keys` agregada con la misma disciplina que
CONTRACT-01 — columnas `id` (uuid PK, `gen_random_uuid`), `label`, `key_hash` (text,
UNIQUE), `role_id` (uuid, FK → `roles(id)` ON DELETE CASCADE), `created_at` (timestamp
default now), `revoked_at` (timestamp, nullable). `Schema.Validate()` y `CompileDDL`
siguen verdes para SQLite y Postgres (ver §3).
`internal/auth/apikey.go`:
- `MintAPIKey(ctx, db, label, roleID)` — genera 32 bytes con **`crypto/rand`** (no
  `math/rand`), base64url + prefijo `lbk_` (256 bits de entropía), guarda solo el hash
  **SHA-256** (hex) del secreto, y devuelve el plaintext **solo acá, al crearla**.
  No bcrypt: la key ya tiene entropía alta, un hash lento no aporta nada.
- `VerifyAPIKey(ctx, db, secret)` — hashea el recibido y hace un **lookup exacto por
  hash** `WHERE key_hash = ?`. Rechaza si no hay fila o si `revoked_at` no es nulo.
  Error genérico `ErrAPIKeyRejected` (sin distinguir “no existe” de “revocada”).
- `RevokeAPIKey(ctx, db, secret)` — `UPDATE ... SET revoked_at = CURRENT_TIMESTAMP WHERE
  key_hash = ? AND revoked_at IS NULL`; idempotente.
- No es un endpoint HTTP (contrato: no hay todavía autorización por ruta que proteja un
  endpoint de creación de keys).

### T5 — Endpoint de demostración protegido
`internal/server/server.go`: `GET /whoami`. Lee `Authorization: Bearer <token>`, prueba
JWT primero; si falla, prueba API key (lookup por hash). 200 con identidad resuelta
(JWT → `{"auth":"jwt","user_id","email","roles"}`; API key →
`{"auth":"apikey","label","role_id"}`); 401 si ninguno valida, sin distinguir ausente
de inválido.

## 2. Decisiones de diseño donde el contrato dejaba margen

- **Igualación de timing en login** (no exigido literal, pero dentro del espíritu
  anti-enumeración): rama de usuario inexistente ejecuta un bcrypt dummy. Trade-off:
  agrega ~costo de un bcrypt por request fallido; justificado por el objetivo explícito
  de anti-enumeración del contrato.
- **`revoked_at` se escanea como `sql.NullString`**, no `sql.NullTime`: el driver SQLite
  (modernc) devuelve `CURRENT_TIMESTAMP` como string, y `NullTime` no puede scanearlo.
  Cualquier valor no-null/no-vacío se interpreta como revocado (no se necesita parsear
  la fecha, solo saber si está seteada).
- **`writeJSON` usa `json.Marshal`** (no `Encoder`, que agregaría `\n`) para mantener
  `/health` byte-exacto `{"status":"ok"}` — no romper la garantía de CONTRACT-01.
- **`/whoami` prueba JWT antes que API key**: un token JWT mal formado falla rápido en
  el parse; una API key no es un JWT válido, así que el fallback es seguro. El orden
  no filtra información (ambas fallas devuelven el mismo 401).
- **API keys con SHA-256 hex** (no base64) para tener una columna de igualdad
  case-insensitive y legible; el prefijo `lbk_` las distingue visualmente de JWTs.
- **`config` como paquete propio** (no en `main`) para testear el fail-closed sin
  levantar el binario; `main` queda como un armador delgado.

## 3. Cómo se cumplió cada punto de seguridad explícito

- **password_hash = bcrypt, nunca texto plano**: `bcrypt.GenerateFromPassword` en
  `CreateUser`; el plaintext se descarta dentro de la función. Nunca se loguea.
- **Mensaje de login idéntico** entre “usuario no existe” y “contraseña incorrecta”:
  mismo `ErrInvalidCredentials` y mismo envelope HTTP `{"error":"invalid credentials"}`
  (verificado por `TestVerifyCredentialsIdenticalMessage` y `TestLoginInvalidCredentials`).
- **Secreto JWT solo de `LIBRARIAN_JWT_SECRET`**: `config.Load` falla si vacía/ausente;
  `server.NewMux` también lo rechaza. Sin default hardcodeado. El secreto nunca se
  loguea.
- **API key secret con `crypto/rand`**, mostrado en plaintext solo al crear, guardado
  como hash SHA-256: `generateSecret` usa `rand.Read`; `MintAPIKey` persiste solo
  `hashSecret(secret)`.
- **Verificación por lookup exacto por hash en SQL** (no comparación `==` en Go de
  secretos plaintext): `VerifyAPIKey` hace `WHERE key_hash = ?` contra el hash
  almacenado. El secreto plaintext nunca se compara en Go → no hay timing side-channel
  sobre el secreto en código de aplicación; la igualdad la resuelve el motor de DB
  contra el hash, no contra el secreto. (`subtle.ConstantTimeCompare` no es necesario:
  el contrato lo deja explícito — el `WHERE key_hash = ?` ya es efectivamente constante
  respecto al secreto porque no se compara el secreto en Go).
- **Ningún secreto se loguea**: ni JWT secret, ni API key secret, ni password aparecen
  en logs, errores o debug. Los errores son genéricos (`ErrInvalidCredentials`,
  `ErrInvalidToken`, `ErrAPIKeyRejected`) sin detalle.

## 4. Trade-offs

- `bcrypt.DefaultCost` (10): balance estándar; no se expone como config en este
  contrato.
- 24h de expiración de JWT como constante (`jwtTTL`): el contrato pide valor fijo por
  ahora; rotación/refresco queda para un contrato posterior.
- Revocación de API keys es por-flag (`revoked_at`), no por borrado de fila: permite
  auditoría; el FK cascade de `role_id` sigue limpiando keys al borrar un rol.
- El dummy-bcrypt en login añade latencia a credenciales inválidas (trade-off aceptado
  por anti-enumeración).

## 5. Salida REAL de los criterios de aceptación

### `go build ./...` y `go vet ./...` limpios
```
$ go build ./... && go vet ./...
BUILD+VET OK   (salida vacía, exit 0)
```

### `go test ./... -count=1` verde, corrido DOS VECES (mismo resultado)

Corrida 1:
```
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	2.655s
ok  	github.com/MauricioPerera/librarian/internal/config	0.621s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.232s
ok  	github.com/MauricioPerera/librarian/internal/server	3.598s
ok  	github.com/MauricioPerera/librarian/internal/store	2.185s
```
Corrida 2:
```
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	2.670s
ok  	github.com/MauricioPerera/librarian/internal/config	0.664s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.254s
ok  	github.com/MauricioPerera/librarian/internal/server	3.577s
ok  	github.com/MauricioPerera/librarian/internal/store	2.200s
```
Ambas verdes, idénticas en resultado.

### `Schema.Validate()` y `CompileDDL` (ambos motores) sin error con `api_keys` agregada
```
=== RUN   TestSchemaValidates
--- PASS: TestSchemaValidates (0.00s)
=== RUN   TestCompileDDLBothEngines
=== RUN   TestCompileDDLBothEngines/sqlite
=== RUN   TestCompileDDLBothEngines/postgres
--- PASS: TestCompileDDLBothEngines (0.00s)
    --- PASS: TestCompileDDLBothEngines/sqlite (0.00s)
    --- PASS: TestCompileDDLBothEngines/postgres (0.00s)
=== RUN   TestExpectedTables
--- PASS: TestExpectedTables (0.00s)
=== RUN   TestAPIKeysTable
--- PASS: TestAPIKeysTable (0.00s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/schema	1.398s
```
Garantía de exportabilidad de CONTRACT-01 intacta: el round-trip exacto del esquema
sigue pasando con `api_keys`:
```
=== RUN   TestRoundTripExact
--- PASS: TestRoundTripExact (0.01s)
=== RUN   TestEnsureSchemaIdempotent
--- PASS: TestEnsureSchemaIdempotent (0.01s)
=== RUN   TestSeedCatalogsIdempotent
--- PASS: TestSeedCatalogsIdempotent (0.04s)
PASS
```

### Seed corrido dos veces sobre el mismo archivo no duplica filas ni falla
```
=== RUN   TestSeedCatalogsIdempotent
--- PASS: TestSeedCatalogsIdempotent (0.04s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/store	2.366s
```
El test corre el seed dos veces y afirma `count(*) FROM roles`/`permissions` igual
antes y después (e igual a `len(schema.Roles)`/`len(schema.Permissions)`).

### Crear usuario + verificar contraseña correcta → ok; incorrecta → error, mismo tipo/mensaje que usuario inexistente
```
=== RUN   TestCreateUserAndVerify
--- PASS: TestCreateUserAndVerify (0.21s)
=== RUN   TestVerifyCredentialsIdenticalMessage
--- PASS: TestVerifyCredentialsIdenticalMessage (0.18s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/auth	2.851s
```
`TestVerifyCredentialsIdenticalMessage` afirma explícitamente
`errWrong.Error() == errMissing.Error()`.

### `POST /auth/login` (httptest) con válidas → 200 + JWT parseable; con inválidas → 401
```
=== RUN   TestLoginSuccess
--- PASS: TestLoginSuccess (0.14s)
=== RUN   TestLoginInvalidCredentials
--- PASS: TestLoginInvalidCredentials (0.18s)
PASS
```
`TestLoginSuccess` decodifica `token`, lo parsea con `auth.VerifyJWT` y afirma las
claims (`email`, `roles` con 2 elementos). `TestLoginInvalidCredentials` afirma 401 y
mismo mensaje entre wrong-password y unknown-user.

### Arrancar SIN `LIBRARIAN_JWT_SECRET` → falla al arrancar (no silencioso, sin default)
```
=== RUN   TestLoadRejectsEmptySecret
--- PASS: TestLoadRejectsEmptySecret (0.00s)
=== RUN   TestLoadRejectsAbsentSecret
--- PASS: TestLoadRejectsAbsentSecret (0.00s)
=== RUN   TestNewMuxRejectsEmptySecret
--- PASS: TestNewMuxRejectsEmptySecret (0.00s)
PASS
```
Cubierto en dos capas: `config.Load` (startup) y `server.NewMux` (handler). Ambos
fallan explícitamente; ningún default hardcodeado.

### `MintAPIKey` + verificación del secreto contra la DB → ok; key revocada → rechazada
```
=== RUN   TestMintAndVerifyAPIKey
--- PASS: TestMintAndVerifyAPIKey (0.04s)
=== RUN   TestRevokedAPIKeyRejected
--- PASS: TestRevokedAPIKeyRejected (0.04s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/auth	2.573s
```
`TestMintAndVerifyAPIKey` afirma label/role_id y rechazo de bogus/empty. `TestRevokedAPIKeyRejected`
afirma verificación previa ok, luego `RevokeAPIKey`, luego rechazo.

### `GET /whoami` (httptest): JWT válido → 200 con identidad; API key válida → 200 con identidad; sin nada/inválida → 401
```
=== RUN   TestWhoamiJWT
--- PASS: TestWhoamiJWT (0.13s)
=== RUN   TestWhoamiAPIKey
--- PASS: TestWhoamiAPIKey (0.04s)
=== RUN   TestWhoamiRevokedAPIKeyRejected
--- PASS: TestWhoamiRevokedAPIKeyRejected (0.05s)
=== RUN   TestWhoamiNoCredentials
--- PASS: TestWhoamiNoCredentials (0.04s)
=== RUN   TestWhoamiGarbageToken
--- PASS: TestWhoamiGarbageToken (0.04s)
PASS
```
Cubre JWT, API key, key revocada (401), sin header (401) y token basura (401).

### Final: suite completa 2× verde
Ver §“Corrida 1” / “Corrida 2” arriba. Verde ambas.

## 6. Archivos tocados (dentro de `librarian` únicamente)

- `internal/schema/schema.go` (+`schema_test.go`): tabla `api_keys` + tests.
- `internal/store/store.go` (+`store_test.go`): `SeedCatalogs` + test idempotente.
- `internal/auth/users.go`, `internal/auth/jwt.go`, `internal/auth/apikey.go`,
  `internal/auth/auth_test.go` (nuevo paquete).
- `internal/config/config.go`, `internal/config/config_test.go` (nuevo).
- `internal/server/server.go` (+`server_test.go`, +`server_auth_test.go`): rutas
  `/auth/login` y `/whoami`, `Deps`, `NewMux` fail-closed.
- `cmd/librarian/main.go`: usa `config.Load` + `SeedCatalogs`.
- `go.mod`/`go.sum`: dos dependencias permitidas.

No se tocó `sqlite-postgres-compat`. No se hizo `git add`/`commit`. Ningún proceso en
foreground queda vivo (todos los servers HTTP son `httptest.Server` con `defer
cleanup()`).
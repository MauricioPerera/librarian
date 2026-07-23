# Contrato 02 — Autenticación dual: API keys + JWT

Prerrequisitos: `CONTRACT-01` completado y verificado (`19c80e2`). Esquema base (`users`, `roles`,
`permissions`, junctions) y servidor mínimo funcionando. Este contrato cierra la capacidad
"Autenticación dual" de `DEFINITION.md`: API keys por rol/servicio + JWT con usuario/contraseña.

> Alcance deliberado: este contrato cubre AUTENTICACIÓN (verificar identidad), no un sistema
> completo de AUTORIZACIÓN por ruta (no hay todavía recursos protegidos reales más allá de un
> endpoint de demostración) — eso se cierra cuando exista contenido real que proteger, en un
> contrato posterior.

## RECON ya resuelto (no re-verificar)

- `compat.Store.DB` es un `*sql.DB` exportado (`compat/store.go:23`) — el seed de catálogos y
  las queries de aplicación (verificar credenciales, mintear API keys) van con `database/sql`
  estándar directo contra ese handle, sin capa extra ni conexión separada.
- El esquema de `roles`/`permissions` ya existe (`internal/schema/schema.go`, `Build()`); los
  catálogos `schema.Roles`/`schema.Permissions` son slices de nombres, sin filas insertadas
  todavía.

## T1 — Seed idempotente de roles y permisos

Hoy `schema.Roles`/`schema.Permissions` son slices en código pero la tabla vive vacía. Sin
filas, no hay a qué asociar usuarios ni API keys.

FIX/OBJETIVO: al arrancar (después de `store.EnsureSchema`), insertar cada rol/permiso de los
catálogos si no existe (`INSERT ... ON CONFLICT(name) DO NOTHING`, dejando que `id` tome su
`DEFAULT gen_random_uuid()` — no generar UUIDs en Go). Idempotente: correr el seed dos veces
sobre el mismo archivo no duplica ni falla.

## T2 — Hashing de contraseñas + creación/verificación de usuarios

Hoy la columna `password_hash` existe pero nada la llena ni la verifica.

FIX/OBJETIVO: paquete que (a) crea un usuario con contraseña (hashea con `bcrypt`, nunca guarda
texto plano, nunca lo loguea), asignándole uno o más roles vía `user_roles`; (b) verifica
credenciales (email + contraseña) contra el hash almacenado, devolviendo el usuario + sus roles
si coincide, error genérico si no (sin distinguir "usuario no existe" de "contraseña incorrecta"
en el mensaje — evitar enumeración de usuarios).

## T3 — Emisión y verificación de JWT

FIX/OBJETIVO: `POST /auth/login` (body `{email, password}`) que verifica credenciales (T2) y
devuelve un JWT firmado (HS256, `golang-jwt/jwt/v5`) con claims `sub` (user id), `email`, `roles`
(lista de nombres) y expiración (24h, valor fijo por ahora). El secreto de firma viene de una
variable de entorno (`LIBRARIAN_JWT_SECRET`) — **el servidor rechaza arrancar si no está seteada
o está vacía** (fail-closed, sin secreto por defecto). Credenciales inválidas → `401` con un
envelope de error JSON, mismo mensaje genérico que T2. Además: una función/middleware de
verificación de JWT reusable (parsea `Authorization: Bearer <token>`, valida firma+expiración,
devuelve las claims o error) para que T5 la use.

## T4 — Emisión y verificación de API keys

Hoy no existe tabla para esto.

FIX/OBJETIVO: agregar `api_keys` al `Schema` de `internal/schema` (misma disciplina que T1/T2 de
CONTRACT-01: `Schema.Validate()` + `CompileDDL` para ambos motores sin error) — columnas: `id`
(uuid PK), `label` (text, para identificar la key humanamente), `key_hash` (text, `UNIQUE`),
`role_id` (uuid, FK → `roles(id)` ON DELETE CASCADE), `created_at` (timestamp default now),
`revoked_at` (timestamp, nullable). El secreto de la key es un valor aleatorio de alta entropía
(`crypto/rand`, no `math/rand`), mostrado en texto plano SOLO al crearla (nunca se puede
recuperar después); se guarda su hash SHA-256 (no bcrypt — las API keys ya tienen entropía alta,
no hace falta cómputo lento). Una función `MintAPIKey(ctx, db, label, roleID) (secret string, err
error)` para generar keys programáticamente (NO un endpoint HTTP en este contrato — no hay
todavía autorización por ruta que proteja un endpoint de creación de keys; ver nota de alcance
arriba). Middleware de verificación (`Authorization: Bearer <key>`) que hashea el valor recibido,
lo busca por `key_hash`, rechaza si no existe o `revoked_at` no es nulo.

## T5 — Endpoint de demostración protegido

Sin esto, nada prueba que T3/T4 funcionan de punta a punta contra un request HTTP real (más allá
de tests unitarios de las funciones sueltas).

FIX/OBJETIVO: `GET /whoami`, protegido por CUALQUIERA de los dos mecanismos (JWT válido O API
key válida en el header `Authorization`) — devuelve `200` con la identidad resuelta (para JWT:
user id + email + roles; para API key: label + rol asociado) o `401` si ninguno de los dos
valida.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces (mismo resultado ambas).
- [ ] `Schema.Validate()` y `CompileDDL` (ambos motores) siguen sin error con `api_keys` agregada
  — no romper la garantía de exportabilidad que CONTRACT-01 dejó verde.
- [ ] Test real: seed corrido dos veces sobre el mismo archivo no duplica filas (`SELECT count(*)
  FROM roles` igual en ambas corridas) ni falla.
- [ ] Test real: crear usuario + verificar con contraseña correcta → ok; con incorrecta → error,
  mismo tipo/mensaje que "usuario inexistente" (sin distinguir).
- [ ] Test real (`httptest`): `POST /auth/login` con credenciales válidas → `200` + JWT
  parseable con las claims esperadas; con inválidas → `401`.
- [ ] Test real: arrancar el proceso SIN `LIBRARIAN_JWT_SECRET` seteada → falla al arrancar
  (no silenciosamente, no con un secreto por defecto).
- [ ] Test real: `MintAPIKey` + verificación del secreto devuelto contra la DB → ok; una key con
  `revoked_at` seteado → rechazada.
- [ ] Test real (`httptest`): `GET /whoami` con JWT válido → `200` con identidad correcta; con
  API key válida → `200` con identidad correcta; sin nada o con credencial inválida → `401`.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Dependencias nuevas permitidas SOLO: `github.com/golang-jwt/jwt/v5`, `golang.org/x/crypto/bcrypt`.
  Ninguna otra sin aprobación explícita.
- El secreto JWT y cualquier secreto de API key NUNCA se loguean, ni siquiera parcialmente
  (ni en error, ni en debug).
- T1→T5 son sustancialmente secuenciales (T3 depende de T2, T5 depende de T3+T4) — un solo
  perímetro, un solo dev.
- NO commitear (el orquestador commitea tras verificar).
- ABORTAR SI: `golang-jwt/jwt/v5` o `bcrypt` no resuelven vía `go get` (verificar RED antes de
  escribir código que dependa de ellos) → documentar el error real y proponer alternativa antes
  de continuar, no forzar un stub.

## Checklist antes de delegar

- [ ] RECON corrido: `Store.DB` confirmado exportado (ver sección RECON arriba); falta verificar
  en vivo que `go get github.com/golang-jwt/jwt/v5` y `go get golang.org/x/crypto/bcrypt`
  resuelven — el dev lo hace como primer paso y aborta si no.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿el mensaje de error de login podría dar información distinta entre "no existe"
  y "contraseña mal"? → explícitamente exigido que NO (enumeración de usuarios). ¿El middleware
  de API key compara el hash con `==` de string (vulnerable a timing attack) o con comparación
  de tiempo constante? → usar `subtle.ConstantTimeCompare` o comparar por lookup exacto en SQL
  (el propio `WHERE key_hash = ?` ya es efectivamente constante respecto al secreto porque no se
  compara el secreto en Go, se compara su hash contra lo almacenado vía la DB — dejar esto
  explícito en el reporte).
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Condiciones de aborto explícitas (arriba).

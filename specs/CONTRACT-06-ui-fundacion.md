# Contrato 06 — Fundación de UI (layout, sesión de navegador, home protegida)

Prerrequisitos: `CONTRACT-01`..`CONTRACT-05` completos (`9708c8e`), `DEFINITION-UI.md` cerrado.
Primer contrato de la fase 2. Sin esto no puede construirse ningún otro contrato de UI.

## RECON ya resuelto (no re-investigar)

- `internal/auth.VerifyCredentials(ctx, db, email, password) (*User, error)` y
  `internal/auth.IssueJWT(secret, user, now) (string, error)` ya existen (`internal/auth/users.go`,
  `internal/auth/jwt.go`) — el login de UI los llama DIRECTO, sin loopback HTTP a `/auth/login`.
  `internal/auth.VerifyJWT(secret, token) (*Claims, error)` verifica el token de la cookie.
- `internal/server/authz.go` ya tiene `Identity`, `identityKey{}`, `identityFromContext`,
  `resolveIdentity` (JWT→APIkey), `permissionsFor`. La UI reusa el MISMO tipo `Identity` y la
  MISMA `identityKey{}` para que un contrato futuro (articles UI) pueda usar
  `requirePermission`/`identityFromContext` sin diferenciar "vino de header" vs "vino de cookie".
- `internal/server/server.go`: `Deps{DB, JWTSecret}`, `NewMux`, `handlers{db, jwtSecret, now}`.
  Las rutas de UI se agregan al MISMO `mux` de `NewMux`, mismo `handlers`.
- DEFINITION-UI.md fija: sesión de navegador = JWT en cookie `httpOnly`+`Secure` (no un session
  store separado). El header `Authorization: Bearer` de la API JSON sigue funcionando sin cambios
  — la cookie es un transporte ADICIONAL del mismo JWT, no un reemplazo.

## T1 — Layout base + assets estáticos (sin dependencias de red en runtime)

FIX/OBJETIVO: un layout HTML base (`html/template` de la stdlib — sin librería de templating
nueva) con un bloque de contenido, un `<nav>` mínimo, y el script de htmx cargado desde un asset
propio servido por el binario (`//go:embed`), **nunca desde un CDN externo en runtime** (el VPS
debe poder operar sin salida a internet para esto). Conseguí el archivo real de htmx
(`htmx.min.js`, la versión estable más reciente que puedas obtener, ej. descargándolo de
`unpkg.com/htmx.org` o `cdn.jsdelivr.net/npm/htmx.org` con `curl`/`Invoke-WebRequest` DURANTE tu
sesión) y commiteálo como archivo real del repo bajo un directorio de assets embebidos (a tu
criterio el path exacto, documentalo). CSS: un archivo propio simple y moderno (variables CSS,
sin framework externo) — no hace falta que sea elaborado, el contrato no pide diseño final (ver
Fuera de alcance de `DEFINITION-UI.md`). Test de aceptación: un test que confirma que el binario
sirve el JS/CSS embebido con el `Content-Type` correcto, sin ninguna llamada de red.

## T2 — Login/logout con cookie httpOnly

FIX/OBJETIVO:
- `GET /login`: renderiza un formulario HTML (email + password, method POST, sin JS necesario
  para funcionar — htmx es progresivo, no obligatorio acá).
- `POST /login`: decodifica el form (`r.ParseForm()`, NO JSON — es un submit de formulario real),
  llama `auth.VerifyCredentials` + `auth.IssueJWT` directo (mismo patrón que `handleLogin` de
  CONTRACT-02, sin loopback HTTP). Éxito: `Set-Cookie` con el JWT (`HttpOnly`, `Secure`,
  `SameSite=Strict`, `Path=/`, nombre a tu criterio — documentalo, ej. `librarian_session`),
  `303 See Other` a `/`. Fallo: re-renderiza el form con un mensaje de error genérico (mismo
  mensaje para usuario inexistente y password incorrecta — anti-enumeración, mismo principio que
  CONTRACT-02), sin filtrar cuál fue.
- `POST /logout`: borra la cookie (`MaxAge: -1`), `303 See Other` a `/login`.

**Nota de testing (importante, no te la saltees):** el atributo `Secure` de una cookie hace que
los clientes HTTP (navegadores Y `net/http/cookiejar`, que es lo que usa un test con
`http.Client{Jar: ...}`) la descarten en conexiones HTTP planas — un test contra
`httptest.NewServer` (HTTP plano) NUNCA vería la cookie volver en el segundo request, y fallaría
por una razón que no es un bug real. Usá `httptest.NewTLSServer` (que sí es HTTPS, con un
certificado autofirmado) y el cliente que trae preconfigurado (`srv.Client()`, que ya confía en
ese certificado) para CUALQUIER test que dependa del ciclo completo cookie-set → cookie-enviada.

## T3 — Middleware de sesión de navegador + home protegida

FIX/OBJETIVO: un middleware (nombre a tu criterio, ej. `requireSession`) para rutas HTML: lee la
cookie, valida con `auth.VerifyJWT`, y si falta o es inválida hace `302 Found` a `/login` (nunca
un 401 JSON — el usuario es un humano en un navegador, no un cliente de API). Si es válida,
construye el MISMO tipo `Identity` (`Kind: "jwt"`, `UserID`/`Email`/`Roles` desde los claims) y lo
guarda con la MISMA `identityKey{}` que ya usa `authz.go` — reusá esa infraestructura, no
inventes una paralela. `GET /` (protegida por este middleware): página simple que saluda
("Hola, {email}") y tiene un botón/form de logout (`POST /logout`). Nada más — el contenido real
(articles, usuarios) es de los próximos contratos.

## T4 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces):
- Test de round-trip completo: `POST /login` con credenciales válidas → cookie seteada → `GET /`
  con esa cookie → 200 y contenido que confirma la identidad. Sin cookie o cookie inválida →
  `GET /` → 302 a `/login`.
- Test de credenciales inválidas: mensaje genérico, nunca revela si el email existe.
- Confirmá explícitamente que las rutas JSON existentes (`POST /auth/login`, `GET /whoami`,
  `/articles/*`) siguen funcionando exactamente igual (header `Authorization`, sin cookie) — pegá
  esa salida.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: asset JS/CSS servido embebido, `Content-Type` correcto, sin red en runtime.
- [ ] T2: login válido → cookie `HttpOnly`+`Secure`+`SameSite=Strict` + redirect 303; login
  inválido → mensaje genérico; logout → cookie borrada + redirect.
- [ ] T3: `GET /` con cookie válida → 200; sin cookie/inválida → 302 a `/login`. `Identity` en
  contexto vía la misma `identityKey{}` de `authz.go`.
- [ ] T4: round-trip completo probado con `httptest.NewTLSServer`, rutas JSON existentes
  confirmadas sin cambios.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias Go nuevas (stdlib: `html/template`, `embed`, `net/http` alcanza). El único
  archivo nuevo "externo" es el JS de htmx vendorizado como asset estático, no una dependencia Go.
- NO commitear (el orquestador commitea tras verificar).
- No rompas ninguna ruta JSON existente (`/health`, `/auth/login`, `/whoami`, `/articles/*`) ni su
  contrato de respuesta — son de contratos anteriores y tienen tests propios que deben seguir en
  verde sin tocarlos.
- ABORTAR SI: conseguir un archivo real de htmx sin acceso a red te resulta imposible en tu
  entorno — documentá el intento (comando exacto, error) y BLOQUEADO; no inventes/generes un
  archivo JS a mano que no sea el htmx real (romper esa integridad sería peor que bloquear).

## Checklist antes de delegar

- [ ] RECON corrido: funciones de auth reusables confirmadas, patrón `Identity`/`identityKey{}`
  confirmado, nota de `httptest.NewTLSServer` para cookies `Secure` incluida.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿qué pasa si alguien manda una cookie con un JWT válido pero FIRMADO CON OTRO
  SECRETO (forjada)? (`VerifyJWT` ya rechaza por firma — confirmalo con un test explícito acá,
  no asumas que el test de CONTRACT-02 alcanza). ¿Qué pasa con una cookie con un JWT EXPIRADO?
  (302 a login, igual que ausente). ¿El mensaje de error del login filtra por timing la diferencia
  entre usuario inexistente y password incorrecta? (ya mitigado por `VerifyCredentials`
  — confirmá que la UI no lo reintroduce comparando antes de llamarlo).
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación EN NAVEGADOR pendiente la hace el orquestador (Claude Browser) después de
  integrar — el dev no necesita un navegador real, solo tests HTTP con `httptest`.

# CONTRACT-06 — Fundación de UI — Reporte

Primer contrato de la fase 2 (UI web). Implementa el layout base + assets embebidos,
login/logout con sesión de navegador (JWT en cookie), y una home protegida — todo sobre el
MISMO `mux`/`handlers` y la MISMA capa de autorización (`Identity`/`identityKey{}`) de la fase 1.
Sin dependencias Go nuevas (solo stdlib: `embed`, `html/template`, `net/http`). El único asset
"externo" es `htmx.min.js` vendorizado como archivo real del repo.

## Archivos

- `internal/server/assets/htmx.min.js` (nuevo) — htmx 2.0.4 real, descargado en sesión.
- `internal/server/assets/app.css` (nuevo) — CSS propio, variables, sin framework.
- `internal/server/templates/{layout,login,home}.html` (nuevos) — plantillas `html/template`.
- `internal/server/ui.go` (nuevo) — rutas UI, middleware `requireSession`, serving de assets.
- `internal/server/server.go` (modificado) — 3 líneas: llama a `h.registerUIRoutes(mux)`.
- `internal/server/server_ui_test.go` (nuevo) — 10 tests de aceptación + red-team.

Ninguna ruta JSON existente (`/health`, `/auth/login`, `/whoami`, `/articles/*`) fue tocada;
sus tests siguen en verde sin modificación.

## Origen del htmx (integridad verificada)

Descargado DURANTE la sesión de `unpkg.com`, verificado contra un segundo mirror (`jsdelivr`):

```
curl -sSL -o internal/server/assets/htmx.min.js https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js
unpkg:    e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447
jsdelivr: e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447
```

SHA-256 idénticos entre ambos CDN → archivo auténtico y completo (50917 bytes, versión 2.0.4).
Se sirve SOLO desde el binario vía `//go:embed`; nunca desde un CDN en runtime.

## Decisiones de diseño (con porqué)

1. **Nombre de la cookie de sesión: `librarian_session`.**
   Prefijo de app explícito → inequívoca en un cookie jar de host compartido y trivial de
   targetear para borrado en logout. Atributos: `HttpOnly` (JS no la lee → mitiga XSS-robo de
   token), `Secure` (solo HTTPS), `SameSite=Strict` (mitiga CSRF en el POST de logout/acciones),
   `Path=/`. `MaxAge=86400` espeja el TTL de 24h del JWT (`auth.IssueJWT`); aun así
   `requireSession` **revalida el JWT en cada request**, así que una cookie presente-pero-expirada
   se trata igual que ausente (302 a /login).

2. **Path de assets embebidos: `internal/server/assets/`, servidos bajo la ruta `/static/`.**
   Junto al código que los sirve (mismo paquete `server`) → el `//go:embed` es local y el
   acoplamiento es visible. Ruta pública `/static/{path...}` (wildcard Go 1.22). El
   `Content-Type` se fija por extensión de forma **explícita** (`text/javascript`, `text/css`),
   NO vía `mime.TypeByExtension`, porque en Windows el registro puede devolver un tipo distinto
   para `.js`/`.css` — el server no debe depender del estado del host y el test exige un valor
   exacto.

3. **Layout HTML base.**
   `layout.html` define `{{define "layout"}}` con `<nav>` (marca + control de logout condicional
   a `.Authenticated`) y un bloque de contenido `{{template "content" .}}`. Cada página
   (`login`, `home`) es su **propio set de plantillas** (`layout + página`) parseado por separado,
   para evitar la colisión sobre la definición compartida `"content"` que produciría un único set
   combinado con ParseFS. `<script src="/static/htmx.min.js" defer>` — htmx es progresivo, el
   login funciona sin JS.

## Trade-offs

- `GET /` es catch-all del sitio HTML (patrón menos específico); las rutas JSON más específicas
  ganan por precedencia de `ServeMux`. `handleHome` hace `404` para cualquier path ≠ `/` para no
  renderizar home espuriamente en subrutas no definidas.
- Fallo de login re-renderiza el form con `401` (status honesto) + mensaje genérico. El status no
  estaba mandado por el contrato; se eligió 401 por coherencia semántica.
- El mensaje de error del login es una **constante única** (`genericLoginError`) usada para
  usuario inexistente y password incorrecta por igual; la UI no compara ni ramifica antes de
  llamar a `auth.VerifyCredentials`, así que no reintroduce el leak de enumeración que esa función
  ya cierra (los cuerpos de ambas respuestas son byte-idénticos, verificado por test).

## Verificación (salida REAL)

### `go build ./...` y `go vet ./...`

```
BUILD OK
VET OK
```

### `go test ./... -count=1` (DOS veces, ambas verdes)

```
===== RUN 1 =====
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	2.652s
ok  	github.com/MauricioPerera/librarian/internal/config	0.625s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.278s
ok  	github.com/MauricioPerera/librarian/internal/server	7.846s
ok  	github.com/MauricioPerera/librarian/internal/store	2.202s
===== RUN 2 =====
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	2.719s
ok  	github.com/MauricioPerera/librarian/internal/config	0.644s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.303s
ok  	github.com/MauricioPerera/librarian/internal/server	7.753s
ok  	github.com/MauricioPerera/librarian/internal/store	2.211s
```

### Detalle por criterio (`go test -run TestUI -v`)

```
=== RUN   TestUIStaticAssetsEmbedded
--- PASS: TestUIStaticAssetsEmbedded (0.05s)          # T1: JS/CSS embebido, Content-Type exacto, bytes == archivo vendorizado, sin red
=== RUN   TestUILoginSuccessSetsCookie
--- PASS: TestUILoginSuccessSetsCookie (0.15s)        # T2: 303 a /, cookie HttpOnly+Secure+SameSite=Strict+Path=/
=== RUN   TestUILoginInvalidGenericError
--- PASS: TestUILoginInvalidGenericError (0.19s)      # T2: mensaje genérico idéntico (wrong-pw == unknown-user), sin cookie — anti-enumeración
=== RUN   TestUILogoutClearsCookie
--- PASS: TestUILogoutClearsCookie (0.05s)            # T2: cookie MaxAge<0 (borrado) + 303 a /login
=== RUN   TestUIHomeNoCookieRedirects
--- PASS: TestUIHomeNoCookieRedirects (0.05s)         # T3: sin cookie → 302 a /login
=== RUN   TestUIHomeInvalidCookieRedirects
--- PASS: TestUIHomeInvalidCookieRedirects (0.05s)    # T3: cookie basura → 302 a /login
=== RUN   TestUIRoundTrip
--- PASS: TestUIRoundTrip (0.15s)                     # T4: POST /login → cookie → GET / autenticado → 200 con email (httptest.NewTLSServer)
=== RUN   TestUIForgedJWTCookieRejected
--- PASS: TestUIForgedJWTCookieRejected (0.05s)       # Red-team (a): JWT firmado con OTRO secreto → 302, sin panic/500
=== RUN   TestUIExpiredJWTCookieRejected
--- PASS: TestUIExpiredJWTCookieRejected (0.05s)      # Red-team (b): JWT real EXPIRADO → 302, igual que ausente
=== RUN   TestUIJSONRoutesUnaffected
--- PASS: TestUIJSONRoutesUnaffected (0.15s)          # T4: /auth/login (JSON) + /whoami (header Bearer, sin cookie) intactos; login JSON NO setea cookie
PASS
ok  	github.com/MauricioPerera/librarian/internal/server	4.476s
```

## Estado

Árbol SIN commitear (el orquestador commitea tras verificar). Modificado: `server.go`.
Nuevos: `assets/`, `templates/`, `ui.go`, `server_ui_test.go`, este reporte.
Verificación en navegador pendiente la hace el orquestador (Claude Browser).

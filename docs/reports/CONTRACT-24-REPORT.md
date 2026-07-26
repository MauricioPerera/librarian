# CONTRACT-24 — Separar "vivo" de "listo", y no disfrazar fallos de infraestructura

Base: `88059f8` en `main`. Árbol **SIN commitear**, como pide el contrato.

**Resultado: LISTO, verificado con el contenedor de PostgreSQL 17 REALMENTE DETENIDO** (`docker
stop pg-c24`, estado `exited`), contra el binario real corriendo sobre ese motor. Las tres
observaciones que cierran el hueco, medidas en esa condición:

```
GET  /health        -> {"status":"ok"}                  [HTTP 200]   el proceso vive: sigue diciéndolo
GET  /ready         -> {"error":"service unavailable"}  [HTTP 503]   la disponibilidad: dice la verdad
POST /auth/login    -> {"error":"service unavailable"}  [HTTP 503]   ya NO es 401
```

Y con la base viva, lo que no podía cambiar sigue igual: un usuario inexistente y un usuario
existente con contraseña mala devuelven **el mismo 401 con el mismo cuerpo**,
`{"error":"invalid credentials"}`, byte por byte el de antes de este contrato.

Las decisiones que el contrato pedía tomar y justificar:

- **El código de estado es `503 Service Unavailable`**, no 500. El caso es "no puedo atender AHORA",
  no "estoy roto" ni "te equivocaste". 503 es lo que un balanceador entiende como *sacá esta
  instancia de rotación y reintentá*, es sobre lo que enganchan las políticas de reintento y las
  reglas de monitoreo, y una base caída es transitoria por naturaleza. 500 reportaría un defecto
  permanente de este proceso —el diagnóstico exactamente equivocado— y además ya está en uso para
  los errores internos de verdad (firmar un token, una fila corrupta); mantenerlos separados es el
  punto.
- **La forma de la ruta nueva es `GET /ready`**, hermana de `/health` y no un cambio suyo.
  `/health` conserva significado, código y cuerpo byte-exacto porque hay monitoreo apuntado ahí.
  El par separado es además la única forma que permite distinguir los dos fallos de un vistazo:
  `/health` 200 + `/ready` 503 significa "el proceso está arriba, su base no", que es justamente
  el diagnóstico que faltaba. Se llama `/ready` y no `/readyz` porque este proyecto ya escribe
  `/health` y no `/healthz`, y el par tiene que leerse como par.

`sqlite-postgres-compat` **no se tocó**. Ninguna dependencia nueva, ningún permiso nuevo, ningún
test preexistente modificado (`git diff --stat -- '*_test.go'` **vacío**).

---

## Resumen de qué se tocó

| Archivo | Qué |
|---|---|
| `internal/server/readiness.go` (**nuevo**) | **T2**: `/ready`, el timeout propio, la memoización, y el vocabulario compartido de "esto es infraestructura" (`infraUnavailableStatus`, `infraUnavailableMessage`, `writeInfraUnavailable`) |
| `internal/server/server.go` | **T1**: la guarda de `handleLogin` **con el razonamiento anti-enumeración escrito al lado**; `handleWhoami` usa el nuevo error; registro de `/ready`; `handlers.ready`; nota en `handleHealth` de por qué NO cambió |
| `internal/server/authz.go` | **T1**: `resolveIdentity` devuelve `error` en vez de `ok bool` — `errIdentityRejected` → 401, error de base → 503; `writeIdentityError` como único punto de mapeo; `authenticate` lo usa |
| `internal/server/ui.go` | **T1**, mitad navegador: el login HTML deja de culpar al usuario por una caída (`unavailableLoginError`, 503) |
| `docs/DEPLOY.md` | **T3**: la verificación post-deploy consulta `/ready` y **corta el deploy** si da 503 |
| `docs/PENDIENTES.md` | hueco 7 marcado RESUELTO |

**Tests nuevos** (2 archivos, ninguno preexistente tocado):

- `internal/server/server_contract24_test.go` — la batería HTTP: `/ready` verde con base viva; las
  cuatro observaciones con la base caída; el 401 de credenciales intacto e indistinguible; el login
  HTML.
- `internal/server/readiness_contract24_internal_test.go` — las dos propiedades que no se ven desde
  afuera: que una base lenta pero viva no cuelga la sonda, y que la sonda no se puede convertir en
  vía de carga.

---

## T1 — Distinguir el fallo de infraestructura del credencial inválido

### La tensión, resuelta explícitamente (y escrita en el código)

El colapso a 401 no era gratuito y **no se tocó**. Existe para que un atacante no pueda distinguir
"ese usuario no existe" de "esa contraseña está mal"; `auth.VerifyCredentials` llega a correr un
`bcrypt` contra un hash fijo en la rama del email inexistente para igualar hasta el TIEMPO. Nada de
eso cambió: `internal/auth` **no tiene ni una línea modificada**.

Lo que se separó es de otra clase. La respuesta "la base no contesta" es **función exclusiva de la
infraestructura**: es idéntica para todo email y toda contraseña, incluidos los que no existen y las
que están mal, así que observarla no le dice a un atacante nada sobre ninguna cuenta — la rama nunca
llegó a mirar una cuenta. Ese razonamiento está escrito **junto a la guarda**, en
`internal/server/server.go`, encabezado con `READ THIS BEFORE "FIXING" IT BACK`, para que nadie lo
revierta creyendo que reintroduce enumeración. `resolveIdentity` remite a él en vez de repetirlo.

Corolario respetado: la respuesta de infraestructura **no lleva detalle**. Ni el error del driver,
ni el DSN, ni el host, ni si la base existe o solo rechazó la conexión. Un código de estado y un
mensaje fijo, `{"error":"service unavailable"}`. Hay un test (`assertNoInfraDetail24`) que exige que
el cuerpo tenga **exactamente un campo** con **exactamente ese texto**.

### Los puntos que se arreglaron, y por qué son esos

| Punto | Antes | Ahora |
|---|---|---|
| `handleLogin` (JSON) | 401 para cualquier error de `VerifyCredentials` | 401 solo si `errors.Is(err, auth.ErrInvalidCredentials)`; si no, 503 |
| `handleLoginSubmit` (HTML) | 401 + "Email o contraseña incorrectos" | igual para credenciales; 503 + mensaje de indisponibilidad si es infraestructura |
| `resolveIdentity` → `authenticate` (todas las rutas con Bearer) | `ok bool`: fallo de base y token rechazado eran el MISMO valor | `error`: `errIdentityRejected` → 401 idéntico; error de la consulta de API key → 503 |
| `handleWhoami` | ídem | ídem, por el mismo `writeIdentityError` |

`resolveIdentity` era el punto que producía el `GET /content-types -> 401` de la medición: con la
base caída, el JWT falla (token inválido) y la búsqueda de API key **no puede ni ejecutarse**, pero
`ok=false` no sabía decir la diferencia. Ahora `auth.VerifyAPIKey` ya distinguía
(`ErrAPIKeyRejected` vs. `query api key: %w`) y esa distinción por fin sobrevive hasta el HTTP.

Puntos **revisados y deliberadamente no tocados**:

- `requireSession` / `sessionIdentity` (ui.go): validan el JWT de la cookie, no tocan la base, así
  que no pueden fallar por infraestructura. Su redirección a `/login` es correcta y se conserva.
- `requirePermission` y `requireSessionPermission` ante un fallo de `permissionsFor`: **ya**
  devolvían 500, no 401. No estaban rotos.
- Las guardas `identityFromContext` de `articles.go` / `products.go` / `content.go`: son defensa
  contra un nil-deref si alguien montara el handler sin middleware. No consultan la base.

### El 401 que tiene que seguir igual

```
POST /auth/login (password mala, usuario existe)   -> {"error":"invalid credentials"} [HTTP 401]
POST /auth/login (usuario inexistente)             -> {"error":"invalid credentials"} [HTTP 401]
```

Mismo código, mismo cuerpo, mismo mensaje, indistinguibles entre sí. Medido contra el PostgreSQL
real, y fijado por `TestCredentialFailuresAreUnchangedAndIndistinguishable`, que compara los dos
cuerpos campo por campo y además exige el texto literal `invalid credentials`, de modo que
cambiarlo en el futuro rompe el test.

---

## T2 — Separar "vivo" de "listo"

`/health` **no cambió**: mismo handler sin dependencias, misma constante, mismo
`{"status":"ok"}` byte-exacto. Se le agregó un comentario que dice por qué no cambió, para que la
próxima persona no "lo complete".

`GET /ready` hace un **ping al pool** (`sql.DB.PingContext`), deliberadamente la comprobación de
alcanzabilidad más barata que existe: toma una conexión y pregunta al driver si vive. No corre
ninguna consulta nuestra, no toca ninguna tabla, no lee ninguna fila — no se puede convertir en una
vía de extracción de datos ni en un escaneo.

Los tres requisitos fijados por el contrato, y cómo se cumplen:

1. **Límite de tiempo propio** — `readinessTimeout = 2s`, un `context.WithTimeout` que desciende del
   contexto de la petición (así un cliente que se desconecta cancela el ping en vez de dejarlo
   corriendo). Dos segundos está muy por encima de un ida y vuelta sano y muy por debajo del timeout
   de sonda de cualquier balanceador.
2. **No filtra detalle** — ver arriba. El error del driver se **descarta** en el handler; tampoco se
   loguea desde ahí, porque un endpoint sin autenticar que cualquiera puede machacar no puede ser una
   forma de llenar los logs del servicio.
3. **5xx, no un 200 con un campo** — un balanceador lee la línea de estado. Un
   `200 {"ready":false}` deja una instancia muerta en rotación, que es exactamente el fallo que este
   endpoint existe para evitar.

**Y el requisito que un ping pelado NO cumplía:** "no puede convertirse en una vía de carga contra la
base". `/ready` es forzosamente **sin autenticar** (un balanceador no tiene credenciales), así que
cualquiera lo puede llamar a la velocidad que quiera, y cada llamada sacaría una conexión del pool
— bajo una avalancha, la propia sonda mataría de hambre al tráfico real sobre el que informa. Por eso
el veredicto se **memoriza un segundo** (`readinessTTL`), lo que acota el costo a **como mucho un
ping por segundo sin importar la tasa de peticiones**. Un segundo de desactualización es irrelevante
para el consumidor: balanceadores y monitores sondean en intervalos de varios segundos.

El reloj de esa caché es `time.Now` y **NO** `handlers.now`. `handlers.now` es el reloj de NEGOCIO,
que los tests congelan para obtener timestamps de JWT deterministas; congelarlo aquí congelaría la
caché para siempre y una base detenida seguiría reportándose lista. Está comentado en el código y
fijado por `TestReadinessVerdictExpires`.

---

## T3 — Que el runbook lo use

`docs/DEPLOY.md`, sección "Verificación post-deploy", paso 1, ahora se titula **"El servicio está
vivo *y además puede servir*"** y son dos comprobaciones obligatorias:

```bash
curl -s https://librarian.ardf.dev/health                                    # {"status":"ok"}
curl -s -o /dev/null -w '%{http_code}\n' https://librarian.ardf.dev/ready    # 200
curl -s https://librarian.ardf.dev/ready                                     # {"status":"ready"}
```

Con la instrucción explícita de que **un 503 en `/ready` corta el deploy** aunque el binario haya
arrancado y `/health` esté verde, con adónde mirar (el motor y `journalctl`), con la aclaración de
que la respuesta no trae detalle a propósito, con el síntoma asociado (login 503, no 401) para
cuando aparezca en una incidencia, y con qué ruta debe consultar un balanceador (`/ready`) frente a
cuál sirve para reiniciar un proceso colgado (`/health`). El bloque de deploy A también gana la
línea de `/ready`.

---

## T4 — LA PRUEBA QUE CIERRA EL HUECO

Contenedor `pg-c24` (PostgreSQL 17 con `pgvector`) en el VPS, dedicado, arrancado y parado por SSH.
Binario real (`go build ./cmd/librarian`) corriendo en `127.0.0.1:8524` con
`LIBRARIAN_ENGINE=postgres` y `LIBRARIAN_DB=postgres://postgres:***@31.220.22.176:5448/postgres?sslmode=disable`.
Identidad creada con el `--bootstrap` real.

### 1. Base ARRANCADA (línea base)

```
$ docker ps -a --filter name=pg-c24 --format '{{.Names}} {{.Status}}'
pg-c24 Up 3 minutes

GET /health                                          -> {"status":"ok"} [HTTP 200]
GET /ready                                           -> {"status":"ready"} [HTTP 200]
POST /auth/login (credenciales correctas)            -> {"token":"<jwt>"} [HTTP 200]
POST /auth/login (password mala, usuario existe)     -> {"error":"invalid credentials"} [HTTP 401]
POST /auth/login (usuario inexistente)               -> {"error":"invalid credentials"} [HTTP 401]
GET /content-types (bearer basura)                   -> {"error":"unauthorized"} [HTTP 401]
```

### 2. Base DETENIDA DE VERDAD

```
$ docker stop pg-c24 && docker ps -a --filter name=pg-c24 --format '{{.Names}} {{.State}} {{.Status}}'
pg-c24
pg-c24 exited Exited (0) Less than a second ago

GET /health                                        -> {"status":"ok"} [HTTP 200]
GET /ready                                         -> {"error":"service unavailable"} [HTTP 503]
POST /auth/login (credenciales CORRECTAS)          -> {"error":"service unavailable"} [HTTP 503]
POST /auth/login (password mala)                   -> {"error":"service unavailable"} [HTTP 503]
GET /content-types (bearer basura)                 -> {"error":"service unavailable"} [HTTP 503]
POST /login (form HTML, credenciales correctas)    -> El servicio no está disponible en este momento.
                                                      Volvé a intentar en unos minutos. [HTTP 503]
```

El login se probó con la contraseña **correcta** de un usuario que existe de verdad: es el caso
donde el 401 anterior era más obviamente una mentira.

### 3. La sonda no se cuelga — medido contra la infraestructura real

Detener el contenedor deja el puerto sin nadie que complete el handshake, así que el driver se queda
esperando. El deadline propio de la sonda es lo que corta:

```
  intento 1: HTTP 503 en 2.001751s     <- readinessTimeout (2s) cortando un driver colgado
  intento 2: HTTP 503 en 0.000759s     <- veredicto memorizado
  intento 3: HTTP 503 en 0.000946s
  intento 4: HTTP 503 en 0.001106s
  intento 5: HTTP 503 en 0.001061s
```

Esa primera línea **es** el caso red-team de "base lenta pero viva", ocurrido contra infraestructura
real y no simulado: sin el timeout, `/ready` se habría colgado indefinidamente. Las siguientes son la
memoización acotando el costo. El veredicto caduca y se vuelve a sondear:

```
  inmediato: HTTP 503 en 2.017356s
  tras 1.5s: HTTP 503 en 2.022794s (re-sondeo real)
```

Y con la base viva, 200 peticiones seguidas a `/ready` tardaron **12,44 s** en total (dominadas por
el arranque de `curl` y la ida y vuelta WAN), con **como mucho ~12 pings reales** a la base por el
TTL de un segundo, en vez de 200.

### 4. Base ARRANCADA DE NUEVO — recuperación sin reiniciar el proceso

```
$ docker start pg-c24
pg-c24 running Up 3 seconds

GET /health                                        -> {"status":"ok"} [HTTP 200]
GET /ready                                         -> {"status":"ready"} [HTTP 200]
POST /auth/login (credenciales correctas)          -> {"token":"<jwt>"} [HTTP 200]
POST /auth/login (password mala)                   -> {"error":"invalid credentials"} [HTTP 401]
POST /auth/login (usuario inexistente)             -> {"error":"invalid credentials"} [HTTP 401]
GET /content-types (bearer basura)                 -> {"error":"unauthorized"} [HTTP 401]
GET /content-types (JWT valido)                    -> {"content_types":[]} [HTTP 200]
GET /whoami (JWT valido)                           -> {"auth":"jwt","email":"c24@example.com",
                                                       "roles":["administrator"],"user_id":"cf1e…"} [HTTP 200]
```

El mismo proceso, sin reiniciar el binario, vuelve solo a estado servible y el 401 de credenciales
regresa exactamente como estaba. Eso es lo que hace que el 503 sea el código correcto: es
transitorio, y se comporta como tal.

### El otro motor

Contra SQLite la prueba equivalente es la del suite (`TestDatabaseDownIsReportedAsInfrastructure`),
que **cierra el pool real** — un `*sql.DB` que ya no tiene conexión que dar, no un doble que devuelve
un error enlatado. **No existe un "detener el contenedor" para SQLite**, porque la base está
embebida en el mismo proceso: si el proceso vive, la base vive. Eso no es una laguna de la
verificación, es exactamente la razón por la que el contrato dice que PostgreSQL es obligatorio y
por la que el hueco 7 importa desde CONTRACT-21 y no antes.

---

## Red-team: las preguntas del contrato, respondidas

**¿Una base lenta pero viva cuelga la comprobación, o corta por tiempo?** Corta por tiempo, y está
medido arriba contra la infraestructura real (2,0017 s) y fijado por
`TestReadinessDoesNotHangOnASlowDatabase`, cuyo ping solo retorna cuando se cancela el contexto que
recibió, de modo que el test solo pasa si el deadline propio de la sonda es lo que lo termina.
Consecuencia de diseño asumida: una base demasiado lenta para responder en 2 s se reporta **no
disponible**, que para servir tráfico es la respuesta honesta.

**¿La ruta nueva sin autenticar filtra algo útil a un atacante (existencia de la base, latencia)?**
Filtra exactamente una cosa: si esta instancia puede servir o no. Es el propósito del endpoint y un
balanceador no puede tener credenciales. Más allá de eso, nada: ni el motor, ni el host, ni el DSN,
ni el error del driver, ni si la base existe frente a si rechazó la conexión — todos los fallos dan
la misma línea. La latencia es la única señal residual y está **acotada por arriba** por el timeout
y **aplanada** por el TTL: repetir la sonda no permite cronometrar la base, porque las respuestas
siguientes salen de la caché.

**¿Un error de base a mitad de una petición ya autenticada?** No cambió y no estaba roto:
`requirePermission` y `requireSessionPermission` ya devolvían 500 ante un fallo de `permissionsFor`,
y los handlers de datos ya mapeaban sus errores de base a 500. El defecto estaba solo en el punto de
*resolución de identidad*, que colapsaba a 401 y ahora no.

**¿El 5xx nuevo rompe algún test o cliente que esperaba 401?** Ningún test: `go test ./... -count=1`
verde dos veces y `git diff --stat -- '*_test.go'` vacío. Ningún cliente que se comportara bien: el
5xx aparece **solo** cuando la base no contesta, situación en la que el cliente antes recibía un 401
falso. Con la base viva, cada 401 de antes sigue siendo un 401.

**¿Qué pasa con la UI, que redirige a `/login` ante un 401?** Es la pregunta más filosa y la respuesta
tiene dos partes.

- La redirección a `/login` la hace `requireSession` (302), y **solo** ante una cookie ausente,
  malformada, mal firmada o vencida. Eso es criptografía sobre el JWT, sin base de por medio, así que
  una caída de la base **nunca** provocó ni provoca esa redirección. No hay bucle de redirección.
- Lo que la UI sí hacía mal es lo que se arregló: el formulario de login respondía 401 con **"Email o
  contraseña incorrectos"** mientras la base estaba caída. En la UI eso es peor que en la API, porque
  le dice a una persona, con palabras, que se equivocó de contraseña — así que la vuelve a tipear,
  después la resetea, y la caída sigue invisible mientras siga creyéndole al mensaje. Ahora responde
  503 con un mensaje que no habla de credenciales y no nombra ninguna causa
  (`TestBrowserLoginDoesNotBlameTheUserForAnOutage` exige las dos cosas: que el 503 **no** contenga
  el mensaje de credenciales, y que la contraseña realmente mala **sí** lo siga conteniendo con su
  401). El formulario de `templates/login.html` es un `<form method="post">` HTML plano —no htmx—,
  así que el navegador simplemente renderiza la página devuelta: el usuario ve el banner en el mismo
  formulario, sin navegación silenciosa a ningún lado. Es la misma mecánica que ya tenía el 401,
  solo cambian el código y el texto.

---

## Un hallazgo colateral, dicho tal cual (NO se arregló: fuera de alcance)

Al correr la batería `dualengine` de `internal/auth` **después** de mi prueba e2e, falló con
`transcript length differs: sqlite=81 postgres=83`. **No es una regresión de este contrato**
(`internal/auth` no tiene ni una línea modificada): es que mi propio `--bootstrap` del e2e creó las
tablas de `librarian` en el esquema `public` de esa base, y las baterías dual-motor se aíslan en un
esquema propio pero con `search_path=<esquema>,public`. `EnsureSchema` vio las tablas por ese
fallback, no las creó en el esquema aislado, y la corrida terminó leyendo mis filas. Limpiado el
`public` (`DROP SCHEMA public CASCADE; CREATE SCHEMA public; CREATE EXTENSION vector`), las baterías
vuelven a pasar.

Queda anotado porque el aislamiento de esas baterías tiene un agujero latente —cualquier consumidor
que deje tablas de `librarian` en `public` de la misma base las contamina en silencio— pero
arreglarlo es tocar tests preexistentes de otros contratos y ampliar el alcance por mi cuenta, cosa
que el contrato prohíbe. **Recomendación para el orquestador:** si reproduce T4 contra esta base,
que limpie `public` después, o que corra el e2e con `?search_path=` en un esquema dedicado.

---

## Cómo reproducir T4 (el orquestador va a correrlo)

```bash
# 0. Contenedor arriba
ssh vps 'docker start pg-c24'

cd D:/Repo/librarian
export LIBRARIAN_ENGINE=postgres
export LIBRARIAN_DB='postgres://postgres:***@31.220.22.176:5448/postgres?sslmode=disable'
export LIBRARIAN_JWT_SECRET=c24-e2e-secret
export LIBRARIAN_ADDR=127.0.0.1:8524

go build -o /tmp/librarian.exe ./cmd/librarian
printf 'c24-admin-password\n' | /tmp/librarian.exe --bootstrap --email c24@example.com
/tmp/librarian.exe &            # deja el servidor corriendo

B=http://127.0.0.1:8524
# 1. Línea base (base viva)
curl -s $B/health ; curl -s $B/ready
curl -s -w ' [%{http_code}]\n' -X POST $B/auth/login -H 'Content-Type: application/json' \
     -d '{"email":"c24@example.com","password":"MALA"}'          # 401 invalid credentials
curl -s -w ' [%{http_code}]\n' -X POST $B/auth/login -H 'Content-Type: application/json' \
     -d '{"email":"fantasma@example.com","password":"MALA"}'     # 401 invalid credentials, idéntico

# 2. LA PRUEBA: parar la base de verdad
ssh vps 'docker stop pg-c24'
curl -s -w ' [%{http_code}]\n' $B/health     # {"status":"ok"}                 200
curl -s -w ' [%{http_code}]\n' $B/ready      # {"error":"service unavailable"} 503
curl -s -w ' [%{http_code}]\n' -X POST $B/auth/login -H 'Content-Type: application/json' \
     -d '{"email":"c24@example.com","password":"c24-admin-password"}'   # 503, NO 401

# 3. Recuperación y limpieza
ssh vps 'docker start pg-c24'
curl -s $B/ready                              # {"status":"ready"} 200
ssh vps "docker exec pg-c24 psql -U postgres -d postgres -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public; CREATE EXTENSION vector;'"
```

El contenedor `pg-c24` quedó **ARRANCADO**, como pide el enunciado, y su `public` quedó **limpio**.

---

## Verificación de la suite

```
$ gofmt -l .
(vacío)

$ go build ./...        OK
$ go vet ./...          OK
$ go vet -tags dualengine ./...   OK

$ go test ./... -count=1          (PASADA 1)
ok  github.com/MauricioPerera/librarian/cmd/librarian     2.869s
ok  github.com/MauricioPerera/librarian/internal/auth     7.045s
ok  github.com/MauricioPerera/librarian/internal/config   1.260s
ok  github.com/MauricioPerera/librarian/internal/dual     1.285s
ok  github.com/MauricioPerera/librarian/internal/schema   1.392s
ok  github.com/MauricioPerera/librarian/internal/server  38.852s
ok  github.com/MauricioPerera/librarian/internal/store    4.111s

$ go test ./... -count=1          (PASADA 2)
ok  github.com/MauricioPerera/librarian/cmd/librarian     2.757s
ok  github.com/MauricioPerera/librarian/internal/auth     6.522s
ok  github.com/MauricioPerera/librarian/internal/config   1.252s
ok  github.com/MauricioPerera/librarian/internal/dual     1.251s
ok  github.com/MauricioPerera/librarian/internal/schema   1.344s
ok  github.com/MauricioPerera/librarian/internal/server  38.713s
ok  github.com/MauricioPerera/librarian/internal/store    3.930s
```

Tests nuevos de este contrato, en detalle:

```
--- PASS: TestReadinessDoesNotHangOnASlowDatabase (2.00s)
--- PASS: TestReadinessProbeIsMemoizedSoItCannotBecomeALoadPath (0.00s)
--- PASS: TestReadinessVerdictExpires (0.00s)
--- PASS: TestReadyIsGreenWhileTheDatabaseAnswers (0.08s)
--- PASS: TestDatabaseDownIsReportedAsInfrastructure (0.11s)
--- PASS: TestCredentialFailuresAreUnchangedAndIndistinguishable (0.20s)
--- PASS: TestBrowserLoginDoesNotBlameTheUserForAnOutage (0.17s)
```

Contratos anteriores contra PostgreSQL 17 real (tag `dualengine`, sobre un `public` limpio) — las
baterías que comparan transcripción byte a byte entre SQLite y PostgreSQL de CONTRACT-19/20/20C/21/22/23:

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5448/postgres?sslmode=disable' \
    go test -tags dualengine -count=1 ./internal/server ./internal/auth
ok  github.com/MauricioPerera/librarian/internal/server  406.175s
ok  github.com/MauricioPerera/librarian/internal/auth    219.935s
```

---

## Criterios de aceptación

- [x] build/vet/gofmt limpios; `go test ./... -count=1` verde **dos veces**.
- [x] **T1**: fallo de infraestructura → 503 en todos los puntos que hoy daban 401 (login JSON,
      login HTML, resolución de identidad por Bearer, `/whoami`); credencial inválida → 401 idéntico
      al de hoy, mismo cuerpo y mismo mensaje.
- [x] **T2**: comprobación de disponibilidad con límite de tiempo propio (2 s), sin detalle, 503
      cuando la base no está; `/health` con su significado, su código y su cuerpo intactos.
- [x] **T3**: `docs/DEPLOY.md` la usa en la verificación post-deploy, y con la instrucción de cortar
      el deploy si da 503.
- [x] **T4**: probado con la base **REALMENTE detenida** (`docker stop pg-c24`, estado `exited`), con
      salida real pegada arriba.
- [x] La anti-enumeración sigue intacta (`internal/auth` sin cambios; usuario inexistente y
      contraseña mala indistinguibles, con test), **con el razonamiento escrito en el código** junto
      a la guarda de `handleLogin`.

## Restricciones

- [x] Solo archivos dentro de `librarian`. `sqlite-postgres-compat` **no se tocó**.
- [x] Sin dependencias nuevas (`go.mod` sin cambios). Ningún permiso nuevo.
- [x] **NO commiteado**: todo queda en el working tree.
- [x] `/health` no cambió de significado ni de respuesta.
- [x] El cuerpo y el mensaje del 401 de credenciales no cambiaron.
- [x] Ninguna respuesta HTTP lleva detalle del error de base.

## Archivos tocados

```
 M docs/DEPLOY.md
 M docs/PENDIENTES.md
 M internal/server/authz.go
 M internal/server/server.go
 M internal/server/ui.go
?? internal/server/readiness.go
?? internal/server/readiness_contract24_internal_test.go
?? internal/server/server_contract24_test.go
```

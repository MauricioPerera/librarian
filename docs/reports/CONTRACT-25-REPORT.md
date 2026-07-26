# CONTRACT-25 — Un fallo de base dice lo mismo en todas las rutas

Base: `88059f8` en `main`, sobre el árbol de CONTRACT-24. Árbol **SIN commitear**, como pide el
contrato.

**Resultado: LISTO.** Las dos mitades, medidas contra PostgreSQL 17 real y no afirmadas:

```
BASE REALMENTE DETENIDA (docker stop pg-c25, estado exited)
GET  /health                 -> {"status":"ok"}                  [HTTP 200]   el proceso vive
GET  /ready                  -> {"error":"service unavailable"}  [HTTP 503]   CONTRACT-24
GET  /articles  (JWT válido) -> {"error":"service unavailable"}  [HTTP 503]   ANTES ERA 500
GET  /admin/articles (sesión)-> service unavailable              [HTTP 503]   ANTES ERA 500

BASE ARRIBA Y SIRVIENDO (/ready = 200 medido en el mismo momento)
GET  /content/reviews        -> {"error":"could not list content"} [HTTP 500] SIGUE SIENDO 500
```

La segunda línea del segundo bloque es la que importa tanto como la primera. El fallo peligroso de
este contrato no era "la caída sigue diciendo 500", era **"ahora todo dice 503"**, que enterraría
bugs reales detrás de un "reintentá más tarde". El 500 de arriba es un fallo interno **genuino**,
provocado de verdad, con la base verificablemente viva en la misma medición.

`sqlite-postgres-compat` **no se tocó**. Ninguna dependencia nueva (`go.mod` sin cambios), ningún
permiso nuevo, **ningún test preexistente modificado** (`git diff --stat -- '*_test.go'` vacío).

---

## Resumen de qué se tocó

| Archivo | Qué |
|---|---|
| `internal/server/datafailure.go` (**nuevo**) | **T1**: el único punto de clasificación — `failureIsInfrastructure` y sus dos escritores, `writeOperationFailure` (JSON) y `httpOperationFailure` (HTML) |
| `internal/server/articles.go`, `content.go`, `contenttypes.go`, `products.go`, `terms.go` | **T1**: 37 sitios JSON pasando por el helper. Cambio mecánico de una línea en cada uno |
| `internal/server/ui.go`, `ui_apikeys.go`, `ui_articles.go`, `ui_content.go`, `ui_contenttypes.go`, `ui_products.go`, `ui_roles.go`, `ui_terms.go`, `ui_users.go` | **T1**: 53 sitios HTML, ídem |
| `internal/server/authz.go` | **T1**: el 500 de `requirePermission` ante un fallo de `permissionsFor` |
| `docs/PENDIENTES.md` | hueco 8 marcado RESUELTO |

**Tests nuevos** (2 archivos, ninguno preexistente tocado):

- `internal/server/server_contract25_test.go` — la batería HTTP: las rutas de datos con la base
  caída, el fallo interno genuino con la base arriba, y el camino feliz intacto.
- `internal/server/datafailure_contract25_internal_test.go` — las cuatro propiedades del
  clasificador que no se ven desde afuera.

---

## T1 — Un punto de clasificación

### La polaridad, que es lo delicado, y por qué es la INVERSA de la de CONTRACT-24

`CONTRACT-24` clasifica por **lista blanca de sentinelas**: en `writeIdentityError`, un error que no
es `errIdentityRejected` se trata como infraestructura. Ahí es correcto, porque la capa de identidad
tiene un sentinela claro para "el llamador se equivocó" y todo lo demás es, genuinamente, la consulta
que no pudo correr.

En las rutas de datos esa polaridad **no se copió**. Sus errores incluyen fallos internos genuinos
—una fila que no se puede interpretar, un valor que no serializa, una definición persistida que dejó
de validar— que **son** un 500 honesto. Tratarlos como infraestructura los disfrazaría de "reintentá
más tarde", que es el mismo pecado que este proyecto acaba de corregir, apuntando al revés.

Así que acá el reconocimiento es **explícito y angosto**: se reconoce el fallo de conexión → 503;
**todo lo demás sigue siendo 500**, con su mismo mensaje de antes. Ese razonamiento está escrito en
`internal/server/datafailure.go`, encabezando el archivo, para que nadie "unifique" las dos
polaridades más adelante creyendo que son la misma cosa.

### El punto único

```go
func (h *handlers) failureIsInfrastructure(ctx context.Context, err error) bool
```

Y dos escritores encima, uno por superficie:

| Superficie | Función | 500 (como siempre) | 503 |
|---|---|---|---|
| Rutas JSON | `writeOperationFailure(w, r, err, msg)` | `{"error": msg}` | `{"error":"service unavailable"}` |
| Rutas HTML del admin | `httpOperationFailure(w, r, err)` | `internal error` (texto plano) | `service unavailable` (texto plano) |

La rama vive **sólo** en `failureIsInfrastructure`. Los dos escritores no deciden nada: eligen cómo
renderizar. Ningún sitio de llamada tiene lógica propia — el cambio en cada uno de los 91 es una
línea, mecánico:

```diff
-		writeError(w, http.StatusInternalServerError, "could not list articles")
+		h.writeOperationFailure(w, r, err, "could not list articles")

-		http.Error(w, "internal error", http.StatusInternalServerError)
+		h.httpOperationFailure(w, r, err)
```

El 503 **descarta** el mensaje por sitio a propósito: una respuesta de infraestructura no lleva
detalle (regla de CONTRACT-24, `infraUnavailableMessage`), y qué operación estaba corriendo cuando se
cayó la base no es asunto del llamador. El 500 lo conserva **byte por byte**.

### Cómo decide, y por qué NO por tipo de error del driver

Pregunta la **sonda de disponibilidad de CONTRACT-24** —la misma memo que contesta `/ready`, así que
las dos no pueden discrepar— y lee "la operación falló **y** la base no está accesible" como
infraestructura. La alternativa evaluada era reconocer errores concretos del driver (`pgconn`,
códigos de `sqlite3`, `net.OpError` envuelto, `driver.ErrBadConn`). Se descartó, y el contrato pedía
justificarlo:

- **Ataría `librarian` a la taxonomía de errores de un motor**, que es exactamente lo que
  `sqlite-postgres-compat` existe para evitar. Habría que mantener el conjunto por motor y por
  versión de driver, y un driver que renombre o envuelva un error haría regresar el 503 a 500 **en
  silencio**, sin que nada falle ruidosamente.
- La sonda es una **observación positiva y agnóstica del motor** del hecho que queremos reportar
  ("¿este proceso alcanza su base ahora mismo?"), no una inferencia sobre la forma de un string.
- **No cuesta nada en el camino feliz**: sólo corre cuando una operación YA falló, y el veredicto se
  memoriza `readinessTTL`, así que una caída completa cuesta como mucho un ping por segundo sin
  importar la tasa de peticiones. Medido: dos rutas de datos en paralelo con la base caída
  compartieron **una sola** sonda (ver abajo).

### La carrera de la memoización, documentada

El veredicto se memoriza un segundo, así que puede estar hasta un segundo desactualizado **en las dos
direcciones**. Ambas están escritas en `datafailure.go`:

- **"Accesible" viejo**: la base murió entre el fallo de la operación y esta consulta, y el
  veredicto en caché todavía dice que estaba viva ⇒ **500 en vez de 503**. Es la **dirección
  conservadora** —se reporta como defecto de este proceso, no como caída transitoria— y el contrato
  dice explícitamente que es aceptable. **No se intentó eliminar.**
- **"Inaccesible" viejo**: la base volvió dentro del mismo segundo y la operación falló por una razón
  interna genuina ⇒ 503 por lo que en realidad era un 500. Esa ventana exige una caída que terminó
  hace menos de un segundo **y** un bug real disparando dentro de ella. Cerrarla significaría sondear
  sincrónicamente en cada fallo, que reintroduce justo la vía de carga que `readinessTTL` existe para
  evitar.

Achicar la ventana no es gratis y no vale la pena: el consumidor de estos códigos es un balanceador o
una política de reintentos que opera en horizontes de varios segundos.

### Los sitios que NO pasan por el helper, y por qué

El contrato avisa: "si en alguno hace falta pensar, es señal de que ese error no era un 500 genérico
y merece mención". Son cuatro, y ninguno toca la base:

| Sitio | Qué es | Por qué queda como 500 |
|---|---|---|
| `server.go` — `"could not issue token"` | `auth.IssueJWT` falló al firmar | Es HMAC en memoria. No hay base de por medio; una caída no puede producirlo. Un 503 acá sería mentira. |
| `ui.go:262` — `renderLogin(500, …)` | ídem, mitad navegador | ídem |
| `content.go` — `writeBindError` → `"could not process content"` | una definición persistida dejó de validar contra el cuerpo | Es **el ejemplo canónico** del 500 honesto que este contrato protege. Además es una función libre sin `h` ni `r`. |
| `ui_products.go:326` / `ui_terms.go:262` | `productWriteError` / `termWriteError` devolviendo `(500, "internal error")` | No escriben respuesta: devuelven un status que el llamador ramifica. Los **llamadores** sí pasan por `httpOperationFailure`. |

---

## T2 — Verificación

Contenedor `pg-c25` (PostgreSQL 17.10 con `pgvector`) en el VPS, dedicado, arrancado y parado por
SSH. Binario real (`go build ./cmd/librarian`) en `127.0.0.1:8525` con `LIBRARIAN_ENGINE=postgres` y
`LIBRARIAN_DB=postgres://postgres:***@31.220.22.176:5449/postgres?sslmode=disable`. Identidad creada
con el `--bootstrap` real.

```
$ docker exec pg-c25 psql -U postgres -c 'SELECT version()'
 PostgreSQL 17.10 (Debian 17.10-1.pgdg12+1) on x86_64-pc-linux-gnu ...
 extensions: plpgsql, vector
```

### 1. Línea base — base ARRANCADA

```
GET    /health                      -> {"status":"ok"}            [HTTP 200]
GET    /ready                       -> {"status":"ready"}         [HTTP 200]
POST   /auth/login (correctas)      -> {"token":"eyJhbGciOiJIUz…"} [HTTP 200]
GET    /articles                    -> {"articles":[]}            [HTTP 200]
GET    /products                    -> {"products":[]}            [HTTP 200]
GET    /terms                       -> {"terms":[]}               [HTTP 200]
GET    /content-types               -> {"content_types":[]}       [HTTP 200]
```

### 2. EL FALLO INTERNO GENUINO, con la base ARRIBA — cómo se provocó

**Provocación** (real, no afirmada): se creó el tipo dinámico `reviews` por la API real y se cargó
una fila; después, **directamente en PostgreSQL**, se borró la tabla que lo respalda dejando la
definición registrada:

```
$ docker exec pg-c25 psql -U postgres -d postgres \
    -c 'DROP TABLE "cpt_reviews";' \
    -c "SELECT to_regclass('public.cpt_reviews') AS cpt_reviews_ahora,
        (SELECT count(*) FROM content_types WHERE name='reviews') AS definicion_sigue_registrada;"
DROP TABLE
 cpt_reviews_ahora | definicion_sigue_registrada
-------------------+-----------------------------
                   |                           1
```

Es el estado que deja una migración a medio aplicar o un `DROP` a mano: el registro describe una
tabla que no existe, así que toda lectura de ese tipo compone un statement perfectamente válido sobre
una relación ausente y falla. **La conexión no se tocó**: el pool está intacto y el motor sigue
contestando.

```
GET    /ready                       -> {"status":"ready"}                 [HTTP 200]  <- LA BASE ESTÁ VIVA
GET    /content/reviews             -> {"error":"could not list content"} [HTTP 500]  <- 500, no 503
GET    /content/reviews/{id}        -> {"error":"could not read content"} [HTTP 500]
POST   /content/reviews             -> {"error":"could not create content"}[HTTP 500]
GET    /admin/content/reviews       -> internal error                     [HTTP 500]  <- la mitad HTML
   y las rutas sanas, en el mismo proceso y el mismo momento:
GET    /articles                    -> {"articles":[]}                    [HTTP 200]
```

El `/ready` 200 en la misma medición es lo que hace la afirmación defendible: ese 500 **no** se puede
achacar a la infraestructura, y el clasificador no lo hizo.

Dos provocaciones más baratas se probaron primero y **fallaron**; quedan anotadas porque su fracaso
informa:

- **Escribir un `field_type` inválido en `content_type_fields` es IMPOSIBLE**: el esquema se defiende
  solo, con un CHECK. `CHECK constraint failed: ("field_type" IN ('text','integer','decimal','boolean','date'))`.
- **Renombrar un campo declarado NO falla**: la lectura simplemente devuelve la columna desconocida
  como `null` y contesta 200. No es una provocación usable.

### 3. LA PRUEBA — base REALMENTE DETENIDA

```
$ docker stop pg-c25 && docker ps -a --filter name=pg-c25 --format '{{.Names}} {{.State}} {{.Status}}'
pg-c25
pg-c25 exited Exited (0) Less than a second ago
```

Rutas JSON, **con un JWT válido** (un JWT se verifica por firma y no toca la base, así que la
petición llega al HANDLER y falla en la operación de datos — que es exactamente el camino que
CONTRACT-24 no cubría):

```
GET    /health                      -> {"status":"ok"}                  [HTTP 200]  0.0013s
GET    /ready                       -> {"error":"service unavailable"}  [HTTP 503]  2.0190s
GET    /articles                    -> {"error":"service unavailable"}  [HTTP 503] 23.0588s
GET    /products                    -> {"error":"service unavailable"}  [HTTP 503] 23.0650s
GET    /terms                       -> {"error":"service unavailable"}  [HTTP 503] 23.0295s
GET    /content-types               -> {"error":"service unavailable"}  [HTTP 503] 23.0344s
GET    /content/reviews             -> {"error":"service unavailable"}  [HTTP 503] 23.0660s
POST   /articles        (permiso)   -> {"error":"service unavailable"}  [HTTP 503] 23.0428s
DELETE /articles/{id}   (permiso)   -> {"error":"service unavailable"}  [HTTP 503] 23.0468s
POST   /auth/login                  -> {"error":"service unavailable"}  [HTTP 503] 21.0446s
```

Las dos formas de middleware quedan cubiertas: `requireAuth` (falla la consulta del handler) y
`requirePermission` (falla el lookup de permisos antes de llegar al handler).

Rutas HTML del admin, con una sesión real por cookie (antes: **500 `internal error`**):

```
GET    /admin/articles              -> service unavailable  [HTTP 503] 23.0263s
GET    /admin/products              -> service unavailable  [HTTP 503] 23.0496s
GET    /admin/terms                 -> service unavailable  [HTTP 503] 23.0495s
GET    /admin/users                 -> service unavailable  [HTTP 503] 23.0407s
```

### 4. Sobre los ~23 s — es la topología, no la aplicación

Es el artefacto que el enunciado advierte y que `docs/PENDIENTES.md` ya había corregido una vez: el
cliente corre en Windows y la base está en el VPS, así que con el contenedor detenido los SYN van a
un puerto que el firewall descarta y Windows reintenta ~21 s antes de dar el connect por fallado.
**Ese tramo es del banco de pruebas.** La descomposición se ve en los propios números:

```
POST /auth/login  21.04s   = ~21 s de reintentos de SYN (CONTRACT-24, sin clasificador de por medio)
GET  /articles    23.05s   = ~21 s de esos reintentos + 2.02 s de la sonda (readinessTimeout)
GET  /ready        2.02s   = SÓLO la sonda, cortada por su deadline propio
```

O sea: **lo que agrega este contrato está acotado por `readinessTimeout`, 2 s**, y sólo en el camino
que ya falló. En la topología real de producción (aplicación y base en el mismo host, por
`127.0.0.1`) un puerto cerrado rechaza al instante y ese tramo de 21 s no existe — `/ready` medía
0,056 s en CONTRACT-24.

Y la memoización acota incluso esos 2 s. Dos rutas de datos **en paralelo** con la base caída:

```
GET /articles  -> [HTTP 503] 23.0198s
GET /products  -> [HTTP 503] 23.0096s
```

No se serializaron ni sumaron sondas: compartieron **una sola**, por el single-flight del mutex de
`readiness.check`. Y la sonda repetida sale de la caché:

```
GET /ready -> 503 en 2.0024s   <- sondeo real
GET /ready -> 503 en 0.0013s   <- veredicto memorizado
```

### 5. Recuperación — mismo proceso, sin reiniciar el binario

```
$ docker start pg-c25
pg-c25 running Up 4 seconds

GET    /health                      -> {"status":"ok"}                 [HTTP 200]
GET    /ready                       -> {"status":"ready"}              [HTTP 200]
GET    /articles                    -> {"articles":[]}                 [HTTP 200]
GET    /products                    -> {"products":[]}                 [HTTP 200]
GET    /terms                       -> {"terms":[]}                    [HTTP 200]
GET    /content-types               -> {"content_types":[{"name":"reviews",…}]} [HTTP 200]
GET    /admin/articles  (sesión)                                       [HTTP 200]
GET    /admin/products  (sesión)                                       [HTTP 200]

   y la mitad que este contrato protege, en el mismo proceso, después de la caída:
GET    /content/reviews             -> {"error":"could not list content"} [HTTP 500]

   y el 401 de CONTRACT-24, intacto:
POST   /auth/login (password MALA)  -> {"error":"invalid credentials"} [HTTP 401]
GET    /articles   (token basura)   -> {"error":"unauthorized"}        [HTTP 401]
```

Esa penúltima línea es el contrato entero en una medición: **la misma ruta que devolvió 503 durante
la caída devuelve 500 por el defecto genuino cuando la base vuelve.** El clasificador distingue, y
distingue en el mismo proceso sin reiniciarlo.

---

## Red-team: las preguntas del contrato, respondidas

**¿Un error de validación que hoy da 400 se ve afectado?** No. El clasificador se invoca sólo desde
los sitios que ya escribían 500. Los 400 (`writeBindError` con `errBadField`, `errDuplicateSKU`,
`errUnknownTaxonomy`, `errParentIsSelf`, …) se resuelven **antes**, en su propio `errors.Is`, y ni
siquiera llegan al helper. `TestHappyPathIsUnchangedByTheClassifier` fija además que un 404 sigue
siendo 404: el clasificador sólo ve operaciones que fallaron, y una fila ausente no es una.

**¿Un `context` cancelado porque el cliente cortó?** No es la base caída y **no** se clasifica como
tal: hay una guarda explícita que devuelve `false` si `ctx.Err() != nil`, así que responde 500 (a un
cliente que ya se fue) y, sobre todo, **no gasta ni contamina la sonda**. Eso último es lo importante:
el veredicto es compartido con `/ready`, y sin la guarda un cliente que se desconecta podría grabar su
propia cancelación como el veredicto de la base y envenenar `/ready` para todos durante un segundo.
Por la misma razón la sonda se llama con `context.WithoutCancel(ctx)`. Fijado por
`TestACancelledRequestIsNotAnOutage`, que corre con `h.db == nil` a propósito: una versión que sondeara
igual entraría en pánico en vez de contestar 503 calladita.

**¿Una transacción que falla al commitear por conflicto?** Con la base viva, la sonda contesta
"accesible" y sigue siendo **500**, exactamente como hoy. Es lo correcto: un conflicto de
serialización no es "esta instancia no puede servir", y un 503 le diría al balanceador que la saque de
rotación por algo que no tiene nada que ver con su salud.

**¿La sonda dice "arriba" pero la operación falló por permisos de base?** **500**, y es lo que
corresponde: la base contesta, así que la instancia puede servir; lo que está mal es la
configuración de este despliegue, que es un defecto permanente y no algo que se arregle
reintentando. Es el mismo caso que el fallo interno genuino medido arriba, y la razón exacta por la
que la polaridad de CONTRACT-24 no se copió.

**¿El 503 nuevo rompe algún test o cliente que esperaba 500?** Ningún test: `go test ./... -count=1`
verde dos veces y `git diff --stat -- '*_test.go'` vacío — ningún test preexistente esperaba 500 para
un caso que ahora es 503, así que no hubo que tocar ninguno ni hay señal que explicar. Ningún cliente
que se comportara bien: el 503 aparece **sólo** cuando la base no contesta, situación en la que el
cliente antes recibía un 500 igualmente inservible, sólo que con el diagnóstico equivocado.

**¿Y el camino feliz?** Sin cambios: mismos códigos, mismos cuerpos. El clasificador no se invoca en
ninguna respuesta exitosa, así que ni siquiera puede tocar una. Medido arriba (línea base y
recuperación) y fijado por `TestHappyPathIsUnchangedByTheClassifier`.

---

## Verificación de la suite

```
$ gofmt -l .
(vacío)

$ go build ./...                   OK
$ go vet ./...                     OK
$ go vet -tags dualengine ./...    OK

$ go test ./... -count=1          (PASADA 1)
ok  github.com/MauricioPerera/librarian/cmd/librarian     2.849s
ok  github.com/MauricioPerera/librarian/internal/auth     6.606s
ok  github.com/MauricioPerera/librarian/internal/config   1.265s
ok  github.com/MauricioPerera/librarian/internal/dual     1.272s
ok  github.com/MauricioPerera/librarian/internal/schema   1.387s
ok  github.com/MauricioPerera/librarian/internal/server  42.633s
ok  github.com/MauricioPerera/librarian/internal/store    3.798s

$ go test ./... -count=1          (PASADA 2)
ok  github.com/MauricioPerera/librarian/cmd/librarian     2.931s
ok  github.com/MauricioPerera/librarian/internal/auth     7.286s
ok  github.com/MauricioPerera/librarian/internal/config   1.293s
ok  github.com/MauricioPerera/librarian/internal/dual     1.303s
ok  github.com/MauricioPerera/librarian/internal/schema   1.395s
ok  github.com/MauricioPerera/librarian/internal/server  43.019s
ok  github.com/MauricioPerera/librarian/internal/store    4.216s
```

Tests nuevos de este contrato, en detalle:

```
--- PASS: TestNoErrorIsNeverInfrastructure (0.00s)
--- PASS: TestACancelledRequestIsNotAnOutage (0.00s)
--- PASS: TestAReachableDatabaseKeepsAFailureAtFiveHundred (0.00s)
--- PASS: TestAnUnreachableDatabaseMakesAFailureInfrastructure (0.00s)
--- PASS: TestDataRoutesReportAnOutageAsUnavailable (0.18s)
--- PASS: TestGenuineInternalFailureIsStillFiveHundred (0.18s)
--- PASS: TestHappyPathIsUnchangedByTheClassifier (0.16s)
```

Baterías dual-motor contra el PostgreSQL 17 real (tag `dualengine`, sobre un `public` limpio) —
CONTRACT-19/20/20C/21/22/23:

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5449/postgres?sslmode=disable' \
    go test -tags dualengine -count=1 ./internal/server ./internal/auth
ok  github.com/MauricioPerera/librarian/internal/server  118.372s
ok  github.com/MauricioPerera/librarian/internal/auth     52.581s
```

---

## Criterios de aceptación

- [x] build/vet/gofmt limpios; `go test ./... -count=1` verde **dos veces**.
- [x] **T1**: un único punto de clasificación (`failureIsInfrastructure`), 91 sitios pasando por él
      con un cambio mecánico de una línea, sin lógica de decisión repartida. Los cuatro sitios que
      NO pasan están enumerados arriba con su razón.
- [x] **T2**: 503 con la base **realmente detenida** (`docker stop pg-c25`, estado `exited`) y 500
      con un fallo interno real (`DROP TABLE cpt_reviews` con `/ready` en 200 en la misma medición),
      ambos con salida medida y pegada arriba.
- [x] La clasificación **NO ata** `librarian` a tipos de error de un motor: pregunta la sonda, no el
      driver. Justificado arriba y en `datafailure.go`.
- [x] La carrera de la memoización, documentada en las **dos** direcciones, en `datafailure.go` y
      arriba, sin intentar eliminarla.

## Restricciones

- [x] Sólo archivos dentro de `librarian`. `sqlite-postgres-compat` **no se tocó**.
- [x] Sin dependencias nuevas (`go.mod` sin cambios). Ningún permiso nuevo.
- [x] **NO commiteado**: todo queda en el working tree.
- [x] `/health` sin cambios; el 401 de credenciales sin cambios; el 503 de login y `/ready` sin
      cambios.
- [x] Ningún error interno genuino convertido en 503 — medido, no afirmado.
- [x] Password enmascarado como `***` en todo el reporte.

## Cómo reproducir T2

```bash
ssh vps 'docker start pg-c25'

cd D:/Repo/librarian
export LIBRARIAN_ENGINE=postgres
export LIBRARIAN_DB='postgres://postgres:***@31.220.22.176:5449/postgres?sslmode=disable'
export LIBRARIAN_JWT_SECRET=c25-e2e-secret
export LIBRARIAN_ADDR=127.0.0.1:8525

go build -o /tmp/librarian.exe ./cmd/librarian
printf 'c25-admin-password\n' | /tmp/librarian.exe --bootstrap --email c25@example.com
/tmp/librarian.exe &

B=http://127.0.0.1:8525
TOK=$(curl -s -X POST $B/auth/login -H 'Content-Type: application/json' \
      -d '{"email":"c25@example.com","password":"c25-admin-password"}' \
      | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')

# --- mitad A: fallo interno genuino con la base ARRIBA -> 500
curl -s -X POST $B/content-types -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
     -d '{"name":"reviews","fields":[{"name":"headline","type":"text"},{"name":"score","type":"integer"}]}'
ssh vps 'docker exec pg-c25 psql -U postgres -d postgres -c "DROP TABLE \"cpt_reviews\";"'
curl -s -w ' [%{http_code}]\n' $B/ready                                              # 200 ready
curl -s -w ' [%{http_code}]\n' -H "Authorization: Bearer $TOK" $B/content/reviews     # 500

# --- mitad B: la base REALMENTE detenida -> 503
ssh vps 'docker stop pg-c25'
curl -s -w ' [%{http_code}]\n' $B/health                                             # 200 ok
curl -s -w ' [%{http_code}]\n' $B/ready                                              # 503
curl -s -w ' [%{http_code}]\n' -H "Authorization: Bearer $TOK" $B/articles           # 503, NO 500

# --- recuperación y limpieza (el bootstrap del e2e deja tablas en public; ver CONTRACT-24)
ssh vps 'docker start pg-c25'
curl -s -w ' [%{http_code}]\n' -H "Authorization: Bearer $TOK" $B/articles           # 200
ssh vps "docker exec pg-c25 psql -U postgres -d postgres -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public; CREATE EXTENSION vector;'"
```

El contenedor `pg-c25` quedó **ARRANCADO**, como pide el enunciado, y su `public` quedó **limpio**
(verificado: `\dt` → "Did not find any relations").

## Archivos tocados

```
 M docs/PENDIENTES.md
 M internal/server/articles.go
 M internal/server/authz.go
 M internal/server/content.go
 M internal/server/contenttypes.go
 M internal/server/products.go
 M internal/server/terms.go
 M internal/server/ui.go
 M internal/server/ui_apikeys.go
 M internal/server/ui_articles.go
 M internal/server/ui_content.go
 M internal/server/ui_contenttypes.go
 M internal/server/ui_products.go
 M internal/server/ui_roles.go
 M internal/server/ui_terms.go
 M internal/server/ui_users.go
?? internal/server/datafailure.go
?? internal/server/datafailure_contract25_internal_test.go
?? internal/server/server_contract25_test.go
?? docs/reports/CONTRACT-25-REPORT.md
```

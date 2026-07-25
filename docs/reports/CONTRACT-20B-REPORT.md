# CONTRACT-20B — Colación en `auth` y helpers compartidos

Base: `e51b93e` (CONTRACT-20 completo). Árbol **SIN commitear**, como pide el contrato.

**Resultado: LISTO**, con una salvedad que el orquestador tiene que leer antes de desplegar
(§ "El único cambio de orden observable en SQLite").

Lo que se hizo, en el orden que el contrato EXIGE:

1. **(a)** Se extendieron las fixtures de la batería de `CONTRACT-19` con pares que sí pueden
   divergir — incluido el par del catálogo REAL (`content.update` vs `content_types.manage`).
2. **(b)** Se corrió la batería **sin tocar una línea de código de producción** y **falló**, en 11
   líneas. La salida real está pegada abajo.
3. **(c)** Recién entonces se arregló: T1 (paquete `internal/dual`) y T2 (el orden, en Go).
4. **(d)** La batería quedó verde: **81 observaciones idénticas** en SQLite real y PostgreSQL 17 real.

`sqlite-postgres-compat` **no se tocó** (su `git status` quedó exactamente como estaba al empezar).
`internal/store` no se tocó. `store.Open` no cambió. Ningún permiso nuevo, ninguna dependencia nueva,
ningún cambio en el contrato público de las rutas HTTP.

---

## Por qué el defecto existía: la prueba pasaba sin poder fallar

Este es el punto del contrato, y es de proceso, no de código.

La batería de `CONTRACT-19` **ya comparaba el ORDEN de cada listado**, línea por línea, entre los dos
motores. Pasaba en verde igual. No porque el orden coincidiera por diseño, sino porque **sus fixtures
no podían producir un par discrepante**:

| Listado | Fixture de CONTRACT-19 | ¿Puede divergir? |
|---|---|---|
| `ListUsers` (por `email`) | `zeta@example.com`, `alpha@example.com` | **No** — difieren en la primera letra, minúsculas, sin puntuación |
| `RolePermissions` (por `permission_name`) | `editor` con `content.create`+`content.update`; después `terms.manage`+`content.publish` | **No** — nunca otorgó a un mismo rol los dos nombres del par discrepante |
| `ListAPIKeys` (por `created_at DESC, label`) | `a-third-minted`, `b-second-minted`, `c-first-minted` | **No** — `created_at` nunca empata, así que `label` nunca decide nada |
| `rolesForUser` (por `role_name`) | catálogo fijo de 4 nombres en minúscula | **No, y nunca podrá** (§ red-team) |

Una prueba de comparación cuyos datos no pueden producir una diferencia no prueba nada, y da una
confianza **peor** que no tener la prueba: `CONTRACT-19` se cerró creyendo que el orden de `auth`
estaba verificado contra los dos motores.

---

## (a) Las fixtures nuevas, y por qué cada par ES discrepante

Todas se agregaron a `internal/auth/dualengine_contract19_test.go`. **No se tocó ni una aserción
existente**: `git diff --numstat` sobre ese archivo da `146 0` — 146 líneas agregadas, **cero
borradas**.

La forma es siempre la misma, la que `CONTRACT-20` midió contra los dos motores: PostgreSQL ordena
`TEXT` por la **colación de la base** (`en_US.utf8`), SQLite lo ordena por **bytes**, y la colación no
le da peso primario ni a la puntuación ni a las mayúsculas.

| Par | Por BYTES (SQLite) | Por COLACIÓN (PostgreSQL) | Dónde |
|---|---|---|---|
| `content.update` / `content_types.manage` | `.` (0x2E) < `_` (0x5F) → `content.update` primero | `contentupdate` vs `contenttypesmanage` → `t` < `u` → `content_types.manage` primero | `RolePermissions` |
| `soporte.web@` / `soporte_admin@` | `.` < `_` → `soporte.web` primero | `soporteweb…` vs `soporteadmin…` → `a` < `w` → `soporte_admin` primero | `ListUsers` |
| `Redaccion@` / `prensa@` | `R` (0x52) < `p` (0x70) → `Redaccion` primero | peso primario insensible a mayúsculas → `prensa` < `redaccion` → `prensa` primero | `ListUsers` |
| `boletin+news@` / `boletinalta@` | `+` (0x2B) < `a` (0x61) → `boletin+news` primero | `boletinnews` vs `boletinalta` → `a` < `n` → `boletinalta` primero | `ListUsers` |
| `informes.web` / `informes_anuales` | `.` < `_` → `informes.web` primero | `informesweb` vs `informesanuales` → `a` < `w` → `informes_anuales` primero | `ListAPIKeys` |

**El par del catálogo REAL está incluido, y en su forma de producción.** No es un caso inventado:
`schema.Permissions` contiene los ocho nombres, entre ellos `content.update` y
`content_types.manage`, y el rol `administrator` de producción los tiene **todos**. La fixture nueva
otorga (i) el par aislado a `editor` y (ii) `schema.Permissions` **entero** a `administrator`, que es
literalmente la fila que hay hoy en la base.

Dos fixtures necesitaron un empujón para que el orden pudiera decidirse:

- **`ListAPIKeys` ordena por `(created_at DESC, label)`**, y `label` solo decide cuando `created_at`
  empata — cosa que acuñar dos claves nunca produce (nanosegundos distintos). El empate se **fuerza**
  con un `UPDATE` crudo del `created_at` de las dos claves (una columna `timestamp` es `TEXT` en los
  dos motores), para que la SEGUNDA clave de orden sea la que elige la secuencia.
- Se agregaron además **tres claves con la MISMA etiqueta y el MISMO `created_at`**, donde las dos
  claves declaradas empatan y el orden declarado **no alcanza para desempatar nada**. Ver § "El único
  cambio de orden observable en SQLite".

---

## (b) LA BATERÍA FALLANDO — salida REAL, sin una línea de código de producción tocada

Esta es la evidencia que el contrato exige y sin la cual no estaría cumplido. Se corrió con las
fixtures nuevas ya en su lugar y **con el arreglo de orden deshabilitado** (las cuatro llamadas de
ordenamiento en Go quitadas, es decir el código de `main`):

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineAuth -count=1 -v ./internal/auth

=== RUN   TestDualEngineAuth
    dualengine_contract19_test.go:76: line 8 diverges:
          sqlite  : listUsers err=<none> order=[Redaccion@example.com alpha@example.com boletin+news@example.com boletinalta@example.com prensa@example.com soporte.web@example.com soporte_admin@example.com todos@example.com zeta@example.com]
          postgres: listUsers err=<none> order=[alpha@example.com boletinalta@example.com boletin+news@example.com prensa@example.com Redaccion@example.com soporte_admin@example.com soporte.web@example.com todos@example.com zeta@example.com]
    dualengine_contract19_test.go:76: line 9 diverges:
          sqlite  : listUsers row email=Redaccion@example.com status=active roles=[author]
          postgres: listUsers row email=alpha@example.com status=active roles=[administrator editor]
    dualengine_contract19_test.go:76: line 10 diverges:
          sqlite  : listUsers row email=alpha@example.com status=active roles=[administrator editor]
          postgres: listUsers row email=boletinalta@example.com status=active roles=[contributor]
    dualengine_contract19_test.go:76: line 12 diverges:
          sqlite  : listUsers row email=boletinalta@example.com status=active roles=[contributor]
          postgres: listUsers row email=prensa@example.com status=active roles=[author]
    dualengine_contract19_test.go:76: line 13 diverges:
          sqlite  : listUsers row email=prensa@example.com status=active roles=[author]
          postgres: listUsers row email=Redaccion@example.com status=active roles=[author]
    dualengine_contract19_test.go:76: line 14 diverges:
          sqlite  : listUsers row email=soporte.web@example.com status=active roles=[editor]
          postgres: listUsers row email=soporte_admin@example.com status=active roles=[editor]
    dualengine_contract19_test.go:76: line 15 diverges:
          sqlite  : listUsers row email=soporte_admin@example.com status=active roles=[editor]
          postgres: listUsers row email=soporte.web@example.com status=active roles=[editor]
    dualengine_contract19_test.go:76: line 42 diverges:
          sqlite  : rolePermissions editor collation-pair order=[content.update content_types.manage]
          postgres: rolePermissions editor collation-pair order=[content_types.manage content.update]
    dualengine_contract19_test.go:76: line 44 diverges:
          sqlite  : rolePermissions administrator full order=[content.create content.delete content.publish content.update content_types.manage roles.manage terms.manage users.manage]
          postgres: rolePermissions administrator full order=[content.create content.delete content.publish content_types.manage content.update roles.manage terms.manage users.manage]
    dualengine_contract19_test.go:76: line 78 diverges:
          sqlite  : listAPIKeys tie-broken-by-label order=[informes.web informes.web informes.web informes_anuales]
          postgres: listAPIKeys tie-broken-by-label order=[informes_anuales informes.web informes.web informes.web]
    dualengine_contract19_test.go:76: line 79 diverges:
          sqlite  : listAPIKeys full order=[informes.web informes.web informes.web informes_anuales a-third-minted b-second-minted c-first-minted]
          postgres: listAPIKeys full order=[informes_anuales informes.web informes.web informes.web a-third-minted b-second-minted c-first-minted]
--- FAIL: TestDualEngineAuth (26.94s)
FAIL
FAIL	github.com/MauricioPerera/librarian/internal/auth	29.479s
FAIL
```

**11 líneas divergentes**, sobre los tres listados de `auth` que podían divergir: `ListUsers`
(puntuación, mayúsculas y `+`), `RolePermissions` (el par real, aislado Y en la fila de producción
completa) y `ListAPIKeys` (el desempate por `label`).

La línea 44 es la más importante del informe: es **la fila del rol `administrator` que existe hoy en
la base de producción**, devuelta en dos secuencias distintas por el mismo `ORDER BY permission_name`
declarado.

---

## (c) El arreglo

### T1 — Dónde viven los helpers compartidos, y por qué ahí

**Paquete nuevo: `internal/dual`.** La elección se hizo mirando el grafo de importación **antes** de
escribirlo, como pide el contrato, porque un ciclo descubierto en `CONTRACT-21` obligaría a rehacerlo.

Grafo ANTES:

```
internal/schema  → (nada interno)
internal/auth    → internal/schema
internal/store   → internal/schema
internal/server  → internal/auth, internal/schema, internal/store
```

`auth` no puede importar `server` (la dependencia va al revés), así que el lugar tenía que ser neutral
respecto de los tres. `internal/dual` **no importa NADA de librarian** — solo `compat` y la librería
estándar — así que es una **hoja**, por debajo incluso de `internal/schema`. Con eso el ciclo no es
"no existe hoy": es **imposible por construcción**, que es la propiedad que `CONTRACT-21` necesita.

Grafo DESPUÉS (salida real):

```
$ for p in dual schema auth store server config; do ... go list -f '{{join .Imports " "}}' ... done
internal/dual    ->
internal/schema  ->
internal/auth    -> internal/dual internal/schema
internal/store   -> internal/schema
internal/server  -> internal/auth internal/dual internal/schema internal/store
internal/config  ->
no import cycle
```

**Se verificó empíricamente que `internal/store` puede usarlo**, que es el criterio explícito del
contrato, con una sonda descartable que se borró enseguida:

```
$ cat > internal/store/zz_c21_probe.go   # import internal/dual + 3 referencias
$ go build ./...
PROBE-OK: internal/store imports internal/dual, no cycle
github.com/MauricioPerera/librarian/internal/dual
github.com/MauricioPerera/librarian/internal/schema
$ rm internal/store/zz_c21_probe.go && go build ./...
probe removed, build still clean
```

**Alternativa considerada y descartada:** meter los helpers en `internal/schema` también habría
compilado (ya es hoja y ya lo importan los tres). Se descartó porque `schema` declara el **MODELO**
canónico (tablas, vistas, rutinas); marcadores de enlace, generación de UUID y accesores de fila son
la **plomería que EJECUTA** contra ese modelo. Mezclarlas obligaría a todo consumidor que solo quiere
las declaraciones a arrastrar la plomería.

**Qué se movió — y qué NO.** El contrato dice "mové solo lo que está realmente duplicado", así que se
midió antes de mover:

| Símbolo | Estaba en | Ahora |
|---|---|---|
| `bind` | `auth/dual.go` + `server/dual.go` | `dual.Bind` |
| `newUUID` | `auth/dual.go` + `server/dual.go` | `dual.NewUUID` |
| `rowText` | `auth/dual.go` + `server/dual.go` | `dual.RowText` |
| `rowIsNull` | `auth/dual.go` + `server/dual.go` | `dual.RowIsNull` |
| `textValue` | `auth/dual.go` + `server/dual.go` | `dual.TextValue` |
| `uuidValue` | `auth/dual.go` + `server/dual.go` | `dual.UUIDValue` |
| `txQuerier` | `auth/dual.go` + `server/dual.go` | `dual.TxQuerier` |
| `dedupe` | `auth/dual.go` + `server/terms.go` | `dual.Dedupe` |
| `sortStrings` / `sortByKeys` | `server/dual.go` (y `auth` los iba a necesitar ahora) | `dual.SortStrings` / `dual.SortByKeys` |

Las **seis** que el contrato enumera, más dos que también estaban escritas dos veces (`dedupe` y
`txQuerier` — el contrato no las lista, se encontraron en el RECON) y la **séptima que el contrato
anticipa**: el orden estable, que sin este paquete se habría copiado a `auth`.

**Lo que NO se movió, a propósito**, porque vive en un solo paquete y consolidarlo crearía una
superficie compartida que nadie comparte: `bindList` y `quote` y `integerValue` y `rowTextPointer`
(solo `server`), `timestampValue` (solo `auth`), `canonicalTimestampLayout`/`nowCanonical` (solo
`server`, y § red-team explica por qué NO debe compartirse), `queryOne`/`queryRoutine` y los handles
de esquema `authSchema`/`serverSchema` (atados a su paquete).

**Cero funciones duplicadas — salida real:**

```
=== the moved helpers: exactly one definition each ===
Bind         1
NewUUID      1
RowText      1
RowIsNull    1
TextValue    1
UUIDValue    1
Dedupe       1
SortStrings  1
SortByKeys   1
TxQuerier    1

=== no local redefinition left in auth/server ===
0
```

Las llamadas se reescribieron a los nombres calificados (`dual.Bind(...)`, `dual.RowText(...)`, …) en
vez de dejar alias locales: un alias habría dado un diff más chico pero escondería el origen
compartido en el punto de uso, y un wrapper habría sido —literalmente— una función duplicada más.

**Un cambio de firma, justificado:** `SortByKeys` ahora toma `[]dual.Key` en vez de `[]string`, porque
`ListAPIKeys` ordena `created_at DESC, label ASC` — direcciones distintas por clave, algo que un
`[]string` no puede expresar. Los tres puntos de uso ascendentes de `server` usan el helper
`dual.Ascending(...)` y quedan igual de legibles.

### T2 — El orden, en `auth`

Misma estrategia que `CONTRACT-20` ya validó contra los dos motores, sin inventar otra: **el `ORDER BY`
declarado en la rutina se conserva como base estable y el orden FINAL se impone en Go, con comparación
BYTE a BYTE**.

Byte a byte y no otra cosa, por una razón que es un requisito del contrato: **es lo que SQLite ya
hace**, así que el orden que ve producción **no cambia**.

Los cuatro listados son **no paginados**, que es la condición para poder hacerlo: reordenar después de
leer elige el conjunto entero, no reordena una página. (Los tres listados paginados del proyecto están
en `internal/server` y siguen resueltos por el otro argumento de `CONTRACT-20` — `created_at` de ancho
fijo e `id` UUID —; este contrato no los tocó.)

| Función | Orden declarado en la rutina | Orden final impuesto en Go |
|---|---|---|
| `ListUsers` | `email` | `dual.SortByKeys` por `email` |
| `RolePermissions` | `permission_name` | `dual.SortStrings` |
| `ListAPIKeys` | `created_at DESC, label` | `dual.SortByKeys` por `created_at DESC, label ASC, id ASC` |
| `rolesForUser` | `role_name` | `dual.SortStrings` |

Sobre `rolesForUser`, dicho con honestidad: **ese listado no podía divergir y su arreglo no arregló
nada**. `schema.Roles` es un catálogo fijo de cuatro nombres en minúsculas sin puntuación, donde la
comparación por bytes y la comparación por colación coinciden demostrablemente — y la batería lo
confirma (`getUser todos roles-order=…` **no** figura entre las líneas divergentes de (b)). Se ordena
igual para que la garantía valga por construcción y no por una propiedad del catálogo actual.

Lo que **no** se tocó de `internal/schema`: ninguna rutina cambió su `ORDER BY`, ninguna vista cambió.
El orden declarado sigue siendo la base estable sobre la que el sort de Go (que es **estable**) opera.

---

## (d) LA BATERÍA VERDE

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineAuth -count=1 -v ./internal/auth
=== RUN   TestDualEngineAuth
    dualengine_contract19_test.go:66: transcript (81 lines, identical on both engines):
        createUser zeta email=zeta@example.com roles=[author]
        createUser alpha email=alpha@example.com roles=[editor administrator]
        createUser todos roles=[administrator editor author contributor]
        verify alpha correct err=<none> email=alpha@example.com roles=[administrator editor]
        verify alpha wrong err=ErrInvalidCredentials
        verify ghost err=ErrInvalidCredentials
        verify messages-identical=true
        listUsers err=<none> order=[Redaccion@example.com alpha@example.com boletin+news@example.com boletinalta@example.com prensa@example.com soporte.web@example.com soporte_admin@example.com todos@example.com zeta@example.com]
        listUsers row email=Redaccion@example.com status=active roles=[author]
        listUsers row email=alpha@example.com status=active roles=[administrator editor]
        listUsers row email=boletin+news@example.com status=active roles=[contributor]
        listUsers row email=boletinalta@example.com status=active roles=[contributor]
        listUsers row email=prensa@example.com status=active roles=[author]
        listUsers row email=soporte.web@example.com status=active roles=[editor]
        listUsers row email=soporte_admin@example.com status=active roles=[editor]
        listUsers row email=todos@example.com status=active roles=[administrator author contributor editor]
        listUsers row email=zeta@example.com status=active roles=[author]
        getUser todos err=<none> roles-order=[administrator author contributor editor]
        …
        rolePermissions editor collation-pair order=[content.update content_types.manage]
        rolePermissions administrator full order=[content.create content.delete content.publish content.update content_types.manage roles.manage terms.manage users.manage]
        …
        listAPIKeys tie-broken-by-label order=[informes.web informes.web informes.web informes_anuales]
        listAPIKeys full order=[informes.web informes.web informes.web informes_anuales a-third-minted b-second-minted c-first-minted]
        listAPIKeys total-order ids-sorted=true
        verify zeta err=<none> roles=[author]
    dualengine_contract19_test.go:80: OK: 81 observations identical on SQLite and PostgreSQL 17
--- PASS: TestDualEngineAuth (26.82s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/auth	29.301s
```

Las once líneas que divergían ahora coinciden, y coinciden **en la secuencia que SQLite daba**, no en
una nueva.

---

## Criterio 5 — el orden observable en SQLite

La comprobación se hizo de la forma más directa posible: **el transcripto del lado SQLite de la
corrida FALLIDA (b) contra el de la corrida VERDE (d)**. Las fixtures son idénticas en las dos; lo
único que cambió entre ellas es el código. Si el arreglo hubiera movido algún orden en SQLite, se
vería aquí.

```
$ diff sqlite-before.txt sqlite-after.txt
80c80
< listAPIKeys total-order ids-sorted=false
---
> listAPIKeys total-order ids-sorted=true
```

**80 de las 81 observaciones son byte-idénticas**, incluidos los cuatro listados completos con su
orden. La suite completa (~30 archivos de test de `internal/server` que ejercitan las rutas HTTP y la
UI de punta a punta, más los de `auth`) pasa verde sin tocar una sola aserción, lo que cubre lo mismo
desde el otro lado.

### El único cambio de orden observable en SQLite — el orquestador tiene que decidir

La línea que cambia es la fixture de las **tres claves de API con la MISMA etiqueta y el MISMO
`created_at`**. Ahí las dos claves de orden declaradas (`created_at`, `label`) **empatan**, y el
`ORDER BY` de la rutina no desempata nada: cada motor devolvía esas filas en su orden natural, que no
está definido. Se agregó el `id` como TERCERA clave (un UUID, cuyos guiones están en offsets idénticos
en todos los valores, así que las dos comparaciones coinciden), y con eso el orden pasa a ser TOTAL.

Es el mismo movimiento que `CONTRACT-20` hizo con sus tres órdenes parciales (§ decisión 9 de aquel
informe). **Lo digo sin adornos: en SQLite eso ES un cambio** — antes esas filas salían en el orden
natural del motor, ahora salen ordenadas por id. Lo que sostengo es que no es un cambio de *orden
observable* en el sentido del criterio, porque **no había un orden que cambiar**: era indefinido, y un
orden indefinido en un listado no paginado es precisamente lo que T2 pide eliminar. Sin esta clave, T2
("todo listado de `auth` con orden idéntico entre motores") quedaría incumplido para ese caso.

Si el orquestador prefiere la lectura literal del criterio 5, quitar `dual.Asc(k.ID)` de
`ListAPIKeys` es una línea — a cambio de dejar ese empate con orden dependiente del motor. Mi
recomendación es dejarlo.

---

## Red-team: las preguntas del contrato, respondidas con evidencia

| Pregunta | Respuesta | Evidencia |
|---|---|---|
| **¿Hay listados en `auth` sin `ORDER BY` explícito?** | **No.** Se auditaron las diez rutinas de lectura de `internal/schema/auth_dual.go`: las cuatro que pueden devolver más de una fila declaran `ORDER BY` (`ListUsers`, `UserRoleNames`, `RolePermissionNames`, `ListAPIKeys`). Las otras seis devuelven **como máximo una fila** por una restricción del esquema, no por suerte: `UserByID`/`APIKeyByID` por PK, `UserCredentialsByEmail` por `unique("email")`, `RoleIDByName`/`PermissionIDByName` por `unique("name")` en `catalogTable`, y `APIKeyByHash` por `unique("key_hash")`. Esta última importa: `VerifyAPIKey` toma `rows[0]`, así que si `key_hash` no fuera única el resultado dependería del orden. Lo es. | `internal/schema/schema.go` líneas 214, 243, 267; `auth_dual.go` completo |
| **¿Algún orden por una columna que puede empatar — qué desempata?** | Sí, `ListAPIKeys`: `created_at` empata y desempataba `label`… que **tampoco es único**. Con los dos empatados el orden era del motor. Ahora desempata el `id`. Los otros tres son totales por esquema: `email` es UNIQUE, `(user_id, role_id)` y `(role_id, permission_id)` son PK compuestas. | La fixture de tres claves homónimas + `listAPIKeys total-order ids-sorted=true` |
| **¿El `created_at` de `api_keys` tiene ancho fijo ahora que lo escribe la app?** | **No, y NO debe tenerlo aquí** — es la única parte donde este contrato se aparta a propósito de `CONTRACT-20`. `server` escribe ancho fijo porque sus listados PAGINADOS ordenan `created_at` **dentro del motor**, donde se compara el texto ALMACENADO. En `auth` nada está paginado: el orden se impone en Go sobre el valor que devuelve compat, **y compat re-renderiza todo timestamp con `time.RFC3339Nano`, que RECORTA los ceros finales** (`compat/store.go:361,428`). Un valor de ancho fijo escrito acá se recortaría igual al leerlo → no cambiaría ningún orden, y sí cambiaría el TEXTO guardado en `api_keys.created_at`, y con él la secuencia que SQLite produce entre filas escritas antes y después del cambio. Eso sí sería violar el criterio 5, a cambio de nada. | `compat/store.go` líneas 361 y 428; la justificación completa está en el comentario de `auth.now()` |
| **¿Un email con `+` o con mayúsculas ordena igual?** | **NO**, ninguno de los dos, y ambos están probados con fixture que se vio fallar. `+` (0x2B) es puntuación sin peso primario, igual que `.`; las mayúsculas ordenan antes por byte y son irrelevantes al peso primario de la colación. Los dos son direcciones que un admin puede registrar hoy por la UI real. | Líneas 8–15 de la corrida (b) |
| **¿Y un `label` de API key con espacios?** | El espacio (0x20) es el caso **más severo** de la misma familia: no tiene peso primario en `en_US.utf8`, así que `"informes web"` y `"informesweb"` comparan IGUAL a nivel primario en PostgreSQL y distinto por bytes en SQLite. Queda cubierto por construcción — el orden ya no lo decide el motor. Se probó con el par `informes.web`/`informes_anuales`, que es la misma clase y además provoca una inversión completa (más visible en el transcripto que un empate). | Líneas 78–79 de la corrida (b) |
| **¿Hay una divergencia de orden que este contrato NO arregle?** | En `auth`, no. Fuera de él: `internal/store` sigue con SQL crudo atado a SQLite y es el `CONTRACT-21`; sus listados no pasaron por este análisis. | — |

### Un defecto latente que este contrato NO arregla, y por qué

`auth.now()` escribe `time.RFC3339Nano`, que **recorta los ceros finales**. Eso hace que
`"…:25Z"` (fracción cero) y `"…:25.5Z"` se comparen como texto por el `.` en vez de por el valor:
`'.'` (0x2E) < `'Z'` (0x5A), así que `25.5` sale ANTES que `25.0`. Es **cronológicamente incorrecto**,
está en `main`, y ocurre en aproximadamente 1 de cada 10 acuñaciones (el último dígito de nanosegundos
es cero).

**No lo arreglé**, y la razón no es pereza: es que **no es una divergencia** — los dos motores se
equivocan igual, y después de este contrato el orden lo decide Go, también igual. Arreglarlo requiere
comparar los timestamps como instantes en vez de como texto, lo que **sí** cambiaría el orden
observable en SQLite respecto de `main` en esos casos. Este contrato no autoriza eso. Es una decisión
del orquestador; el arreglo son tres líneas en `ListAPIKeys` si se quiere.

---

## Verificación — salida REAL

### build / vet / gofmt

```
=== go build ./... ===
(ok)
=== go vet ./... ===
(ok)
=== go vet -tags dualengine,exportfixture ===
VET-TAGS-OK
=== gofmt -l . ===
(empty above = ok)
```

### `go test ./... -count=1`, dos veces

```
=== RUN 1 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.489s
ok  	github.com/MauricioPerera/librarian/internal/auth	4.482s
ok  	github.com/MauricioPerera/librarian/internal/config	0.690s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.463s
ok  	github.com/MauricioPerera/librarian/internal/server	37.312s
ok  	github.com/MauricioPerera/librarian/internal/store	3.788s
=== RUN 2 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.288s
ok  	github.com/MauricioPerera/librarian/internal/auth	4.462s
ok  	github.com/MauricioPerera/librarian/internal/config	0.653s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.278s
ok  	github.com/MauricioPerera/librarian/internal/server	34.533s
ok  	github.com/MauricioPerera/librarian/internal/store	3.770s
```

### Las DOS baterías dual-motor, contra los motores reales

Motor destino verificado en vivo por las propias baterías: PostgreSQL 17 con `pgvector`.

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable' \
    go test -tags dualengine -run 'TestDualEngineServer|TestDualEngineVectorPrecision' -count=1 ./internal/server
ok  	github.com/MauricioPerera/librarian/internal/server	46.287s

$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineAuth -count=1 ./internal/auth
ok  	github.com/MauricioPerera/librarian/internal/auth	30.394s
```

La de `CONTRACT-20` (121 observaciones sobre el mux HTTP real) sigue verde después de que sus 109
llamadas a los helpers pasaran a `internal/dual`.

### Ni una aserción existente tocada

```
$ git diff --stat -- '*_test.go'
 internal/auth/dualengine_contract19_test.go   | 146 ++++++++++++++++++++++++++
 internal/server/dualengine_contract20_test.go |   3 +-
 2 files changed, 148 insertions(+), 1 deletion(-)

$ git diff --numstat -- internal/auth/dualengine_contract19_test.go
146	0	internal/auth/dualengine_contract19_test.go

$ git diff -- internal/server/dualengine_contract20_test.go
+	"github.com/MauricioPerera/librarian/internal/dual"
-	id, err := newUUID()
+	id, err := dual.NewUUID()
```

En la batería del 19: **146 líneas agregadas, CERO borradas** — fixtures nuevas, ninguna observación
existente modificada. En la del 20: el import y el rename de un helper en el andamiaje. Los ~30
archivos de test de `internal/server` y los de `internal/auth` que ejercitan CONTRACT-01..18 pasan sin
tocarse.

### `sqlite-postgres-compat` no se tocó

`git status` en ese repo quedó exactamente como estaba al empezar (solo el
`experiments/vector/vector-exp.exe` sin trackear que ya estaba).

---

## Estado de los criterios de aceptación

| Criterio | Estado |
|---|---|
| build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces | **CUMPLIDO**, salida arriba |
| T1: cero funciones duplicadas; grafo sin ciclos y utilizable desde `internal/store` | **CUMPLIDO**, con la sonda de importación real |
| T2: todo listado de `auth` con orden idéntico entre motores | **CUMPLIDO**, 81 observaciones idénticas |
| T3: la secuencia (a)–(d) documentada, **incluida la batería fallando** | **CUMPLIDO**, § (b) con las 11 líneas divergentes |
| El orden observable en SQLite no cambia respecto de `main` | **CUMPLIDO CON UNA SALVEDAD** — 80/81 observaciones byte-idénticas; la única que cambia es el empate total de `ListAPIKeys`, que en `main` no tenía orden definido. Ver § "El único cambio de orden observable en SQLite" |

---

## Archivos tocados

**Nuevos**

- `internal/dual/dual.go` — el paquete hoja con los helpers compartidos (T1)
- `docs/reports/CONTRACT-20B-REPORT.md` — este informe

**Modificados**

- `internal/auth/dualengine_contract19_test.go` — las fixtures discrepantes (T3 (a)); **solo agregados**
- `internal/auth/users.go` — orden en Go en `ListUsers` y `rolesForUser` (T2)
- `internal/auth/roles.go` — orden en Go en `RolePermissions` (T2) — el defecto de producción
- `internal/auth/apikey.go` — orden en Go en `ListAPIKeys`, con el `id` como tercera clave (T2)
- `internal/auth/dual.go` — reducido a lo específico de `auth`; la nota de por qué NO adopta el ancho fijo
- `internal/server/dual.go` — reducido a lo específico de `server`
- `internal/server/{articles,authz,content,products,terms,ui_apikeys,ui_roles,ui_users}.go` — llamadas a `internal/dual` (T1)
- `internal/server/dualengine_contract20_test.go` — un helper del andamiaje
- `docs/OPERATIONS.md` — la nota de colación, extendida a `auth` y a `internal/dual`

**NO tocados**

- `sqlite-postgres-compat` (todo el repo)
- `internal/store` (ni una línea; la sonda de importación se borró)
- `internal/schema` (ningún `ORDER BY`, ninguna vista, ninguna rutina)
- `store.Open` y cualquier elección de motor (`CONTRACT-21`)
- El contrato público de las rutas HTTP

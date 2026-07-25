# Contrato 20 — `internal/server` dual-motor (parte 2 de 3)

Prerrequisitos: `CONTRACT-19` completo (`internal/auth` ya es dual-motor, `go.mod` en compat
v0.4.0). Este contrato NO toca `sqlite-postgres-compat`.

Parte 2 de 3 de la migración. **Al terminar, la aplicación sigue corriendo solo en SQLite**: la
elección de motor es el contrato 21. Lo que entrega es que `internal/server` deje de tener SQL
atado a un motor, verificable de forma aislada contra los dos motores reales.

Alcance: **45 sentencias** en `articles.go` (13), `terms.go` (13), `content.go` (7),
`products.go` (7), `authz.go` (2), `ui_apikeys.go`, `ui_roles.go`, `ui_users.go` (1 c/u).

## El objetivo no es "cero SQL crudo"

Es **cero SQL atado a un motor**. SQL crudo compuesto con `compat.Placeholder` y sentencias
estándar es dual-motor y es una solución legítima, no un parche. Preferí rutinas y vistas donde
entran bien; usá SQL crudo donde no. Lo que NO puede quedar es un `?` literal escrito a mano ni una
construcción que solo un motor entienda.

## RECON ya resuelto (no re-investigar)

Leé primero `docs/reports/CONTRACT-19-REPORT.md`: el reparto, las firmas y el patrón de la batería
dual-motor ya están establecidos ahí, y este contrato los continúa en vez de reinventarlos.

- **`CallRoutine` no informa cuántas filas afectó.** Devuelve solo `error`. `updateContentRow` y
  `deleteContentRow` (`content.go`) usan `RowsAffected()` para distinguir 404 de 200, igual que
  varias rutas de `articles.go`/`products.go`/`terms.go`. **Donde el conteo de filas decide la
  respuesta HTTP, la escritura va por SQL crudo con `Placeholder`.** No lo simules con una lectura
  previa: agrega un viaje y una carrera donde hoy no hay ninguna.
- **`content.go` construye el SQL en runtime** a partir de la definición del tipo dinámico. Dato ya
  verificado: el `UPDATE` asigna **todos** los campos de la definición, no un subconjunto variable,
  así que su forma sí es estática por tipo. Aun así aplica lo anterior: necesita `RowsAffected`.
  Las **lecturas** de esa capa (listar con `LIMIT`/`OFFSET`, y leer por id) sí entran en rutinas
  `select`, que se pueden generar por tipo en `schema.BuildWith` — el mismo lugar donde ya se genera
  su tabla. Decidí si vale la pena generarlas o si conviene SQL crudo también ahí, y justificá:
  es la única decisión de diseño realmente abierta de este contrato.
- **Los `JOIN`** de `authz.go` (2, resolución de permisos) y `terms.go` (3) van a **vistas** del
  esquema canónico, como en CONTRACT-19.
- **`articles.go` ramifica la misma escritura en 4 variantes** según haya o no `embedding` y
  `metadata` (líneas 115/120/125 y 215/220). No es alcance de este contrato rediseñarlo, pero
  tampoco lo multipliques: si migrarlo tal cual produce cuatro rutinas casi iguales, es señal de
  que ahí conviene SQL crudo con `Placeholder`.
- Las firmas de `auth` ya toman `*compat.Store` (CONTRACT-19). `handlers` tiene `h.db`; necesitás
  el motor disponible donde compongas SQL. Seguí lo que CONTRACT-19 dejó en `NewMux` y **no
  cambies `Deps`**.

## RIESGO ALTO — verificalo antes de dar nada por bueno

**La columna `articles.embedding` es `vector(1536)`**: `TEXT` en SQLite, tipo **nativo `vector`** en
PostgreSQL (requiere `pgvector`). Hoy se enlaza como parámetro desde Go.

Enlazar un parámetro a una columna `vector` nativa **no es lo mismo** que enlazarlo a `TEXT`:
PostgreSQL tiene que inferir el tipo del parámetro, y puede rechazar la asignación sin una
conversión explícita. **Comprobalo contra PostgreSQL real antes de escribir el resto**, porque si
falla condiciona el diseño de todo `articles.go`. Si hace falta una conversión, que sea la mínima y
documentá por qué. **No la asumas en ninguna dirección.**

Lo mismo al LEER: `QueryRoutine` canonicaliza por familia declarada, pero una lectura por SQL crudo
te devuelve lo que el driver decida — y para `vector` eso difiere entre motores.

## T1 — Vistas y rutinas

FIX/OBJETIVO: declarar en el esquema canónico las vistas que reemplazan los `JOIN` y las rutinas
que uses. Nombralas de forma que se vea a qué operación sirven, como en CONTRACT-19.

## T2 — Migrar `internal/server`

FIX/OBJETIVO: las 45 sentencias. Sugerencia de orden, de menor a mayor riesgo: `authz.go` y los
tres `ui_*.go` → `terms.go` → `products.go` → `content.go` → `articles.go` (el del vector, último,
con lo aprendido).

El comportamiento observable de cada ruta HTTP **no cambia**: mismos códigos de estado, mismos
cuerpos, mismos mensajes de error. Los tests existentes de `internal/server` son el contrato de eso.
Podés adaptar su **andamiaje** si una firma lo exige (CONTRACT-19 sentó el precedente), pero **ni
una aserción ni un código de estado esperado**. Si tocás una aserción, explicá por qué en el
reporte.

## T3 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- **La prueba que da sentido al contrato**: extender la batería `dualengine` de CONTRACT-19 para
  cubrir las superficies de este, comparando resultados entre **SQLite real y PostgreSQL 17 real**.
  Seguí el patrón que ya existe (transcripción comparada línea por línea): probó su valor atrapando
  una divergencia real.
- Esa batería tiene que cubrir, explícitamente: el CRUD completo de `articles` **con y sin
  `embedding`** (leído de vuelta y comparado componente a componente), el CRUD de `products`
  incluida la violación de `sku` duplicado, el de `terms` con su jerarquía y su vista, la
  resolución de permisos de `authz.go`, y el CRUD genérico de un tipo **dinámico** creado durante
  la prueba.
- **El orden de todo listado** debe compararse explícitamente entre motores. Es la divergencia que
  no rompe ningún test que no la mire.
- Los 404: que una actualización o borrado sobre un id inexistente dé 404 en **ambos** motores.
- Confirmá que TODO lo de contratos anteriores sigue funcionando igual.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] Cero `?` literales escritos a mano en `internal/server`.
- [ ] T1: vistas y rutinas declaradas en el esquema canónico.
- [ ] T2: comportamiento HTTP idéntico; tests existentes verdes, sin aserciones modificadas.
- [ ] T3: batería dual-motor cubriendo las cinco superficies, con salida real.
- [ ] El caso `vector` resuelto y DOCUMENTADO, con la evidencia de qué hace PostgreSQL real.
- [ ] La decisión sobre las lecturas de `content.go` (rutinas generadas vs SQL crudo) justificada.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. **NO toques `sqlite-postgres-compat`**; si creés que le
  falta algo, PARÁ y reportalo.
- Sin dependencias nuevas. NINGÚN permiso nuevo.
- NO commitear (el orquestador commitea y despliega tras verificar).
- NO cambies `store.Open` ni agregues elección de motor: es el contrato 21.
- NO toques `internal/store` más allá de lo mínimo que exija una firma.
- El contrato público de las rutas HTTP no cambia en absoluto.
- Respetá el guardián de CONTRACT-15 (`h.page(r, title)`, nunca un literal `pageData{`).

## Checklist antes de delegar

- [ ] RECON corrido: el reporte de CONTRACT-19 leído, el límite de `RowsAffected` entendido como el
  criterio que decide rutina vs SQL crudo, y el riesgo del `vector` identificado como lo primero a
  medir.
- [ ] Red-team: ¿el `ORDER BY` de cada listado es explícito y total (¿hay empates que cada motor
  desempata distinto)? ¿`LIMIT`/`OFFSET` sin orden en algún lado? ¿`metadata` JSON vuelve idéntico?
  ¿El `CHECK` de `status` rechaza lo mismo? ¿La violación de `sku` sigue dando 400 y no 500 — usá
  `IsUniqueViolation`, nunca el texto? ¿Un `embedding` de dimensión equivocada falla igual en los
  dos? ¿`published_at` nulo vs no nulo se lee igual?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador; password enmascarado como `***`.

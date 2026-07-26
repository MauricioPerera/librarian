# Contrato 24 — Separar "vivo" de "listo", y no disfrazar fallos de infraestructura

Cierra el hueco 7 de `docs/PENDIENTES.md`. NO toca `sqlite-postgres-compat`.

## El problema, medido en producción

Con el contenedor de la base **detenido**, contra la instancia real:

```
/health             -> {"status":"ok"}
POST /auth/login    -> 401
GET  /content-types -> 401
```

Dos defectos distintos:

1. **`/health` comprueba que el proceso vive, no que el sistema pueda servir.** Un monitor externo
   o un balanceador ven la instancia sana mientras no puede atender nada.
2. **Una caída de la base se presenta como credenciales incorrectas.** Quien diagnostique el
   incidente va a perseguir un problema de login mientras la causa es que la base no está.

Importa ahora y no antes: con SQLite embebido la base no podía estar caída por separado. Desde
`CONTRACT-21` el motor puede ser PostgreSQL, hay un segundo proceso que falla solo, y la distinción
entre *vivo* y *listo para servir* pasa a ser real.

## RECON ya resuelto (no re-investigar)

- **`auth.VerifyCredentials` YA distingue los dos casos**: devuelve `ErrInvalidCredentials` para
  usuario inexistente / contraseña incorrecta / cuenta no activa, y un error envuelto distinto
  (`query user: %w`) cuando falla la consulta. **El que pierde la información es la capa HTTP**:
  `handleLogin` (`internal/server/server.go`) manda todo a 401 con el comentario "Same generic
  message for unknown user and wrong password". El arreglo es del lado del handler, no de `auth`.
- Los otros puntos que colapsan a 401 están en `internal/server/authz.go` (resolución de identidad
  por Bearer/sesión) y en `internal/server/server.go`. Revisalos todos: el defecto es el mismo.
- `handleHealth` (`internal/server/server.go`) es una función sin dependencias que responde una
  constante.
- El `Store` expone `store.DB`, que tiene `PingContext`.

## La tensión de diseño, y hay que resolverla explícitamente

El colapso a 401 **no era gratuito**: existe para que un atacante no pueda distinguir "ese usuario
no existe" de "esa contraseña está mal", que es enumeración de usuarios. `VerifyCredentials` llega
al extremo de correr un bcrypt contra un hash fijo cuando el usuario no existe, para igualar el
tiempo de respuesta. **Eso no se toca.**

Lo que hay que separar es otra cosa: un fallo de infraestructura **no depende del usuario ni de la
contraseña** que mandó el llamador, así que distinguirlo no filtra nada sobre ninguna cuenta.
Escribí ese razonamiento en el código, junto a la guarda, para que nadie lo "arregle" de vuelta
creyendo que reintroduce enumeración.

Corolario que también hay que respetar: la respuesta de infraestructura **no lleva detalle** — ni
el error del driver, ni el DSN, ni si la base existe. Un código de estado y un mensaje genérico.

## T1 — Distinguir el fallo de infraestructura del credencial inválido

FIX/OBJETIVO: que un error que no sea "credenciales inválidas" produzca un **5xx**, no un 401, en
todos los puntos que hoy colapsan. Elegí el código y justificalo (el caso es "no puedo atender
ahora", no "me equivoqué"). El 401 se conserva **exactamente igual** —mismo cuerpo, mismo
mensaje— para el caso que de verdad es de credenciales.

## T2 — Separar "vivo" de "listo"

FIX/OBJETIVO: una comprobación que sí mire la base.

**`/health` no cambia de significado**: hay monitoreo apuntado ahí y su contrato es "el proceso
vive". Agregá la comprobación de disponibilidad como superficie nueva. Decidí la forma y justificá.

Requisitos fijados:

- No puede ser cara ni convertirse en una vía de carga contra la base: una comprobación de conexión
  barata, con un límite de tiempo propio para que no se cuelgue si la base no responde.
- No filtra detalle de la conexión.
- Cuando la base no está, responde un 5xx, no un 200 con un campo que diga que algo anda mal:
  un balanceador mira el código de estado.

## T3 — Que el runbook lo use

FIX/OBJETIVO: la verificación post-deploy de `docs/DEPLOY.md` tiene que comprobar la ruta nueva,
no solo `/health`. Un chequeo de disponibilidad que nadie consulta deja el hueco igual de abierto.

## T4 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- **La prueba que cierra el hueco**: con la base REALMENTE caída (no simulada con un doble),
  comprobar que la ruta de disponibilidad da 5xx, que `/health` sigue dando 200, y que el login da
  **5xx y no 401**. Contra los dos motores si es posible; contra PostgreSQL es obligatorio, porque
  es donde la base puede caerse sola.
- Que un login con credenciales realmente incorrectas siga dando **401 con el mismo cuerpo de
  antes**, y que un usuario inexistente y uno existente con contraseña mala sigan siendo
  indistinguibles.
- Confirmá que TODO lo de contratos anteriores sigue funcionando.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T1: fallo de infraestructura → 5xx en todos los puntos que hoy dan 401; credencial inválida →
  401 idéntico al de hoy.
- [ ] T2: comprobación de disponibilidad con límite de tiempo, sin detalle, 5xx cuando la base no
  está; `/health` con su significado intacto.
- [ ] T3: `docs/DEPLOY.md` la usa en la verificación post-deploy.
- [ ] T4: probado con la base REALMENTE detenida, con salida real.
- [ ] La anti-enumeración sigue intacta, con el razonamiento escrito en el código.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO toques `sqlite-postgres-compat`.
- Sin dependencias nuevas. NINGÚN permiso nuevo.
- NO commitear.
- NO cambies el significado ni la respuesta de `/health`.
- NO cambies el cuerpo ni el mensaje del 401 de credenciales.
- NO agregues detalle del error de base a ninguna respuesta HTTP.

## Checklist antes de delegar

- [ ] RECON corrido: entendido que `auth` ya distingue y que el defecto es del handler, y por qué
  el colapso a 401 existía.
- [ ] Red-team: ¿una base lenta pero viva —la comprobación se cuelga o corta por tiempo? ¿La ruta
  nueva sin autenticar filtra algo útil a un atacante (existencia de la base, latencia)? ¿Un error
  de base a mitad de una petición ya autenticada? ¿El 5xx nuevo rompe algún test o cliente que
  esperaba 401? ¿Qué pasa con la UI, que redirige a `/login` ante un 401 — ahora qué hace?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador, que el dev pueda **detener** —
  ese es el punto. Password enmascarado como `***`.

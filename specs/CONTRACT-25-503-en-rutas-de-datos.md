# Contrato 25 — Un fallo de base dice lo mismo en todas las rutas

Cierra el hueco 8 de `docs/PENDIENTES.md`. Contrato **corto**. NO toca `sqlite-postgres-compat`.

## El problema

Ante la misma causa —la base no responde— el sistema dice dos cosas distintas: `CONTRACT-24` dejó
el login y `/ready` devolviendo **503**, y las rutas de datos siguen devolviendo **500**.

Las dos son 5xx, así que no hay nada roto. Pero significan cosas distintas para quien las recibe:
**503 le dice a un balanceador "sacame de rotación y reintentá"; 500 le dice "esto está roto"**.
Ante la misma causa deberían decir lo mismo.

## La decisión de diseño, y es lo único delicado del contrato

`CONTRACT-24` clasifica por **lista blanca de sentinelas**: si el error no es uno que reconozca
como culpa del llamador (`errIdentityRejected`, `ErrInvalidCredentials`), lo trata como
infraestructura. Ahí funciona, porque la capa de identidad tiene un sentinela claro para "el
llamador se equivocó".

**En las rutas de datos esa polaridad sería peligrosa** y NO se debe copiar. Sus errores incluyen
fallos internos genuinos —una fila que no se puede interpretar, un fallo al serializar— que **son**
un 500 honesto. Tratarlos como infraestructura los disfrazaría de "reintentá más tarde" y
escondería bugs reales detrás de un mensaje tranquilizador. Es el mismo pecado que este proyecto
acaba de corregir, en la dirección contraria.

Entonces, para las rutas de datos: **reconocer explícitamente el fallo de conexión → 503; todo lo
demás sigue siendo 500**, exactamente como hoy.

## RECON ya resuelto (no re-investigar)

- Hay **~100 sitios** con `http.StatusInternalServerError` en `internal/server`. No los toques uno
  por uno con lógica propia: introducí **un solo punto** que reciba el error y decida, y hacé que
  los sitios pasen por ahí. Una clasificación en cien lugares diverge; en uno, no.
- `writeInfraUnavailable` y el resto del vocabulario compartido ya existen
  (`internal/server/readiness.go`). Reusalo.
- **La sonda de disponibilidad ya sabe si la base responde**, y memoiza 1 s. Es un candidato
  natural para decidir la clasificación sin acoplarse a tipos de error del driver: "la operación
  falló **y** la base no está accesible" → 503. Evaluá esa vía contra la alternativa de reconocer
  tipos concretos del driver, elegí una y justificá. Preferí la que NO ate `librarian` a los tipos
  de error de un motor: eso es exactamente lo que `compat` existe para evitar.
- La memoización de 1 s implica que puede haber una carrera: la base vuelve entre el fallo y la
  consulta a la sonda. En ese caso se responde 500 en vez de 503. **Es la dirección conservadora y
  es aceptable** — documentala, no intentes eliminarla.

## T1 — Un punto de clasificación

FIX/OBJETIVO: el helper único, y los sitios de `internal/server` pasando por él. El cambio en cada
sitio debe ser mecánico y de una línea; si en alguno hace falta pensar, es señal de que ese error
no era un 500 genérico y merece mención en el reporte.

## T2 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- **Con la base REALMENTE detenida** (no un doble): una ruta de datos con token válido devuelve
  **503**, igual que el login y `/ready`. Con salida real.
- **Con la base ARRIBA**: un fallo interno genuino sigue devolviendo **500**. Provocalo de verdad
  —no lo afirmes— y decí en el reporte cómo lo provocaste.
- Ningún cambio de comportamiento en el camino feliz: mismos códigos, mismos cuerpos.
- Los tests existentes verdes sin modificarlos. Si alguno esperaba 500 para un caso que ahora es
  503, eso es una señal, no un obstáculo: explicalo en el reporte antes de tocarlo.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T1: un único punto de clasificación; sin lógica de decisión repartida por los sitios.
- [ ] T2: 503 con la base caída y 500 con un fallo interno real, ambos con salida medida.
- [ ] La clasificación NO ata `librarian` a tipos de error de un motor concreto, o se justifica por
  qué no se pudo evitar.
- [ ] La carrera de la memoización, documentada.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO toques `sqlite-postgres-compat`.
- Sin dependencias nuevas. NINGÚN permiso nuevo. NO commitear.
- NO cambies `/health`, ni el 401 de credenciales, ni el 503 que ya dan login y `/ready`.
- NO conviertas errores internos genuinos en 503: ese es el punto del contrato, no un efecto
  colateral aceptable.

## Checklist antes de delegar

- [ ] RECON corrido: entendido por qué la polaridad de `CONTRACT-24` no se copia acá.
- [ ] Red-team: ¿un error de validación que hoy da 400 se ve afectado? ¿Un `context` cancelado
  porque el cliente cortó —eso no es la base caída, ¿qué devuelve? ¿Una transacción que falla al
  commitear por conflicto? ¿La sonda dice "arriba" pero la operación falló por permisos de base?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 que el dev pueda **detener**. Password enmascarado como `***`.

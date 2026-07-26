# Contrato 29 — Aislar las baterías dual-motor

Contrato **corto**, de infraestructura de pruebas. NO toca código de producción ni
`sqlite-postgres-compat`.

## El problema, vivido

Las baterías dual-motor crean un esquema propio en PostgreSQL, pero conectan con
`search_path=<esquema>,public`. Ese `,public` es un agujero real: una tabla que quede en `public`
—dejada por otra prueba, por una sonda manual, por una corrida anterior interrumpida— **es visible
desde la batería**, así que `EnsureSchema` la ve por el fallback, concluye que no falta nada, y no
crea la tabla aislada. Después las escrituras van a otro lado y la batería falla con un error
desconcertante y sin relación aparente con la causa: un 404 al crear contenido cuyo tipo "existe".

Pasó dos veces en el proyecto. La segunda le costó al orquestador un diagnóstico falso: buscó una
regresión en trabajo recién entregado cuando la causa eran restos de su propia sonda de
verificación.

## El nudo, ya medido (no re-investigar)

Quitar `,public` a secas **no funciona**. El tipo `vector` que instala `pgvector` vive en el
esquema `public`, y sin `public` en el `search_path` no resuelve:

```
SET search_path=probe_iso;         CREATE TABLE t (v vector(3));   -> ERROR: type "vector" does not exist
SET search_path=probe_iso,public;  CREATE TABLE t (v vector(3));   -> CREATE TABLE
```

Por eso el `,public` estaba ahí. No fue un descuido: era la forma de que el tipo resolviera.

## Lo que hay que lograr

**Que el aislamiento no dependa de que `public` esté limpio.** Esa es la propiedad, no el
mecanismo.

La vía que el orquestador considera correcta —y que viene usando a mano toda la sesión— es **una
BASE DE DATOS por corrida** en vez de un esquema: la extensión se instala en la base nueva, su
`public` nace vacío, y el aislamiento deja de depender de la higiene de nadie. Evaluala primero.

Si encontrás un impedimento real (permisos del usuario del DSN, `CREATE DATABASE` fuera de
transacción, costo de arranque inaceptable), **decilo con evidencia** y caé al mínimo aceptable:
que la batería **compruebe al arrancar que `public` no tiene tablas y se niegue a correr** con un
mensaje que diga qué encontró y cómo limpiarlo. Eso no aísla, pero convierte un fallo
desconcertante en uno legible, que es la mitad del valor.

## T1 — El aislamiento

FIX/OBJETIVO: aplicarlo a **todas** las baterías dual-motor, no a una — hoy el patrón está
duplicado en `internal/auth`, `internal/server` y `internal/store`. Si el patrón se repite, ese es
el momento de que viva en un solo lugar; si consolidarlo obliga a mover código entre paquetes de
prueba, evaluá si vale y justificá la decisión.

Cada corrida tiene que dejar el entorno como lo encontró, también cuando falla: si una corrida
interrumpida deja restos, el problema vuelve con otro disfraz.

## T2 — Verificación

- `go build ./...`, `go vet ./...` (con y sin el tag), `gofmt -l .` vacío, `go test ./... -count=1`
  verde dos veces.
- **Todas** las baterías dual-motor verdes contra PostgreSQL 17 real.
- **La prueba que da sentido al contrato**: ensuciar `public` a propósito —crear ahí tablas con los
  nombres que usa la aplicación, incluida `__compat_schema`— y confirmar que las baterías **siguen
  pasando** (si lograste el aislamiento) o **fallan con un mensaje claro que nombra la
  contaminación** (si caíste al mínimo). Con salida real. Sin esta prueba el contrato no está
  cumplido: es exactamente el escenario que causó los dos incidentes.
- Confirmá que tras una corrida el entorno queda limpio: sin bases ni esquemas huérfanos.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] Todas las baterías dual-motor verdes contra PostgreSQL real.
- [ ] T2: el escenario de contaminación probado de verdad, con el resultado que corresponda a la
  vía elegida.
- [ ] Sin restos tras la corrida, incluso si falla.
- [ ] Cero cambios en código de producción.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`, y dentro de eso **solo infraestructura de pruebas**.
  Si creés que hace falta tocar producción, PARÁ y explicá por qué.
- NO toques `sqlite-postgres-compat`. Sin dependencias nuevas. NO commitear.
- NO cambies lo que las baterías VERIFICAN: este contrato cambia dónde corren, no qué comprueban.
  Las transcripciones comparadas deben seguir dando las mismas observaciones.

## Checklist antes de delegar

- [ ] RECON corrido: el `,public` localizado en los tres paquetes, y entendido por qué está.
- [ ] Red-team: ¿dos corridas en paralelo (`go test ./...` corre paquetes concurrentemente) chocan
  entre sí? ← esa es la importante, porque el aislamiento nuevo tiene que soportarlo. ¿Una corrida
  interrumpida con Ctrl-C deja restos? ¿El usuario del DSN tiene permiso para lo que elegiste?
  ¿Qué pasa si la extensión `pgvector` no está disponible en la base nueva?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador; password enmascarado.

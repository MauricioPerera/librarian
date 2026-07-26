# Contrato 28 — Cerrar la ventana de carrera de las referencias

Contrato **corto**. Requiere `sqlite-postgres-compat` **v0.5.0** (hoy `go.mod` pide v0.4.0 —
subirla es parte de esto). NO toca `sqlite-postgres-compat`.

## Qué cierra

`CONTRACT-27` resolvió la clasificación de violaciones de clave foránea con **comprobaciones
previas** (`checkReferenceTargets` y `checkNoIncomingReferences` en `internal/server/content.go`),
porque `compat` no tenía clasificador. Ambas están documentadas con su límite declarado, no
escondido: entre la comprobación y la escritura el estado puede cambiar, y **en esa ventana el
rechazo llega como 500 en vez de 400**. Nada se corrompe —la clave foránea real sigue rechazando—
pero la respuesta miente sobre de quién es la culpa.

`compat` v0.5.0 trae `Store.IsForeignKeyViolation`. Esto lo consume.

## Decisión YA TOMADA: las comprobaciones previas SE QUEDAN

No las reemplaces por el clasificador. Se **complementan**, y la razón es la calidad del mensaje:

- La comprobación previa sabe **qué** referencia falló y **quién** apunta a la fila, así que produce
  un error que le sirve a una persona: *"la fila X sigue referenciada por Y"*.
- El clasificador solo sabe *"esto fue una violación de clave foránea"*. Por documentación explícita
  de `compat`, **ni siquiera sabe la dirección**: los dos motores usan el mismo código para
  "referencia inexistente" y para "fila referenciada".

Entonces: la comprobación previa sigue siendo el camino normal y da el mensaje bueno; el
clasificador es la **red** que atrapa la carrera y evita el 500.

## La dirección la sabe el sitio de llamada, no el error

`compat` no puede decir cuál de las dos direcciones fue. **No hace falta que lo diga**: quien
escribe la sentencia sabe qué operación emitió. Un `INSERT`/`UPDATE` que falla por clave foránea es
una referencia a algo inexistente; un `DELETE` que falla por clave foránea es una fila que alguien
todavía referencia. La dirección sale del contexto, no del error.

Usá eso, y **no** intentes deducirla del mensaje.

## T1 — Consumir el clasificador

FIX/OBJETIVO: subir `go.mod` a v0.5.0 y, en los sitios donde hoy una escritura de contenido puede
fallar por clave foránea, clasificar el error del driver y traducirlo al mismo código de estado y
al mismo tipo de mensaje que produciría la comprobación previa — reconociendo que el mensaje será
necesariamente más genérico, porque en ese punto ya no se sabe qué fila concreta faltaba.

Los comentarios de `checkReferenceTargets` y del borrado dicen hoy, textualmente, que `compat` no
ofrece el clasificador. **Eso quedó viejo**: actualizalos para que expliquen la división de trabajo
nueva, no para que la contradigan.

## T2 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- **La prueba que da sentido al contrato**: provocar la carrera de verdad, no simularla. Con la
  comprobación previa pasada y la fila destino borrada **después**, la escritura debe devolver el
  código correcto y no un 500. Si reproducir la carrera exige un punto de sincronización en el
  código, decilo y justificá cómo lo hiciste — pero no la sustituyas por un doble que devuelva un
  error inventado: lo que se prueba es que el error REAL del motor se clasifica bien.
- Las dos direcciones, contra **ambos motores**: `INSERT` con referencia inexistente, y `DELETE` de
  una fila referenciada.
- El camino normal no cambia: los mensajes buenos de la comprobación previa siguen apareciendo
  cuando no hay carrera.
- Las baterías dual-motor existentes verdes.

## Criterios de aceptación

- [ ] `go.mod` en v0.5.0; build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T1: clasificación consumida; comprobaciones previas conservadas; comentarios actualizados.
- [ ] T2: la carrera provocada de verdad devuelve el código correcto, con salida real.
- [ ] Los mensajes del camino normal no se degradan.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO toques `sqlite-postgres-compat`.
- Sin dependencias nuevas más allá del bump. NINGÚN permiso nuevo. NO commitear.
- NO elimines las comprobaciones previas.
- NO deduzcas la dirección de la violación del texto del error.

## Checklist antes de delegar

- [ ] RECON corrido: los dos sitios de comprobación previa localizados y entendida la razón de
  conservarlos.
- [ ] Red-team: ¿una violación de clave foránea en una escritura que NO es de contenido dinámico
  —por ejemplo la que ya existe hacia `users`— cae en el camino nuevo y devuelve algo razonable?
  ¿Un `UPDATE` que cambia la referencia a una fila borrada en el medio? ¿El clasificador confunde
  una violación única con una foránea en algún camino compartido?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador; password enmascarado.

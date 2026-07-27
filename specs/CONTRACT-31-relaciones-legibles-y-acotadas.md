# Contrato 31 — Las relaciones, legibles en el listado y acotadas en costo

Contrato de UI sobre `internal/server`. Cierra **juntos** los huecos 10 y 11 de
`docs/PENDIENTES.md`, porque son el mismo problema en dos pantallas: convertir la referencia de una
fila en algo que una persona pueda leer cuesta consultas, y hoy ese costo no está acotado en un lado
y la traducción no existe en el otro.

## Los dos huecos, y por qué van juntos

**10 — el formulario paga N+1.** `referenceInputs` (`internal/server/ui_content.go`) recorre
`def.References` y por CADA UNA hace un `FetchContentType` (resolver el destino contra el registro)
más un `listContentRows`. Dos relaciones al MISMO destino pagan el doble por la misma información.

**11 — el listado no muestra las relaciones.** `handleAdminContentList` arma sus columnas recorriendo
`def.Fields`, así que una relación declarada no aparece: dos filas que apuntan a destinos distintos
se ven idénticas.

El nudo compartido: la etiqueta legible de una fila destino sale de sus datos, y obtenerla cuesta una
lectura. Resolver el 11 sin pensar el 10 produce **una consulta por fila listada**, que es el mismo
defecto multiplicado por cien.

## El dato que evita el trabajo inútil (RECON, no lo busques de nuevo)

**El id de la relación YA viaja en las filas del listado**: `listContentRows` devuelve las columnas
propias de la tabla, y la columna de la referencia es una de ellas. No hay que ir a buscarlo.

Lo único que falta es traducir esos ids a etiquetas. Entonces el trabajo NO es "leer las relaciones",
es "traducir ids que ya tengo", y eso admite una cota que el número de filas no rompe.

Nota de método: `internal/server/content.go` ya compone **SQL crudo con `dual.Bind`** en dos sitios
(`checkReferenceTargets`, `checkNoIncomingReferences`). Es dual-motor legítimo y es el patrón de la
casa; la regla del proyecto es "cero SQL atado a un motor", no "cero SQL crudo". Si necesitás un
`IN` con N parámetros —que una rutina no puede expresar, porque su lista de acciones es estática—,
ese es el camino y no hace falta pedir permiso.

## T1 — El costo del listado no puede crecer con las filas

FIX/OBJETIVO: que el listado muestre una columna por relación declarada, con la MISMA etiqueta
legible que ya usa el selector, y que el número de consultas que eso agrega dependa **solo de la
cantidad de tipos destino distintos**, nunca de la cantidad de filas mostradas.

**Esa cota es el criterio de aceptación, y hay que MEDIRLA**, no argumentarla. Decidí vos cómo
—instrumentar en código de test, un envoltorio que cuente, lo que sea— y **explicá cómo la mediste**.
Un listado de 100 filas con relación tiene que costar lo mismo, en cantidad de consultas, que uno de
3 filas.

La etiqueta sale de **una sola función** (hoy `referenceOptionLabel`). Si el listado calcula la suya
por otro lado, la misma fila se va a ver distinta en dos pantallas y nadie va a entender por qué.

## T2 — El formulario deja de pagar dos veces por lo mismo

FIX/OBJETIVO: resolver cada tipo destino **una vez** aunque varias relaciones apunten al mismo. Ojo
con lo que NO hay que romper: el rescate del valor vigente que cae fuera del tope (CONTRACT-30) es lo
que impide que el `<select>` caiga en la opción vacía y el siguiente guardado borre una relación que
nadie tocó. Tiene test propio; que siga verde no es opcional.

## T3 — Los casos que hay que resolver bien y son fáciles de arruinar

- **Relación en NULL**: se muestra como cualquier otro NULL del listado (la raya, no un vacío
  ambiguo ni el texto "null").
- **La fila destino no aparece entre las que trajiste**: puede pasar si acotás la traducción. NO
  inventes una etiqueta ni dejes la celda vacía en silencio — mostrá algo honesto y decidí qué. Lo
  que no puede pasar es que una relación PUESTA se vea igual que una relación AUSENTE.
- **Dos relaciones al mismo destino**: las dos columnas, cada una con su valor.
- Un tipo **sin ninguna relación** produce exactamente el listado de hoy, sin consultas de más ni
  columnas de más.

## T4 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- La batería dual-motor verde contra PostgreSQL real (infra provista). Si emitís SQL, va con
  `dual.Bind`; ni un `?` literal.
- **La medición de T1, con salida real**: cantidad de consultas con 3 filas y con 100 filas.
- Los tests de CONTRACT-30 verdes sin tocarlos.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T1: columnas de relación en el listado, etiqueta compartida con el selector, y **cota medida**.
- [ ] T2: un solo resuelto por tipo destino distinto.
- [ ] T3: los cuatro casos cubiertos con test.
- [ ] Dual-motor verde. CONTRACT-30 intacto.

## Restricciones

- Tocar SOLO `librarian`, y dentro de eso el perímetro de UI (`ui_content.go`, plantillas, tests). Si
  necesitás un helper de lectura en `content.go`, se acepta, pero **no cambies la forma de las
  respuestas de la API JSON**: este contrato es sobre el panel.
- NO toques `sqlite-postgres-compat`. Sin dependencias nuevas. NINGÚN permiso nuevo. NO commitear.
- NO cambies el tope de 100 del listado ni el del selector.

## Checklist antes de delegar

- [x] RECON corrido: el id ya viaja en las filas; `referenceOptionLabel` existe; el SQL crudo con
  `dual.Bind` ya es el patrón de la casa en este mismo archivo.
- [ ] Red-team: ¿cien filas apuntando a cien destinos distintos del MISMO tipo — cuántas consultas?
  ¿Y si el `IN` lleva 100 parámetros, hay límite en algún motor? ¿Una fila que referencia a otra
  borrada entre que se leyó el listado y se tradujeron las etiquetas? ¿Un tipo destino con CERO
  campos declarados (la etiqueta es el id pelado) se ve razonable en una columna? Medilos.
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador; password enmascarado.

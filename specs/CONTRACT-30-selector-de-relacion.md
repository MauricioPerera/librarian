# Contrato 30 — El selector de fila destino, y la pérdida silenciosa que esconde

Contrato de UI sobre `internal/server`. NO toca `sqlite-postgres-compat` ni el esquema.

## El pedido, y lo que la medición encontró debajo

`CONTRACT-27` dejó declarada una punta menor: *"el formulario de CONTENIDO del panel no tiene
selector para elegir la fila destino; el valor va por la API JSON"*. Suena a comodidad faltante.

**No lo es.** Medido por el orquestador contra el mux real (SQLite), con un tipo `libros` que
referencia a `autores` y un libro cuyo `autor` se puso por la API:

```
1 — alta por API:                                  autor=17ac0826-979b-4b6c-865e-568706af7ccf
2 — el formulario de edición contiene name="autor": false
3 — PUT del panel, cambiando SOLO el titulo:        200
4 — despues:            titulo=Ficciones (2a ed)    autor=<nil>
```

**Editar cualquier campo desde el panel borra la relación, en silencio y con 200.** No hay aviso,
no hay error, y la única señal de que pasó algo es que el dato ya no está.

## La causa, leída en el código (no la busques de nuevo)

Tres piezas correctas por separado componen la pérdida:

- `bindValues` (`internal/server/content.go`) recorre `def.Fields` **y `def.References`**, y para
  una referencia ausente del cuerpo produce `nil` — la regla documentada de "ausente = NULL", que
  para la API JSON es correcta y deliberada.
- `updateContentRow` (mismo archivo) arma un `UPDATE` con **todas** las columnas propias, no con
  las que cambiaron.
- `bindContentForm` (`internal/server/ui_content.go`) construye el cuerpo recorriendo **solo**
  `def.Fields`. Las referencias nunca entran.

Entonces el panel manda un cuerpo sin la referencia, `bindValues` la interpreta como "quiere NULL",
y el UPDATE la pisa. Ninguna de las tres piezas está mal sola; falta la cuarta.

## T1 — Que el panel deje de perder el dato

FIX/OBJETIVO: que crear y editar contenido desde el panel maneje las relaciones declaradas igual
que maneja los campos, de punta a punta — el formulario las ofrece, el POST/PUT las conserva, y el
formulario de edición llega precargado con la relación vigente.

**El control es un `<select>`, no un campo de texto.** Pedirle a una persona que pegue un uuid es
la razón por la que este hueco existe. Tiene que incluir una opción vacía que signifique
explícitamente "sin relación", porque la columna es nullable por construcción y quitar una relación
es una operación legítima que hoy no tiene forma de expresarse a propósito.

**Qué se muestra en cada opción**: el uuid no le dice nada a nadie. El destino es una fila de un
tipo dinámico, así que la etiqueta legible tiene que salir de sus datos. **Elegí una regla y
justificala en el código**; el orquestador NO la fija de antemano, pero sí fija dos límites:

- La regla tiene que funcionar para un tipo destino **sin ningún campo declarado** (es legal: la
  lista genérica ya contempla ese caso), y para un valor NULL en el campo que uses de etiqueta.
- El `value` del `<option>` es SIEMPRE el id. Lo que se muestra es cosmético; lo que se manda, no.

**El límite de cuántas filas ofrecer es una decisión real, no un detalle.** `listContentRows` ya se
usa con un tope en la lista genérica. Un `<select>` con todas las filas de un tipo grande es un
problema distinto —y resolverlo bien excede este contrato—, así que **elegí un tope, decilo en el
código, y hacé que el formulario DIGA que está recortado cuando lo esté**. Un recorte silencioso
acá reproduce exactamente la clase de fallo que este contrato viene a cerrar: el admin no puede
elegir una fila que no ve, y no tiene forma de saber que no la ve.

## T2 — La regresión que hay que fijar, no solo arreglar

La pérdida silenciosa **tiene que quedar asertada**: un caso que cree una fila con relación, edite
OTRO campo desde el panel, y verifique que la relación sigue puesta. Sin ese caso, el próximo que
toque `bindContentForm` reintroduce el defecto sin enterarse.

Cubrí también, contra el mux real y con sesión real (no llamando funciones internas):

- Crear desde el panel **con** relación y **sin** relación.
- Editar poniendo una relación que no estaba, y editar **quitándola** (la opción vacía).
- El formulario de edición llega con la relación vigente **preseleccionada**.
- Un tipo destino sin filas: el formulario dice que no hay a qué apuntar, y no ofrece un control
  inutilizable. Es el mismo criterio que `CONTRACT-27` ya aplicó al formulario de tipos
  (`no-reference-targets`) — mirá cómo lo resolvió ahí antes de inventar otra cosa.
- Un id que no existe enviado a mano: sigue siendo 400 por `checkReferenceTargets`, no un 500. El
  `<select>` es una comodidad, **no** una validación; nada de lo que agregues puede debilitar las
  comprobaciones que ya existen.

## T3 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- La batería dual-motor pasa contra PostgreSQL real (infra provista por el orquestador). Si el
  camino nuevo emite SQL, va con `dual.Bind` — no hay `?` literales.
- **Mostrá la medición del defecto contra `main`**: el caso de T2 debe FALLAR antes de tu arreglo.
  Es la prueba de que el caso mide lo que dice medir.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T1: el panel ofrece un `<select>` con opción vacía, y crear/editar conservan la relación.
- [ ] T2: el caso de la pérdida silenciosa existe y falla contra `main`, con la salida mostrada.
- [ ] El tope de filas ofrecidas está elegido, justificado, y **visible para el admin** cuando
  recorta.
- [ ] Las validaciones existentes (`bindReference`, `checkReferenceTargets`) intactas.
- [ ] Dual-motor verde.

## Restricciones

- Tocar SOLO `librarian`, y dentro de eso el perímetro de UI (`internal/server/ui_content.go`, sus
  plantillas y sus tests). Si creés que hace falta cambiar `bindValues`, `updateContentRow` o la
  API JSON, **PARÁ y explicá por qué**: la semántica "ausente = NULL" es correcta para JSON y
  cambiarla rompería el contrato de esa superficie.
- NO agregues permisos. NO agregues dependencias. NO commitees.
- NO uses la API JSON desde el panel ni JavaScript propio: el proyecto usa htmx y plantillas del
  servidor, y el resto del panel funciona sin scripts.

## Checklist antes de delegar

- [x] RECON corrido: las tres piezas de la causa localizadas y leídas.
- [x] Defecto MEDIDO contra el mux real, no deducido.
- [ ] Red-team: ¿qué pasa si el tipo destino se borra mientras el formulario está abierto? ¿Y si
  una fila destino se borra entre que se dibuja el `<select>` y se envía? ¿Un tipo con DOS
  relaciones al MISMO destino? ¿Una relación cuyo nombre coincide con el de un campo — puede
  pasar? ¿El valor preseleccionado sobrevive a un reenvío del formulario tras un error de
  validación en otro campo?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador; password enmascarado.

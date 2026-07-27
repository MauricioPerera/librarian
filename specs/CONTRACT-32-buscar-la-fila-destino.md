# Contrato 32 — Buscar la fila destino, sin reintroducir la pérdida silenciosa

Cierra el hueco 9 de `docs/PENDIENTES.md`, el último abierto.

## Qué falta

El selector de relación (CONTRACT-30) ofrece las 100 filas más recientes del tipo destino. Por
encima de eso, la única vía es la API JSON mandando el uuid a mano. El formulario **avisa** que
recortó —por eso el hueco es BAJA y no MEDIA—, pero avisar no es resolver: un tipo destino con miles
de filas es inadministrable desde el panel.

Hace falta **buscar**: escribir un texto y que el selector se recargue con las filas que coinciden,
buscando **en la base**, no entre las 100 ya traídas. Filtrar lo ya traído no resuelve nada — el
problema es justamente lo que quedó afuera.

## LO QUE NO SE PUEDE ROMPER, y es fácil romperlo sin darse cuenta

`CONTRACT-30` existe porque el panel **borraba relaciones en silencio**: el formulario no enviaba la
referencia, `bindValues` interpretaba "ausente = NULL" y el UPDATE la pisaba. La defensa es que el
control siempre viaje con su valor, incluido el rescate del valor vigente que cae fuera de la página
ofrecida.

**Una búsqueda mal hecha reconstruye ese defecto exactamente.** Si al buscar "zzz" el fragmento
devuelve un `<select>` sin la opción del valor vigente, el control cae en la opción vacía y **el
siguiente guardado borra una relación que nadie tocó**. El usuario ni siquiera tiene que elegir nada:
le alcanza con escribir en el buscador y guardar.

FIX/OBJETIVO de esta parte: **el valor vigente viaja SIEMPRE**, coincida o no con la búsqueda, y
sigue seleccionado. Va con test propio, y el test tiene que fallar si alguien quita esa garantía.

## T1 — La búsqueda

Un control de texto que recarga las opciones desde el servidor. **htmx y plantillas del servidor**,
como todo el panel: sin JavaScript propio. Ya hay fragmentos así en el proyecto
(`GET /admin/content-types/new/reference`) — mirá ese patrón antes de inventar otro.

El tope de opciones no cambia: la búsqueda sirve para **alcanzar** filas que el tope dejaba afuera,
no para mostrar más de una vez.

## T2 — La decisión técnica de fondo: dónde y cómo se filtra

Hay dos caminos y **los dos tienen un defecto medible**. Elegí uno con evidencia y justificalo; no
hay respuesta obvia y por eso no la fijo yo.

1. **Rutina canónica con `like`.** Empuja el filtro a la base, así que escala. Pero `compat` compila
   `like` a `LIKE` en SQLite y a `ILIKE` en PostgreSQL (verificado en `compat/runtime.go`), y hay una
   **sospecha que tenés que MEDIR, no creerme**: el `LIKE` de SQLite pliega mayúsculas solo en ASCII,
   mientras que `ILIKE` de PostgreSQL plegaría también las acentuadas. Si es cierto, buscar `ñandú`
   contra una fila `ÑANDÚ` encontraría en un motor y no en el otro: divergencia entre motores en una
   funcionalidad visible.
2. **`compat.Store.SearchText`.** Hace el matching en Go, así que es idéntico en los dos motores por
   construcción. Pero **lee la tabla entera** (`SELECT ... FROM tabla`, sin `WHERE` — leelo), o sea
   que resuelve el problema de alcance pagando un costo que crece con la tabla, que es justo lo que
   `CONTRACT-31` se ocupó de acotar.

**El criterio de aceptación no es cuál elegís, es que los dos motores devuelvan LO MISMO.** Probalo
con datos que lo pongan a prueba de verdad: mayúsculas/minúsculas, y **texto acentuado o con `ñ`**,
que es donde vive la divergencia sospechada. Si tu camino diverge, arreglalo o cambiá de camino —
pero medilo antes de elegir, no después.

Si el camino elegido exige declarar una rutina nueva en `internal/schema`, está autorizado.

## T3 — Los casos que hay que resolver bien

- **Búsqueda vacía** = el comportamiento de hoy (las N más recientes). No un listado vacío.
- **Sin coincidencias**: se dice, y el control sigue usable — con el valor vigente presente.
- **La búsqueda no valida nada**: `checkReferenceTargets` y `bindReference` siguen siendo la única
  autoridad sobre qué id es aceptable. El buscador es una comodidad.
- **Un tipo destino sin campos declarados** (la etiqueta es el id): la búsqueda no puede romperse ahí.
- **La etiqueta sigue saliendo de `referenceOptionLabel`**, la misma que el listado (CONTRACT-31). Si
  aparece un segundo cálculo, la misma fila se verá distinta en dos pantallas.

## T4 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- Batería dual-motor verde contra PostgreSQL real, **incluida la comparación de resultados de
  búsqueda entre motores** con el texto acentuado.
- **La prueba de la pérdida silenciosa**: buscar algo que NO coincide con la relación vigente,
  guardar el formulario tal como queda, y verificar que la relación **sigue puesta**. Mostrala
  fallando contra una versión sin la garantía —comentá el rescate y corré el test— para probar que
  el test mide lo que dice.
- Los tests de CONTRACT-30 y 31 verdes sin tocarlos.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T1: se puede alcanzar una fila que el tope dejaba afuera, desde el panel, sin JS propio.
- [ ] T2: camino elegido con medición pegada, y **los dos motores devuelven lo mismo**, acentos
  incluidos.
- [ ] T3: los cinco casos cubiertos con test.
- [ ] La garantía del valor vigente, probada Y mostrada fallando sin ella.
- [ ] CONTRACT-30 y 31 intactos.

## Restricciones

- Tocar SOLO `librarian`: UI (`ui_content.go`, plantillas, tests) y, si tu camino lo exige,
  `internal/schema`. **NO toques `sqlite-postgres-compat`**: si creés que el arreglo correcto vive
  ahí, PARÁ y explicá por qué — puede ser cierto y es una conversación, no una licencia.
- Sin dependencias nuevas. NINGÚN permiso nuevo. Sin JavaScript propio. NO commitear.
- NO cambies el tope de opciones ni el de traducción del listado.

## Checklist antes de delegar

- [x] RECON corrido: el patrón de fragmento htmx existe; `like`→`ILIKE` está en `compat/runtime.go`;
  `SearchText` lee la tabla entera.
- [ ] Red-team: ¿qué pasa si la búsqueda trae MÁS del tope? ¿Un texto de búsqueda con `%` o `_`
  —comodines de `LIKE`— hace que la búsqueda devuelva cualquier cosa? ¿Y con comillas o un `'`?
  ¿Buscar mientras el resto del formulario tiene datos a medio escribir los pierde? ← esa importa,
  porque un htmx que reemplace de más se lleva puesto lo que la persona venía escribiendo.
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador; password enmascarado.

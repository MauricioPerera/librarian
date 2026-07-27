# Contrato 33 — Migrar el buscador a `contains`

## Por qué es obligatorio ahora

`go.mod` ya está en `sqlite-postgres-compat` **v0.7.0**, que **rechaza `like`** en el `WHERE` de una
rutina: compilaba a `LIKE` en SQLite y a `ILIKE` en PostgreSQL, o sea que devolvía conjuntos distintos
según el motor. Medido y sin migrar, el buscador de CONTRACT-32 está **roto**:

```
--- FAIL: TestSearchReachesARowBeyondTheCap
--- FAIL: TestSearchDoesNotDropTheCurrentRelation
--- FAIL: TestSearchWithNoMatchesStaysUsable
--- FAIL: TestSearchWildcardsAreNotWildcards
```

El reemplazo portable es `contains`: subcadena, **sensible a mayúsculas**, **sin comodines** (un `%`
es un porcentaje literal), compilado a `instr`/`strpos`. La aguja va **pelada**: sin `%…%` y sin
sustituir metacaracteres.

## El costo, que hay que aceptar de frente y no disimular

El diseño de CONTRACT-32 se sostiene sobre una invariante: **la base ACOTA y Go DECIDE**, y para eso
la base **nunca puede descartar un match verdadero**. Con `like` se cumplía porque plegaba al menos
ASCII. Con `contains` no: buscar `borges` descarta la fila `BORGES` antes de que Go la vea.

**Entonces la búsqueda pasa a ser sensible a mayúsculas.** No lo escondas:

- El test `TestSearchIsCaseInsensitiveIncludingAccents` afirma lo contrario y **hay que reescribirlo
  para que afirme la verdad nueva**, no borrarlo. Un test que se borra al migrar es una propiedad que
  se pierde sin registro.
- El formulario tiene que **decirlo** donde se busca, con una línea sobria. Un buscador que parece
  insensible y no lo es hace que la persona concluya que la fila no existe.
- `referenceSearchMatches` (el match final en Go) **puede quedar como está**: sigue siendo correcto y
  sigue siendo la autoridad. Lo que cambia es qué llega hasta él.

## T1 — La migración

FIX/OBJETIVO: que el buscador vuelva a funcionar contra v0.7.0, con `contains` y la aguja pelada, y
que **la reachability siga siendo la de CONTRACT-32**: una fila fuera del tope de 100 se alcanza
buscándola. Eso es lo que el hueco 9 cerró y no se puede perder.

`referenceSearchPattern` construye hoy un patrón grueso (`%`, metacaracteres a `_`, no-ASCII a `_`).
Con `contains` esa función pierde sentido: revisá si sobrevive simplificada o si desaparece, y dejá
escrito por qué. **Si desaparece, quitá también el import que quede huérfano** (`unicode`).

## T2 — Lo que NO se puede romper

- **La garantía del valor vigente de CONTRACT-30**: el valor seleccionado viaja SIEMPRE con cada
  búsqueda, coincida o no. Su test tiene que seguir verde sin tocarlo.
- **La cota de CONTRACT-31**: el costo de una búsqueda no crece con la tabla destino.
- Los cinco casos de T3 de CONTRACT-32 (búsqueda vacía, sin coincidencias, no valida nada, destino sin
  campos, etiqueta compartida) siguen valiendo.

## T3 — Registrar el hueco nuevo

`docs/PENDIENTES.md` gana una entrada: **la búsqueda es sensible a mayúsculas**, con su causa (el
operador portable lo es por diseño, porque plegar Unicode diverge entre motores) y la vía de salida
conocida: **una columna plegada mantenida por la aplicación** (la app escribe el valor en minúsculas,
la base hace `contains` sobre eso con la aguja en minúsculas). Es portable porque el plegado ocurre en
Go, pero es un cambio de esquema para tipos dinámicos y merece contrato propio.

Peso: MEDIA. Es una regresión funcional visible para quien usa el panel, no una incomodidad.

## T4 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- Batería dual-motor verde contra PostgreSQL real (infra provista).
- **La prueba de reachability**: una fila fuera del tope se alcanza por búsqueda, con la caja exacta.
- Los tests de CONTRACT-30 y 31 verdes **sin tocarlos**.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; suite verde dos veces; dual-motor verde.
- [ ] T1: buscador funcionando con `contains`; reachability preservada.
- [ ] El test de insensibilidad **reescrito** para afirmar la verdad nueva, no borrado.
- [ ] El formulario dice que la búsqueda distingue mayúsculas.
- [ ] T3: hueco registrado en `docs/PENDIENTES.md` con índice y sección coherentes entre sí.

## Restricciones

- Tocar SOLO `librarian`. NO toques `sqlite-postgres-compat`. NO bajes la versión de `go.mod`.
- Sin dependencias nuevas. NINGÚN permiso nuevo. Sin JavaScript propio. NO commitear.
- NO reintroduzcas `like` por ningún camino.

## Checklist antes de delegar

- [x] Medido que sin migrar el buscador está roto (4 tests en rojo).
- [x] Medido que con el cambio mínimo pasan 7 de 8, y cuál es la que falla y por qué.
- [ ] Red-team: ¿la búsqueda con una aguja que contiene `%` sigue encontrando la fila que tiene `%`
  literal? ¿Y con `\`? ¿Una aguja vacía sigue devolviendo las más recientes? ¿El aviso nuevo se
  renderiza también en el fragmento htmx, o solo en la página completa?

# Contrato 34 — Devolverle a la búsqueda la insensibilidad a mayúsculas

Cierra el hueco 12 de `docs/PENDIENTES.md`.

## El triángulo, para que la decisión no parezca arbitraria

CONTRACT-33 migró el buscador a `contains`, que es **sensible a mayúsculas por diseño** — plegar
Unicode es exactamente lo que diverge entre motores, y por eso `compat` no lo hace. Buscar `borges`
dejó de encontrar `BORGES`.

Hay tres propiedades y solo se pueden tener dos:

| | insensible | alcance ilimitado | sin cambio de esquema |
|---|---|---|---|
| Hoy (`contains` sobre el campo) | ✗ | ✓ | ✓ |
| **Columna plegada** | ✓ | ✓ | ✗ |
| Escaneo acotado + filtro en Go | ✓ | ✗ | ✓ |

**Se elige la columna plegada.** El escaneo acotado desharía lo que cerró el hueco 9 —alcanzar una
fila más allá del tope— y ese fue trabajo caro. El precio es un cambio de esquema, y ahí está la
dificultad real de este contrato.

## La idea

Por cada tipo dinámico se mantiene una columna **plegada** del campo por el que se busca: la
aplicación escribe ahí el valor en minúsculas (`strings.ToLower`, Unicode completo, **en Go**, que es
donde el plegado es idéntico en los dos motores), y la búsqueda hace `contains` sobre esa columna con
la aguja también en minúsculas.

Es portable porque **el plegado no ocurre en la base**: la base sigue haciendo una comparación exacta,
que es lo único que hace igual en los dos motores.

## T1 — La columna y su nombre

FIX/OBJETIVO: que la tabla de un tipo dinámico lleve la columna plegada, y que el nombre **no pueda
colisionar** con un campo de usuario. Dato del RECON: el validador de identificadores exige
`^[a-z][a-z0-9_]*$`, así que **un nombre que empiece con `_` es inalcanzable para un campo declarado**
— usalo y decí por qué en el código.

La columna es del sistema, como `id`/`created_at`: no se muestra, no se edita, no aparece en el
formulario ni en el listado, y el CRUD genérico la mantiene sola. Un admin no tiene que saber que
existe.

## T2 — LA PARTE DIFÍCIL: los tipos que YA existen

`EnsureSchema` **solo crea tablas faltantes; nunca altera una existente** — es la restricción que hace
seguro cada reinicio, y no se toca. Entonces un tipo dinámico ya creado NO obtiene la columna sola.

La maquinaria para cambiar la forma de una tabla existe: `store.EditContentType` (CONTRACT-18)
reconstruye la tabla completa en UNA transacción (crear de paso, copiar, borrar, recrear, copiar,
borrar). **Evaluá usarla** y decidí el mecanismo de migración, con estas condiciones innegociables:

- **Nada silencioso.** Si un tipo todavía no tiene la columna, su búsqueda sigue siendo sensible a
  mayúsculas, y **el formulario tiene que decir cuál de los dos modos aplica**. Dos tipos que se
  comportan distinto sin avisar es peor que el hueco que estamos cerrando.
- **Nada automático al arrancar.** Reconstruir todas las tablas dinámicas en el arranque convierte un
  reinicio en una operación destructiva. Si proponés migración automática, justificá por qué es segura
  y qué pasa si falla a la mitad.
- **La migración es idempotente y verificable**: correrla dos veces no rompe nada, y hay forma de
  preguntar qué tipos ya están migrados.

Si concluís que la migración no cabe en este contrato, **decilo y entregá solo los tipos nuevos**, con
el marcador del formulario y el hueco actualizado. Es una salida legítima; lo que no es legítimo es que
un tipo viejo y uno nuevo se vean iguales y se comporten distinto.

## T3 — Mantener la columna coherente

El valor plegado tiene que seguir al valor real **siempre**: alta, edición, y la reconstrucción de
CONTRACT-18 (donde el campo puede renombrarse o desaparecer). Un plegado que se desactualiza es peor
que no tenerlo: la búsqueda encontraría por el texto viejo.

Pensá también qué pasa si el campo de búsqueda cambia de nombre, y si el tipo se edita para quedarse
sin campos.

## T4 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- Batería dual-motor verde contra PostgreSQL real (infra provista), **incluida una comparación de
  resultados de búsqueda entre motores con acentos y `ñ` en ambas cajas**.
- **La prueba que da sentido al contrato**: buscar `borges` encuentra `BORGES`, y buscar `ñandú`
  encuentra `ÑANDÚ` — con la fila **fuera del tope de 100**, para que quede claro que no se perdió el
  alcance del hueco 9.
- **La coherencia probada como propiedad**: escribir, editar y renombrar, y verificar después que la
  búsqueda encuentra por el valor NUEVO y **no** por el viejo.
- Los tests de CONTRACT-30, 31, 32 y 33 verdes. El de CONTRACT-33 que afirma sensibilidad a mayúsculas
  **hay que reescribirlo otra vez**, ahora en el otro sentido — y eso es correcto: afirma la verdad
  vigente, y su historia queda en el git.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; suite verde dos veces; dual-motor verde.
- [ ] T1: columna plegada con nombre incolisionable, invisible para el admin.
- [ ] T2: mecanismo de migración decidido y justificado; **ningún tipo se comporta distinto en
  silencio**.
- [ ] T3: el plegado sigue al valor en alta, edición y reconstrucción.
- [ ] `borges` encuentra `BORGES` con la fila fuera del tope.
- [ ] Hueco 12 actualizado (índice y sección coherentes) según lo que quede cubierto.

## Restricciones

- Tocar SOLO `librarian`. NO toques `sqlite-postgres-compat` ni bajes su versión.
- **NO plegues en la base**: ni `lower()` en SQL, ni `like`, ni nada que dependa del motor. El plegado
  ocurre en Go o no ocurre.
- Sin dependencias nuevas. NINGÚN permiso nuevo. NO commitear.

## Checklist antes de delegar

- [x] RECON: `DynamicTable` compone las columnas; `EnsureSchema` no altera existentes;
  `EditContentType` reconstruye en una transacción; el validador impide `_` inicial en campos.
- [ ] Red-team: ¿qué pasa con una fila escrita ANTES de la migración —queda con el plegado vacío y por
  lo tanto invisible a la búsqueda? ¿La reconstrucción de CONTRACT-18 preserva el plegado o hay que
  recalcularlo? ¿El export a PostgreSQL (`--dump-schema` + `compat copy`) incluye la columna nueva sin
  romperse? ← esa importa, porque toca la promesa central del sistema.
- [ ] Infra PostgreSQL 17 con pgvector provista; password enmascarado.

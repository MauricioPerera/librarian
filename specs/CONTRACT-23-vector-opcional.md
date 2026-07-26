# Contrato 23 — La capacidad vectorial, opcional

Cierra el hueco 5 de `docs/PENDIENTES.md`. NO toca `sqlite-postgres-compat`.

## El problema

`articles.embedding` es `vector(1536)`. La columna es **nullable** —una instancia puede no guardar
jamás un embedding— pero **existe en el esquema de todas las instalaciones**, así que crear las
tablas en PostgreSQL exige la extensión `pgvector`. Sin ella, el primer arranque falla.

Consecuencia concreta: **una instalación que solo quiere administrar contenido no puede correr
sobre un PostgreSQL administrado que no ofrezca `pgvector`**. Un requisito de infraestructura
impuesto por una capacidad opcional que esa instalación no usa.

La opcionalidad era real a nivel de los datos y no se propagó al despliegue.

## Alcance

Que la capacidad vectorial se pueda **no declarar** en una instalación, de modo que su requisito de
infraestructura aparezca solo cuando se usa. Nada más: no se agrega búsqueda vectorial, ni se
cambia el formato canónico, ni se toca `compat`.

## RECON ya resuelto (no re-investigar)

- El vector **no tiene superficie propia**: no hay rutas de búsqueda. Es una columna de `articles`
  que se escribe y se lee como parte del artículo. Las referencias están en `internal/schema/`
  (declaración y rutinas), `internal/server/articles.go`, `internal/server/vector.go`
  (canonicalización del texto) y `internal/store/`.
- `internal/server/vector.go` canonicaliza el texto del vector **a mano** para que sea byte-idéntico
  a lo que produce `compat`, porque `librarian` escribe SQL parametrizado y no pasa por el camino de
  escritura de `compat.Store`. Esa equivalencia es lo que hace que el export converja: no la rompas.
- **`EnsureSchema` solo crea tablas FALTANTES; jamás altera una existente.** Esto es lo que fija la
  restricción central de abajo, y es una decisión deliberada que hace seguro el reinicio.
- El esquema canónico ya es por-instancia (incluye los tipos dinámicos), así que que varíe no es
  una novedad estructural. Lo que sí es nuevo es que varíe una tabla **de código**.

## La restricción central, y hay que fijarla explícitamente

Como `EnsureSchema` nunca altera una tabla existente, **la elección es irreversible después del
primer arranque**:

- Una instalación que arrancó SIN la capacidad tiene una tabla `articles` sin la columna.
  Habilitarla después no la agregaría: `articles` ya existe, así que `EnsureSchema` no la toca.
- Una instalación que arrancó CON la capacidad no puede quitarla por la misma razón.

**Cambiar la decisión sobre una instalación ya arrancada tiene que FALLAR de forma ruidosa**, con
un mensaje que explique por qué y qué se puede hacer. Lo que NO puede pasar es que arranque
igual: el esquema canónico diría una cosa y la tabla física otra, y eso rompe el export, la
verificación de equivalencia y las escrituras, en silencio y más tarde.

Detectarlo es posible: el estado real está en la tabla, y `compat.Store.TableExists` más la
inspección de columnas permiten compararlo con lo declarado. **No lo confíes a la metadata
`__compat_schema`** por las razones de siempre.

Hacer la decisión reversible exigiría reconstruir una tabla de código, que es una capacidad que no
existe y que este contrato NO construye.

## T1 — La declaración condicional

FIX/OBJETIVO: que el esquema canónico incluya la columna vectorial solo si la instalación la
habilita. Decidí cómo se expresa la elección —seguí la forma que ya usa la configuración
(`CONTRACT-21`) en vez de inventar otra— y cuál es el valor por defecto, **justificando** cuál de
los dos comportamientos preferís que herede una instalación que no dice nada.

## T2 — El comportamiento de la API cuando está deshabilitada

FIX/OBJETIVO: si la capacidad no está, un pedido que traiga `embedding` **se rechaza con un error
que lo explique**. No se ignora en silencio: aceptar un campo y descartarlo es la clase de
degradación silenciosa que este proyecto no hace. Las lecturas simplemente no traen el campo.

## T3 — La guarda de coherencia

FIX/OBJETIVO: al arrancar, si lo declarado y lo que hay en la base no coinciden en este punto, el
servicio **no arranca** y dice exactamente qué pasó: que la instalación se creó con/sin la
capacidad, que la configuración actual dice lo contrario, y que la elección no es reversible.

## T4 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- **La prueba que cierra el hueco**: arrancar en limpio con la capacidad deshabilitada contra un
  **PostgreSQL SIN `pgvector`**, y que funcione — bootstrap, login, y CRUD de artículos por HTTP.
  Hoy eso es imposible. Sin esta prueba el contrato no está cumplido.
- Con la capacidad habilitada, contra un PostgreSQL CON `pgvector`: todo sigue igual que hoy,
  incluido el round-trip de un embedding de 1536 componentes.
- Ambas cosas también sobre SQLite.
- La guarda de T3, en las dos direcciones (habilitar después, deshabilitar después).
- `--dump-schema` refleja la elección.
- Las baterías dual-motor existentes verdes.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T4: **instalación funcionando sobre un PostgreSQL sin `pgvector`**, con salida real.
- [ ] Con la capacidad habilitada nada cambia respecto de hoy.
- [ ] T2: `embedding` rechazado con explicación cuando está deshabilitada, nunca ignorado.
- [ ] T3: cambiar la decisión sobre una instalación existente falla de forma ruidosa y explicada.
- [ ] La canonicalización del texto del vector no cambia.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO toques `sqlite-postgres-compat`.
- Sin dependencias nuevas. NINGÚN permiso nuevo.
- NO commitear.
- NO construyas reconstrucción de tablas de código: la elección es irreversible y así se documenta.
- NO agregues búsqueda vectorial ni cambies el formato canónico del vector.
- El contrato público de las rutas HTTP no cambia cuando la capacidad está habilitada.

## Checklist antes de delegar

- [ ] RECON corrido: entendido que `EnsureSchema` nunca altera una tabla existente y que de ahí
  sale la irreversibilidad, y que la canonicalización de `vector.go` debe seguir byte-idéntica.
- [ ] Red-team: ¿qué pasa con una instalación que YA existe con la columna (todas las de hoy)?
  ¿Y si la base tiene la columna pero la config dice que no, y al revés? ¿El export de una
  instalación sin la capacidad sigue siendo válido y auditable? ¿`InferFeatures` deja de reportar
  la familia vectorial — cambia eso el contrato de equivalencia? ¿Un artículo creado con embedding
  antes de deshabilitar (no debería poder pasar, pero comprobalo)?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra provista por el orquestador: DOS PostgreSQL, uno CON `pgvector` y otro SIN — el segundo
  es el que prueba el punto. Password enmascarado como `***`.

# Contrato 14 — CRUD JSON genérico sobre tipos de contenido dinámicos

Prerrequisitos: `CONTRACT-13` completo y desplegado (`1982648`). Segundo contrato de la fase 3
(`DEFINITION-CPT-DINAMICOS.md`).

Hoy un admin puede CREAR un tipo dinámico (`POST /content-types`) pero no puede cargarle ni un
solo dato: el tipo existe, la tabla existe, y es inutilizable. Este contrato cierra eso con un
CRUD JSON que funciona para CUALQUIER tipo dinámico a partir de su definición, sin un archivo Go
por tipo. La UI genérica es CONTRACT-15, no entra acá.

## RECON ya resuelto (no re-investigar)

- `internal/schema/dynamic.go` ya tiene todo el modelo: `FieldType` (los 5 tipos permitidos con
  su mapeo a `compat.TypeFamily`), `FieldDefinition`, `ContentTypeDefinition`, `DynamicTable(d)`.
- `internal/store/contenttypes.go` ya tiene la lectura: `LoadContentTypeDefinitions(ctx, db)`
  (todas) y `FetchContentType(ctx, db, name)` (una, con `ErrContentTypeNotFound`). REUSALAS —
  no escribas otra forma de leer definiciones.
- `schema.ValidateIdentifier` (`internal/schema/identifier.go`) es EL portón para cualquier
  identificador. Todo nombre de tabla o columna que llegue a una query DEBE haber pasado por
  ahí. Los nombres persistidos ya fueron validados al crearse el tipo, pero un nombre que viene
  del PATH de una request (`/content/{type}`) es entrada del usuario hasta que se resuelve contra
  una definición existente — resolvelo SIEMPRE contra la definición, nunca lo interpoles directo.
- **Un identificador SQL no se puede parametrizar con `?`** — el nombre de tabla y los nombres de
  columna se interpolan sí o sí. Esa es exactamente la razón por la que existe el validador. Los
  VALORES, en cambio, van SIEMPRE como parámetros (`?`), nunca interpolados. Esta distinción es
  la pieza de seguridad central del contrato.
- Patrón de handlers a replicar: `internal/server/products.go` (CONTRACT-11) es la plantilla más
  cercana — mismos códigos de estado (404 nunca 500 para id inexistente o malformado, 400 en
  validación), misma traducción de errores de constraint a 400, mismo gateo por permiso.
- Permisos: reusá los `content.*` genéricos que ya gatean `articles`/`products`
  (`content.create`/`content.update`/`content.delete`; leer solo exige identidad válida). NO
  agregues permisos nuevos. `content_types.manage` gatea definir TIPOS (CONTRACT-13), no cargar
  CONTENIDO — son cosas distintas y no se mezclan.
- Autoría: `DynamicTable` inyecta `author_id NOT NULL` con FK a `users` (via `ContentType()`),
  igual que los tipos de código. Replicá la decisión de `articles`/`products`: crear con una
  identidad de API key (que no tiene usuario humano detrás) se rechaza con 403 y mensaje claro,
  no se inserta un autor nulo.
- Namespace: los tipos dinámicos NO pueden colgar de `/{tipo}` a nivel raíz (colisionaría con
  rutas existentes y futuras). Usá un prefijo dedicado — `/content/{type}` y
  `/content/{type}/{id}` — para que el espacio de nombres dinámico esté aislado del estático.

## T1 — Lectura genérica

FIX/OBJETIVO: `GET /content/{type}` (listar, con el mismo paginado simple `?limit=&offset=` que
usa `articles`) y `GET /content/{type}/{id}` (detalle). Resolvé `{type}` contra una definición
real: si no existe, 404 — nunca intentes consultar una tabla cuyo nombre no salió de una
definición persistida. Las filas se devuelven como objetos JSON con los campos propios del tipo
más los comunes (`id`, `author_id`, `created_at`, `updated_at`, `metadata`), con los valores en
el tipo JSON que corresponde a cada `FieldType` (un entero como número, un booleano como
booleano, no todo como string). Escanear columnas cuyo tipo se conoce solo en runtime es el
núcleo técnico de este contrato: documentá cómo lo resolviste.

## T2 — Escritura genérica

FIX/OBJETIVO: `POST /content/{type}` (crear, gateado `content.create`),
`PUT /content/{type}/{id}` (actualizar, `content.update`) y `DELETE /content/{type}/{id}`
(borrar, `content.delete`). Validación por campo contra la definición: un campo que no existe en
el tipo → 400; un valor cuyo tipo JSON no corresponde al `FieldType` declarado (un texto donde
va un entero, por ejemplo) → 400 claro, nunca 500 ni un valor corrupto guardado. Campos
faltantes: decidí vos el criterio (¿todos requeridos, o nulos permitidos?) — la definición de
campos de CONTRACT-13 no tiene noción de "requerido", así que documentá qué elegiste y por qué,
y que sea consistente entre crear y actualizar.

## T3 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces):
- Round-trip completo sobre un tipo dinámico creado en el mismo test vía la API real de
  CONTRACT-13: crear una fila con los 5 tipos de campo → listar → leer por id → actualizar →
  borrar → 404 al leerla de nuevo. Con los valores REALES de vuelta, verificando que cada tipo
  volvió como el tipo JSON correcto.
- Aislamiento entre tipos: creá DOS tipos dinámicos distintos, cargá filas en ambos, y confirmá
  que listar uno no devuelve filas del otro.
- Seguridad (lo más importante): `{type}` inexistente → 404. `{type}` con un nombre hostil
  (inyección SQL, comillas, `;`) → 404 o 400, NUNCA una query ejecutada — y confirmá con una
  consulta directa que las tablas del sistema siguen intactas después. Un campo con nombre hostil
  en el body → 400. Valores hostiles (strings con comillas, `;DROP TABLE`) guardados y devueltos
  VERBATIM como datos, sin ejecutarse — probalo explícitamente, es la prueba de que los valores
  van parametrizados.
- Gateo por permiso: sin `content.create` → 403 en crear; ídem update/delete con los suyos.
  Crear con API key (sin usuario humano) → 403 con mensaje claro.
- Confirmá explícitamente que TODO lo de contratos anteriores (JSON y UI de
  articles/products/users/roles/api-keys/terms/content-types) sigue funcionando exactamente igual.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: listar y leer por id sobre un tipo dinámico real, con tipos JSON correctos por campo.
- [ ] T2: crear/actualizar/borrar reales; campo inexistente → 400; tipo de valor incorrecto → 400.
- [ ] T3: round-trip completo; aislamiento entre dos tipos; batería de seguridad (tipo hostil,
  campo hostil, valores hostiles verbatim, tablas del sistema intactas); gateo por permiso;
  contratos anteriores sin cambios.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias nuevas.
- NO commitear (el orquestador commitea y despliega tras verificar).
- NINGÚN permiso nuevo — reusá los `content.*`.
- Ningún identificador (tabla o columna) llega a una query sin haber sido resuelto contra una
  definición persistida. Ningún valor se interpola: todos van como parámetro.
- NO toques `internal/store/store.go` (`EnsureSchema` y su maquinaria) — este contrato no cambia
  el esquema, solo lee y escribe filas.
- El contrato público de las rutas de contratos 01-13 no cambia.

## Checklist antes de delegar

- [ ] RECON corrido: modelo de definiciones y funciones de lectura ya existentes identificados,
  distinción identificador-interpolado / valor-parametrizado explícita, namespace `/content/`
  decidido para no colisionar.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿qué pasa si el body trae un campo con el nombre de una columna común (`id`,
  `author_id`, `created_at`)? — no debe permitir pisarlas; documentá el comportamiento. ¿Un
  `{id}` malformado (no-UUID)? → 404, nunca 500. ¿Un tipo dinámico creado y luego consultado
  desde otra conexión/reinicio? (las definiciones se leen de la DB, así que debería funcionar —
  confirmalo, no lo asumas). ¿Listar un tipo con cero filas? → array vacío, no null ni 404.
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación HTTP real y el DEPLOY (con el protocolo de copia-real-de-producción) los hace
  el orquestador después de integrar.

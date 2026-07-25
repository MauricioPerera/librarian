# librarian — Definición (Fase 3: tipos de contenido dinámicos)

Extiende [`DEFINITION.md`](DEFINITION.md) (fase 1, API, completa) y
[`DEFINITION-UI.md`](DEFINITION-UI.md) (fase 2, UI, completa). Esta fase agrega la capacidad de
definir un tipo de contenido nuevo desde la UI, sin recompilar ni redesplegar código.

## Qué es

Un usuario con el permiso dedicado puede crear un tipo de contenido (CPT) nuevo desde la UI —
nombre + campos escalares simples — y la tabla real se crea en la base de datos en tiempo de
ejecución, sin tocar código Go ni desplegar un binario nuevo.

## Arquitectura

Las definiciones de CPT dinámico se persisten como datos (nombre del tipo + lista de campos con
su nombre y tipo). Al confirmarse la creación, esa definición se traduce a un `compat.Table` real
y se aplica vía el mecanismo de aplicación incremental de esquema que ya existe (`EnsureSchema` —
el mismo camino que ya usan los tipos de contenido de código, arreglado en `CONTRACT-11` para
agregar tablas sin tocar las existentes). Sobre esos datos opera una capa de servidor GENÉRICA
(CRUD JSON + UI) que funciona a partir de la lista de campos de cada definición, en vez de un
archivo Go escrito a mano por tipo — a diferencia de `articles`/`products`, que siguen existiendo
tal cual y conviven con este camino nuevo. Los tipos de contenido de código y los dinámicos no se
unifican en esta fase: son dos caminos paralelos, uno para lo que un desarrollador construye con
control total, otro para lo que un admin arma sin deploy.

**Espacio de nombres de tablas (CONTRACT-17).** La tabla real de un tipo dinámico lleva SIEMPRE el
prefijo `cpt_`: un tipo llamado `eventos` vive en la tabla `cpt_eventos`. El prefijo está vedado
para las tablas de código (garantizado por un test sobre `schema.Build()`), así que los dos
espacios de nombres no pueden intersecarse y la colisión entre un tipo dinámico y una tabla de
código futura es IMPOSIBLE, no meramente detectable. El prefijo es un detalle de la capa de datos:
el nombre público del tipo no cambia en ninguna superficie (`/content/{tipo}`,
`/admin/content/{tipo}`, la API de definiciones y la sidebar siguen usando `eventos`), y el admin
nunca lo ve ni lo escribe.

## Capacidades objetivo

- Crear un tipo de contenido nuevo desde la UI: nombre + una lista de campos, cada uno con un
  tipo elegido de un subconjunto simple (texto, número entero, decimal, booleano, fecha).
- CRUD genérico (JSON y UI) para cualquier tipo de contenido dinámico, sin escribir código nuevo
  por tipo — funciona automáticamente a partir de la definición de campos.
- Gateado por un permiso NUEVO y dedicado, `content_types.manage` (crear tablas reales en
  producción es una acción de mayor riesgo que administrar contenido existente; se decidió que
  fuera un permiso asignable, no una capacidad hardcodeada al rol `administrator`).

## Por qué es un caso válido / motivación real

Sin esto, cada tipo de contenido nuevo (como `products` en `CONTRACT-11`) exige un ciclo completo
de desarrollo — contrato, código, deploy — incluso para necesidades simples que un administrador
podría resolver solo si tuviera la herramienta. Es además una prueba de que el modelo declarativo
de `sqlite-postgres-compat` (`compat.Schema`/`ApplySchema`/`CompileDDL`, ya genérico y sin
necesidad de structs Go por tabla) puede sostener una capa de administración de esquema en
runtime — no solo esquemas fijados en tiempo de compilación — sin perder la garantía de
exportabilidad dual-motor que motiva todo el proyecto.

## Fuera de alcance

- ~~**Editar los campos de un CPT dinámico ya creado**~~ — **YA NO: implementado por CONTRACT-18**
  (`docs/reports/CONTRACT-18-REPORT.md`). Agregar, renombrar y quitar campos se hace por
  `PUT /content-types/{nombre}` y por `/admin/content-types/{nombre}/edit`, reconstruyendo la tabla
  real dentro de UNA transacción y componiendo SOLO operaciones que `compat` ya expresa (`CompileDDL`
  + `CompileDropTable`, v0.2.0): no se construyó ningún mecanismo de migración por fuera de `compat`,
  así que la objeción de abajo se respetó en vez de saltearse. Cambiar el TIPO de un campo sigue
  fuera de alcance (el casteo entre familias diverge entre motores). El texto original se conserva:

  **Editar los campos de un CPT dinámico ya creado** (agregar, quitar o renombrar un campo
  después de la creación) — decisión forzada por una limitación real: `sqlite-postgres-compat`
  no tiene ningún soporte de `ALTER TABLE` (verificado, cero resultados en todo el paquete), y
  construir un mecanismo de migración propio por fuera de `compat` introduciría una segunda
  fuente de verdad del esquema (contradice el principio "Go es la única fuente de verdad" que
  sostiene el resto del proyecto). v1 de esta fase es **crear-solamente**: los campos quedan
  congelados una vez aplicada la tabla; cambiar campos implica crear un tipo nuevo. Editar campos
  existentes queda considerado explícitamente para una fase futura, no descartado.
- **Relaciones (FK real) en campos de un CPT dinámico** — solo campos escalares en v1. Si hace
  falta una relación real (como `article_terms`), sigue siendo un tipo de contenido de código.
- **Tipos de campo avanzados** (JSON, `vector(N)`, dominios, columnas generadas) en la UI de
  creación — reservados a los tipos de contenido definidos por código, donde un desarrollador
  entiende las implicancias (ej. `vector(N)` requiere `pgvector` en el destino de export).
- **Borrar un CPT dinámico completo** (la tabla) — no se discutió, queda fuera de alcance salvo
  que se decida explícitamente en un contrato futuro.
- **Cualquier capacidad ya excluida en `DEFINITION.md`/`DEFINITION-UI.md`** — sigue excluida;
  esta fase no las reabre.

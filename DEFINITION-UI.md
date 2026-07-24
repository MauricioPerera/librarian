# librarian — Definición (Fase 2: UI de administración)

Extiende [`DEFINITION.md`](DEFINITION.md) (fase 1, API-only, completa). Esta fase agrega un
panel de administración web sobre el mismo backend — no reemplaza ni reabre ninguna decisión de
la fase 1 salvo donde se dice explícitamente.

## Qué es

Un panel de administración server-rendered (htmx + CSS moderno, como ya fijaba `DEFINITION.md`
fase 1) para operar `librarian` desde un navegador en vez de solo por API/JSON.

## Arquitectura

Vive en el MISMO binario de `librarian` (consistente con la fase 1: "toda la lógica vive en el
único binario", sin componentes nuevos que desplegar por separado). Rutas HTML nuevas
(`text/html`, fragmentos htmx) conviven con las rutas JSON existentes; ambas comparten la misma
capa de autorización (`internal/server/authz.go`) y el mismo acceso a datos — la UI no reimplementa
lógica de negocio, la reusa.

**Sesión de navegador (decisión de esta fase, no reabre CONTRACT-02):** el login humano vía UI
llama al `/auth/login` ya existente y guarda el JWT resultante en una cookie `httpOnly` +
`Secure`, en vez de requerir que el navegador maneje un header `Authorization` a mano. Las rutas
de la UI leen el JWT de esa cookie; las rutas JSON de la API siguen aceptando el header
`Authorization: Bearer` como hasta ahora, sin cambios — la cookie es un mecanismo de transporte
adicional para el mismo token, no un sistema de sesión paralelo.

## Capacidades objetivo

- Login/logout vía formulario (cookie httpOnly con el JWT).
- CRUD de `articles` (crear, listar, editar, publicar, borrar) — el flujo primario de la UI.
- Gestión de usuarios: alta, edición, estado, y asignación de roles YA EXISTENTES (definidos en
  código, ver "Fuera de alcance" — no se editan roles/permisos desde acá).
- Vista de solo lectura del catálogo de roles y sus permisos (para que un admin humano entienda
  qué puede hacer cada rol antes de asignarlo), sin edición.
- Gestión de API keys: alta (mostrar el secreto una única vez), revocación, listado (sin volver a
  mostrar el secreto).

## Por qué es un caso válido / motivación real

Sin UI, cualquier operación administrativa (crear el primer usuario, dar de alta contenido,
revocar una API key comprometida) requiere `curl`/Postman o un script — inviable para un admin
no técnico, que es exactamente el usuario que un backend "tipo WordPress" está pensado para
servir. Es además la validación de producción de que el mismo backend headless puede exponer
tanto una API programática (fase 1) como una superficie humana (fase 2) sin duplicar lógica de
negocio ni reescribir la capa de datos.

## Fuera de alcance

- **Crear/editar roles y permisos en runtime** — decisión explícita de esta ronda de definición:
  el catálogo de roles/permisos sigue predefinido en código (fase 1, sin cambios). La UI asigna
  roles existentes a usuarios; no los crea ni los edita. Si esto cambia en el futuro es una
  decisión de alcance nueva, no una extensión implícita de esta fase.
- **Gestión de tipos de contenido desde la UI** — siguen siendo declarados en código (fase 1,
  sin cambios); la UI opera sobre los tipos que ya existen (`articles`), no los define.
- **Búsqueda semántica o cualquier uso de los embeddings `vector(N)` desde la UI** — la fase 1
  ya excluyó el pipeline de embeddings; la UI no lo reintroduce por la puerta de atrás. Como
  mucho, un campo de solo lectura/edición manual del array, si el primer contrato lo justifica.
  (Decisión de implementación, no de esta definición.)
- **Diseño visual final, paleta, layout exacto de páginas** — decisiones de implementación que se
  cierran en el primer contrato de ejecución de esta fase (PLAN), no acá.
- **Cualquier capacidad ya excluida en `DEFINITION.md` fase 1** (PostgreSQL como storage
  primario, hosting gestionado, theming/renderizado público) — sigue excluida; esta fase no la
  reabre.

# Contrato 15 — UI genérica: crear tipos y administrar su contenido

Prerrequisitos: `CONTRACT-13` y `CONTRACT-14` completos (`893a28c`). Tercer y último contrato de
la fase 3 (`DEFINITION-CPT-DINAMICOS.md`), que cierra la capacidad: hoy todo lo dinámico existe
solo por API, así que un admin no técnico no puede usarlo.

Dos superficies: crear un tipo desde la UI (gateado `content_types.manage`) y administrar el
contenido de cualquier tipo dinámico desde la UI (gateado con los `content.*` de siempre).

## RECON ya resuelto (no re-investigar)

- **El hallazgo estructural de este contrato:** `navSections` (`internal/server/ui_nav.go`) es un
  `var` ESTÁTICO de paquete, y `pageData.Nav()` es un método sobre un struct por VALOR que lo lee
  sin acceso a base de datos y sin retorno de error. Los tipos dinámicos tienen que aparecer en la
  sidebar de TODAS las páginas autenticadas (una sidebar es global, no de una página), y esa lista
  vive en la base. Es decir: el menú deja de poder ser estático. Hay ~20 sitios que construyen
  `pageData{Title, Authenticated, Email, Path}` — el mecanismo que elijas tiene que hacer
  IMPOSIBLE (o al menos ruidoso) que una página nueva se olvide de las entradas dinámicas; una
  solución que exija acordarse de pasar un campo más en cada handler va a fallar en silencio en la
  próxima página que alguien agregue. Documentá el mecanismo y por qué garantiza eso.
- La API genérica ya está entera en `internal/server/content.go` (CONTRACT-14): `resolveType`,
  `listContentRows`, `fetchContentRow`, `insertContentRow`, `updateContentRow`,
  `deleteContentRow`, `bindValues`/`bindValue` (validación por campo), `jsonValue`. REUSALAS —
  la UI no reimplementa acceso a datos ni validación, igual que `ui_products.go` reusa
  `products.go`.
- La creación de tipos ya existe en `internal/store/contenttypes.go`: `CreateContentType`
  (transacción atómica definición + tabla) y `LoadContentTypeDefinitions`. Reusalas.
- Patrón de UI a replicar exactamente: `internal/server/ui_products.go` + sus templates
  (CONTRACT-11) — `requireSession` para lectura, `requireSessionPermission` para escritura,
  un template set por página vía `mustParseFS`, `renderNotFound`/`renderForbidden` compartidos,
  htmx para las escrituras, `Path: r.URL.Path` en `pageData`.
- Los templates se parsean al iniciar el paquete con una lista FIJA de archivos
  (`//go:embed` + `mustParseFS`). Eso NO cambia: los templates genéricos son 3-4 archivos fijos
  que reciben la definición del tipo COMO DATO y renderizan los campos con un `range`. No generes
  templates en runtime.
- Namespaces (decididos, no los cambies): `/admin/content-types` y `/admin/content-types/new`
  para gestionar TIPOS; `/admin/content/{type}`, `/admin/content/{type}/new`,
  `/admin/content/{type}/{id}/edit` para el CONTENIDO de un tipo dinámico. No colisionan con
  `/admin/articles`, `/admin/products`, etc.
- Tipos de campo y su control de formulario: `text` → `<input type="text">`, `integer` →
  `<input type="number">`, `decimal` → `<input type="text">` (NO `number`: el paso decimal del
  navegador puede alterar la precisión, y el proyecto guarda decimales como texto canónico a
  propósito — ver `products.price`), `boolean` → checkbox, `date` → `<input type="date">`.
  Documentá el mapeo que uses.
- **Ojo con el formulario y los booleanos:** un checkbox no marcado NO se envía en un form POST.
  Con el criterio de CONTRACT-14 (campo ausente → NULL), un checkbox desmarcado guardaría NULL en
  vez de `false`. Resolvelo (probablemente enviando explícitamente el valor) y decilo en el
  reporte — es exactamente el tipo de diferencia entre la API JSON y un form HTML que se cuela
  silenciosa.

## T1 — Menú consciente de los tipos dinámicos

FIX/OBJETIVO: que la sidebar liste los tipos dinámicos existentes (además de las secciones
estáticas), en TODAS las páginas autenticadas, leyendo las definiciones de la base. Más la
entrada de gestión de tipos ("Tipos de contenido" o el rótulo que elijas) con su submenú
(listar / crear nuevo). Resolvé la tensión estructural del RECON con el mecanismo que prefieras,
pero que una página nueva no pueda quedarse sin las entradas dinámicas por olvido.

## T2 — UI de gestión de tipos

FIX/OBJETIVO: `GET /admin/content-types` (listar los tipos existentes con sus campos, solo
sesión) y `GET /admin/content-types/new` + `POST /admin/content-types` (crear, gateado
`content_types.manage`). El formulario de creación tiene que permitir definir N campos con su
nombre y tipo — el número de campos es variable, así que necesitás una forma de agregar filas de
campo; hacelo con lo que ya hay (htmx está embebido) o con un enfoque sin JS, a tu criterio,
documentalo. Un nombre inválido o duplicado → se re-renderiza el formulario con el error claro
(mismo patrón que el alta de usuario en CONTRACT-08), NUNCA un 500 ni un JSON crudo.

## T3 — UI genérica de contenido

FIX/OBJETIVO: para cualquier tipo dinámico: `GET /admin/content/{type}` (listado con sus
columnas), `GET /admin/content/{type}/new` + `POST /admin/content/{type}` (crear, gateado
`content.create`), `GET /admin/content/{type}/{id}/edit` + `PUT /admin/content/{type}/{id}`
(editar, `content.update`), y borrar (`content.delete`), con el mismo patrón htmx que
`/admin/products`. Un `{type}` inexistente → 404 HTML (nunca JSON crudo, nunca 500). Errores de
validación de campo → se re-renderiza el formulario con el mensaje, preservando lo que el usuario
cargó.

## T4 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces, `httptest.NewTLSServer` para
lo que dependa de la cookie de sesión):
- Flujo completo por HTTP con cookie real, extremo a extremo y SIN tocar la API JSON: crear un
  tipo desde la UI → aparece en la sidebar → entrar a su listado → crear una fila con los 5 tipos
  de campo → aparece en el listado → editarla → borrarla. Ese es el flujo que un admin real haría.
- El booleano: crear una fila con el checkbox DESMARCADO y confirmar con una consulta directa que
  quedó `false`/0 y no NULL.
- Seguridad: `{type}` hostil o inexistente en las rutas de UI → 404 HTML, con las tablas del
  sistema confirmadas intactas después. Sin `content_types.manage` → 403 HTML al crear un tipo.
  Sin `content.create` → 403 HTML al crear contenido.
- Confirmá explícitamente que TODO lo de contratos anteriores (JSON y UI de
  articles/products/users/roles/api-keys/terms/content-types, y la API genérica de CONTRACT-14)
  sigue funcionando exactamente igual.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: los tipos dinámicos aparecen en la sidebar de todas las páginas autenticadas; el
  mecanismo hace imposible/ruidoso que una página nueva los omita (explicado en el reporte).
- [ ] T2: crear un tipo desde la UI real; nombre inválido/duplicado → formulario con error.
- [ ] T3: CRUD completo de contenido dinámico por UI; `{type}` inexistente → 404 HTML.
- [ ] T4: flujo completo de admin verificado por HTTP; checkbox desmarcado → `false` no NULL;
  batería de seguridad; contratos anteriores sin cambios.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias nuevas (ni Go ni JS — htmx ya está embebido).
- NO commitear (el orquestador commitea y despliega tras verificar).
- NINGÚN permiso nuevo.
- NO toques `internal/store/store.go` ni `internal/schema/*` — este contrato es presentación
  sobre capacidades que ya existen.
- Ningún identificador llega a una query sin haberse resuelto contra una definición persistida
  (la regla de CONTRACT-14 sigue vigente; reusá sus funciones en vez de escribir SQL nuevo).
- El contrato público de las rutas de contratos 01-14 no cambia.

## Checklist antes de delegar

- [ ] RECON corrido: tensión estructural del menú estático identificada, API genérica de
  CONTRACT-14 disponible para reuso, namespaces decididos, trampa del checkbox anticipada.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿qué pasa si se crea un tipo mientras hay una sesión abierta — aparece en la
  sidebar sin reiniciar? (debería, porque se lee de la base por request; confirmalo). ¿Un tipo
  con cero campos declarados es creable desde la UI y su listado funciona? ¿Un tipo con muchos
  campos rompe el layout de la tabla? (no hace falta que sea lindo, sí que no explote). ¿El
  formulario de edición precarga bien un valor `false` y un valor NULL, distinguiéndolos?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación EN NAVEGADOR/HTTP y el DEPLOY (CONTRACT-14 + 15 juntos, con el protocolo de
  copia-real-de-producción) los hace el orquestador después de integrar.

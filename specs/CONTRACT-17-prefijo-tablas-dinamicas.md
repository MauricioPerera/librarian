# Contrato 17 — Prefijo en tablas dinámicas (cierra el hueco 3 de PENDIENTES.md)

Prerrequisitos: `CONTRACT-01`..`CONTRACT-16` completos y desplegados (`dc85fa2`).

Cierra el hueco 3: hoy un tipo de contenido dinámico llamado `X` crea una tabla llamada
exactamente `X`, en el mismo espacio de nombres que las tablas de código. El validador impide que
un tipo dinámico pise una tabla de código EXISTENTE, pero no cubre el sentido inverso: si un
contrato futuro agrega una tabla de código con el nombre de un tipo dinámico que alguien ya creó,
el esquema compuesto tiene tablas duplicadas, `Schema.Validate()` falla y **el servicio no
arranca**.

El riesgo no es teórico: los nombres que un contrato futuro agregaría (`comments`, `media`,
`revisions`, `menus`) son exactamente los que un admin elegiría para un tipo propio.

## Decisión YA TOMADA (no la reabras)

**Prefijo estructural.** Un tipo dinámico llamado `eventos` crea la tabla `cpt_eventos` (el
prefijo exacto lo elegís vos, documentalo). El prefijo queda vedado para las tablas de código, de
modo que la colisión se vuelve **imposible**, no meramente detectable. Se descartó explícitamente
la alternativa de "detectar y fallar con mensaje claro": convierte un fallo opaco en uno legible,
pero no elimina el problema.

## RECON ya resuelto (no re-investigar)

- `schema.DynamicTable` (`internal/schema/dynamic.go`) hoy hace `ContentType(d.Name, own)` — usa
  el nombre del tipo DIRECTAMENTE como nombre de tabla. Ahí es donde entra el prefijo. NO cambies
  la firma de `ContentType()`.
- `schema.BuildWith` compone código + dinámicas; `store.CanonicalSchema` la usa, y de ahí sale
  todo (`EnsureSchema`, `--dump-schema`, el contrato del audit).
- `schema.ValidateIdentifier` + `ReservedNames()` (`internal/schema/identifier.go`) derivan los
  reservados de `Build()` — no hardcodean. Con el prefijo, la reserva de nombres de tablas de
  código para los NOMBRES DE TIPO deja de ser necesaria para evitar el crash (un tipo `users`
  crearía `cpt_users`, sin colisión). Decidí si la conservás igual por claridad (un tipo llamado
  `users` conviviendo con `/users` es confuso para un humano) o si la relajás, y documentá el
  porqué. Lo que NO cambia: los nombres de columna que inyecta `ContentType()` (`id`,
  `author_id`, `created_at`, `updated_at`, `metadata`) siguen reservados para los CAMPOS.
- **El nombre público del tipo NO cambia.** Las rutas siguen siendo `/content/{type}` y
  `/admin/content/{type}` con el nombre que el admin eligió (`eventos`), y la API de definiciones
  sigue devolviendo ese nombre. El prefijo es un detalle de la capa de datos: el usuario nunca lo
  ve ni lo escribe. Verificá esto explícitamente, es lo que hace que el cambio no sea disruptivo.
- La capa genérica de CONTRACT-14 (`internal/server/content.go`) resuelve el tipo contra la
  definición persistida y arma las queries con `quoteIdentifier`. Tiene que pasar a usar el
  nombre de TABLA (con prefijo), no el nombre público — revisá cada sitio.
- **Enforcement del prefijo del lado del código:** ninguna tabla de `Build()` puede empezar con
  el prefijo. Como las tablas de código son literales en Go y no pasan por el validador, la
  garantía tiene que ser un TEST que recorra `Build()` y falle si alguna lo usa. Sin ese test el
  prefijo es una convención, no una garantía.
- **Migración de lo que ya existe:** producción tiene HOY un tipo dinámico `eventos` con su tabla
  sin prefijo y una fila de prueba. Esa migración la hace el ORQUESTADOR como paso operativo
  explícito del deploy (con backup previo, como siempre) — NO la implementes en el código, y NO
  agregues lógica de compatibilidad para tablas dinámicas sin prefijo. Es aceptable únicamente
  porque ese dato es descartable; el código debe quedar limpio, con un solo camino.

## T1 — El prefijo en la capa de datos

FIX/OBJETIVO: constante del prefijo (exportada si otras capas la necesitan) y `DynamicTable`
componiendo el nombre real de la tabla a partir de ella. Todo lo que derive el nombre de tabla de
un tipo dinámico debe pasar por un único lugar — no repartas la concatenación por el código.
Test: una definición produce una `compat.Table` cuyo `Name` es el prefijado, y el esquema
compuesto (`BuildWith`) contiene esa tabla y NO una con el nombre pelado.

## T2 — Enforcement y validador

FIX/OBJETIVO: el test que garantiza que ninguna tabla de `Build()` usa el prefijo (con un mensaje
que explique la consecuencia si alguien lo intenta). Más la revisión de `ReservedNames()` según
la decisión que tomes del RECON. Y un test que confirme lo inverso de la garantía: que un nombre
de tipo que coincide con una tabla de código (si lo permitís) genera una tabla prefijada que NO
colisiona, y que el esquema compuesto sigue validando.

## T3 — El nombre público no cambia

FIX/OBJETIVO: que todas las capas de arriba (API de definiciones, CRUD JSON genérico, UI
genérica, sidebar) sigan usando el nombre que el admin eligió, y solo la capa de datos use el
prefijado. Revisá `internal/server/content.go`, `ui_content.go`, `ui_contenttypes.go`,
`ui_nav.go` y el store. Un error acá se ve como "el tipo existe pero su contenido no aparece".

## T4 — Verificación

Además de lo de siempre (`go build`/`vet`/`test` limpios, dos veces):
- Crear un tipo dinámico por la API real y confirmar con consulta directa que la tabla REAL se
  llama con el prefijo, y que no existe ninguna tabla con el nombre pelado.
- Round-trip completo por las rutas públicas (crear tipo → crear fila → listar → leer → editar →
  borrar) usando SIEMPRE el nombre sin prefijo en las URLs: nada de la superficie pública cambia.
- Lo mismo por la UI genérica, con cookie real, y confirmando que la sidebar muestra el nombre
  público.
- `--dump-schema` incluye la tabla prefijada (o sea, el export a Postgres la lleva).
- Ciclo de reinicio: crear el tipo, reiniciar `EnsureSchema`, confirmar que no falla y que la
  tabla prefijada sigue ahí (el mismo escenario de dos capas de CONTRACT-11/13).
- Un tipo cuyo nombre coincide con una tabla de código (`users`, si lo permitís tras T2): crear,
  cargar una fila, y confirmar que ni la tabla `users` real ni el login se ven afectados.
- Confirmá explícitamente que TODO lo de contratos anteriores sigue funcionando igual.

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: tabla real prefijada; un único lugar deriva el nombre.
- [ ] T2: test que impide que una tabla de código use el prefijo; decisión sobre `ReservedNames()`
  documentada y testeada.
- [ ] T3: nombre público intacto en API, UI y navegación.
- [ ] T4: todos los puntos de arriba con salida real, incluido el ciclo de reinicio y el export.
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat`.
- Sin dependencias nuevas. NINGÚN permiso nuevo.
- NO commitear (el orquestador commitea y despliega tras verificar).
- NO implementes compatibilidad con tablas dinámicas sin prefijo ni ninguna migración automática:
  la única instancia existente la migra el orquestador a mano (ver RECON). El código queda con un
  solo camino.
- El contrato público de las rutas de contratos 01-16 no cambia — incluidas `/content/{type}` y
  `/admin/content/{type}`, que siguen usando el nombre SIN prefijo.
- Respetá el guardián de CONTRACT-15 (`h.page(r, title)`, nunca un literal `pageData{`).

## Checklist antes de delegar

- [ ] RECON corrido: punto único donde se deriva el nombre de tabla identificado, enforcement por
  test entendido como la única garantía real, migración de lo existente asignada al orquestador.
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿queda algún lugar que use el nombre público como nombre de tabla, o algún lugar
  que exponga el prefijado al usuario? (buscalos los dos, son los dos errores simétricos). ¿El
  prefijo puede hacer que un nombre válido supere el largo máximo de identificador? (el límite es
  32 y el prefijo suma — decidí si el límite aplica al nombre del tipo o al de la tabla, y
  documentalo; un tipo de 32 caracteres no puede generar una tabla que rompa el límite real de
  Postgres). ¿Un tipo llamado igual que el prefijo mismo, o empezando con él?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Verificación HTTP/UI real y el DEPLOY (con migración manual de `eventos`) los hace el
  orquestador.

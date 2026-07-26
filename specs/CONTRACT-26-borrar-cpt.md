> **NOTA (autor del contrato):** este documento invierte una decisión de alcance previa
> (`DEFINITION-CPT-DINAMICOS.md` listaba "borrar un CPT dinámico completo" como fuera de alcance).
> Se retoma porque la razón original —"no se discutió"— no era una objeción técnica, y porque
> `compat` v0.2.0 trajo `DROP TABLE`, que era la pieza que faltaba.

# Contrato 26 — Borrar un tipo de contenido dinámico

Cierra el ciclo de vida de los tipos dinámicos: hoy se pueden **crear** (CONTRACT-13) y **editar**
(CONTRACT-18), pero no borrar. NO toca `sqlite-postgres-compat`.

## Qué se construye

Borrar un tipo de contenido dinámico: su definición, sus campos y **su tabla real con todas sus
filas**. Es la operación más destructiva del producto, y el contrato trata la guarda como la parte
principal, no como un adorno.

## RECON ya resuelto (no re-investigar)

- **Nada más depende de un tipo dinámico.** Las tablas de unión con términos (`article_terms`,
  `product_terms`) existen solo para los tipos de CÓDIGO; un tipo dinámico no tiene ninguna.
  Verificado en `schema.Build()`.
- **`content_type_fields` ya cascadea**: tiene `foreignKeyCascade("content_type_id",
  ContentTypesTable, "id")`, así que borrar la fila del tipo se lleva sus campos sin trabajo extra.
- **Las rutinas generadas por tipo** (CONTRACT-20) se derivan de las definiciones persistidas, así
  que desaparecen solas del esquema compuesto cuando la definición ya no está. Lo que **no**
  desaparece solo es la metadata: ver abajo.
- **`compat.Store.DropTable` abre su propia transacción**, así que NO sirve acá. Usá la función
  PURA `compat.CompileDropTable` (o su variante `IfExists`) y ejecutá la sentencia dentro de tu
  transacción — exactamente como hace `EditContentType` desde CONTRACT-18. Leé esa función entera
  antes de escribir: es el modelo.
- **Consecuencia de usar la función pura**: mantener veraz `__compat_schema` pasa a ser
  responsabilidad tuya. La transacción debe dejar escrita la metadata compuesta COMPLETA al final.
  Es la caché que `InspectSchema` PREFIERE por sobre el catálogo físico y la que más veces mordió
  en este proyecto.
- El permiso es `content_types.manage`, que ya existe. **NINGÚN permiso nuevo.**
- No existe hoy ninguna ruta `DELETE` para tipos de contenido.

## T1 — La operación

FIX/OBJETIVO: borrar definición, campos y tabla en **UNA transacción**. Si algo falla, no queda
nada a medias: ni un tipo sin tabla, ni una tabla huérfana sin definición. Probalo forzando un
fallo, no lo afirmes.

Casos que hay que resolver explícitamente, no descubrir en producción:

- **El tipo no existe** → 404, sin efectos.
- **La definición existe pero su tabla física NO** (estado inconsistente, posible si alguien tocó
  la base a mano). Decidí: ¿se limpia igual la definición, o se rechaza? Justificá. Hay una
  variante `IfExists` del compilador que existe justamente para esta clase de duda; usar o no
  usarla es la decisión, y tiene consecuencias distintas.

## T2 — La guarda, que es el punto del contrato

FIX/OBJETIVO: que **no se pueda borrar por accidente**, y que quien lo haga sepa exactamente qué
está destruyendo.

- La confirmación debe exigir algo que **no se pueda mandar sin haberlo leído**. Un booleano
  `confirm: true` no sirve: se manda igual de fácil por error que a propósito. `CONTRACT-18` sentó
  el precedente para la pérdida parcial (confirmar por **lista de nombres**, no por bandera);
  extendé ese criterio, no lo aflojes.
- **Quien confirma tiene que ver cuántas filas se van a destruir ANTES de confirmar.** Un tipo con
  cero filas y uno con diez mil no son la misma decisión, y hoy no hay forma de saberlo sin salir
  a consultar.
- La respuesta dice qué se borró: el tipo y cuántas filas se fueron con él.

## T3 — La UI

FIX/OBJETIVO: borrar desde el panel, con la misma confirmación en dos pasos que ya usa la edición
de campos (`ui_contenttypes.go`), mostrando el recuento de filas. Respetá el guardián de
CONTRACT-15 (`h.page(r, title)`, nunca un literal `pageData{`).

## T4 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- Ciclo completo por HTTP contra **ambos motores**: crear un tipo, cargar filas, intentar borrar
  SIN confirmar (rechazado, **sin efectos**), confirmar mal (rechazado), confirmar bien (borrado),
  y comprobar por consulta directa al catálogo que **la tabla ya no existe** y que la definición y
  sus campos tampoco.
- Que el nombre quede **reutilizable**: crear un tipo con el mismo nombre después de borrarlo debe
  funcionar, y su tabla debe quedar vacía —no con las filas del anterior.
- Atomicidad: forzar un fallo a mitad y confirmar que no quedó ni la definición borrada ni la tabla
  caída.
- Ciclo de reinicio: borrar, reiniciar, confirmar que arranca limpio y no intenta recrear nada.
- `--dump-schema` deja de incluir la tabla, y `/content/{tipo}` pasa a 404.
- Confirmá que TODO lo de contratos anteriores sigue funcionando.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T1: operación atómica, probada forzando un fallo; caso "tabla ausente" decidido y justificado.
- [ ] T2: confirmación que no se puede mandar sin leerla, y recuento de filas visible ANTES.
- [ ] T3: UI en dos pasos con el recuento.
- [ ] T4: ciclo completo en los dos motores con salida real, incluida la reutilización del nombre.
- [ ] La metadata `__compat_schema` coincide con el catálogo tras el borrado.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO toques `sqlite-postgres-compat`.
- Sin dependencias nuevas. NINGÚN permiso nuevo. NO commitear.
- NO agregues borrado de tipos de contenido de CÓDIGO (`articles`, `products`): son parte del
  esquema fijo y no se administran desde el producto.
- NO toques el borrado de FILAS de contenido, que ya existe y funciona.
- El contrato público de las rutas existentes no cambia.

## Checklist antes de delegar

- [ ] RECON corrido: cascada de campos entendida, `CompileDropTable` identificada como la única
  utilizable dentro de una transacción, y la responsabilidad sobre la metadata asumida.
- [ ] Red-team: ¿un tipo con filas y otro sin filas? ¿Borrar dos veces? ¿Dos borrados concurrentes
  del mismo tipo? ¿Borrar mientras otro cliente escribe contenido de ese tipo? ¿Un nombre que
  necesita comillas? ¿Qué pasa con un token de sesión de alguien que estaba en la pantalla de ese
  tipo? ¿El borrado deja tablas de paso (`cptmp_`) si falla a mitad?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador; password enmascarado.

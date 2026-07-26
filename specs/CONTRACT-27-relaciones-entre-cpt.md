> **NOTA (autor del contrato):** invierte una decisión de alcance de
> `DEFINITION-CPT-DINAMICOS.md` ("Relaciones (FK real) en campos de un CPT dinámico — solo campos
> escalares en v1"). Se retoma con un límite nuevo y declarado, no eliminando la objeción: ver
> "El precio, decidido de antemano".

# Contrato 27 — Relaciones entre tipos de contenido dinámicos

Un tipo dinámico puede declarar una **referencia** a otro tipo dinámico, materializada como una
clave foránea real en la base. NO toca `sqlite-postgres-compat`.

## El precio, decidido de antemano (no lo reabras)

`compat` **nunca emite `CASCADE`** por diseño: si algo referencia una tabla, el `DROP` se rechaza.
La edición de campos (CONTRACT-18) funciona **reconstruyendo**: crea una tabla de paso, copia,
**borra la original**, la recrea y copia de vuelta.

De ahí sale el precio, y ya está aceptado: **un tipo que es destino de una referencia no se puede
editar ni borrar** mientras la referencia exista. No es un defecto a esconder ni a resolver acá —
resolverlo exigiría quitar y recrear las FK entrantes, y `compat` no tiene `ALTER TABLE`.

Lo que este contrato SÍ debe garantizar es que ese límite se encuentre **de la forma correcta**: un
error que diga qué tipo lo referencia y qué hay que hacer para liberarlo, ANTES de intentar nada,
no un fallo crudo del motor a mitad de una transacción.

## RECON ya resuelto (no re-investigar)

- **El destino de la referencia NO puede guardarse en `content_type_fields`.** Esa tabla tiene un
  `CHECK` que fija `field_type` al vocabulario cerrado de `schema.FieldTypes`, y agregarle un valor
  exigiría alterar el `CHECK` de una tabla existente — algo que `EnsureSchema` no hace (solo crea
  tablas faltantes). En una instalación ya desplegada, el `CHECK` viejo rechazaría el valor nuevo
  **al insertar**, o sea un fallo en runtime, no al arrancar. Agregarle una columna choca contra el
  mismo muro.
- **La salida es una TABLA NUEVA** para las referencias: es puramente aditiva y `EnsureSchema` la
  crea sola, en instalaciones nuevas y existentes. Las referencias quedan como un concepto
  **hermano** de los campos escalares, no como un tipo de campo más. La tabla compuesta toma sus
  columnas escalares de los campos y sus columnas de FK de las referencias.
- `schema.DynamicTable` / `BuildWith` componen hoy la tabla a partir de los campos; ahí entran las
  referencias.
- `compat` soporta acciones referenciales (`no_action`, `restrict`, `cascade`, `set_null`,
  `set_default`); `schema.foreignKeyCascade` es el helper existente para las de código.
- Los tipos de código (`articles`, `products`) NO son destinos válidos: este contrato es sobre
  relaciones entre tipos DINÁMICOS. Una relación con un tipo de código sigue exigiendo un tipo de
  código, como dice la definición de fase.

## Decisiones YA TOMADAS

1. **`ON DELETE` NO es `CASCADE`.** Borrar una fila referenciada no puede destruir en silencio las
   filas que la apuntan; el motor debe rechazar y el error propagarse. Elegí entre `restrict` y
   `no_action` y justificá la diferencia real entre las dos en ambos motores — no asumas que son
   sinónimos.
2. **Las referencias son opcionales** (la columna admite ausencia), como el resto de los campos
   dinámicos hoy.
3. **Sin autorreferencia en v1.** Un tipo que se apunta a sí mismo exige que la tabla exista antes
   de su propia FK, lo que rompe la creación en un solo paso. Rechazalo con un mensaje que diga que
   es una limitación conocida, no un error del usuario.

## T1 — El modelo y su validación

FIX/OBJETIVO: la tabla nueva de referencias, y la composición de la tabla real del tipo incluyendo
sus columnas de FK. Validación: el tipo destino debe existir y ser dinámico; el nombre de la
referencia sigue el mismo gate de identificadores que los campos y no puede chocar con un campo ni
con las columnas que inyecta `ContentType()`; sin autorreferencia; sin duplicados.

## T2 — Crear, y el orden que impone

FIX/OBJETIVO: crear un tipo con referencias. El destino tiene que existir **antes**; si no,
rechazo explícito. Cuidado con `missingTables`, que hoy espera exactamente **una** tabla faltante:
verificá que sigue siendo cierto cuando la tabla nueva trae una FK.

## T3 — La guarda sobre editar y borrar

FIX/OBJETIVO: antes de reconstruir (CONTRACT-18) o borrar (CONTRACT-26) un tipo, comprobar si algo
lo referencia. Si sí, **fallar antes de tocar nada**, nombrando qué tipo lo referencia y por qué la
operación no es posible.

Es el punto donde este contrato se hace o se arruina: el usuario tiene que entender que su tipo
está congelado **por una relación que él creó**, y qué borrar para liberarlo. Un `SQLSTATE 23503`
crudo no es eso.

## T4 — La UI

FIX/OBJETIVO: declarar una referencia al crear un tipo, eligiendo el destino de la lista de tipos
existentes. Y que el mensaje de la guarda de T3 se vea igual de claro en el panel. Respetá el
guardián de CONTRACT-15.

## T5 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- Ciclo completo por HTTP contra **ambos motores**: crear el tipo destino, crear el que lo
  referencia, cargar filas con y sin la referencia, y confirmar por consulta directa al catálogo
  que **la FK existe de verdad** —no una columna suelta.
- **Que la FK se cumpla**: insertar una referencia a un id inexistente debe fallar, en los dos
  motores, y con un error traducido a 400, no a 500.
- Borrar una fila referenciada: rechazado, con error legible.
- **La guarda de T3, en las dos operaciones y en los dos motores**: editar y borrar el tipo
  destino, ambos rechazados nombrando al que lo referencia. Y que tras borrar el que referencia,
  el destino vuelva a ser editable y borrable.
- Ciclo de reinicio y `--dump-schema` incluyendo la FK.
- Confirmá que TODO lo de contratos anteriores sigue funcionando.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T1: tabla nueva aditiva; `content_type_fields` sin cambios de esquema.
- [ ] T2: creación con referencia; orden validado con mensaje claro.
- [ ] T3: editar y borrar un tipo referenciado fallan ANTES de tocar nada, nombrando al referente.
- [ ] T5: FK real verificada contra el catálogo de los dos motores, y su cumplimiento probado.
- [ ] Tras eliminar la referencia, el destino vuelve a ser editable y borrable.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO toques `sqlite-postgres-compat`.
- Sin dependencias nuevas. NINGÚN permiso nuevo. NO commitear.
- NO cambies el esquema de `content_type_fields` ni de `content_types`: son tablas existentes y
  `EnsureSchema` no las altera.
- NO agregues `CASCADE` en ninguna dirección.
- NO implementes autorreferencia ni referencias a tipos de código.

## Checklist antes de delegar

- [ ] RECON corrido: entendido por qué el destino va en una tabla nueva y no en una columna nueva.
- [ ] Red-team: ¿crear dos tipos que se referencien mutuamente (ciclo)? ¿Borrar el destino
  mientras alguien crea una fila que lo referencia? ¿Una referencia a un tipo que se borró entre la
  validación y la creación? ¿El orden de las tablas en `--dump-schema` respeta las dependencias
  para que el export se pueda aplicar? ← esa última es la que rompe el export en silencio.
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador; password enmascarado.

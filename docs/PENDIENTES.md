# Huecos conocidos del producto

Cosas que faltan y ya están identificadas con evidencia real, para que no se
redescubran desde cero. No es un backlog de ideas: cada entrada acá se encontró
operando el sistema de verdad.

Las entradas resueltas **se conservan**, con el texto original de cómo se
descubrió el hueco: sirve para reconocer la misma forma de problema la próxima
vez. El estado va en el índice para no tener que leerlas para saberlo.

| # | Hueco | Estado |
|---|---|---|
| 1 | [Otorgar permisos a un rol desde el producto](#1-no-hay-forma-de-otorgar-permisos-a-un-rol-desde-el-producto-alta) | **RESUELTO** (CONTRACT-16) |
| 2 | [Editar campos de un tipo dinámico](#2-editar-campos-de-un-tipo-de-contenido-dinámico-media) | **RESUELTO** (CONTRACT-18) |
| 3 | [Colisión de nombre entre un CPT y una tabla de código](#3-colisión-de-nombre-entre-un-cpt-dinámico-y-una-tabla-de-código-futura-baja) | **RESUELTO** (CONTRACT-17) |
| 4 | [Crear la primera identidad desde el producto](#4-no-hay-forma-de-crear-la-primera-identidad-desde-el-producto-media) | **RESUELTO** (CONTRACT-22) |
| 5 | [`pgvector` obligatorio aunque no se usen vectores](#5-pgvector-es-obligatorio-aunque-no-se-usen-vectores-media) | **RESUELTO** (CONTRACT-23) |
| 6 | [La migración sin ventana de corte nunca se ejercitó](#6-la-migración-sin-ventana-de-corte-nunca-se-ejercitó-media) | **RESUELTO** (ensayo 2026-07-25) |
| 7 | [`/health` no mira la base, y una caída se ve como 401](#7-health-no-mira-la-base-y-una-caída-se-ve-como-401-alta) | **RESUELTO** (CONTRACT-24) |
| 8 | [Un fallo de base da 500 en las rutas de datos y 503 en el resto](#8-un-fallo-de-base-da-500-en-las-rutas-de-datos-y-503-en-el-resto-baja) | **RESUELTO** (CONTRACT-25) |
| 9 | [El selector de relación ofrece 100 filas y no tiene buscador](#9-el-selector-de-relación-ofrece-100-filas-y-no-tiene-buscador-baja) | **RESUELTO** (CONTRACT-32) |
| 10 | [Abrir un formulario con relaciones cuesta N+1 consultas](#10-abrir-un-formulario-con-relaciones-cuesta-n1-consultas-baja) | **RESUELTO** (CONTRACT-31) |
| 11 | [El listado genérico no muestra las columnas de relación](#11-el-listado-genérico-no-muestra-las-columnas-de-relación-baja) | **RESUELTO** (CONTRACT-31) |

## 1. No hay forma de otorgar permisos a un rol desde el producto (ALTA)

**Estado:** RESUELTO por CONTRACT-16 (`specs/CONTRACT-16-gestion-de-permisos.md`,
reporte en `docs/reports/CONTRACT-16-REPORT.md`). Ya existe una vía de escritura
real: `GET /admin/roles/{name}/edit` + `POST /admin/roles/{name}/permissions`,
gateada por `roles.manage` (que por fin tiene consumidor), con reemplazo atómico
del conjunto (`auth.SetRolePermissions`) y una guarda anti-bloqueo que impide que
quien ejecuta el cambio se quede sin `roles.manage`. Ya no hace falta SQL manual.
El texto de abajo se conserva como registro de cómo se descubrió el hueco.

**Qué pasa:** el catálogo de permisos es fijo en código y se siembra solo
(`schema.Permissions` → `store.SeedCatalogs`, idempotente), y los roles también
(`schema.Roles`). Pero la tabla que los CONECTA — `role_permissions`, la que
decide qué puede hacer realmente cada rol — no tiene ninguna vía de escritura:

- `/admin/roles` es deliberadamente de **solo lectura** (decisión explícita de
  `DEFINITION-UI.md` / CONTRACT-08: la UI asigna roles a usuarios, no edita el
  catálogo).
- No existe ninguna ruta JSON para otorgar/revocar un permiso a un rol.
- Los tests locales siempre otorgaron los grants con SQL directo (helpers
  descartables), así que la ausencia nunca apareció en verde/rojo.

**Cómo se descubrió (2026-07-24, durante el deploy de CONTRACT-13):** al
intentar crear un tipo de contenido dinámico en producción, la respuesta fue
`403 forbidden`. Inspeccionando la base real: `role_permissions` estaba
**completamente vacía** — cero grants, para todos los roles, desde el primer
deploy. El usuario admin tenía el rol `administrator`, pero ese rol no tenía
ningún permiso. En la práctica producción era de **solo lectura para todo el
mundo** desde el día uno, y nunca se notó porque todas las verificaciones
previas en producción habían sido lecturas (logins y GETs).

Causa concreta: el helper de bootstrap del primer deploy asignó el ROL al
usuario, pero nunca otorgó permisos AL ROL — y no había forma de hacerlo por
el producto.

**Workaround aplicado:** se otorgaron los 8 permisos del catálogo al rol
`administrator` con SQL directo contra la base del VPS. Producción quedó
usable, pero cualquier cambio futuro de grants (otro rol, otro permiso,
revocar) exige repetir el SQL manual.

**Qué haría falta:** una capacidad real de gestión de grants — API + UI para
otorgar/revocar permisos a un rol, gateada por `roles.manage` (que ya existe en
el catálogo y hoy **no se usa en ninguna parte**, precisamente porque esta
capacidad nunca se construyó). Ojo al diseñarlo: es la capacidad que puede
quitarle permisos al último rol que los tiene, así que necesita una guarda
contra el auto-bloqueo (dejar el sistema sin nadie que pueda administrarlo).

## 2. Editar campos de un tipo de contenido dinámico (MEDIA)

**Estado:** RESUELTO por CONTRACT-18 (`specs/CONTRACT-18-editar-campos-cpt.md`,
reporte en `docs/reports/CONTRACT-18-REPORT.md`). Un tipo dinámico ya no es
crear-solamente: `PUT /content-types/{nombre}` y `/admin/content-types/{nombre}/edit`
(ambos gateados por `content_types.manage`, sin permiso nuevo) agregan, renombran
y quitan campos. La tabla real se RECONSTRUYE dentro de UNA transacción
(crear tabla de paso con la forma nueva → copiar → borrar la original → crearla
otra vez con el mismo nombre → copiar de vuelta → borrar la de paso), componiendo
solo operaciones que `compat` ya expresa (`CompileDDL` + `CompileDropTable` de
v0.2.0): **no** se agregó ningún mecanismo de migración por fuera de `compat`, así
que la objeción de abajo sigue respetada. Los datos de los campos que sobreviven
se preservan (el mapeo es por `content_type_fields.id`, no por nombre, así que un
renombre cruzado `a`→`b` / `b`→`a` funciona), los `id` de las filas no cambian, y
quitar un campo exige confirmación explícita campo por campo. Cambiar el TIPO de
un campo sigue fuera de alcance por la divergencia de casteo entre motores. Se
conserva el texto de abajo como registro de por qué estuvo abierto tanto tiempo.

**Estado anterior:** fuera de alcance por decisión, documentado en
`DEFINITION-CPT-DINAMICOS.md`.

`sqlite-postgres-compat` no tiene ningún soporte de `ALTER TABLE` (verificado:
cero resultados en todo el paquete). Los CPT dinámicos son **crear-solamente**:
una vez aplicada la tabla, sus campos quedan congelados. Cambiar campos exige
crear un tipo nuevo.

Resolverlo implicaría construir un mecanismo de migración propio por fuera de
`compat`, lo que introduciría una segunda fuente de verdad del esquema —
contradice el principio que sostiene el resto del proyecto. Es una decisión de
alcance, no un olvido.

## 3. Colisión de nombre entre un CPT dinámico y una tabla de código futura (BAJA)

**Estado:** RESUELTO por CONTRACT-17 (`specs/CONTRACT-17-prefijo-tablas-dinamicas.md`,
reporte en `docs/reports/CONTRACT-17-REPORT.md`). La tabla real de un tipo
dinámico lleva ahora el prefijo `cpt_` (`schema.DynamicTablePrefix`, aplicado en
el único punto `schema.DynamicTableName`), y ninguna tabla de código puede
usarlo — lo garantiza un test que recorre `schema.Build()`. Los dos espacios de
nombres son disjuntos, así que la colisión descrita abajo pasó de "falla ruidosa
que exige intervención manual" a IMPOSIBLE. El nombre público del tipo no
cambió: las rutas y la API siguen usando el nombre que el admin eligió. Se
conserva el texto de abajo como registro de cómo se descubrió el hueco.

**Estado previo:** documentado, no resuelto (CONTRACT-13).

El validador de identificadores reserva los nombres de todas las tablas que
produce `schema.Build()`, derivándolos en vez de hardcodearlos — así que hoy un
CPT dinámico no puede pisar una tabla de código existente. Pero al revés no
está cubierto: si un contrato futuro agrega una tabla de código con el nombre de
un CPT dinámico que alguien ya creó, el esquema compuesto tendría tablas
duplicadas y `Schema.Validate()` fallaría → **el servicio no arrancaría**.

Es falla ruidosa, no corrupción silenciosa, pero exigiría intervención manual en
la base. Mitigación barata si algún día molesta: chequear los nombres dinámicos
existentes antes de agregar una tabla de código nueva.

## 4. No hay forma de crear la primera identidad desde el producto (MEDIA)

**Estado:** RESUELTO por CONTRACT-22 (`specs/CONTRACT-22-bootstrap-inicial.md`,
reporte en `docs/reports/CONTRACT-22-REPORT.md`). El binario tiene un modo
`librarian --bootstrap --email <dirección>` que crea la primera identidad **y**
le otorga al rol `administrator` todos los permisos del catálogo, en **una sola
transacción**, sobre los dos motores. La contraseña se lee de la **entrada
estándar** (nunca es argumento: quedaría en el historial del shell y visible en
la lista de procesos de toda la máquina). No agrega superficie al servicio en
marcha.

Al verificarlo apareció que el hueco era **más grande que su título**: no era
solo que faltara la primera identidad, sino que `EnsureSchema` + `SeedCatalogs`
dejan `role_permissions` VACÍA, así que un usuario con rol `administrator` recién
creado no tenía **ningún** permiso — y la UI que otorga permisos (CONTRACT-16, el
arreglo del hueco 1) está gateada por `roles.manage`, que tampoco tenía. Bloqueo
circular: **una instalación en limpio no se podía administrar por ninguna vía del
producto**. Por eso el bootstrap hace las dos cosas juntas o ninguna.

Es imposible de usar dos veces, y la garantía es **de la base**: la tabla
`bootstrap` tiene una única clave representable (PRIMARY KEY sobre `id` + CHECK
que lo fija a `'bootstrap'`), así que un segundo intento —incluso simultáneo,
desde otro proceso— viola la clave y se revierte entero. Una instalación que ya
tiene usuarios (**el estado de producción hoy**) es RECHAZADA, no reparada: una
vía que acuña administradores sobre un sistema que ya tiene identidades es un
agujero de autenticación, no una herramienta de reparación. Se conserva el texto
de abajo como registro de cómo se descubrió el hueco.

**Qué pasa:** `librarian` no tiene registro público —decisión correcta para un
backend de administración— pero tampoco tiene ninguna otra vía de alta inicial.
El primer usuario se crea **fuera de banda**: escribiendo contra la base o desde
un programa que use `auth.CreateUser`. A partir de ahí sí se administra todo por
el producto (usuarios, roles, permisos, claves de API).

**Cómo se descubrió (2026-07-25, al verificar CONTRACT-21):** para ejercitar la
aplicación recién levantada sobre PostgreSQL en limpio hubo que sembrar el
usuario administrador con un programa aparte; no existe forma de hacerlo con el
binario que se despliega.

Es la misma forma que tenía el hueco 1 —una capacidad sin vía de escritura— pero
menos grave, porque quien instala tiene acceso a la base por definición. Se
vuelve molesto en cuanto la instalación en limpio deje de ser excepcional.

**Qué haría falta:** un modo de arranque para el alta inicial (un subcomando, o
una ruta que solo funcione mientras no exista ningún usuario). La guarda
importante es que **no debe poder usarse dos veces**: una vía de creación de
administradores que sobrevive al primer uso es un agujero de autenticación, no
una comodidad.

## 5. `pgvector` es obligatorio aunque no se usen vectores (MEDIA)

**Estado:** RESUELTO por CONTRACT-23 (`specs/CONTRACT-23-vector-opcional.md`,
reporte en `docs/reports/CONTRACT-23-REPORT.md`). La capacidad vectorial se
declara por instalación con `LIBRARIAN_VECTOR` (por defecto `enabled`, que es lo
que toda instalación desplegada ya tiene): con `disabled`, `articles.embedding`
no se declara, no hay ningún `vector(N)` en el esquema y la instalación **arranca
y sirve sobre un PostgreSQL 17 sin `pgvector`** — verificado con el binario real
contra un servidor donde la extensión ni siquiera se puede instalar. La elección
es IRREVERSIBLE después del primer arranque (`EnsureSchema` nunca altera una
tabla existente), así que cambiarla sobre una instalación ya creada **no
arranca**: falla con un mensaje que explica qué pasó y qué se puede hacer, y la
incoherencia se detecta contra la BASE (la columna física), nunca contra la
metadata `__compat_schema`. Con la capacidad habilitada nada cambió. El texto de
abajo se conserva como registro de cómo se descubrió el hueco.

**Qué pasa:** `articles.embedding` es una columna `vector(1536)`, y aunque es
**nullable** —una instancia puede no guardar jamás un embedding— la columna
existe en el esquema, así que crear las tablas en PostgreSQL exige la extensión
`pgvector`. Sin ella el primer arranque falla.

**Consecuencia real:** una instancia que solo quiere administrar contenido
**no puede correr sobre un PostgreSQL administrado que no ofrezca `pgvector`**.
Es un requisito de infraestructura impuesto por una capacidad opcional.

**Cómo se descubrió (2026-07-25):** al migrar la base real a PostgreSQL, contra
un `postgres:17-alpine` limpio el export falló con
`type "vector" does not exist`. Está documentado como prerrequisito duro en
`docs/DEPLOY.md` y `docs/OPERATIONS.md`, pero documentarlo no lo elimina.

**Qué haría falta:** que la capacidad vectorial sea opcional en el esquema —que
la columna solo se declare si la instancia la habilita— de modo que el requisito
de infraestructura aparezca solo cuando se usa. Ojo: eso convierte el esquema
canónico en condicional, y hoy es una constante; hay que pensar qué significa
eso para el export y para el contrato de equivalencia.

## 6. La migración sin ventana de corte nunca se ejercitó (MEDIA)

**Estado:** RESUELTO por un ensayo operativo (2026-07-25), documentado en
`docs/OPERATIONS.md` → "Migración con el servicio arriba". Se ejecutó un
`compat cutover` real contra una instancia de `librarian` con escrituras
concurrentes: auditoría exacta en 10 features, captura instalada, snapshot,
drenaje de 41 cambios y digests idénticos. Verificado además que la ÚLTIMA
escritura previa al quiesce llegó al destino, y que `librarian` arranca contra
ese destino y sirve —login con la credencial de la fuente y escritura nueva 201.

**El hallazgo, que cambia cómo se planifica:** `cutover` NO significa "nunca
dejar de escribir". El drenaje termina cuando la captura ve N sondeos
consecutivos sin cambios, así que **si la aplicación sigue escribiendo esa
condición no se cumple nunca** — medido: con una escritura cada 0.4 s, a los 10
minutos el journal tenía 1234 entradas y seguía creciendo. La ventana sin
servicio existe igual; lo que cambia es que pasa de "toda la copia" a "lo que
tarde en drenar lo escrito mientras copiaba", que es una ganancia real y grande.
El texto de abajo se conserva como registro de cómo se identificó el hueco.

**Qué pasaba:** la promesa del sistema es migrar a PostgreSQL "sin apagar la
aplicación". Hoy están verificadas las dos mitades por separado: la aplicación
**corre** sobre PostgreSQL (CONTRACT-21, con transcripción HTTP real), y los
datos **se trasladan** con equivalencia verificada por digest (`compat copy`,
sobre la base real de producción). Lo que **no** se ejercitó nunca es
`compat cutover` —captura de cambios, drenaje y corte— contra el esquema y los
datos de `librarian`.

**Por qué importa la distinción:** `compat copy` es una migración por snapshot;
las escrituras que ocurran durante la copia no viajan. O sea que hoy la
migración real exige una ventana de corte, aunque sea corta. La capacidad que la
elimina existe en el paquete y está probada en SU suite, no con este esquema.

**Cómo se descubrió (2026-07-25):** al escribir `docs/OPERATIONS.md` se
documentó explícitamente que el runbook cubre el camino de snapshot y que el de
cutover no está ejercitado, en vez de dejarlo insinuado.

**Qué haría falta:** un ensayo de `compat cutover` contra una copia de la base
real, con escrituras concurrentes durante la copia, verificando que se drenan.
Es trabajo con su propio contrato, no un paso de un deploy.


## 7. `/health` no mira la base, y una caída se ve como 401 (ALTA)

**Estado:** RESUELTO por CONTRACT-24
(`specs/CONTRACT-24-salud-y-errores-de-infraestructura.md`, reporte en
`docs/reports/CONTRACT-24-REPORT.md`). Se separó *vivo* de *listo para servir*:
`/health` **conserva exactamente su significado y su cuerpo** (el proceso vive —
hay monitoreo apuntado ahí), y la disponibilidad es una ruta NUEVA, `GET /ready`,
que sí hace un `ping` a la base con su propio límite de 2 s y responde **503**
`{"error":"service unavailable"}` cuando no la alcanza. Y un fallo de
infraestructura ya no se disfraza de credencial: el login y la resolución de
identidad por API key devuelven **503** cuando la base no contesta, en vez de
401. Verificado con el contenedor de PostgreSQL **realmente detenido**: `/health`
200, `/ready` 503, login 503. La anti-enumeración quedó intacta —un usuario
inexistente y uno existente con contraseña mala siguen dando el mismo 401 con el
mismo cuerpo— y el razonamiento de por qué distinguir la infraestructura no
reintroduce enumeración está escrito junto a la guarda, en `handleLogin`. El
texto de abajo se conserva como registro de cómo se descubrió el hueco.

**Qué pasa:** `/health` responde `{"status":"ok"}` aunque la base esté caída —
comprueba que el proceso vive, no que el sistema pueda servir. Y peor: con la
base detenida, el login y las lecturas devuelven **401**, no un 5xx. Una caída
de infraestructura se presenta como *credenciales incorrectas*.

**Por qué importa ahora y antes no:** con SQLite embebido la base no podía estar
"caída" por separado; era un archivo en el mismo proceso. Desde que el motor
puede ser PostgreSQL (CONTRACT-21) hay un segundo proceso que puede fallar solo,
y esa distinción entre *vivo* y *listo para servir* pasa a ser real.

**Cómo se descubrió (2026-07-26, al desplegar la instancia en limpio sobre
PostgreSQL):** en la verificación post-deploy se detuvo el contenedor de la base
a propósito para probar la resiliencia. Medido con la base en `exited`:

```
/health            -> {"status":"ok"}
POST /auth/login   -> 401
GET  /content-types -> 401
```

**Las dos consecuencias, y la segunda es la grave:** un monitor externo o un
balanceador ven la instancia sana mientras no puede atender nada; y quien
diagnostique el incidente va a perseguir un problema de credenciales mientras la
causa es que la base no está.

**Qué haría falta:** separar *vivo* de *listo* — que la comprobación de salud
consulte la base (un `ping` barato basta) o que exista una segunda ruta que sí lo
haga, y que un fallo de conexión a la base se traduzca en 5xx y no en 401. Ojo al
diseñarlo: la ruta de salud no debe quedar cara ni convertirse en una vía de
carga contra la base, y no debe filtrar detalles de la conexión.


## 8. Un fallo de base da 500 en las rutas de datos y 503 en el resto (BAJA)

**Estado:** RESUELTO por CONTRACT-25 (`specs/CONTRACT-25-503-en-rutas-de-datos.md`,
reporte en `docs/reports/CONTRACT-25-REPORT.md`). Un único punto de clasificación
(`internal/server/datafailure.go`) decide, para cada operación de datos que falla,
si la base está accesible: si no lo está, 503 con el mismo cuerpo fijo que el
login y `/ready`; si lo está, el 500 de siempre, con su mensaje de siempre. La
clasificación consulta la sonda de CONTRACT-24 y **no** reconoce tipos de error
del driver, así que no ata `librarian` a un motor. Medido con `pg-c25` realmente
detenido (`docker stop`, estado `exited`) y, del otro lado, con un fallo interno
genuino provocado con la base arriba.

**Qué pasa:** ante un fallo de base, `CONTRACT-24` devuelve `503` en el login y
en `/ready`, pero las rutas de datos siguen devolviendo `500`. Las dos son 5xx
—o sea que el contrato se cumple— pero significan cosas distintas para quien las
recibe: `503` le dice a un balanceador "sacame de rotación y reintentá", `500`
le dice "esto está roto". Ante la misma causa deberían decir lo mismo.

**Qué haría falta:** que un error de conexión a la base se traduzca a `503`
también en las rutas de datos. Ojo: hay que distinguirlo de un `500` legítimo
(una fila corrupta, un fallo al firmar un token), que debe seguir siendo `500`.

### Corrección de una entrada previa, y por qué vale registrarla

Esta entrada decía originalmente que **las rutas de datos tardaban ~21 s en
fallar** con la base caída, y estaba **mal**. Los 21 segundos eran un artefacto
del banco de pruebas: el cliente corría en una máquina Windows y el PostgreSQL en
el VPS, así que al detener el contenedor los paquetes iban a un puerto que el
firewall descarta, y 21 s son los reintentos de SYN de Windows. No era el driver
ni la aplicación.

Medido después en la **topología real de producción**, donde la aplicación y la
base comparten host y se hablan por `127.0.0.1`, con la base detenida de verdad:

```
/ready              -> 503 en 0,056 s
POST /auth/login    -> 503 en 0,051 s
```

Un puerto cerrado en localhost rechaza al instante; no hay reintentos que
esperar. El problema descrito no existe en producción.

La lección operativa: **una medición de latencia hecha desde una topología
distinta a la real mide la topología, no el sistema**. El número era correcto y
la conclusión falsa.

Queda como observación menor, no como hueco: las rutas de datos **no tienen
límite de tiempo propio**, así que una base *lenta pero viva* podría demorarlas
sin techo. Eso no se midió porque no se reprodujo; anotarlo como problema sin
evidencia sería repetir el error de arriba.

## 9. El selector de relación ofrece 100 filas y no tiene buscador (BAJA)

**Estado:** RESUELTO por CONTRACT-32. El control ahora trae un buscador que
recarga las opciones **desde la base** (htmx + fragmento de servidor, sin
JavaScript propio: `GET /admin/content/{tipo}/reference?name={relación}`), así
que una fila que el tope dejaba afuera se alcanza desde el panel. **El tope no se
movió** —sigue ofreciendo 100 y sigue avisando cuando recorta—: lo que cambió es
el conjunto alcanzable, no el mostrado.

Dos decisiones que conviene no perder:

- **El filtro va a la base, no a Go.** La alternativa (`compat.Store.SearchText`)
  es idéntica en los dos motores por construcción pero lee la tabla entera, que
  es justo la cota que el hueco 10 acababa de poner. Medido: una búsqueda cuesta
  7 sentencias con 21 filas destino y 7 con 221.
- **El precio de esa decisión, y cómo se paga.** `compat` compila `like` a `LIKE`
  en SQLite y a `ILIKE` en PostgreSQL, y los dos difieren de verdad: medido
  contra PostgreSQL 17, `%ñandú%` trae `[ñandú menor]` en SQLite y
  `[ÑANDÚ ñandú menor]` en Postgres, y una barra invertida —que en Postgres es el
  escape por defecto de `LIKE` y en SQLite no es nada— trae `[barra\invertida]`
  en uno y `[]` en el otro. 6 de 18 búsquedas divergían en crudo. El patrón que
  se bindea es por eso deliberadamente GRUESO (todo lo que no sea ASCII literal
  pasa a `_`), la base solo ACOTA, y el match exacto lo decide Go. El resultado
  es idéntico en los dos motores: `TestDualEngineReferenceSearch`.

**Residuo declarado:** la búsqueda filtra por el PRIMER campo declarado del tipo
destino, que es el mismo del que sale la etiqueta. Un destino sin campos, o cuyo
primer campo no es `text`, no se puede buscar por nombre (PostgreSQL no tiene
`LIKE` para `integer`/`numeric`/`date`); el formulario lo dice en vez de ofrecer
un control que no funciona. Tampoco es insensible a acentos: `nandu` no encuentra
`ñandú`, igual que no lo encontraría `ILIKE`.

**Lo que NO se rompió, y era lo fácil de romper:** el valor vigente viaja con cada
búsqueda y vuelve siempre seleccionado, coincida o no. Sin eso, buscar cualquier
cosa que no coincida y guardar borraría una relación que nadie tocó — el defecto
de CONTRACT-30 reconstruido desde su propio arreglo. Lo fija
`TestSearchDoesNotDropTheCurrentRelation`, y se verificó comentando el rescate:
el formulario pasa a mandar `autor=""` y la columna queda en NULL.

El texto de abajo se conserva como registro de cómo se descubrió el hueco.

**Qué pasa:** el `<select>` que CONTRACT-30 agregó al formulario de contenido
ofrece las 100 filas más recientes del tipo destino — el MISMO tope que usa el
listado genérico, para que lo elegible y lo visible coincidan. Si el destino
tiene más, las demás no se pueden elegir desde el panel: la única vía es la API
JSON, mandando el id.

**Por qué es BAJA y no MEDIA:** el formulario **lo dice**. Cuando recorta,
aparece un aviso con el número: *"Se están ofreciendo solo las 100 filas más
recientes de «X». Si la fila que buscás no está en la lista, no la vas a poder
elegir desde acá."* Un recorte silencioso sí sería un hueco serio —el admin no
puede elegir lo que no ve, y no tendría cómo saber que no lo ve—; uno declarado
es una limitación conocida.

**Lo que SÍ está cubierto, y conviene no perderlo de vista al arreglar esto:** si
la relación vigente de la fila que estás editando cae fuera del corte, se busca
aparte y se antepone al `<select>`, ya seleccionada. Sin eso el control caería en
la opción vacía y el siguiente guardado borraría una relación que nadie tocó —
que es exactamente el defecto que CONTRACT-30 vino a cerrar, reconstruido a
partir de su propio arreglo. Lo fija el test
`TestPanelSaysWhenTheSelectorIsTruncated`, y se verificó rompiéndolo a propósito:
al quitar el rescate, se pone rojo nombrando exactamente eso.

**Qué haría falta:** un control con búsqueda (escribir para filtrar contra el
servidor) en vez de una lista completa. Es un problema de UI distinto del que
resolvió CONTRACT-30 y por eso quedó fuera de su alcance.

## 10. Abrir un formulario con relaciones cuesta N+1 consultas (BAJA)

**Estado:** RESUELTO por CONTRACT-31, junto con el hueco 11: son el mismo
problema en dos pantallas. Cada tipo destino distinto se resuelve **una vez por
petición** (`referenceTargetCache`), así que dos relaciones al mismo destino ya
no pagan dos veces — medido: 11 consultas con una relación y 11 con dos al mismo
destino. El caché es POR PETICIÓN a propósito: uno compartido entre peticiones
ofrecería filas ya borradas, que es cambiar un problema de costo por uno de
corrección. El texto de abajo se conserva como registro.

**Qué pasa:** dibujar el formulario de un tipo con relaciones cuesta, **por cada
relación declarada**, un `FetchContentType` (para resolver el destino desde el
registro, que es lo que evita tratar un nombre como identificador) más un
`listContentRows` de sus opciones. Y una consulta extra si el valor vigente cae
fuera del tope del hueco 9. Un tipo con varias relaciones multiplica consultas
cada vez que alguien abre el alta o la edición.

**Por qué es BAJA:** son lecturas por rutina, acotadas por el mismo tope de 100,
y ocurren al dibujar un formulario — no en un camino caliente ni en un bucle. No
se midió que duela; se registra para que no se redescubra creyendo que es un bug.

**Qué haría falta:** resolver las definiciones de destino de una sola vez, o
cachearlas por petición. Cualquier arreglo tiene que respetar la razón por la que
hoy se releen del registro: el nombre de un tipo nunca se trata como identificador
sin resolverlo antes.

## 11. El listado genérico no muestra las columnas de relación (BAJA)

**Estado:** RESUELTO por CONTRACT-31. El listado tiene una columna por relación,
con la MISMA etiqueta que el selector (las dos llaman a `referenceOptionLabel`,
no hay dos cálculos que puedan divergir). **La cota es el criterio que se midió**:
un listado de 100 filas cuesta las mismas 12 consultas que uno de 3, porque los
ids ya viajan en las filas leídas y se traducen contra UNA página por tipo
destino distinto — no una consulta por fila. Una relación enteramente en NULL
cuesta 0 extra (carga perezosa).

**Residuo declarado, que es el precio de la cota:** una relación que apunta a una
fila más vieja que esa página se muestra como `(sin resolver) · <id8>` — visible
como PUESTA y visiblemente sin traducir, nunca confundible con la raya del NULL.
El listado no avisa que su traducción está acotada, al estilo del aviso que sí
tiene el formulario; la celda es la única señal.

**Qué pasa:** `/admin/content/{tipo}` arma sus columnas recorriendo `def.Fields`,
así que una relación declarada no aparece en la tabla. Se ve al abrir la fila,
donde el `<select>` de CONTRACT-30 la muestra ya seleccionada, pero no en el
listado: dos filas que apuntan a destinos distintos se ven idénticas.

**Por qué es BAJA:** el dato no se pierde ni se corrompe, y hay una vía en el
producto para verlo (abrir la fila). Es incompletitud de la vista, no un fallo.

**Qué haría falta:** una columna más por relación. La decisión real no es
técnica sino de presentación —mostrar el id no le sirve a nadie, así que habría
que resolver la MISMA etiqueta legible que ya calcula el selector
(`referenceOptionLabel`: primer campo declarado + 8 caracteres del id)—, y
hacerlo para todas las filas del listado son N lecturas más. Es el hueco 10 otra
vez, en otra pantalla: conviene resolver los dos juntos.

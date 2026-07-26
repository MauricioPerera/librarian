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
| 5 | [`pgvector` obligatorio aunque no se usen vectores](#5-pgvector-es-obligatorio-aunque-no-se-usen-vectores-media) | Abierto (MEDIA) |
| 6 | [La migración sin ventana de corte nunca se ejercitó](#6-la-migración-sin-ventana-de-corte-nunca-se-ejercitó-media) | Abierto (MEDIA) |

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

**Estado:** abierto, por diseño heredado.

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

**Estado:** abierto.

**Qué pasa:** la promesa del sistema es migrar a PostgreSQL "sin apagar la
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

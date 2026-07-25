# Huecos conocidos del producto

Cosas que faltan y ya están identificadas con evidencia real, para que no se
redescubran desde cero. No es un backlog de ideas: cada entrada acá se encontró
operando el sistema de verdad.

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

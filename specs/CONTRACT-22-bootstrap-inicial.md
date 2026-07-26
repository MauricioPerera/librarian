# Contrato 22 — Bootstrap: dejar administrable una instalación en limpio

Cierra el hueco 4 de `docs/PENDIENTES.md`, que resultó ser más grande que su título.

## El problema real, medido

El hueco estaba anotado como "no hay forma de crear la primera identidad desde el producto". Al
verificarlo contra una base limpia apareció que eso es la mitad:

```
usuario creado: d01f4eef-…
filas en role_permissions: 0
permisos del rol administrator: []
```

`EnsureSchema` crea las tablas y `SeedCatalogs` siembra los catálogos de roles y de permisos, pero
**nada los conecta**. Un usuario con rol `administrator` recién creado no tiene ningún permiso.

La consecuencia es un **bloqueo circular**: la UI que otorga permisos a un rol (CONTRACT-16, que
existe justamente para eso) está gateada por `roles.manage`, que el administrador no tiene. Y
ninguna otra escritura funciona: `content.*`, `users.manage`, `terms.manage`,
`content_types.manage`, todas gateadas y ninguna otorgada.

**Una instalación en limpio hoy no se puede administrar por ninguna vía del producto.** Hay que
escribir en la base a mano. Es el mismo incidente que documenta el hueco 1 —producción estuvo
semanas en modo solo-lectura efectiva sin que nadie lo notara— y el arreglo del hueco 1 no lo
resuelve porque es inalcanzable.

## Decisión YA TOMADA (no la reabras)

**Un bootstrap explícito que hace las dos cosas juntas**: crea la primera identidad **y** otorga
los permisos al rol administrador, en una sola operación, usable **una única vez**.

Se descartó sembrar los permisos del administrador dentro de `SeedCatalogs`: convertiría una
decisión de política —qué puede el administrador— en algo fijado por código y re-aplicado en cada
arranque, pisando cualquier cambio que un admin hiciera después por la UI.

## RECON ya resuelto (no re-investigar)

- `store.SeedCatalogs` siembra `roles` y `permissions` de forma idempotente y **no toca**
  `role_permissions`. No lo cambies.
- `auth.CreateUser(ctx, store, email, password, roleNames)` crea el usuario y le asigna roles, con
  bcrypt. `auth.SetRolePermissions(ctx, store, roleName, permissionNames)` reemplaza el conjunto de
  permisos de un rol de forma atómica (CONTRACT-16).
- `schema.Roles` y `schema.Permissions` son los catálogos; el rol de mayor privilegio es
  `administrator`.
- El binario ya tiene un modo que no levanta el servidor (`--dump-schema`), con su propio manejo de
  argumentos en `cmd/librarian/main.go`. Es el precedente de forma.
- La elección de motor (CONTRACT-21) es `LIBRARIAN_ENGINE` + `LIBRARIAN_DB`. El bootstrap tiene que
  funcionar contra **los dos motores**, usando esa misma resolución y no una propia.

## T1 — La operación

FIX/OBJETIVO: dejar el sistema administrable en una sola operación: crear la primera identidad,
asignarle el rol administrador, y otorgar a ese rol sus permisos.

**Atómica.** Si algo falla, no queda nada a medias — y en particular **nunca** un usuario con rol
pero sin permisos en el rol, que es exactamente el estado que causó el incidente histórico. Probalo
forzando un fallo, no lo afirmes.

**Usable una sola vez.** Definí qué condición lo impide y por qué es la correcta; el criterio es
que sea inequívoca y no dependa de que el operador recuerde nada. Una vía de creación de
administradores que sobrevive a su primer uso es un agujero de autenticación, no una comodidad.
Que dos ejecuciones concurrentes no puedan pasar las dos: la garantía tiene que ser de la base, no
del orden de las comprobaciones.

**Qué permisos otorga**: todos los del catálogo al rol `administrator`. Los otros roles
(`editor`, `author`, `contributor`) quedan sin permisos a propósito: no existe en ningún lado una
definición de qué debería poder cada uno, y **inventarla acá sería fijar política por código**. Una
vez hecho el bootstrap, el administrador se los otorga por la UI, que es el camino que CONTRACT-16
construyó y que recién ahora es alcanzable.

## T2 — La forma

FIX/OBJETIVO: cómo se invoca. Decidí entre un modo del binario o una ruta HTTP, y justificá.
Requisitos fijados:

- **La contraseña NO puede ser un argumento de línea de comandos.** Queda en el historial del shell
  y es visible en la lista de procesos de todo el sistema mientras corre. Elegí otra vía y
  documentá por qué.
- Usa la misma resolución de motor que el arranque normal. No dupliques esa lógica.
- El resultado dice **qué quedó hecho**: qué identidad se creó y que el rol quedó con sus permisos.
  Si falla porque ya se usó, el mensaje tiene que decir eso y no un error genérico.
- No debe agregar superficie al servicio en marcha si elegís el camino del binario.

## T3 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- **La prueba que cierra el hueco, contra AMBOS motores**: base en limpio → bootstrap → **entrar
  por HTTP y hacer una escritura gateada por permiso que hoy fallaría**. Es la demostración de que
  el bloqueo circular se rompió; sin eso el contrato no está cumplido.
- Segunda ejecución del bootstrap: rechazada, con mensaje claro, y **sin haber modificado nada**.
- Atomicidad: forzar un fallo a mitad y confirmar que no queda ni el usuario ni los permisos.
- Que una instalación **ya existente** no cambie: el bootstrap se niega y el sistema sigue igual.
- Confirmá que TODO lo de contratos anteriores sigue funcionando.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T1: operación atómica; probada forzando un fallo.
- [ ] T1: imposible de usar dos veces, con la garantía en la base y no en el orden de chequeos.
- [ ] T2: la contraseña no viaja por línea de comandos; decisión justificada.
- [ ] T3: **escritura gateada por permiso funcionando tras el bootstrap, en los dos motores**, con
  salida real.
- [ ] `SeedCatalogs` sin cambios de comportamiento.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO toques `sqlite-postgres-compat`.
- Sin dependencias nuevas. NINGÚN permiso nuevo.
- NO commitear.
- NO otorgues permisos a `editor`/`author`/`contributor`.
- El contrato público de las rutas HTTP existentes no cambia.

## Checklist antes de delegar

- [ ] RECON corrido: el bloqueo circular entendido, `SetRolePermissions` identificada como la pieza
  a reusar, y la resolución de motor de CONTRACT-21 como la que hay que compartir.
- [ ] Red-team: ¿qué pasa si hay usuarios pero `role_permissions` está vacía (una instalación
  anterior a este contrato, que es el estado de producción HOY)? ¿Y si existe el rol pero fue
  borrado del catálogo? ¿Un email inválido o repetido? ¿Una contraseña vacía? ¿Dos bootstraps
  concurrentes? ¿El bootstrap sobre una base a la que le falta el esquema — lo crea o falla?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador; password enmascarado.

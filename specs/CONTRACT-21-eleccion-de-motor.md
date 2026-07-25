# Contrato 21 — `internal/store` dual-motor y elección de motor (parte 3 de 3)

Prerrequisitos: `CONTRACT-19`, `20`, `20B` y `20C` completos y en `main`. Requiere
`sqlite-postgres-compat` v0.4.0. NO toca `sqlite-postgres-compat`.

Este es el contrato que **cierra la promesa**: al terminar, `librarian` debe poder **arrancar y
servir contra PostgreSQL**, no solo exportar a él.

## Premisa de alcance, ya decidida

**Instalación en limpio.** No hay datos heredados que migrar: el esquema lo crea la aplicación
desde cero en el motor elegido, y todo —tipos de contenido, relaciones, metadatos— queda acotado
por el sistema desde el primer arranque. **NO implementes ninguna ruta de migración de datos
existentes**, ni compatibilidad con filas escritas por versiones anteriores. Si algo solo tendría
sentido para una base preexistente, no va.

## Alcance

1. Las **19 sentencias crudas de `internal/store`**, que se quedan crudas por diseño (corren dentro
   de transacciones propias donde `CallRoutine`/`QueryRoutine` no se pueden usar) pero deben pasar a
   ser dual-motor.
2. Las **dos sondas a `sqlite_master`**.
3. La **elección de motor**.
4. La verificación de que la aplicación **funciona** contra PostgreSQL.

## RECON ya resuelto (no re-investigar)

- **`compat.Store.TableExists` (v0.4.0) existe exactamente para las dos sondas**
  (`contenttypes.go:65`, `contenttypes_edit.go:444`). Consulta el catálogo real de cada motor y
  **deliberadamente NO consulta `__compat_schema`** — que es justo la razón por la que esos dos
  lugares evitaban la metadata. Leé el comentario que ya está en `contenttypes.go:51`: explica el
  porqué y sigue siendo válido. Usala; no escribas una sonda propia.
- **`compat.Placeholder`** (expuesto por `internal/dual`) es el camino para las 19 sentencias.
  Ninguna puede quedar con un `?` literal.
- **Los puntos donde el motor está clavado hoy son dos**: `store.Open` (`internal/store/store.go`),
  que llama a `compat.OpenSQLite`, y `internal/server/server.go:48`, que compone el `Store` con
  `schema.SQLiteTarget`. `cmd/librarian/main.go` llama a `store.Open` en dos lugares (arranque y
  `--dump-schema`).
- **`schema.PostgresTarget` ya existe** y hoy se usa solo para probar exportabilidad.
- **`compat.OpenPostgres` ya existe.** No hay que escribir conexión nueva.
- **La columna `articles.embedding` es `vector(1536)`**: contra PostgreSQL, `ApplySchema` **falla**
  si falta la extensión `pgvector`. En una instalación en limpio eso ocurre en el primer arranque.
- `internal/dual` es hoja (no importa nada de librarian), así que `internal/store` puede usarlo sin
  ciclo. Ya se verificó al crearlo.

## T1 — `internal/store` dual-motor

FIX/OBJETIVO: las 19 sentencias y las dos sondas. Cero `?` literales en el paquete.

Cuidado especial con `CreateContentType` y `EditContentType`: son transacciones que mezclan DDL
compilado por `compat` con SQL propio y con la escritura de la metadata, y su atomicidad es una
garantía establecida (CONTRACT-13 y CONTRACT-18, con tests que la prueban forzando fallos). **Esa
atomicidad no se negocia**: si algo no se puede expresar dual-motor sin romperla, PARÁ y reportalo.

Ojo con el DDL transaccional: SQLite y PostgreSQL lo soportan, pero **no** se comportan igual ante
un error dentro de la transacción. Los tests de rollback existentes son el juez; tienen que pasar
en **ambos** motores.

## T2 — Elección de motor

FIX/OBJETIVO: que la instancia elija motor por configuración, y que `store.Open` y la composición
del `Store` dejen de estar clavadas a SQLite.

Decidí la forma (una variable nueva, o inferirlo del DSN) y justificala. Requisitos que sí están
fijados:

- **Inequívoca.** Ante una configuración ambigua o inválida, el arranque **falla con un mensaje
  que diga qué se esperaba**. Nunca caer a SQLite por defecto cuando la intención era PostgreSQL:
  ese fallo silencioso terminaría con la aplicación sirviendo desde una base vacía.
- **El `Target` que usa el `Store` y el motor de la conexión salen del MISMO lugar.** Un `Target`
  de un motor sobre una conexión de otro compila y emite el placeholder equivocado; hacelo
  imposible por construcción, no por convención.
- **`--dump-schema` sigue funcionando en ambos** (lee la base para incluir los tipos dinámicos).
- Si el motor es PostgreSQL y falta `pgvector`, el fallo debe ser **legible**: que diga que la
  extensión es requerida y por qué, no un error crudo del driver. Es el primer obstáculo real que
  se va a encontrar quien instale en limpio.

## T3 — La verificación que cierra la promesa

Todo lo anterior es medio contrato. Esto es el otro medio:

- **Arrancar el binario REAL contra PostgreSQL, en limpio**, y ejercitar la aplicación por HTTP:
  crear un usuario y autenticarse, otorgar permisos, crear un tipo de contenido dinámico, crear y
  editar contenido, listar, y borrar. Con salida real. **Si esto no está, el contrato no está
  cumplido**, por perfecto que sea el resto.
- El mismo guion contra SQLite, confirmando que **nada cambió** para el motor que corre hoy.
- Los tests de rollback de `CreateContentType`/`EditContentType` verdes contra **ambos** motores.
- Un ciclo de reinicio contra PostgreSQL: arrancar, crear un tipo dinámico, reiniciar, confirmar
  que arranca limpio y no intenta recrear nada. Dos arranques seguidos, no uno.
- `--dump-schema` contra ambos.
- Las baterías de `CONTRACT-19` y `CONTRACT-20` verdes.
- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T1: cero `?` literales en `internal/store`; sondas por `compat.TableExists`; atomicidad
  intacta, probada por los tests de rollback en ambos motores.
- [ ] T2: elección de motor inequívoca; `Target` y conexión de la misma fuente; fallo legible sin
  `pgvector`.
- [ ] T3: **la aplicación real sirviendo sobre PostgreSQL**, con la transcripción HTTP completa.
- [ ] El comportamiento sobre SQLite no cambia.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO toques `sqlite-postgres-compat`; si creés que le
  falta algo, PARÁ y reportalo.
- Sin dependencias nuevas. NINGÚN permiso nuevo.
- NO commitear.
- NO implementes migración de datos ni compatibilidad con bases preexistentes (ver premisa).
- El contrato público de las rutas HTTP no cambia.
- Respetá el guardián de CONTRACT-15 (`h.page(r, title)`).

## Checklist antes de delegar

- [ ] RECON corrido: `TableExists` identificado como la respuesta a las sondas, los dos puntos
  donde el motor está clavado, y la atomicidad de las dos operaciones de esquema entendida como
  intocable.
- [ ] Red-team: ¿el DDL transaccional revierte igual en los dos motores cuando falla a mitad?
  ¿`TableExists` responde bien para una tabla dinámica recién creada dentro de la misma
  transacción? ¿Qué pasa si el DSN de PostgreSQL es válido pero la base no existe? ¿Y si
  `pgvector` está pero en otro esquema? ¿El `search_path` afecta a `TableExists`? ¿Un tipo
  dinámico creado en SQLite y el mismo nombre creado en PostgreSQL producen el mismo esquema
  canónico (`--dump-schema` comparable)?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador; password enmascarado.

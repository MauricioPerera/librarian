# Contrato 20B — Colación en `auth` y helpers compartidos

Contrato **corto**, entre `CONTRACT-20` y `CONTRACT-21`. Cierra un defecto real dejado por
`CONTRACT-19` y consolida la duplicación que la serie viene acumulando. NO toca
`sqlite-postgres-compat`.

## El defecto

**PostgreSQL ordena `TEXT` por la colación de la base; SQLite lo ordena por bytes.** Con el MISMO
`ORDER BY` los resultados salen en orden distinto cuando los valores difieren solo en puntuación,
porque la colación no le da peso primario a `.` ni a `_`. Medido contra los dos motores reales:

```
SQLite  : content.update , content_types.manage
Postgres: content_types.manage , content.update
```

`CONTRACT-20` lo resolvió en `internal/server`. **`internal/auth` quedó sin corregir** y sus
listados son exactamente los que exponen el problema:

- `RolePermissions` ordena por `permission_name`. El catálogo real (`schema.Permissions`) contiene
  `content.update` y `content_types.manage`, y el rol `administrator` de producción tiene los ocho
  permisos: el par discrepante existe **hoy, en producción**.
- `ListUsers` ordena por `email`, `UserRoles` por `role_name`, `ListAPIKeys` por `created_at` y
  `label` — todos valores de texto libre donde la puntuación puede decidir el orden.

Impacto funcional hoy: bajo (las comprobaciones de permiso son de pertenencia, no de orden). Pero
es divergencia silenciosa, que es lo que este proyecto no acepta, y **se vuelve real en cuanto el
motor pueda ser PostgreSQL — o sea en `CONTRACT-21`**.

### Por qué no lo detectó la batería del 19 (y esto es lo que hay que arreglar de fondo)

La batería de `CONTRACT-19` **sí** compara el orden entre motores. Pasó igual, porque **sus
fixtures nunca produjeron un par discrepante**: nombres de usuario y de permiso que ordenan igual
en ambos motores. Una prueba de comparación cuyos datos no pueden producir una diferencia no prueba
nada, y da una confianza peor que no tener la prueba.

Por eso este contrato exige **primero** extender las fixtures para que contengan pares
discrepantes, **ver la batería FALLAR**, y recién entonces arreglar. Si arreglás primero no vas a
saber nunca si la prueba servía.

## La duplicación

`CONTRACT-19` creó `internal/auth/dual.go` y `CONTRACT-20` creó `internal/server/dual.go`. Hoy
comparten **seis funciones duplicadas**: `bind`, `newUUID`, `rowText`, `rowIsNull`, `textValue`,
`uuidValue`. El arreglo de colación duplicaría una séptima (el orden estable), y `CONTRACT-21` va a
necesitar las mismas en `internal/store`.

Dos copias que hoy son idénticas divergen en cuanto alguien arregle un caso de borde en una sola —
y son justo las funciones donde un caso de borde significa "un motor hace otra cosa".

## T1 — Un lugar para los helpers compartidos

FIX/OBJETIVO: un paquete interno con las funciones que hoy están duplicadas, y que `auth`, `server`
y (después) `store` usen esa única copia. `auth` no puede importar `server`, así que el lugar tiene
que ser neutral respecto de los tres.

Decidí dónde vive y justificá la elección. Criterio: que `CONTRACT-21` pueda usarlo desde
`internal/store` sin crear un ciclo de importación — verificá el grafo antes de elegir, no después.

Mové solo lo que está **realmente duplicado**. Lo que hoy es específico de un paquete se queda
donde está; consolidar de más es tan malo como no consolidar.

## T2 — El orden, en `auth`

FIX/OBJETIVO: que todo listado de `auth` devuelva el mismo orden en ambos motores, con la misma
estrategia que `CONTRACT-20` ya validó (leé `internal/server/dual.go` y su reporte antes de
inventar otra): imponer el orden en Go donde el listado no está paginado, y para lo paginado
ordenar por claves donde la comparación por bytes y la comparación por colación coinciden.

**El orden observable no cambia respecto de hoy en SQLite.** Producción corre en SQLite: si el
orden cambiara, sería un cambio de comportamiento visible en la UI y en la API, y este contrato no
lo autoriza. Verificalo.

## T3 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- **El orden exigido de trabajo, y hay que mostrarlo en el reporte**: (a) fixtures extendidas con
  pares discrepantes, (b) batería del 19 **FALLANDO**, con la salida real pegada, (c) arreglo,
  (d) batería verde. Sin la evidencia de (b) el contrato no está cumplido: sería otra prueba que
  pasa sin poder fallar.
- Los pares discrepantes tienen que incluir los del catálogo REAL (`content.update` vs
  `content_types.manage`), no solo casos inventados.
- Las baterías de `CONTRACT-19` y `CONTRACT-20` verdes contra los dos motores reales.
- Confirmá que ninguna respuesta HTTP ni de la UI cambia de orden en SQLite respecto de `main`.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T1: cero funciones duplicadas entre `auth`, `server` y el paquete nuevo; grafo de
  importación sin ciclos y utilizable desde `internal/store`.
- [ ] T2: todo listado de `auth` con orden idéntico entre motores.
- [ ] T3: la secuencia (a)–(d) documentada con salida real, **incluida la de la batería fallando**.
- [ ] El orden observable en SQLite no cambia respecto de `main`.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO toques `sqlite-postgres-compat`.
- Sin dependencias nuevas. NINGÚN permiso nuevo.
- NO commitear.
- NO cambies `store.Open` ni agregues elección de motor: es el `CONTRACT-21`.
- NO migres ninguna sentencia de `internal/store`: también es el 21.
- El contrato público de las rutas HTTP no cambia.

## Checklist antes de delegar

- [ ] RECON corrido: las seis funciones duplicadas identificadas, la estrategia de orden de
  `CONTRACT-20` leída, y el grafo de importación revisado antes de elegir el paquete.
- [ ] Red-team: ¿hay listados en `auth` sin `ORDER BY` explícito (el orden natural no es el mismo
  en los dos motores)? ¿Algún orden por una columna que puede empatar — qué desempata? ¿El
  `created_at` de `api_keys` tiene ancho fijo ahora que lo escribe la app? ¿Un email con `+` o con
  mayúsculas ordena igual? ¿Y un `label` de API key con espacios?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador; password enmascarado.

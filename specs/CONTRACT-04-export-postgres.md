# Contrato 04 — Prueba end-to-end del export a PostgreSQL

Prerrequisitos: `CONTRACT-01`, `CONTRACT-02`, `CONTRACT-03` completados y verificados
(`2bb8354`). Este contrato cierra la última capacidad pendiente de `DEFINITION.md`:
"Exportación bajo demanda a PostgreSQL vía `compat`, sin ventana de corte".

> Naturaleza distinta de los contratos anteriores: acá NO se escribe la capacidad de export
> (ya existe, en `sqlite-postgres-compat`, auditada 12 rondas hoy) — se PRUEBA que funciona de
> punta a punta para el esquema REAL de `librarian`, con datos reales generados por la app real
> (no inserts sintéticos que se salten la API), usando la herramienta operativa real (el CLI
> `compat`, no llamadas Go internas — es el camino que un operador usaría de verdad). El
> entregable incluye dejar el runbook documentado y repetible, no solo la corrida de hoy.

## RECON ya resuelto (no re-verificar)

- `compat copy` (leído `cmd/compat/copy.go` del módulo `sqlite-postgres-compat`) **exige** el
  esquema explícito en el JSON de config (`Schema` inline o `SchemaRef` a un archivo) — NO lo
  descubre de `__compat_schema` en la DB origen. `librarian` necesita volcar su `schema.Build()`
  (Go) a JSON — Go sigue siendo la ÚNICA fuente de verdad; el JSON se GENERA, nunca se
  mantiene a mano por separado (evita que las dos formas diverjan).
- El CLI se instala con `go install github.com/MauricioPerera/sqlite-postgres-compat/cmd/compat@v0.1.0`
  (queda un binario real `compat` en el PATH de Go — no usar `go run` para medir exit codes, ya
  se sabe por experiencia de hoy que en Windows `go run` colapsa exit≠0 a 1).
- PostgreSQL real YA PROVISIONADO por el orquestador: DSN en la variable de entorno
  `LIBRARIAN_EXPORT_PG_DSN` (password enmascarado en CUALQUIER salida que pegues en el reporte
  — SIEMPRE `***`, nunca literal). Se desmonta al cerrar el contrato — no depende de él para
  nada permanente.

## T1 — Volcado del esquema a JSON + round-trip

Sin esto, `compat copy` no tiene de dónde sacar el esquema.

FIX/OBJETIVO: un mecanismo en `librarian` (flag en el binario, ej. `librarian --dump-schema`, o
un comando separado — a tu criterio, pero debe vivir en el repo como código permanente, no un
script de un solo uso) que serializa `schema.Build()` a JSON (`json.Marshal`, `compat.Schema` ya
tiene tags json) y lo escribe a stdout o a un archivo. Prueba de round-trip: el JSON generado se
deserializa de vuelta a `compat.Schema`, se corre `Schema.Validate()` y `CompileDDL` para AMBOS
motores, y el resultado es IDÉNTICO (mismos statements) al de `schema.Build()` original sin pasar
por JSON — si algo se pierde en la serialización (un campo sin tag, un `Expression` con
`omitempty` mal puesto), se ve acá, no en el CLI real más adelante.

## T2 — Datos reales vía la app real

Sin datos reales, la "prueba end-to-end" sería una migración de tablas vacías — no prueba nada
que las auditorías de `compat` no probaran ya en abstracto.

FIX/OBJETIVO: un script o test que (a) levanta `librarian` de verdad (o llama directamente sus
funciones de arranque — `store.EnsureSchema` + el seed de CONTRACT-02 T1) sobre un archivo SQLite
real; (b) usa la API HTTP real (no inserts directos a la tabla) para: crear un usuario con rol
(`internal/auth.CreateUser` está bien si preferís no pasar por HTTP para esto, PERO el artículo
SÍ tiene que crearse vía `POST /articles` real, con JWT real, para probar que el dato que la app
realmente produce —incluyendo lo que compila `gen_random_uuid()`, los timestamps `CURRENT_TIMESTAMP`,
el `metadata` JSON— sobrevive el viaje completo). Al menos: 1 usuario, sus roles, 1 API key
minteada (`MintAPIKey`), y 2-3 artículos (al menos uno publicado vía `POST /articles/{id}/publish`,
al menos uno sin publicar) con `metadata` no vacío en alguno.

## T3 — Export real contra PostgreSQL, con verificación

FIX/OBJETIVO: usando el binario `compat` real (instalado en T-recon) y el JSON de T1, correr
`compat audit` (contrato con `RequiredFeatures` inferidas del schema) contra el par
SQLite-real-con-datos-de-T2 / PostgreSQL-real-del-DSN-dado — debe dar `exact` para todo. Luego
`compat copy` con un `migration.json` real (source_dsn = el archivo SQLite de T2, destination_dsn
= `LIBRARIAN_EXPORT_PG_DSN`, schema_ref = el JSON de T1) contra el PostgreSQL real — debe
terminar con `VerificationReport.equivalent = true`, exit 0. Verificación ADICIONAL más allá de
lo que `compat` ya certifica: una query SQL directa contra PostgreSQL (`SELECT count(*) FROM
articles`, y comparar al menos UN valor concreto —p.ej. el `title` de un artículo conocido— entre
SQLite y Postgres) para que la evidencia del reporte no dependa solo de confiar en el veredicto
de `compat`, sino en un chequeo independiente tuyo.

## T4 — Runbook documentado

Sin esto, la prueba de hoy es un evento único que nadie puede repetir sin releer este contrato.

FIX/OBJETIVO: `docs/OPERATIONS.md` (o extender uno si ya existe) con los pasos EXACTOS y
reproducibles para exportar una instancia real de `librarian` a PostgreSQL: instalar el CLI,
generar el JSON del esquema (`librarian --dump-schema`), armar el `migration.json`, correr
`compat audit` y `compat copy`, qué hacer si `audit` no da `exact` (no debería pasar hoy, pero
documentá el significado del fallo) y qué hacer si `copy` diverge (`ERR_VERIFY_DIVERGED` — según
la doctrina de `compat`, eso significa detenerse y investigar, nunca reintentar ciegamente).

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido dos veces.
- [ ] T1: round-trip JSON→Schema→CompileDDL idéntico al original, para ambos motores (test
  unitario, sin necesitar PG real).
- [ ] T2: datos reales creados vía HTTP real, confirmados con una query directa a SQLite antes de
  intentar el export (evidencia de que existen antes de exportar, no solo "debería haber pasado").
- [ ] T3: `compat audit` real → `exact` en todas las features. `compat copy` real → exit 0,
  `equivalent: true`, password del DSN SIEMPRE `***` en la salida pegada. Query directa a
  PostgreSQL confirmando al menos un valor de dato real coincide con el de origen.
- [ ] T4: `docs/OPERATIONS.md` existe con el runbook completo y los comandos son los MISMOS que
  usaste de verdad en T3 (no una versión idealizada no probada).
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO tocar `sqlite-postgres-compat` (si algo de la
  exportabilidad no funciona para el esquema real de `librarian`, es ABORTAR SI — no arregles
  `compat` desde acá).
- El password de `LIBRARIAN_EXPORT_PG_DSN` NUNCA en texto plano en ningún archivo del repo, ni
  en el reporte, ni en ningún commit — siempre `***` al pegarlo. La variable de entorno se usa en
  memoria, nunca se escribe a disco salvo en un archivo temporal que borrás vos mismo antes de
  terminar (si necesitás un `migration.json` con el DSN real para correr `compat copy`, generalo
  en un tempdir, usalo, BORRALO al final — no lo dejes en el repo).
- NO commitear (el orquestador commitea tras verificar).
- ABORTAR SI: el `compat audit` real da algo distinto de `exact` para el esquema de `librarian`
  contra PostgreSQL, o `compat copy` diverge (`ERR_VERIFY_DIVERGED`) → PARAR, documentar con la
  salida REAL del CLI qué falló, NO investigar dentro de `sqlite-postgres-compat` para
  "arreglarlo" (ese repo ya está auditado y estable; si algo falla acá es del lado de cómo
  `librarian` arma su schema/config, no de `compat`).

## Checklist antes de delegar

- [ ] RECON corrido: formato real de `compat copy` config confirmado (arriba), instalación del
  CLI confirmada (`go install ...@v0.1.0`), PostgreSQL real ya provisionado (DSN en env var).
- [ ] Todo criterio de aceptación tiene comando + resultado esperado.
- [ ] Red-team: ¿el `migration.json` temporal con el DSN real podría quedar olvidado en disco
  fuera del tempdir del sistema? → verificar explícitamente que se limpia. ¿El reporte podría
  pegar por accidente una salida de `compat copy` con el DSN sin enmascarar (viene en el mensaje
  de error de conexión, por ejemplo)? → revisar CADA bloque de salida pegado antes de escribir el
  reporte.
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Condiciones de aborto explícitas (arriba).

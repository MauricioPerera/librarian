# Contrato 01 — Núcleo: esquema canónico base + patrón de tipos de contenido + servidor mínimo

Prerrequisitos: `DEFINITION.md` cerrado. `sqlite-postgres-compat` con module path real y tagueado
(`github.com/MauricioPerera/sqlite-postgres-compat@v0.1.0`), verificado importable desde este repo.
Este es el primer contrato: establece la base de datos (usuarios/roles/permisos) y el patrón
reusable para declarar tipos de contenido, de la que dependen todos los contratos siguientes
(auth, CRUD de usuarios, API de tipos de contenido, vectores, export a Postgres).

> Capa: contrato de ejecución (nivel proyecto). No hay task contracts CCDD separados en este
> repo (decisión de DEFINICIÓN: solo la convención KDD, sin el tooling Python de gates) — el
> criterio de aceptación por tarea son comandos Go reales (`go build`/`go vet`/`go test`).

## T1 — Esquema canónico base: usuarios, roles, permisos

Hoy no existe ningún esquema. Se necesita el modelo de datos fundacional del que depende todo
lo demás: usuarios autenticables, un catálogo de roles y permisos predefinido en código (per
DEFINITION.md: "roles y permisos: catálogo predefinido en código, no editable en runtime en
v1"), y las tablas de relación M:N.

FIX/OBJETIVO: un paquete Go (`internal/schema` o similar) que construye un `compat.Schema`
con:
- `users`: `id` (uuid, PK, `DEFAULT gen_random_uuid()`), `email` (text, `UNIQUE`), `password_hash`
  (text), `status` (text, `CHECK IN ('active','suspended','invited')`), `created_at`/`updated_at`
  (timestamp, `DEFAULT CURRENT_TIMESTAMP`), `metadata` (json).
- `roles`: `id` (uuid, PK), `name` (text, `UNIQUE`) — catálogo fijo, sembrado en código (no vía
  API en v1): al menos `administrator`, `editor`, `author`, `contributor`.
- `permissions`: `id` (uuid, PK), `name` (text, `UNIQUE`) — catálogo fijo similar (ej.
  `content.create`, `content.publish`, `users.manage`).
- `role_permissions` (M:N `roles`↔`permissions`) y `user_roles` (M:N `users`↔`roles`), ambas con
  PK compuesta y `FOREIGN KEY ... ON DELETE CASCADE`.

Invariante que NO puede cambiar: el esquema completo debe validar (`Schema.Validate`) y compilar
DDL sin error para **ambos** motores (SQLite y PostgreSQL) — es la garantía de exportabilidad
futura que motiva todo el proyecto; si algo del modelo cae fuera de la gramática canónica de
`compat`, se replantea el modelo, no se fuerza.

## T2 — Patrón reusable de "tipo de contenido"

Per DEFINITION.md: tipos de contenido fijos en código, cada uno una tabla real + columna
`metadata` JSON de escape (equivalente a `wp_postmeta`). Hoy no hay ningún patrón para
declararlos sin repetir boilerplate.

FIX/OBJETIVO: un helper Go (`internal/schema.ContentType(name string, ownColumns []compat.Column) compat.Table`,
firma exacta a definir por el dev) que arma consistentemente: PK `id` (uuid), FK `author_id` →
`users(id)`, `created_at`/`updated_at`, columna `metadata` (json), más las columnas propias del
tipo. Se prueba con UN tipo de contenido real de ejemplo (`articles`: `title` text not null,
`body` text not null, `published_at` timestamp nullable) construido con el helper, integrado al
`Schema` de T1, y validado igual que T1 (ambos motores).

## T3 — Servidor HTTP mínimo

Hoy no hay servidor. Se necesita el esqueleto sobre el que cuelgan las rutas de los próximos
contratos (auth, CRUD).

FIX/OBJETIVO: un binario (`cmd/librarian/main.go`) que levanta un servidor HTTP (stdlib
`net/http` con `http.ServeMux` de rutas por método+patrón — sin framework/router externo, es
suficiente para esta base) con `GET /health` → `200 {"status":"ok"}`. Al arrancar, abre (o crea
si no existe) el archivo libSQL local, aplica el `Schema` de T1+T2 si las tablas no existen
(idempotente: no falla si ya están aplicadas), y solo entonces empieza a escuchar. Puerto
configurable por variable de entorno (`LIBRARIAN_ADDR`, default `:8080`).

## Criterios de aceptación

- [ ] `go build ./...` y `go vet ./...` limpios.
- [ ] `go test ./... -count=1` verde, corrido **dos veces** (mismo resultado ambas → sin flaky).
- [ ] Test unitario: el `Schema` completo (T1+T2) pasa `Schema.Validate()` sin error.
- [ ] Test unitario: `CompileDDL` del `Schema` completo no da error para `compat.SQLite` NI para
  `compat.Postgres` (aunque Postgres no se use en runtime en v1 — es la prueba de
  exportabilidad).
- [ ] Test de round-trip real: `ApplySchema` sobre un libSQL real (archivo temporal, no
  `:memory:` — más representativo del uso real del servidor) + `InspectSchema` posterior
  confirma `Exact == true` (el esquema aplicado se reconstruye canónico byte a byte).
- [ ] Test HTTP real (`httptest` o servidor real + client HTTP): `GET /health` → status 200,
  body `{"status":"ok"}`.
- [ ] Arranque idempotente probado: aplicar el schema, parar, volver a arrancar sobre el MISMO
  archivo libSQL → no falla (no intenta recrear tablas existentes).
- [ ] Final: suite completa 2× verde.

## Restricciones

- Tocar SOLO archivos dentro de este repo (`librarian`). NO tocar `sqlite-postgres-compat` (ya
  está resuelto y tagueado; si algo de su gramática no alcanza, es ABORTAR SI, no un fix ahí).
- Sin dependencias nuevas más allá de `github.com/MauricioPerera/sqlite-postgres-compat`
  (stdlib alcanza para el servidor HTTP en esta tarea — no agregar un router externo todavía).
- T1, T2 y T3 son secuenciales (T2 depende del `Schema` de T1; T3 depende de T1+T2) — no hay
  paralelismo real posible en este contrato, van en un único perímetro.
- NO commitear (el orquestador commitea tras verificar).
- ABORTAR SI: algún constructo del modelo de datos (T1 o T2) no compila DDL válido para ambos
  motores sin comprometer el modelo deseado → documentar con evidencia real (salida de
  `CompileDDL`) qué falló y por qué, proponer el ajuste mínimo de modelo que sí entra en la
  gramática, NO forzarlo silenciosamente.

## Checklist antes de delegar

- [ ] RECON corrido: `go get github.com/MauricioPerera/sqlite-postgres-compat@v0.1.0` verificado
  funcionando (hecho hoy — ver commit `24c7550` y release `v0.1.0` de compat).
- [ ] Todo criterio de aceptación tiene comando + resultado esperado, ninguno "por lectura".
- [ ] Red-team: ¿el round-trip de T3 podría dar falso verde si el archivo libSQL ya tenía las
  tablas de una corrida anterior del test? → el test crea su propio archivo temporal por
  corrida, nunca reusa uno preexistente fuera de la prueba explícita de idempotencia.
- [ ] Perímetro: un solo dev, un solo perímetro (T1→T2→T3 secuenciales, mismo repo).
- [ ] Condiciones de aborto explícitas (arriba, no genéricas).

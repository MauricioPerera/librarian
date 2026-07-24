# CONTRACT-09 — UI de API keys (alta, revocación, listado) — Reporte

Cuarto y último contrato de la fase 2 (UI). Cierra `DEFINITION-UI.md`. HEAD previo: `e3a129b`.
Árbol dejado SIN commitear (el orquestador commitea tras verificar).

## Resumen por tarea

### T1 — Datos base (`internal/auth/apikey.go`)

Se agregaron tres funciones nuevas al paquete `auth`, sin tocar la firma de `RevokeAPIKey` ni de
`MintAPIKey`/`VerifyAPIKey` existentes:

- **`RevokeAPIKeyByID(ctx, db, id string) error`** — cierra el GAP del RECON: `RevokeAPIKey`
  revoca buscando por el SECRETO en texto plano (lo hashea y compara `key_hash`), pero la UI
  NUNCA tiene el secreto (`MintAPIKey` lo devuelve una sola vez y no lo persiste). Esta función
  revoca por el `id` de la fila: `UPDATE api_keys SET revoked_at = CURRENT_TIMESTAMP WHERE id = ?
  AND revoked_at IS NULL`. Idéntico criterio de idempotencia que la existente: revocar una key ya
  revocada o un id inexistente afecta 0 filas y es un no-op exitoso (no error).
- **`ListAPIKeys(ctx, db) ([]APIKeyRecord, error)`** — nueva. Un solo `JOIN roles` resuelve el
  NOMBRE del rol (no el `role_id`), sin N+1. Ordena por `created_at DESC`. NUNCA selecciona
  `key_hash`. `revoked_at` se escanea como `sql.NullString` (SQLite devuelve `CURRENT_TIMESTAMP`
  como texto); no-null = revocada, expuesto como `Revoked bool` + `RevokedAt string`.
- **`GetAPIKey(ctx, db, id) (APIKeyRecord, bool, error)`** — nueva, auxiliar. Misma query con
  JOIN pero de una fila; `found=false` para id inexistente (nunca error SQL crudo). La usa el
  handler de revocación para re-renderizar la fila actualizada tras revocar.

`APIKeyRecord` (nuevo struct público) NO tiene campo `key_hash` ni secreto: por diseño ninguno
puede llegar a la UI ni siquiera por accidente. El scan de fila se comparte entre `ListAPIKeys` y
`GetAPIKey` vía `scanAPIKeyRecord` (interfaz `scanRow` mínima común a `*sql.Row`/`*sql.Rows`).

Tests unitarios propios (`internal/auth/apikey_contract09_test.go`), independientes de la UI:
`TestListAPIKeysResolvesRoleName` (verifica que devuelve el NOMBRE del rol, no el id, y el orden),
`TestRevokeAPIKeyByID` (revoca por id → la verificación del secreto pasa a rechazar → el listado
lo marca revocado sin borrarlo → re-revocar e id inexistente son no-op), `TestGetAPIKey`.

### T2 — Crear (secreto mostrado una única vez) (`internal/server/ui_apikeys.go`)

- `GET /admin/api-keys/new` (`requireSession`) — formulario: `label` (texto) + selector de rol
  poblado desde `schema.Roles` (catálogo fijo, no se agregó nada).
- `POST /admin/api-keys` (`requireSessionPermission("users.manage")`) — resuelve el nombre de rol
  a su id (`roleIDForName`; rol fuera de catálogo → 400 con el label preservado), llama a
  `auth.MintAPIKey` tal cual (no se reimplementa la cripto), y en vez de redirigir renderiza
  DIRECTAMENTE la página de éxito (`apikeys_created.html`) mostrando el secreto en texto plano una
  sola vez, con advertencia clara ("Guardá este secreto ahora. Es la única vez que se muestra...")
  y link al listado. Esa es la ÚNICA respuesta HTTP del contrato que contiene el `lbk_...`.

### T3 — Listar y revocar

- `GET /admin/api-keys` (`requireSession`) — tabla: label, rol (nombre), creada, estado
  (Activa/Revocada), acciones. NUNCA el secreto ni el hash.
- `POST /admin/api-keys/{id}/revoke` (`requireSessionPermission("users.manage")`) — llama
  `RevokeAPIKeyByID`, re-fetchea con `GetAPIKey` y devuelve el fragmento `<tr>` actualizado; htmx
  lo swapea `outerHTML` in-place: la fila pasa de Activa a Revocada y NO desaparece. Id realmente
  inexistente → 404 HTML (nunca 500 ni JSON).

### T4 — Verificación

Ver "Salida real por criterio" abajo.

## Decisiones de diseño (con el porqué)

1. **Verbo de revocar: `hx-post` (no `hx-delete`).** Revocar es una transición de estado que
   CONSERVA la fila como registro histórico — el análogo exacto de "publicar" un artículo en
   CONTRACT-07 (`hx-post` → `<tr>` actualizado swapeado in-place), NO de "borrar" (`hx-delete` →
   fila eliminada). Una API key revocada sigue siendo un registro válido; no se borra.

2. **Página de éxito de creación: render directo, sin redirect.** El secreto solo es recuperable
   en el instante de creación (`MintAPIKey` no lo persiste). Un redirect lo perdería. Se renderiza
   inline en `apikeys_created.html`: advertencia destacada + label + rol + el secreto en un
   `<pre><code>` (no en un atributo `value=` de input, para evitar cualquier eco). Es el único
   punto del contrato donde existe el plaintext en una respuesta.

3. **Fila de una key revocada: se reemplaza el botón por el texto "Revocada el <fecha>".** No hay
   nada que revocar en una key ya revocada, así que el botón desaparece (el estado se muestra
   también como badge en la columna Estado). La fila permanece en el listado.

4. **Permiso de gateo: `users.manage` existente (no se inventa `apikeys.manage`).** Decisión ya
   tomada en el RECON: el catálogo `schema.Permissions` es fijo y no se expande en este contrato.
   Las API keys son un recurso de control de acceso como los usuarios. No se tocó
   `schema.Permissions` ni `schema.Roles`.

5. **Selector de rol poblado desde `schema.Roles` (nombres), resuelto a id en el handler.** Mismo
   patrón que CONTRACT-08 (checkboxes de roles): el catálogo fijo es la fuente de lo ofrecido; un
   rol crafteado fuera de catálogo se rechaza con 400 sin mintear nada.

## Trade-offs

- `GetAPIKey` es una segunda query tras `RevokeAPIKeyByID` (revoca, luego re-fetchea para el
  fragmento). Se prefirió reusar la misma query con JOIN (fuente de verdad única para el nombre de
  rol y el estado) antes que construir el `<tr>` a mano en el handler. Corre en una acción
  administrativa puntual; el costo es irrelevante.
- El label no es único en el esquema (sin `UNIQUE`), y no se trata como identificador: todas las
  operaciones son por `id`. Los tests que buscan por label usan `ORDER BY created_at DESC` para
  quedarse con la más reciente.

## Salida real por criterio de aceptación

### `go build ./...` y `go vet ./...` limpios

```
BUILD_OK
VET_OK
```

### `go test ./... -count=1` verde, DOS veces

Corrida 1:
```
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	3.549s
ok  	github.com/MauricioPerera/librarian/internal/config	0.597s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.238s
ok  	github.com/MauricioPerera/librarian/internal/server	12.027s
ok  	github.com/MauricioPerera/librarian/internal/store	2.202s
```

Corrida 2:
```
?   	github.com/MauricioPerera/librarian/cmd/librarian	[no test files]
ok  	github.com/MauricioPerera/librarian/internal/auth	3.649s
ok  	github.com/MauricioPerera/librarian/internal/config	0.679s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.362s
ok  	github.com/MauricioPerera/librarian/internal/server	12.004s
ok  	github.com/MauricioPerera/librarian/internal/store	2.246s
```

### T1: `RevokeAPIKeyByID` y `ListAPIKeys` con tests unitarios propios (incl. JOIN del nombre del rol)

```
=== RUN   TestListAPIKeysResolvesRoleName
--- PASS: TestListAPIKeysResolvesRoleName (0.05s)
=== RUN   TestRevokeAPIKeyByID
--- PASS: TestRevokeAPIKeyByID (0.04s)
=== RUN   TestGetAPIKey
--- PASS: TestGetAPIKey (0.05s)
=== RUN   TestMintAndVerifyAPIKey
--- PASS: TestMintAndVerifyAPIKey (0.04s)
=== RUN   TestRevokedAPIKeyRejected
--- PASS: TestRevokedAPIKeyRejected (0.04s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/auth	2.639s
```

### T2/T3/T4: UI de alta, listado, revocación, round-trip y red-team

```
=== RUN   TestAdminAPIKeyCreateShowsSecretOnce
--- PASS: TestAdminAPIKeyCreateShowsSecretOnce (0.15s)
=== RUN   TestAdminAPIKeyCreateUnknownRoleRejected
--- PASS: TestAdminAPIKeyCreateUnknownRoleRejected (0.14s)
=== RUN   TestAdminAPIKeyRoundTrip
--- PASS: TestAdminAPIKeyRoundTrip (0.15s)
=== RUN   TestAdminAPIKeyRevokeIdempotentAndMissing
--- PASS: TestAdminAPIKeyRevokeIdempotentAndMissing (0.15s)
=== RUN   TestAdminAPIKeysWriteWithoutPermission
--- PASS: TestAdminAPIKeysWriteWithoutPermission (0.24s)
=== RUN   TestAdminAPIKeysNoSessionRedirects
--- PASS: TestAdminAPIKeysNoSessionRedirects (0.04s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/server	4.568s
```

- **Round-trip (T4)**: `TestAdminAPIKeyRoundTrip` crea la key vía UI, captura el `lbk_...` de la
  página de éxito, lo usa como `Authorization: Bearer` contra `GET /whoami` real → 200 con
  `"auth":"apikey"` y el label; revoca vía UI (fragmento in-place con "Revocada") → repite el
  mismo `/whoami` → 401; confirma que la key revocada sigue en el listado marcada Revocada.
- **Gateo por permiso (red-team)**: `TestAdminAPIKeysWriteWithoutPermission` — sesión `author` sin
  `users.manage` haciendo `POST /admin/api-keys` y `POST /admin/api-keys/{id}/revoke` directo con
  la cookie → 403 HTML ("Sin permiso", no JSON), sin mintear ni revocar nada server-side.
- **Ausencia de secreto/hash en el HTML del listado**: verificada explícitamente en
  `TestAdminAPIKeyCreateShowsSecretOnce` — el body del listado contiene label+rol pero NO contiene
  `lbk_`, NI el secreto exacto, NI el `key_hash` leído de la DB. También se confirma que el
  new-form no contiene `lbk_`.

### Auditoría de fuga de secreto (grep manual, además de los tests)

```
=== lbk_ en fuentes no-test (solo debe estar el const keyPrefix) ===
internal/auth/apikey.go:16:const keyPrefix = "lbk_"
=== .Secret en templates (solo apikeys_created.html) ===
internal/server/templates/apikeys_created.html
=== key_hash en templates (ninguno) ===
NONE
```

### Rutas de contratos anteriores sin cambios

```
--- PASS: TestUIRoundTrip (0.13s)
--- PASS: TestUIJSONRoutesUnaffected (0.13s)
--- PASS: TestAdminUserCreateAppearsInListAndDetail (0.19s)
--- PASS: TestAdminUserCreateUnknownRoleRejected (0.18s)
--- PASS: TestAdminUserStatusChange (0.18s)
--- PASS: TestAdminUserRolesChange (0.18s)
--- PASS: TestAdminUserDetailMissingIs404 (0.12s)
--- PASS: TestAdminRolesViewReflectsRealGrants (0.13s)
--- PASS: TestAdminUserRoundTripLoginRejection (0.35s)
--- PASS: TestAdminUsersWriteWithoutPermissionServerSide (0.17s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/server	5.444s
```

`TestUIJSONRoutesUnaffected` cubre explícitamente el JSON `POST /auth/login` + `GET /whoami` con
JWT por header. Toda la suite completa quedó verde 2× (arriba).

## Archivos tocados

- `internal/auth/apikey.go` — +`RevokeAPIKeyByID`, `ListAPIKeys`, `GetAPIKey`, `APIKeyRecord`,
  `scanAPIKeyRecord` (firmas existentes intactas).
- `internal/auth/apikey_contract09_test.go` — NUEVO (tests unitarios T1).
- `internal/server/ui_apikeys.go` — NUEVO (handlers + rutas `/admin/api-keys`).
- `internal/server/server_ui_apikeys_test.go` — NUEVO (tests de aceptación T2-T4 + red-team).
- `internal/server/ui.go` — `//go:embed` de los 4 templates nuevos + `registerAdminAPIKeyRoutes`.
- `internal/server/templates/apikeys_{list,row,new,created}.html` — NUEVOS.
- `internal/server/templates/home.html` — link "API keys" en el home (descubribilidad).

## Restricciones respetadas

- Solo archivos dentro de `librarian`. `sqlite-postgres-compat` intacto.
- Sin dependencias Go nuevas. Sin commits. Sin permisos nuevos en el catálogo; `schema.Permissions`
  y `schema.Roles` sin tocar. `RevokeAPIKey`/`MintAPIKey`/`VerifyAPIKey` con firmas intactas.
- El secreto en texto plano solo aparece en `apikeys_created.html` (verificado por grep + tests).

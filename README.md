# librarian

Backend de administración de contenido en Go: API JSON + panel de administración, sobre un esquema
declarativo que **corre igual sobre SQLite/libSQL embebido y sobre PostgreSQL**.

La idea que lo ordena todo: **arrancar sin costo operativo** —un archivo, sin servidor de base de
datos que administrar— y **poder mudarse a PostgreSQL cuando la escala lo exija, sin reescribir la
aplicación**. Eso no es una aspiración del README: es una propiedad verificada de punta a punta
(ver [Dual-motor](#dual-motor)).

Está construido sobre [`sqlite-postgres-compat`](https://github.com/MauricioPerera/sqlite-postgres-compat),
que declara el esquema una sola vez y lo compila a los dos motores, negándose a degradar en
silencio lo que no puede garantizar idéntico.

## Qué hace

- **Contenido**: tipos definidos por código (`articles`, `products`) y **tipos dinámicos creados
  desde la UI**, sin recompilar ni desplegar. Sus campos se pueden editar después: agregar,
  renombrar preservando los datos, y quitar con confirmación explícita.
- **Taxonomías y términos**, con relación por tipo de contenido.
- **Identidad y permisos**: usuarios, cuatro roles (`administrator`, `editor`, `author`,
  `contributor`) y ocho permisos (`content.create/update/publish/delete`, `users.manage`,
  `roles.manage`, `terms.manage`, `content_types.manage`), asignables desde el producto.
- **Dos vías de autenticación**: sesión con cookie para el panel, y `Authorization: Bearer` con
  JWT o API key para la API.
- **Vectores**: `articles.embedding` es `vector(1536)` — se almacena lo que el cliente ya calculó;
  no hay pipeline de embeddings.

## Dual-motor

| | |
|---|---|
| Motor por defecto | SQLite/libSQL embebido (un archivo) |
| Motor alternativo | PostgreSQL 17 (requiere la extensión `pgvector`) |
| Cómo se elige | `LIBRARIAN_ENGINE` contrastado con la forma de `LIBRARIAN_DB` |

```bash
# SQLite (por defecto)
LIBRARIAN_JWT_SECRET=... LIBRARIAN_DB=librarian.db ./librarian

# PostgreSQL
LIBRARIAN_ENGINE=postgres \
LIBRARIAN_DB='postgres://usuario:password@host:5432/librarian?sslmode=disable' \
LIBRARIAN_JWT_SECRET=... ./librarian
```

Las dos variables **se contrastan entre sí** y el binario se niega a arrancar si se contradicen.
La guarda existe por un fallo concreto: un DSN de PostgreSQL con el motor en `sqlite` haría que
SQLite creara un archivo local vacío y el servicio quedara sirviendo desde ahí — respondiendo sano
y sin datos.

El esquema lo crea la aplicación en el primer arranque; no hay script de creación que correr.

### Cómo se verifica que "corre igual" no es una frase

Hay baterías que ejecutan el mismo guion contra **SQLite real y PostgreSQL real** y comparan los
resultados línea por línea — a nivel de las funciones de dominio y a nivel del mux HTTP completo.
No comparan que ambos "funcionen": comparan que devuelvan **lo mismo**.

```bash
COMPAT_POSTGRES_DSN='postgres://...' go test -tags dualengine -count=1 ./internal/...
```

Encontraron divergencias reales que ningún compilador señala: la colación (PostgreSQL ordena texto
por la colación de la base y SQLite por bytes), los formatos de `CURRENT_TIMESTAMP`, y la
clasificación de errores por el texto del mensaje. Están documentadas en los reportes de contrato.

## Empezar

```bash
go build ./cmd/librarian
LIBRARIAN_JWT_SECRET=$(openssl rand -hex 32) ./librarian
```

El servicio escucha en `:8080` por defecto (`LIBRARIAN_ADDR`) y la comprobación de salud está en
`/health`. El panel se entra por `/login`; una vez con sesión, `/` es el inicio y las vistas de
administración cuelgan de `/admin/...`.

**Una instalación en limpio necesita un paso de bootstrap antes de poder administrarse.** El
arranque crea las tablas y siembra los catálogos, pero no conecta roles con permisos, así que sin
este paso ni siquiera un usuario con rol `administrator` puede escribir nada:

```bash
# La contraseña se lee de la ENTRADA ESTÁNDAR, nunca como argumento
./librarian --bootstrap --email admin@example.com < /run/secrets/admin-password
```

Crea la primera identidad y le otorga al rol `administrator` todos los permisos del catálogo, en una
sola operación atómica, usable **una única vez**. Detalle en
[`docs/DEPLOY.md`](docs/DEPLOY.md#bootstrap-inicial-contract-22).

`./librarian --dump-schema --db <ruta>` emite el esquema canónico en JSON, incluidas las tablas de
los tipos dinámicos. Es lo que consume la exportación a PostgreSQL.

## Documentación

| Documento | Para qué |
|---|---|
| [`DEFINITION.md`](DEFINITION.md) | Qué es, por qué, y qué queda fuera de alcance |
| [`DEFINITION-UI.md`](DEFINITION-UI.md) | La fase de panel de administración |
| [`DEFINITION-CPT-DINAMICOS.md`](DEFINITION-CPT-DINAMICOS.md) | La fase de tipos de contenido dinámicos |
| [`docs/DEPLOY.md`](docs/DEPLOY.md) | Runbook de despliegue, para los dos motores |
| [`docs/OPERATIONS.md`](docs/OPERATIONS.md) | Exportar una instancia a PostgreSQL con el CLI `compat` |
| [`docs/PENDIENTES.md`](docs/PENDIENTES.md) | Huecos conocidos, con la evidencia de cómo se encontraron |
| `specs/` y `docs/reports/` | Un contrato y un reporte por unidad de trabajo |

Los runbooks son operativos: sus comandos se ejecutaron de verdad antes de escribirse, y cada paso
de verificación existe porque su ausencia causó un problema concreto.

## Desarrollo

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1
```

Las suites que necesitan un PostgreSQL vivo están detrás de build tags (`dualengine`,
`exportfixture`) y **saltean** si les falta su DSN, en vez de pasar en falso.

```bash
# Compara SQLite real contra PostgreSQL real, resultado contra resultado
COMPAT_POSTGRES_DSN='postgres://...' go test -tags dualengine -count=1 ./...
```

Cada corrida crea **su propia base de datos** y la borra al terminar, así que no le importa qué
haya en el servidor y dos corridas en paralelo no se pisan. El usuario del DSN necesita permiso
para `CREATE DATABASE`.

### El PostgreSQL sin `pgvector`

Hay un caso que **solo** se puede probar contra un PostgreSQL que NO tenga la extensión: que una
instalación con `LIBRARIAN_VECTOR=disabled` arranque y funcione ahí, que es la razón de ser de esa
opción. Ese servidor no se puede simular —la extensión está o no está— así que va en su propia
variable:

```bash
# Un segundo servidor, deliberadamente sin la extensión
docker run -d --name pg-novector -e POSTGRES_PASSWORD=... -p 5458:5432 postgres:17-alpine

LIBRARIAN_PG_NO_VECTOR_DSN='postgres://...:5458/postgres?sslmode=disable' \
COMPAT_POSTGRES_DSN='postgres://...' \
  go test -tags dualengine -count=1 ./...
```

**Sin esa variable el caso saltea**, y saltear se lee como verde en la salida resumida. Si vas a
tocar algo que roce la capacidad vectorial opcional, levantá el segundo servidor: es la diferencia
entre haberlo probado y creer que lo probaste.

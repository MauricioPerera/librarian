# DEPLOY — Desplegar `librarian` en el VPS

Runbook operativo real. Los comandos son los que se ejecutan de verdad, no una versión
idealizada: cada paso de verificación de acá existe porque su ausencia causó un problema concreto
(las referencias a incidentes son reales y están en `docs/PENDIENTES.md` y en los reportes de
contrato).

## Instancia

| Qué | Dónde |
|---|---|
| Servicio | `librarian.service` (systemd), escucha en `127.0.0.1:8500` |
| Binario | `/opt/librarian/librarian` |
| Motor | `LIBRARIAN_ENGINE` en el `.env`: `sqlite` (por defecto) o `postgres` |
| Base de datos | `LIBRARIAN_DB`: ruta de archivo si el motor es SQLite, DSN si es PostgreSQL |
| Configuración | `/opt/librarian/.env` (incluye `LIBRARIAN_JWT_SECRET`, modo `0600`) |
| Público | `https://librarian.ardf.dev` (nginx → `127.0.0.1:8500`) |

**Antes de tocar nada, mirá contra qué motor corre la instancia.** El grueso de este runbook es el
mismo para los dos; lo que cambia está en
[Si la instancia corre sobre PostgreSQL](#si-la-instancia-corre-sobre-postgresql), y es sobre todo
el respaldo, que es de lo que depende el rollback.

```bash
grep -E '^LIBRARIAN_(ENGINE|DB)=' /opt/librarian/.env
```

## Elegí el procedimiento correcto

| Situación | Procedimiento |
|---|---|
| El cambio NO toca el esquema | [A. Deploy simple](#a-deploy-simple) |
| El cambio AGREGA tablas o columnas nuevas | [B. Deploy con cambio de esquema](#b-deploy-con-cambio-de-esquema) |
| El cambio requiere tocar tablas existentes a mano (renombrar, mover datos) | [C. Deploy con migración manual](#c-deploy-con-migración-manual) |

Ante la duda, usá el procedimiento MÁS estricto de los que apliquen. El costo de B sobre A son
unos minutos; el costo de equivocarse es producción caída.

---

## Si la instancia corre sobre PostgreSQL

Los procedimientos A, B y C valen igual. Lo que cambia es **el respaldo, el destino de los datos y
el rollback** — o sea, precisamente aquello de lo que depende poder deshacer un error.

### Configuración

```
LIBRARIAN_ENGINE=postgres
LIBRARIAN_DB=postgres://usuario:password@host:5432/librarian?sslmode=disable
```

**Las dos van juntas y se contrastan entre sí.** El binario se niega a arrancar si se contradicen,
con un mensaje que dice qué esperaba. El caso que esa guarda existe para impedir es un DSN de
PostgreSQL con el motor en `sqlite`: sin ella, SQLite **crearía un archivo local vacío** y el
servicio quedaría sirviendo desde ahí, respondiendo sano y sin datos.

El DSN lleva la contraseña, así que vive en el `.env` con modo `0600` y **nunca** se pega en un
reporte sin enmascarar. El servicio la enmascara solo en su log de arranque.

### `pgvector` es un prerrequisito duro

`articles.embedding` es `vector(1536)`, que en PostgreSQL requiere la extensión `pgvector`. Sin
ella, la creación del esquema falla en el primer arranque. Instalala en la base **antes** de
arrancar el servicio por primera vez:

```bash
psql "$LIBRARIAN_DB" -c "CREATE EXTENSION IF NOT EXISTS vector;"
psql "$LIBRARIAN_DB" -tAc "select extversion from pg_extension where extname='vector'"
```

Si el PostgreSQL es administrado y no ofrece `pgvector`, **la instancia no se puede levantar** tal
cual. Averigualo antes de planificar el despliegue, no el día del corte.

### Instalación en limpio

El esquema lo crea la aplicación en el primer arranque: no hay script de creación que correr ni
migración desde otra base. Alcanza con una base vacía y la extensión instalada.

La primera identidad la crea el binario, igual que en SQLite. Ver **Bootstrap inicial** abajo.

### Respaldo: el paso del que depende el rollback

Donde el procedimiento dice "copiar el archivo de base de datos", acá va un volcado:

```bash
# En formato custom (-Fc): comprimido y restaurable con pg_restore
PGPASSWORD='...' pg_dump -U <usuario> -h <host> -d <base> -Fc \
  -f /opt/librarian/backups/librarian-pre-<tag>-$(date +%Y%m%d%H%M%S).dump
ls -la /opt/librarian/backups/
```

**El respaldo se toma con el servicio ARRIBA**, como en el resto del runbook: `pg_dump` es
consistente y no requiere detener nada.

### Probar contra una copia real (procedimiento B)

El equivalente de "bajar el archivo y correr el binario contra la copia" es restaurar el volcado en
una base aparte y apuntar ahí:

```bash
psql "<dsn-admin>" -c "CREATE DATABASE librarian_ensayo;"
psql "<dsn-ensayo>" -c "CREATE EXTENSION IF NOT EXISTS vector;"
PGPASSWORD='...' pg_restore -U <usuario> -h <host> -d librarian_ensayo <volcado>.dump

LIBRARIAN_ENGINE=postgres LIBRARIAN_DB="<dsn-ensayo>" \
  LIBRARIAN_JWT_SECRET=ensayo LIBRARIAN_ADDR=127.0.0.1:8099 ./librarian
```

**Dos arranques**, igual que en SQLite, y por la misma razón: hubo un bug real que solo aparecía en
el reinicio siguiente. Borrá la base de ensayo al terminar.

### La caché de metadata también aplica acá

Todo lo de [La invalidación de metadata NO es opcional](#la-invalidación-de-metadata-no-es-opcional)
vale igual: `__compat_schema` es una tabla de la propia base, no un detalle de SQLite, y la función
de inspección la prefiere por sobre el catálogo físico en los dos motores.

Una diferencia a favor: PostgreSQL **sí** tiene `ALTER TABLE ... RENAME`, así que una migración
manual es más directa que en SQLite. Eso no cambia nada de la regla — si tocás el catálogo a mano,
la invalidación va igual.

---

## Bootstrap inicial (CONTRACT-22)

Una instalación en limpio **nace inadministrable** si no se hace este paso: el arranque crea las
tablas y siembra los catálogos de roles y permisos, pero `role_permissions` queda VACÍA, así que un
usuario con rol `administrator` no tiene ningún permiso y todas las escrituras responden `403`
—incluida la UI que otorga permisos, que está gateada por `roles.manage`—. Es el incidente del
hueco 1 de `docs/PENDIENTES.md`: producción estuvo semanas en modo solo-lectura efectiva sin que
nadie lo notara, porque todas las verificaciones eran lecturas.

El bootstrap crea la primera identidad **y** le otorga al rol `administrator` todos los permisos del
catálogo, en una sola transacción. Se corre **una vez, con el servicio abajo**, contra la misma base
que va a servir (usa la misma resolución de motor: `LIBRARIAN_ENGINE` + `LIBRARIAN_DB`). No necesita
`LIBRARIAN_JWT_SECRET`: no sirve tráfico.

```bash
# La contraseña va por ENTRADA ESTÁNDAR, nunca como argumento: un argumento queda
# en el historial del shell y es visible en la lista de procesos de toda la máquina.
librarian --bootstrap --email admin@example.com < /run/secrets/admin-password

# Alternativas equivalentes:
pass show librarian/admin | librarian --bootstrap --email admin@example.com
```

Imprime qué quedó hecho: la identidad creada y los permisos que quedaron en el rol. Después de eso
el panel se entra por `/login` y la gestión de usuarios, roles y permisos es por la aplicación.

Detalles que importan operativamente:

- **Se puede usar una sola vez.** La garantía es de la base (una única clave representable en la
  tabla `bootstrap`), no del orden de las comprobaciones: dos ejecuciones simultáneas no pasan las
  dos. Un segundo intento sale con código distinto de cero y un mensaje que dice quién reclamó la
  instalación y cuándo.
- **Un intento fallido no consume el uso**: la transacción se revierte entera y se puede repetir.
- **Sobre una instalación que ya tiene usuarios, se niega y no cambia nada.** Es deliberado: acuñar
  administradores sobre un sistema que ya tiene identidades sería un agujero de autenticación. Para
  ese caso el camino es la UI de roles (`/admin/roles/{rol}/edit`).
- `editor`, `author` y `contributor` quedan **sin permisos a propósito**; se los otorga el
  administrador desde la UI.

## A. Deploy simple

```bash
# 1. Compilar (con el servicio ARRIBA — no hace falta bajarlo para esto)
cd D:/Repo/librarian
GOOS=linux GOARCH=amd64 go build -o /tmp/librarian-linux-amd64 ./cmd/librarian
```

**Antes de subir, preguntale al sistema a dónde va** en vez de escribir la ruta de memoria. El
`ExecStart` del service es la única fuente de verdad de dónde vive el binario, y es un comando:

```bash
# En el VPS. Imprime la ruta REAL del binario que systemd ejecuta.
DEST=$(systemctl show -p ExecStart --value librarian.service | sed -E 's/.*path=([^ ;]+).*/\1/')
echo "$DEST"   # /opt/librarian/librarian
```

Esto no es ceremonia: un deploy de este proyecto ya se subió a `/root/librarian` de memoria. El
servicio reinició con el binario VIEJO, `/health` y `/ready` dieron 200 **porque el binario viejo
estaba sano**, y el despliegue se dio por bueno sin haber desplegado nada.

Anotá también el tamaño de lo que compilaste, que es lo que vas a contrastar en el paso siguiente:

```bash
# En tu máquina, después de compilar.
stat -c '%s' /tmp/librarian-linux-amd64
```

Subir por SFTP a `$DEST.new`, y después:

```bash
cp -a "$DEST" "$DEST.bak-$(date +%Y%m%d-%H%M%S)"   # el rollback del binario, gratis
mv "$DEST.new" "$DEST"                              # reemplazo atómico
chmod +x "$DEST"
systemctl restart librarian.service
systemctl is-active librarian.service               # debe decir "active"
```

Después, la [verificación post-deploy](#verificación-post-deploy), que **empieza por comprobar que
lo que está corriendo es lo que subiste** y no es solo lecturas.

---

## B. Deploy con cambio de esquema

Aplica cuando el binario nuevo agrega tablas (un tipo de contenido de código, tablas de una
capacidad nueva). `EnsureSchema` las crea solo al arrancar, de forma incremental, y reescribe la
metadata de esquema por su cuenta — no hay paso manual. Lo que cambia respecto de A es que
**se prueba contra una copia real de producción antes de tocar el servicio**.

```bash
# 1. Compilar primero (servicio arriba)
cd D:/Repo/librarian
GOOS=linux GOARCH=amd64 go build -o /tmp/librarian-linux-amd64 ./cmd/librarian
```

Descargar por SFTP `/opt/librarian/data/librarian.db` a una copia local, y correr el binario
nuevo contra ESA COPIA — **dos veces**. (Sobre PostgreSQL la copia se hace restaurando un volcado
en una base aparte: ver [Probar contra una copia real](#probar-contra-una-copia-real-procedimiento-b).)

```bash
# Primer arranque: ejercita el camino incremental (crea lo que falta)
LIBRARIAN_DB=/tmp/prod-copy.db LIBRARIAN_JWT_SECRET=test LIBRARIAN_ADDR=:8099 ./librarian
# Segundo arranque sobre la MISMA copia: ejercita el camino "ya todo aplicado"
```

Ambos tienen que arrancar limpio. El segundo importa tanto como el primero: hubo un bug real
(CONTRACT-11) que solo se manifestaba en el reinicio siguiente, no en el primer arranque.
Confirmá además que los datos preexistentes siguen ahí (logueá con un usuario real, listá
contenido existente).

Recién entonces: backup, subir, reiniciar. El backup es lo que hace posible el rollback, así que
usá el que corresponde al motor — sobre PostgreSQL, el `pg_dump` de
[Respaldo](#respaldo-el-paso-del-que-depende-el-rollback).

```bash
cp /opt/librarian/data/librarian.db /opt/librarian/data/librarian.db.bak-pre-<tag>-$(date +%Y%m%d%H%M%S)
mv /opt/librarian/librarian.new /opt/librarian/librarian
systemctl restart librarian.service
```

---

## C. Deploy con migración manual

Aplica cuando hay que tocar tablas existentes a mano (renombrar, mover datos) porque
`sqlite-postgres-compat` no expresa esa operación. **Este es el procedimiento que causó una caída
real**; seguilo en orden.

### El orden importa

Todo lo que se pueda hacer con el servicio ARRIBA va antes de bajarlo. La ventana de caída debe
contener solo lo que estrictamente la requiere.

```
1. Compilar                    ← servicio ARRIBA
2. Probar contra copia de prod ← servicio ARRIBA (igual que B)
3. Subir el binario a .new     ← servicio ARRIBA
4. Backup                      ← servicio ARRIBA
──────── acá empieza la caída ────────
5. Detener el servicio
6. Migrar (incluida la invalidación de metadata, ver abajo)
7. Intercambiar el binario (mv)
8. Arrancar
──────── acá termina ────────
9. Verificar
```

En el incidente real se compiló en el paso 6 (con el servicio ya caído), y compilar resultó ser
lo más lento de todo — minutos de indisponibilidad puramente evitables.

### La invalidación de metadata NO es opcional

`compat.InspectSchema` **prefiere la metadata guardada en la tabla `__compat_schema` por sobre el
catálogo físico de la base**. Si una migración manual cambia el catálogo (renombrar, borrar o
crear una tabla a mano) y deja esa metadata sin tocar, el arranque siguiente razona sobre un
esquema que ya no es cierto, e intenta crear tablas que existen → **el servicio no arranca**.

Esta caché causó tres incidentes en este proyecto. Si tu migración toca el catálogo físico, el
paso de invalidación va SIEMPRE, en la misma transacción mental que el cambio:

```sql
-- después de la migración, antes de arrancar
DELETE FROM __compat_schema WHERE key = 'canonical_schema';
```

Borrar la fila es lo más seguro: obliga a `InspectSchema` a re-derivar la verdad del catálogo
físico. `EnsureSchema` la regenera completa en el próximo cambio de esquema. **No intentes
reescribirla a mano** — serializar el esquema canónico por fuera de Go es exactamente la clase de
segunda fuente de verdad que el proyecto evita.

### Ejemplo real (CONTRACT-17)

```bash
systemctl stop librarian.service
```
```python
import sqlite3
c = sqlite3.connect('/opt/librarian/data/librarian.db')
c.execute('ALTER TABLE eventos RENAME TO cpt_eventos')
c.execute("DELETE FROM __compat_schema WHERE key='canonical_schema'")   # ← el paso que se olvidó
c.commit()
print('filas preservadas:', c.execute('SELECT count(*) FROM cpt_eventos').fetchone()[0])
```
```bash
mv /opt/librarian/librarian.new /opt/librarian/librarian
systemctl start librarian.service
```

---

## Verificación post-deploy

**Una batería de solo lecturas no prueba que el sistema funciona.** Producción estuvo semanas en
modo solo-lectura efectiva (la tabla de permisos vacía) sin que ninguna verificación lo
detectara, porque todas eran `GET` y las lecturas no requieren permiso.

### 1. Lo que está corriendo es lo que subiste

**Este paso va PRIMERO, antes de mirar la salud.** Un servicio sano sirviendo el binario viejo se
ve exactamente igual que uno sirviendo el nuevo: los dos contestan `200` en `/health` y en
`/ready`. Una vez que viste el verde ya no vas a buscar, así que la pregunta "¿desplegué?" hay que
hacerla cuando todavía dudás.

Pasó de verdad, en el deploy de `CONTRACT-30`: el binario se subió a una ruta equivocada, el
servicio arrancó de nuevo con el anterior, y las dos comprobaciones de salud dieron verde. No
mintieron — contestaron con exactitud la pregunta que se les hizo, que era *"¿el servicio
responde?"* y no *"¿el servicio responde con lo que acabás de subir?"*. Se descubrió por
casualidad, mirando el `ExecStart` por otro motivo.

```bash
# En el VPS. El tamaño y la fecha tienen que ser los del binario que compilaste.
DEST=$(systemctl show -p ExecStart --value librarian.service | sed -E 's/.*path=([^ ;]+).*/\1/')
ls -la "$DEST"

# Y que el PROCESO VIVO sea ese archivo, no una copia que quedó de antes.
readlink -f /proc/$(systemctl show -p MainPID --value librarian.service)/exe
```

El tamaño tiene que coincidir con el `stat -c '%s'` que anotaste al compilar. Si no coincide, el
deploy no ocurrió: **volvé al procedimiento, no sigas verificando**. Todo lo que midas a partir de
acá estará midiendo la versión anterior.

Mejor todavía cuando el cambio lo permite: verificá con **una respuesta que la versión vieja no
sepa dar** (una ruta nueva, un campo nuevo en una respuesta, un control nuevo en una página). Es la
única comprobación que no se puede pasar por accidente.

### 2. El servicio está vivo **y además puede servir**

Son **dos comprobaciones distintas y hay que hacer las dos** (CONTRACT-24). `/health` dice
solamente que el proceso está vivo; `/ready` es la que mira la base. Con la base detenida,
`/health` sigue respondiendo `200 {"status":"ok"}` — es correcto, el proceso está vivo — así que
un deploy verificado solo con `/health` da por bueno un servicio que no puede atender nada. Eso
pasó de verdad: ver el hueco 7 de `docs/PENDIENTES.md`.

```bash
systemctl is-active librarian.service
journalctl -u librarian.service --no-pager -n 5      # sin errores de arranque

# El proceso vive.
curl -s https://librarian.ardf.dev/health            # {"status":"ok"}

# El proceso puede servir: la base responde. ESTE es el que corta el deploy.
curl -s -o /dev/null -w '%{http_code}\n' https://librarian.ardf.dev/ready   # 200
curl -s https://librarian.ardf.dev/ready                                    # {"status":"ready"}
```

`/ready` responde **503** `{"error":"service unavailable"}` cuando no consigue conexión con la
base (o cuando la base tarda más de 2 s en contestar). Si da 503, **parar acá**: el deploy no
está bien aunque el binario haya arrancado y `/health` esté verde. Mirar el motor —
`systemctl status postgresql` o `docker ps` del contenedor de la base, según la instalación — y
`journalctl -u librarian.service`. La respuesta de `/ready` no trae detalle a propósito (ni DSN,
ni host, ni error del driver): el diagnóstico se hace en el servidor, no leyendo el endpoint.

Síntoma asociado, por si aparece durante una incidencia: con la base caída, el login responde
**503**, no 401. Un 401 en el login significa credenciales de verdad incorrectas y nada más.

Si hay un balanceador o un monitor delante, `/ready` es la ruta que tiene que consultar para
decidir si mandarle tráfico a la instancia; `/health` sirve para reiniciar el proceso colgado.

### 3. Conseguir una identidad para verificar

La verificación necesita autenticarse. **No uses la contraseña de una persona para esto**: acuñá
una credencial efímera, de alcance mínimo, y revocala como parte del mismo procedimiento. Es más
seguro y además no depende de que alguien tenga la contraseña a mano — en el deploy de
`CONTRACT-18` esa contraseña sencillamente no estaba disponible, y el paso de verificación no
puede quedar bloqueado por eso.

```bash
# En el VPS. Crea una API key con rol administrator y deja el secreto en /tmp.
python3 -c "
import sqlite3, hashlib, secrets, base64
secret = 'lbk_' + base64.urlsafe_b64encode(secrets.token_bytes(32)).rstrip(b'=').decode()
c = sqlite3.connect('/opt/librarian/data/librarian.db')
rid = c.execute(\"SELECT id FROM roles WHERE name='administrator'\").fetchone()[0]
c.execute('INSERT INTO api_keys (label, key_hash, role_id) VALUES (?,?,?)',
          ('deploy-check', hashlib.sha256(secret.encode()).hexdigest(), rid))
c.commit(); open('/tmp/deploy-check.key','w').write(secret)
"
K=$(cat /tmp/deploy-check.key)
```

El prefijo `lbk_` y el hash SHA-256 hex son el formato que espera `auth.VerifyAPIKey`; se envía
como `Authorization: Bearer <secreto>`, igual que un JWT.

**La revocación va en este mismo procedimiento, no en una lista de pendientes** (paso 6). Una
credencial de prueba que sobrevive a su prueba es una puerta abierta que nadie recuerda haber
dejado.

**Si el cambio es de UI, la API key no alcanza**: autentica la API, no el login del navegador. Y
no hace falta la contraseña de nadie — acuñá una IDENTIDAD efímera, que es la misma idea un nivel
más arriba, y borrala en el paso 6 junto con la key. Se usó así para verificar `CONTRACT-30`:

```bash
# 1. En tu máquina: generar el hash bcrypt de una contraseña descartable.
#    (cualquier utilidad bcrypt sirve; el costo 12 es el que usa el proyecto)

# 2. En el VPS: insertar la identidad y darle el rol administrator.
#    docker exec necesita -i o el heredoc se descarta EN SILENCIO con exit 0.
docker exec -i <contenedor-db> psql -U postgres -d librarian -v ON_ERROR_STOP=1 <<'SQL'
INSERT INTO users (id, email, password_hash, status)
VALUES ('00000000-0000-4000-8000-0000000000ff', 'verif@sandbox.local', '<hash>', 'active');
INSERT INTO user_roles (user_id, role_id)
SELECT '00000000-0000-4000-8000-0000000000ff', id FROM roles WHERE name='administrator';
SQL

# 3. Iniciar sesión guardando la cookie, y navegar con ella.
curl -s -c /tmp/v.jar -o /dev/null -w "login=%{http_code}\n" \
  --data-urlencode "email=verif@sandbox.local" --data-urlencode "password=<descartable>" \
  https://librarian.ardf.dev/login          # 303
curl -s -b /tmp/v.jar https://librarian.ardf.dev/admin/content/<tipo>/new | grep -o '<select[^>]*>'
```

**Lo que crees para verificar, borralo por la RUTA REAL del producto, nunca por SQL.** Un tipo de
contenido borrado a mano deja `__compat_schema` desincronizada y el próximo reinicio no arranca
(ver [La invalidación de metadata NO es opcional](#la-invalidación-de-metadata-no-es-opcional)). La
ruta del panel es `POST /admin/content-types/{nombre}/delete` con `confirm_name` y `confirm_rows`
—el `DELETE` de la API JSON es otra ruta, y usarla contra el panel devuelve `405`—; si no te
acordás de los campos, pedí la página de borrado y leelos del formulario en vez de adivinarlos.

### 4. Lecturas (necesarias pero NO suficientes)

```bash
for r in /articles /products /terms /content-types; do
  echo -n "$r="; curl -s -o /dev/null -w "%{http_code}\n" "https://librarian.ardf.dev$r" \
    -H "Authorization: Bearer $K"
done
```

### 5. UNA ESCRITURA REAL — el paso que no se puede saltear

Tiene que atravesar la capa de autorización y ejercitar **lo que este deploy cambió**, no
cualquier escritura genérica.

**Elegí una secuencia que sea su propio inverso**, para que la verificación no deje rastro. Casi
siempre existe: una capacidad que puede quitar algo suele poder también agregarlo. En el deploy
de `CONTRACT-18` (edición de campos de un tipo dinámico, que RECONSTRUYE la tabla entera y puede
destruir datos) la secuencia fue: agregar un campo → escribir contenido en él → volver a quitarlo
confirmando la pérdida. Se ejercitan los dos caminos, el aditivo y el destructivo con su guarda de
confirmación, y el sistema termina exactamente como empezó.

```bash
# Ejemplo simple, cuando el deploy no toca nada delicado:
curl -s -X POST https://librarian.ardf.dev/articles -H "Authorization: Bearer $K" \
  -H "Content-Type: application/json" -d '{"title":"deploy check","body":"x"}'
# ...y borrar lo creado.
```

**Qué mirar cuando el deploy toca datos existentes** (procedimiento C, o cualquier cambio que
reconstruya tablas): que los datos preexistentes conserven su **IDENTIDAD**, no solo sus valores.
Mismos `id`, mismos `created_at`. Un registro con los mismos valores pero identidad nueva es un
registro distinto que se le parece, y toda referencia externa a él ya está rota. Verificá además
que esos datos concretos siguen accesibles **por su ruta pública**, no solo que la tabla tiene
filas.

Si el deploy tocó el esquema, confirmá también que el catálogo físico, la metadata
`__compat_schema` y el registro de campos dicen **los tres lo mismo** — es la comprobación que
detecta la caché desincronizada antes de que la detecte un crashloop:

```bash
python3 -c "
import sqlite3, json
c = sqlite3.connect('/opt/librarian/data/librarian.db')
print('catalogo :', [r[1] for r in c.execute('PRAGMA table_info(cpt_eventos)')])
d = json.loads(c.execute(\"SELECT value FROM __compat_schema WHERE key='canonical_schema'\").fetchone()[0])
t = next(t for t in d['tables'] if t['name'] == 'cpt_eventos')
print('metadata :', [x['name'] for x in t['columns']])
print('sobrantes:', [r[0] for r in c.execute(\"SELECT name FROM sqlite_master WHERE name LIKE 'cptmp%'\")] or 'ninguna')
"
```

### 6. Cerrar la credencial efímera

```bash
python3 -c "
import sqlite3
c = sqlite3.connect('/opt/librarian/data/librarian.db')
c.execute(\"DELETE FROM api_keys WHERE label='deploy-check'\"); c.commit()
print('keys activas:', c.execute('SELECT count(*) FROM api_keys').fetchone()[0])
"
rm -f /tmp/deploy-check.key
```

El recuento de credenciales activas tiene que quedar igual que antes de empezar.

## Rollback

El backup del paso 4 es el rollback.

**Antes de restaurar, leé el error real** (`journalctl -u librarian.service -n 20`). En el
incidente de CONTRACT-17 el mensaje decía exactamente cuál era el problema (`table "cpt_eventos"
already exists`) y el arreglo fue de una línea — restaurar el backup habría deshecho una
migración correcta por un paso faltante trivial. Restaurar es lo último, no lo primero.

### Sobre SQLite

```bash
systemctl stop librarian.service
cp /opt/librarian/data/librarian.db.bak-pre-<tag>-<timestamp> /opt/librarian/data/librarian.db
# y restaurar el binario anterior si el problema es del binario
systemctl start librarian.service
```

### Sobre PostgreSQL

Restaurar **no** es volcar el dump encima de la base existente: `pg_restore` no vacía lo que
encuentra, así que superponer deja un estado mezclado, peor que el que querías deshacer. Se
reemplaza la base entera:

```bash
systemctl stop librarian.service

# 1. Apartá la base rota en vez de borrarla — todavía es la evidencia de qué pasó
psql "<dsn-admin>" -c 'ALTER DATABASE librarian RENAME TO librarian_rota_<timestamp>;'

# 2. Base nueva, con la extensión, y restaurá
psql "<dsn-admin>" -c 'CREATE DATABASE librarian;'
psql "$LIBRARIAN_DB" -c 'CREATE EXTENSION IF NOT EXISTS vector;'
PGPASSWORD='...' pg_restore -U <usuario> -h <host> -d librarian <volcado>.dump

systemctl start librarian.service
```

El `RENAME` en vez del `DROP` es deliberado: una vez que borrás la base rota, perdiste la única
copia del estado que causó el incidente, y la causa se investiga después de que el servicio vuelve.
Borrala cuando el incidente esté cerrado, no antes.

Requiere que no haya conexiones abiertas contra la base al renombrarla; detener el servicio suele
alcanzar, y si no, cerrá las sesiones sobrantes antes de insistir.

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
| Base de datos | `/opt/librarian/data/librarian.db` |
| Configuración | `/opt/librarian/.env` (incluye `LIBRARIAN_JWT_SECRET`, modo `0600`) |
| Público | `https://librarian.ardf.dev` (nginx → `127.0.0.1:8500`) |

## Elegí el procedimiento correcto

| Situación | Procedimiento |
|---|---|
| El cambio NO toca el esquema | [A. Deploy simple](#a-deploy-simple) |
| El cambio AGREGA tablas o columnas nuevas | [B. Deploy con cambio de esquema](#b-deploy-con-cambio-de-esquema) |
| El cambio requiere tocar tablas existentes a mano (renombrar, mover datos) | [C. Deploy con migración manual](#c-deploy-con-migración-manual) |

Ante la duda, usá el procedimiento MÁS estricto de los que apliquen. El costo de B sobre A son
unos minutos; el costo de equivocarse es producción caída.

---

## A. Deploy simple

```bash
# 1. Compilar (con el servicio ARRIBA — no hace falta bajarlo para esto)
cd D:/Repo/librarian
GOOS=linux GOARCH=amd64 go build -o /tmp/librarian-linux-amd64 ./cmd/librarian
```

Subir por SFTP a `/opt/librarian/librarian.new`, y después:

```bash
mv /opt/librarian/librarian.new /opt/librarian/librarian   # reemplazo atómico
systemctl restart librarian.service
systemctl is-active librarian.service                       # debe decir "active"
curl -s http://127.0.0.1:8500/health                        # {"status":"ok"}
```

Después, la [verificación post-deploy](#verificación-post-deploy) — que NO es solo lecturas.

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
nuevo contra ESA COPIA — **dos veces**:

```bash
# Primer arranque: ejercita el camino incremental (crea lo que falta)
LIBRARIAN_DB=/tmp/prod-copy.db LIBRARIAN_JWT_SECRET=test LIBRARIAN_ADDR=:8099 ./librarian
# Segundo arranque sobre la MISMA copia: ejercita el camino "ya todo aplicado"
```

Ambos tienen que arrancar limpio. El segundo importa tanto como el primero: hubo un bug real
(CONTRACT-11) que solo se manifestaba en el reinicio siguiente, no en el primer arranque.
Confirmá además que los datos preexistentes siguen ahí (logueá con un usuario real, listá
contenido existente).

Recién entonces: backup, subir, reiniciar.

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

```bash
# 1. El servicio está vivo
systemctl is-active librarian.service
journalctl -u librarian.service --no-pager -n 5      # sin errores de arranque
curl -s https://librarian.ardf.dev/health            # {"status":"ok"}

# 2. Autenticación real
TOKEN=$(curl -s -X POST https://librarian.ardf.dev/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"<admin>","password":"<pass>"}' | grep -o '"token":"[^"]*"' | cut -d'"' -f4)

# 3. Lecturas (necesarias pero NO suficientes)
for r in /articles /products /terms /content-types; do
  echo -n "$r="; curl -s -o /dev/null -w "%{http_code}\n" "https://librarian.ardf.dev$r" \
    -H "Authorization: Bearer $TOKEN"
done

# 4. UNA ESCRITURA REAL que atraviese la capa de autorización — el paso que no se puede saltear
curl -s -X POST https://librarian.ardf.dev/articles -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" -d '{"title":"deploy check","body":"x"}'
# ...y borrar lo creado.
```

Si el deploy tocó datos existentes (procedimiento C), verificá además que **esos datos concretos
siguen accesibles por su ruta pública**, no solo que la tabla tiene filas.

## Rollback

El backup del paso 4 es el rollback. Si el servicio no arranca y la causa no es obvia en
`journalctl`:

```bash
systemctl stop librarian.service
cp /opt/librarian/data/librarian.db.bak-pre-<tag>-<timestamp> /opt/librarian/data/librarian.db
# y restaurar el binario anterior si el problema es del binario
systemctl start librarian.service
```

Antes de restaurar, **leé el error real** (`journalctl -u librarian.service -n 20`). En el
incidente de CONTRACT-17 el mensaje decía exactamente cuál era el problema (`table "cpt_eventos"
already exists`) y el arreglo fue de una línea — restaurar el backup habría deshecho una
migración correcta por un paso faltante trivial.

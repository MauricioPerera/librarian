# CONTRACT-20C — Orden correcto de timestamps

Base: `fb061ea` (CONTRACT-20B completo). Árbol **SIN commitear**, como pide el contrato.

**Resultado: LISTO**, con una decisión de T2 que el orquestador tiene que tomar
(§ "T2 — la decisión, y el paso operativo que NO ejecuté").

Lo que se hizo:

1. **T1** — el ordenamiento en Go compara **instantes**, no las cadenas que los transportan.
   El test que fija el defecto está en dos niveles (el helper y la ruta real `auth.ListAPIKeys`),
   y los **dos fallan con el código de `main`**. Las dos salidas están pegadas abajo.
2. **T2** — se **midió** el `ORDER BY` de SQL contra los DOS motores reales, con una tabla que
   mezcla el formato viejo del motor y el nuevo de la aplicación. Cuatro escenarios, salida real
   pegada. La conclusión **corrige una hipótesis mía que la medición refutó**.
3. **T3** — build/vet/gofmt limpios, suite verde dos veces, baterías de `CONTRACT-19` y
   `CONTRACT-20` verdes contra los dos motores reales, y el transcripto de la batería del 19
   **byte-idéntico** antes y después del arreglo.

`sqlite-postgres-compat` **no se tocó**: su `git status` quedó exactamente como estaba al empezar
(solo el `experiments/vector/vector-exp.exe` sin trackear que ya estaba). `internal/store` no se
tocó. `store.Open` no cambió. `internal/schema` no cambió: ninguna rutina, ningún `ORDER BY`,
ninguna vista. Ninguna dependencia nueva. Ningún cambio en el contrato público de las rutas HTTP.
**Ningún dato de producción se modificó ni se escribió código que lo haga.**

---

## T1 — Comparar instantes, no cadenas

### Dónde estaba el defecto, exactamente

Un valor de la familia `timestamp` se guarda como TEXTO y compat lo **re-renderiza con
`time.RFC3339Nano` al leerlo** (`compat/store.go:361` y `:428`), formato que **recorta los ceros
finales** de la fracción. El único ordenamiento en Go de todo el proyecto que usa un timestamp como
clave era `ListAPIKeys` (`dual.Desc(k.CreatedAt)`), y comparaba ese texto recortado byte a byte.

Se auditaron los ocho llamadores de los helpers de orden; el resto ordena por `email`,
`permission_name`, `role_name`, `taxonomy/name/slug` y `label`/`id` — ninguna clave de la familia
`timestamp`. O sea: **un solo punto en Go**, y es el que se arregló.

### El arreglo

`internal/dual` gana el vocabulario de instantes:

| Símbolo | Qué hace |
|---|---|
| `dual.ParseInstant(text)` | Convierte el texto en el instante que denota. Acepta **los mismos layouts que compat** (`timestampFormats`), incluido el separador con espacio del formato viejo. Falla, visible, con el texto entre comillas, en cualquier otra cosa — incluida la cadena vacía, que es lo que `RowText` devuelve para un NULL. |
| `dual.InstantSortValue(text)` | Renderiza ese instante con `dual.TimestampLayout` en UTC: ancho fijo, puntuación en offsets idénticos. **Comparar dos de esos renderizados byte a byte ES comparar los dos instantes.** |
| `dual.AscInstant` / `dual.DescInstant` | Construyen una `dual.Key` con ese valor, así el orden por instante **compone** con las claves de texto que van al lado sin una máquina de comparación aparte. |
| `dual.SortByKeysE` | `SortByKeys` para una función de claves que **puede fallar**. |

No es un comparador a medida: es normalizar la clave y seguir usando la maquinaria de `Key` que
`CONTRACT-20B` ya había validado contra los dos motores. `SortByKeys` quedó reimplementado sobre
`SortByKeysE`, así que hay **una sola** implementación de comparación en el paquete.

### El modo de falla, documentado y testeado

El contrato exige que un texto que no parsea "falle de forma visible o degrade de una manera
documentada y testeada". Lo elegido, y por qué:

- **Falla visible.** `SortByKeysE` devuelve error; `ListAPIKeys` lo envuelve nombrando la clave
  (`api key <id> created_at: ...`) y lo propaga al handler, que da un 500. No hay fallback a
  comparación de texto: eso es justamente el comportamiento accidental que este contrato saca.
- **Las claves se computan ANTES de comparar nada.** No es un detalle de performance (aunque son n
  cómputos en vez de O(n log n)): un comparador **no se llama nunca** cuando el slice tiene menos de
  dos elementos, así que una clave rota pasaría desapercibida exactamente en los listados chicos.
  Hay un test que fija esto sobre un slice de UN elemento.
- **Si falla, no se reordena nada.** El slice queda como vino, con el `ORDER BY` declarado de la
  rutina, no a medio ordenar. También testeado.

### El cambio de `auth.now()` — revisando la decisión de CONTRACT-20B

`CONTRACT-20B` decidió a propósito **no** adoptar el ancho fijo de `internal/server` en
`internal/auth`, con este argumento: como `auth` ordena en Go sobre el valor que compat devuelve
(siempre recortado), el ancho fijo no cambiaba ningún orden y sí cambiaba el texto guardado en
`api_keys.created_at`, y con él la secuencia que SQLite produce.

**Este contrato revierte esa decisión, porque las dos mitades de su premisa se movieron:**

- El orden de `ListAPIKeys` ya **no depende del texto**: compara el instante. Que el texto guardado
  cambie de forma es ahora **observacionalmente gratis** para ese listado — T1 sacó justamente el
  argumento que bloqueaba el cambio.
- La razón que queda para cuidar el texto guardado es SQL, y ahí la medición de T2 (escenario D)
  muestra que el formato recortado **es incorrecto por sí solo**, sin necesidad de mezclarlo con
  nada.

Así que `auth.now()` pasa a `dual.Now()` y `canonicalTimestampLayout` de `internal/server` pasa a
ser `dual.TimestampLayout`: **un solo formato de escritura para toda la aplicación**, con una sola
definición.

### (a) EL TEST DEL DEFECTO FALLANDO CONTRA `main` — salida REAL

Los valores están construidos para las dos causas que el contrato enumera, y son los valores que
compat efectivamente devuelve (o sea, recortados).

**Nivel 1 — el helper (`internal/dual`).** Se corrió el test tal cual, cambiando ÚNICAMENTE
`dual.AscInstant`/`dual.DescInstant` por `dual.Asc`/`dual.Desc`, que es el código de `main`:

```
$ go test ./internal/dual/ -count=1 -run TestInstantKeysOrderChronologically -v
=== RUN   TestInstantKeysOrderChronologically
    instant_contract20c_test.go:107: ascending order is NOT chronological:
          got  [c-nanos-ending-in-one b-nanos-ending-in-zero d-second-25-half a-second-25-no-fraction e-second-26]
          want [a-second-25-no-fraction b-nanos-ending-in-zero c-nanos-ending-in-one d-second-25-half e-second-26]
--- FAIL: TestInstantKeysOrderChronologically (0.00s)
FAIL
FAIL	github.com/MauricioPerera/librarian/internal/dual	1.390s
FAIL
```

Se ven **las dos causas a la vez** en una sola línea de salida:

- `c-nanos-ending-in-one` (`…25.123456781Z`) sale ANTES que `b-nanos-ending-in-zero`
  (`…25.12345678Z` una vez recortado) — es POSTERIOR y ordena primero, porque `'Z'` (0x5A) > `'1'`
  (0x31).
- `a-second-25-no-fraction` (`…25Z`) sale DESPUÉS de `d-second-25-half` (`…25.5Z`) — es el más
  ANTIGUO de los cinco y ordena cuarto, porque `'Z'` (0x5A) > `'.'` (0x2E).

**Nivel 2 — la ruta real de producción (`auth.ListAPIKeys` sobre SQLite real).** Se corrió el test
tal cual, con `internal/auth/apikey.go` revertido al código de `main`:

```
$ go test ./internal/auth/ -count=1 -run TestListAPIKeysOrdersByInstantNotByText -v
=== RUN   TestListAPIKeysOrdersByInstantNotByText
    apikey_contract20c_test.go:68: ListAPIKeys order is NOT chronological:
          got  [oldest-whole-second newest-half-second middle-nanos-ending-in-zero]
          want [newest-half-second middle-nanos-ending-in-zero oldest-whole-second]
          (canonicalized created_at, in returned order: [2026-07-25T12:00:25Z 2026-07-25T12:00:25.5Z 2026-07-25T12:00:25.12345678Z])
--- FAIL: TestListAPIKeysOrdersByInstantNotByText (0.08s)
FAIL
FAIL	github.com/MauricioPerera/librarian/internal/auth	2.424s
FAIL
```

La tercera línea es la prueba de que el defecto es el que dice el contrato y no otra cosa: los tres
`created_at` fueron escritos en ancho fijo (`…25.000000000Z`, `…25.123456780Z`, `…25.500000000Z`) y
**compat los devolvió recortados**. La clave más ANTIGUA (`oldest-whole-second`) encabeza un listado
declarado "newest first".

### (b) LOS MISMOS TESTS EN VERDE

```
$ go test ./internal/dual/ -count=1 -v
=== RUN   TestInstantKeysOrderChronologically
--- PASS: TestInstantKeysOrderChronologically (0.00s)
=== RUN   TestInstantKeysTieBreakOnTheNextKey
--- PASS: TestInstantKeysTieBreakOnTheNextKey (0.01s)
=== RUN   TestInstantKeysHandleTheCarrierVariants
    --- PASS: TestInstantKeysHandleTheCarrierVariants/offset_vs_utc,_same_instant (0.00s)
    --- PASS: TestInstantKeysHandleTheCarrierVariants/offset_earlier_than_utc (0.00s)
    --- PASS: TestInstantKeysHandleTheCarrierVariants/legacy_space_form_parses (0.00s)
    --- PASS: TestInstantKeysHandleTheCarrierVariants/legacy_space_form_is_older (0.00s)
    --- PASS: TestInstantKeysHandleTheCarrierVariants/trimmed_vs_fixed_width,_same_instant (0.00s)
    --- PASS: TestInstantKeysHandleTheCarrierVariants/trimmed_half_second_is_earlier (0.00s)
--- PASS: TestInstantKeysHandleTheCarrierVariants (0.00s)
=== RUN   TestInstantKeysFailVisiblyOnUnparseableText
--- PASS: TestInstantKeysFailVisiblyOnUnparseableText (0.00s)
=== RUN   TestSortByKeysELeavesInputUntouchedOnError
--- PASS: TestSortByKeysELeavesInputUntouchedOnError (0.00s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/dual	1.292s

$ go test ./internal/auth/ -count=1 -run 'TestListAPIKeysOrdersByInstantNotByText|TestListAPIKeysRejectsAnUnparseableCreatedAt' -v
=== RUN   TestListAPIKeysOrdersByInstantNotByText
--- PASS: TestListAPIKeysOrdersByInstantNotByText (0.09s)
=== RUN   TestListAPIKeysRejectsAnUnparseableCreatedAt
--- PASS: TestListAPIKeysRejectsAnUnparseableCreatedAt (0.07s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/auth	2.609s
```

**Honestidad sobre uno de esos tests.** `TestListAPIKeysRejectsAnUnparseableCreatedAt` **también
pasa con el código de `main`**, y por eso no lo presento como prueba del arreglo: compat rechaza el
timestamp impresentable en `canonicalValue` ANTES de que la fila llegue a mi código, así que el 500
ya existía. Lo dejo porque fija que ese es el comportamiento (no un orden accidental) y porque
`dual.AscInstant` es la segunda línea de defensa si un día la lectura deja de canonicalizar. El test
que SÍ prueba el arreglo en la ruta real es el de arriba.

---

## T2 — El `ORDER BY` de SQL, decidido con evidencia MEDIDA

La medición está en `internal/auth/dualengine_contract20c_test.go`: crea una tabla con
`created_at TEXT` (el mapeo que la familia `timestamp` recibe en los DOS motores), inserta filas con
el texto exacto de cada formato, corre el `ORDER BY` del motor y compara la secuencia devuelta
contra el orden cronológico REAL — que sale de un rango declarado en la fixture, no de comparar
texto, para que el oráculo sea independiente de lo que se está midiendo.

### La salida REAL, contra los dos motores

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineTimestampOrderBy -count=1 -v ./internal/auth

=== RUN   TestDualEngineTimestampOrderBy
  A-production-shape | sqlite   | ORDER BY created_at ASC -> [legacy-1 legacy-2 app-1 app-2 app-3]
  A-production-shape | postgres | ORDER BY created_at ASC -> [legacy-1 legacy-2 app-1 app-2 app-3]
  A-production-shape | truth    | chronological           -> [legacy-1 legacy-2 app-1 app-2 app-3]

  B1-legacy-row-newer-on-a-LATER-DAY | sqlite   | ORDER BY created_at ASC -> [app-1 app-2 legacy-newer-next-day]
  B1-legacy-row-newer-on-a-LATER-DAY | postgres | ORDER BY created_at ASC -> [app-1 app-2 legacy-newer-next-day]
  B1-legacy-row-newer-on-a-LATER-DAY | truth    | chronological           -> [app-1 app-2 legacy-newer-next-day]

  B2-legacy-row-newer-on-the-SAME-DAY | sqlite   | ORDER BY created_at ASC -> [legacy-newer-same-day app-1 app-2]
  B2-legacy-row-newer-on-the-SAME-DAY | postgres | ORDER BY created_at ASC -> [legacy-newer-same-day app-1 app-2]
  B2-legacy-row-newer-on-the-SAME-DAY | truth    | chronological           -> [app-1 app-2 legacy-newer-same-day]

  C-application-rows-only-FIXED-width | sqlite   | ORDER BY created_at ASC -> [t0 t1 t2 t3 t4]
  C-application-rows-only-FIXED-width | postgres | ORDER BY created_at ASC -> [t0 t1 t2 t3 t4]
  C-application-rows-only-FIXED-width | truth    | chronological           -> [t0 t1 t2 t3 t4]

  D-application-rows-only-TRIMMED | sqlite   | ORDER BY created_at ASC -> [t2 t1 t3 t0 t4]
  D-application-rows-only-TRIMMED | postgres | ORDER BY created_at ASC -> [t2 t1 t3 t0 t4]
  D-application-rows-only-TRIMMED | truth    | chronological           -> [t0 t1 t2 t3 t4]
--- PASS: TestDualEngineTimestampOrderBy (8.26s)
PASS
ok  	github.com/MauricioPerera/librarian/internal/auth	10.550s
```

### Una hipótesis mía que la medición REFUTÓ

Antes de medir escribí el escenario B como "una fila vieja más NUEVA que las de la aplicación
→ orden incorrecto", con la fila vieja en el día SIGUIENTE. **Salió correcto en los dos motores**, y
tuve que corregir el escenario. La razón es que los dos formatos difieren recién en el carácter 11
(el separador `' '` contra `'T'`), así que **el prefijo de fecha decide primero** y el separador solo
llega a importar cuando las dos fechas son iguales. Por eso el escenario quedó partido en B1 (día
distinto → correcto) y B2 (mismo día → incorrecto). Es exactamente lo que el contrato pedía al exigir
medición en vez de razonamiento: mi razonamiento tenía el mecanismo bien y el alcance mal.

### Las tres preguntas del contrato, respondidas

**1. ¿Qué orden produce hoy la mezcla de formatos?**

Con la forma que tiene producción (escenario A: filas viejas del motor, todas anteriores a las que
escribió la aplicación) el `ORDER BY` sale **cronológicamente correcto, e idéntico en los dos
motores**. Los dos motores coincidieron en los CUATRO escenarios, incluidos los dos incorrectos:
esto **no es una divergencia entre motores** en ninguna de sus formas, es corrección cronológica.

**2. ¿Es seguro por construcción, o hay un caso donde se rompe?**

**No es seguro por construcción. Se rompe, y hay dos roturas distintas:**

- **B2 — una fila en formato viejo con fecha del MISMO DÍA que una fila de la aplicación.** Con los
  prefijos de fecha iguales la comparación llega al separador, y `' '` (0x20) < `'T'` (0x54), así que
  la fila vieja queda forzada a parecer la MÁS ANTIGUA sin importar la hora que lleve. Esto no es
  hipotético: **los `DEFAULT CURRENT_TIMESTAMP` de las columnas siguen ahí** (a propósito, como red
  de seguridad para escrituras que no vienen de la aplicación — decisión de `CONTRACT-19`/`20`), o
  sea que cualquier INSERT que no venga de la app produce hoy mismo una fila en formato viejo con
  fecha de hoy. Esa fila se ordena como la más antigua del día.
- **D — el formato recortado se ordena mal SOLO CONSIGO MISMO.** Este es el hallazgo más importante
  de la medición, y no involucra filas viejas en absoluto: cinco filas escritas por la aplicación con
  `time.RFC3339Nano` pelado salieron en el orden `[t2 t1 t3 t0 t4]` contra el cronológico
  `[t0 t1 t2 t3 t4]`, en los dos motores. Es el mismo defecto de T1, del lado de SQL. Y `D` es
  exactamente lo que `internal/auth` venía escribiendo en `api_keys.created_at` desde
  `CONTRACT-19`.

**3. ¿Alcanza con que la aplicación escriba siempre ancho fijo, o hay que normalizar lo existente?**

- **Escribir siempre ancho fijo es NECESARIO** — el escenario D lo prueba sin ambigüedad — y ya está
  hecho en este contrato (`auth.now()` → `dual.Now()`; `internal/server` ya lo hacía).
- **No es SUFICIENTE para las filas que ya existen.** Quedan dos poblaciones defectuosas:
  las filas viejas del motor (rotura B2, latente hasta que aparezca una colisión de mismo día) y las
  filas de `api_keys` escritas en formato recortado desde `CONTRACT-19` (rotura D, presente **ahora**
  entre esas filas si alguien las ordena en SQL).

### La decisión, y el paso operativo que NO ejecuté

**Mi conclusión: hace falta normalizar las filas existentes.** El contrato dice que en ese caso NO
lo implemente, y no lo implementé — ni una línea de código toca datos. Va acá para que el
orquestador decida.

Es una decisión que se puede **posponer sin riesgo inmediato**, y quiero ser exacto sobre por qué:
hoy no hay ningún listado cuyo orden observable dependa de esos textos. Los tres listados paginados
de `internal/server` ordenan en SQL, pero sus filas se escriben en ancho fijo desde `CONTRACT-20`;
`ListAPIKeys` es el único que toca filas en formato recortado, y su orden final lo decide Go por
instante desde este contrato. Lo que la normalización compra es que **el texto guardado deje de ser
una trampa** para el `CONTRACT-21` y para cualquier rutina futura que ordene por `created_at` en SQL.

**El SQL, para revisar antes de correr.** Funciona igual en SQLite y en PostgreSQL (solo
`substr`, `length`, `||` y `CASE`). Primero el SELECT que muestra qué se tocaría; el UPDATE es
idempotente (el `WHERE` deja de matchear una vez normalizada la fila).

```sql
-- 1) INSPECCIÓN: cuántas filas están en cada formato, por columna.
SELECT
  SUM(CASE WHEN created_at LIKE '____-__-__ __:__:__%' THEN 1 ELSE 0 END) AS formato_viejo,
  SUM(CASE WHEN created_at LIKE '%Z' AND length(created_at) <> 30 THEN 1 ELSE 0 END) AS recortado,
  SUM(CASE WHEN length(created_at) = 30 AND created_at LIKE '%Z' THEN 1 ELSE 0 END) AS ancho_fijo,
  COUNT(*) AS total
FROM api_keys;

-- 2) Formato VIEJO del motor ("2026-07-24 07:46:36", sin zona, sin fracción)
--    → ancho fijo UTC. Interpreta el valor como UTC, que es como lo lee compat.
UPDATE api_keys
   SET created_at = substr(created_at, 1, 10) || 'T' || substr(created_at, 12, 8) || '.000000000Z'
 WHERE created_at LIKE '____-__-__ __:__:__';

-- 3) Formato RECORTADO (RFC3339Nano con la fracción acortada o ausente)
--    → rellena la fracción a 9 dígitos.
UPDATE api_keys
   SET created_at = substr(created_at, 1, 19) || '.' ||
       substr(CASE WHEN substr(created_at, 20, 1) = '.'
                   THEN substr(created_at, 21, length(created_at) - 21)
                   ELSE '' END || '000000000', 1, 9) || 'Z'
 WHERE created_at LIKE '%Z' AND length(created_at) <> 30;

-- 4) VERIFICACIÓN: debe dar 0.
SELECT COUNT(*) FROM api_keys
 WHERE NOT (length(created_at) = 30 AND created_at LIKE '%Z');
```

**Columnas a las que aplica** (verificadas en `internal/schema`): `users.created_at`,
`users.updated_at`, `api_keys.created_at`, `api_keys.revoked_at` (nullable — agregar
`AND created_at IS NOT NULL` en la columna correspondiente), `content_types.created_at`, y
`created_at`/`updated_at`/`published_at` de **cada tabla de tipo de contenido** (`articles`,
`products` y las dinámicas, enumerables desde `content_types`). `terms`, `taxonomies`, `roles`,
`permissions` y las junction tables no tienen columnas de esta familia.

**Advertencias que el orquestador tiene que ver antes de decidir:**

- Una fila en formato viejo NO tiene zona horaria. El SQL de arriba la interpreta como **UTC**, que
  es lo que hace compat al leerla. Si el `CURRENT_TIMESTAMP` de SQLite en producción escribió hora
  local en vez de UTC, la normalización **congela ese error** en vez de arreglarlo. Verificarlo
  antes: SQLite escribe UTC por defecto, pero conviene comprobar una fila conocida.
- Correr con backup y en una transacción.
- El paso NO cierra la rotura B2 a futuro por sí solo: mientras los `DEFAULT CURRENT_TIMESTAMP`
  sigan en las columnas, una escritura que no venga de la aplicación vuelve a introducir una fila en
  formato viejo. Sacar esos DEFAULT es un cambio de esquema y queda fuera de este contrato.

### La prueba que falla si se rompe la condición — y su límite, dicho sin adornos

Los escenarios A y C **asertan** resultado cronológico, así que el test se pone rojo si alguien
revierte el escritor al formato recortado o cambia el formato de escritura por otro que no sea
ordenable. El escenario B2 **fija la rotura**: si un día el `ORDER BY` de B2 saliera correcto, sería
porque el formato de las filas cambió, y el test también se pone rojo.

El límite, dicho claro: **es un test de fixtures, no un centinela sobre los datos de producción.**
Fija la CONDICIÓN (formato de escritura uniforme y ordenable; filas viejas siempre anteriores dentro
de su propio día) y pone en rojo a quien la rompa en el CÓDIGO. No puede detectar que alguien
insertó a mano una fila en formato viejo con la fecha de hoy — eso lo detecta la query de inspección
del paso 1, o lo elimina la normalización. No voy a presentar el test como algo que no es.

---

## Criterio 6 — el orden observable en SQLite

### La comprobación

La misma que hizo `CONTRACT-20B`: el transcripto de la batería de `CONTRACT-19` (82 observaciones,
que incluyen el orden completo de los cuatro listados de `auth`) con el código de `main` contra el
transcripto con el arreglo. Las fixtures son idénticas; lo único que cambió es el código.

```
$ diff <(transcripto con el código de main) <(transcripto con 20C)
before lines 82 after lines 82
IDENTICAL: the SQLite/PostgreSQL transcript is byte-identical before and after
```

**82 de 82 observaciones byte-idénticas**, contra los dos motores reales. La suite completa
(~30 archivos de test de `internal/server` que ejercitan las rutas HTTP y la UI de punta a punta,
más los de `auth`) pasa verde **sin tocar una sola aserción existente** — `git status` muestra
tres archivos de test NUEVOS y ningún archivo de test modificado.

### Los cambios de orden observable, enumerados uno por uno

Son **dos**, los dos acotados, y los dos van de incorrecto a correcto.

**1. `ListAPIKeys`, entre dos claves creadas dentro del MISMO SEGUNDO donde el texto recortado
miente.**

- Antes: la clave posterior podía salir primera, o la anterior última. Ocurre cuando el campo de
  nanosegundos de una de ellas termina en cero (aprox. 1 de cada 10 escrituras) o cuando una cae en
  un segundo exacto.
- Ahora: orden cronológico.
- Justificación: es literalmente el defecto que el contrato manda arreglar, y la salida de (a) lo
  muestra pasando de `[oldest, newest, middle]` a `[newest, middle, oldest]` en un listado declarado
  "newest first".
- Alcance real: dos claves de API acuñadas en el mismo segundo. En producción es raro (acuñar una
  clave es un acto manual del admin), pero **no imposible**, y en la UI se ve como claves fuera de
  orden. Entre segundos distintos el texto y el instante coinciden, así que **ahí no cambia nada** —
  que es por qué el transcripto de 82 líneas es idéntico.

**2. El TEXTO guardado en `api_keys.created_at` y `api_keys.revoked_at`, de acá en adelante.**

- Antes: `time.RFC3339Nano` recortado (`2026-07-25T12:00:25.5Z`).
- Ahora: ancho fijo (`2026-07-25T12:00:25.500000000Z`).
- **No es un cambio de orden observable**, y esto es medible, no una opinión: lo que la aplicación
  ve es el valor que compat devuelve, que se re-renderiza **recortado en los dos casos**, y el orden
  de `ListAPIKeys` lo decide Go por instante. El `ORDER BY created_at DESC` declarado en la rutina
  queda como base estable y su resultado se descarta. Por eso las 82 observaciones no se mueven.
- Justificación: es la mitad de T2 que sí se puede hacer sin tocar datos, y sin ella la aplicación
  seguiría **produciendo** filas en el formato que el escenario D demuestra incorrecto.
- El efecto es que las filas nuevas quedan bien ordenadas en SQL entre ellas y respecto de las de
  `internal/server`; las viejas quedan como están hasta que se decida la normalización.

**Lo que NO cambia:** los tres listados PAGINADOS de `internal/server` (`listArticles`,
`listProducts`, `listContentRows`) ordenan `created_at DESC, id` dentro del motor sobre filas que ya
se escriben en ancho fijo desde `CONTRACT-20`. Ni su SQL ni su formato de escritura se tocaron; el
escenario C es precisamente su caso, y sale correcto en los dos motores.

---

## Red-team: las preguntas del contrato, respondidas con evidencia

| Pregunta | Respuesta | Evidencia |
|---|---|---|
| **¿Un timestamp con zona distinta de UTC?** | Denota el mismo instante que su rendering en UTC y compara igual: `InstantSortValue` normaliza a UTC antes de renderizar. Probado en las dos direcciones (igual y anterior). | `TestInstantKeysHandleTheCarrierVariants/offset_vs_utc…` y `/offset_earlier_than_utc` |
| **¿Un texto vacío o nulo como clave de orden?** | Error visible, nombrando la clave, propagado al llamador. `RowText` devuelve `""` para un NULL, y `""` no parsea. `api_keys.created_at` es NOT NULL de todos modos, así que llegar ahí es una fila corrupta. | `TestInstantKeysFailVisiblyOnUnparseableText` (incluye `""`) y el test del slice de un elemento |
| **¿Dos instantes exactamente iguales — qué desempata, y es total?** | Desempata `label` y después `id`, igual que en `CONTRACT-20B`, y el orden sigue siendo TOTAL (`id` es PK). Lo nuevo es que ahora empatan **de verdad**: dos textos distintos que denotan el mismo instante (ancho fijo contra recortado, o zona distinta) comparan iguales y dejan decidir a la clave siguiente. Antes el CARRIER decidía. | `TestInstantKeysTieBreakOnTheNextKey` |
| **¿El formato viejo con espacio parsea igual en el camino nuevo?** | Sí. `timestampLayouts` **espeja** la lista de compat (`timestampFormats`) a propósito: aceptar menos rechazaría texto que la capa de datos considera válido, y aceptar más dejaría pasar algo que compat mismo habría rechazado. | `/legacy_space_form_parses` y `/legacy_space_form_is_older`; escenarios A/B1/B2 de la medición |
| **¿Qué pasa con un valor que compat aceptaría pero Go no?** | No puede haber uno: la lista es la misma, y además compat canonicaliza en la LECTURA — una fila que compat no puede parsear falla en `canonicalValue` antes de llegar al ordenamiento. Verificado: `TestListAPIKeysRejectsAnUnparseableCreatedAt` pasa también con el código de `main`, porque el error viene de compat. | `compat/store.go:424-428`; el test citado |
| **¿Quedan ordenamientos en Go con clave de timestamp sin arreglar?** | No. Se auditaron los ocho llamadores de `SortStrings`/`SortByKeys`: el único con clave de la familia `timestamp` es `ListAPIKeys`. | `grep` sobre `internal/` (`authz.go`, `terms.go`, `ui_users.go`, `users.go`, `roles.go`, `apikey.go`) |
| **¿Y los `ORDER BY` de SQL que quedan?** | Los paginados de `internal/server` ordenan por `created_at` en ancho fijo + `id` UUID: escenario C, correcto en los dos motores. `internal/store` sigue con SQL crudo atado a SQLite y es el `CONTRACT-21`. | Escenario C de la medición |

---

## Verificación — salida REAL

### build / vet / gofmt

```
$ go build ./... && go vet ./... && echo BUILD-VET-OK && gofmt -l . && echo GOFMT-DONE
BUILD-VET-OK
GOFMT-DONE

$ go vet -tags dualengine ./internal/auth/
VET-OK
```

(`gofmt -l .` no imprimió ningún archivo: la línea `GOFMT-DONE` sale inmediatamente después.)

### `go test ./... -count=1`, dos veces

```
=== RUN 1 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.102s
ok  	github.com/MauricioPerera/librarian/internal/auth	4.452s
ok  	github.com/MauricioPerera/librarian/internal/config	0.618s
ok  	github.com/MauricioPerera/librarian/internal/dual	1.193s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.280s
ok  	github.com/MauricioPerera/librarian/internal/server	36.736s
ok  	github.com/MauricioPerera/librarian/internal/store	3.691s

=== RUN 2 ===
ok  	github.com/MauricioPerera/librarian/cmd/librarian	2.466s
ok  	github.com/MauricioPerera/librarian/internal/auth	4.690s
ok  	github.com/MauricioPerera/librarian/internal/config	0.384s
ok  	github.com/MauricioPerera/librarian/internal/dual	1.349s
ok  	github.com/MauricioPerera/librarian/internal/schema	1.417s
ok  	github.com/MauricioPerera/librarian/internal/server	34.558s
ok  	github.com/MauricioPerera/librarian/internal/store	3.830s
```

### Las baterías de CONTRACT-19 y CONTRACT-20, contra los motores reales

PostgreSQL 17 con `pgvector`, verificado en vivo por las propias baterías.

```
$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable' \
    go test -tags dualengine -run TestDualEngineAuth -count=1 ./internal/auth
ok  	github.com/MauricioPerera/librarian/internal/auth	28.641s

$ COMPAT_POSTGRES_DSN='postgres://postgres:***@31.220.22.176:5443/postgres?sslmode=disable' \
    go test -tags dualengine -run 'TestDualEngineServer|TestDualEngineVectorPrecision' -count=1 ./internal/server
ok  	github.com/MauricioPerera/librarian/internal/server	45.356s
```

### El árbol, sin commitear y sin aserciones tocadas

```
$ git status --short
 M internal/auth/apikey.go
 M internal/auth/dual.go
 M internal/dual/dual.go
 M internal/server/dual.go
?? internal/auth/apikey_contract20c_test.go
?? internal/auth/dualengine_contract20c_test.go
?? internal/dual/instant_contract20c_test.go
?? specs/CONTRACT-20C-orden-de-timestamps.md

$ git diff --numstat
19	3	internal/auth/apikey.go
23	16	internal/auth/dual.go
162	2	internal/dual/dual.go
11	9	internal/server/dual.go
```

**Ningún archivo de test modificado**: los tres de este contrato son nuevos. Los cuatro archivos de
producción tocados suman 215 líneas agregadas y 30 borradas, y las 30 son el reemplazo de los tres
bloques descritos arriba (el ordenamiento de `ListAPIKeys`, `auth.now()` y
`canonicalTimestampLayout`) más sus comentarios.

### `sqlite-postgres-compat` no se tocó

`git status` en ese repo quedó exactamente como estaba al empezar (solo el
`experiments/vector/vector-exp.exe` sin trackear que ya estaba).

---

## Estado de los criterios de aceptación

| Criterio | Estado |
|---|---|
| build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces | **CUMPLIDO**, salida arriba |
| T1: comparaciones por instante; test del defecto con su fallo contra `main` documentado | **CUMPLIDO**, en DOS niveles (helper y ruta real), las dos salidas de fallo pegadas |
| T2: decisión con evidencia MEDIDA contra ambos motores; si pide normalizar datos, descrita y no implementada | **CUMPLIDO** — cuatro escenarios medidos, decisión "hace falta normalizar", SQL descrito y **NO ejecutado** |
| Baterías del 19 y del 20 verdes contra los dos motores reales | **CUMPLIDO** |
| Los cambios de orden observable, enumerados y justificados uno por uno | **CUMPLIDO** — dos casos, § Criterio 6 |

**Lo que queda en manos del orquestador:** decidir si se corre la normalización de datos descrita en
T2. Sin ella el sistema es correcto en todo lo observable hoy, pero el texto guardado sigue teniendo
dos poblaciones que un `ORDER BY` futuro ordenaría mal.

---

## Archivos tocados

**Nuevos**

- `internal/dual/instant_contract20c_test.go` — el test del defecto en el helper, más el modo de
  falla y las variantes del portador (T1/T3)
- `internal/auth/apikey_contract20c_test.go` — el test del defecto sobre `auth.ListAPIKeys`, la ruta
  real, con SQLite real (T1/T3)
- `internal/auth/dualengine_contract20c_test.go` — la medición de T2 contra los dos motores
- `docs/reports/CONTRACT-20C-REPORT.md` — este informe

**Modificados**

- `internal/dual/dual.go` — `ParseInstant`, `InstantSortValue`, `AscInstant`, `DescInstant`,
  `SortByKeysE`, `TimestampLayout`, `Now` (T1)
- `internal/auth/apikey.go` — `ListAPIKeys` ordena por instante y propaga el fallo (T1)
- `internal/auth/dual.go` — `now()` pasa a `dual.Now()`; la nota que revisa la decisión de 20B
- `internal/server/dual.go` — `canonicalTimestampLayout` pasa a ser `dual.TimestampLayout` y
  `nowCanonical()` a `dual.Now()`; una sola definición del formato de escritura

**NO tocados**

- `sqlite-postgres-compat` (todo el repo)
- `internal/store` (ni una línea)
- `internal/schema` (ninguna rutina, ningún `ORDER BY`, ninguna vista, ningún DEFAULT)
- `store.Open` y cualquier elección de motor (`CONTRACT-21`)
- El contrato público de las rutas HTTP
- **Los datos de producción**

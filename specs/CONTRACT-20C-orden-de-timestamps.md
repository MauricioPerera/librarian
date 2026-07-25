# Contrato 20C — Orden correcto de timestamps

Contrato **corto**, entre `CONTRACT-20B` y `CONTRACT-21`. NO toca `sqlite-postgres-compat`.

Cierra un defecto que `CONTRACT-20B` encontró y declaró sin arreglar, porque arreglarlo excedía su
alcance. No es una divergencia entre motores —los dos se equivocan igual— pero es **orden
cronológicamente incorrecto**, y el contrato 21 lo va a exponer más.

## El defecto, medido

La familia `timestamp` de `compat` se guarda y se lee como texto **RFC3339Nano**, que **recorta los
ceros finales** de la parte fraccionaria. Comparar esos textos lexicográficamente no es comparar
instantes:

```
cronológico real          ordenado como TEXTO (lo que hace el código hoy)
  …25.12345678Z             …25.123456781Z    <-- posterior, ordena primero
  …25.123456781Z            …25.12345678Z
  …25.5Z                    …25.5Z            <-- posterior, ordena antes que …25Z
  …25Z                      …25Z
```

Dos causas distintas en el mismo ejemplo: un nanosegundo que termina en cero produce un texto más
corto (`…8Z` contra `…81Z`, y `'Z'` > `'1'`), y un instante sin fracción produce `…25Z`, donde
`'Z'` > `'.'`, así que ordena después de cualquier `…25.<algo>Z` del mismo segundo.

Ocurre en cualquier comparación de texto sobre un valor de esa familia: tanto en el orden que
`CONTRACT-20B` impone en Go (que opera sobre el valor **ya canonicalizado por compat**, o sea
recortado) como en un `ORDER BY` de SQL sobre la columna.

## RECON ya resuelto (no re-investigar)

- **Compat re-renderiza todo timestamp al leerlo** (`canonicalValue` → `time.RFC3339Nano`), sea cual
  sea el texto guardado. O sea que el valor que ve la aplicación **siempre** viene recortado, y por
  eso el orden en Go hereda el defecto aunque la escritura use ancho fijo.
- **`internal/server` ya escribe ancho fijo** (`nowCanonical`, con el nanosegundo de ancho fijo);
  **`internal/auth` no** (`now()` usa `time.RFC3339Nano` pelado). Esa asimetría fue una decisión
  consciente de `CONTRACT-20B`, tomada porque el ancho fijo no cambiaba ningún orden **en Go**; con
  este contrato la premisa cambia.
- **Compat acepta al leer varios formatos** además del canónico (`timestampFormats` en
  `compat/store.go`), incluido el separador con espacio. Por eso las filas viejas se leen bien.
- **Producción tiene filas escritas con el formato anterior**, de cuando el valor lo ponía
  `CURRENT_TIMESTAMP` del motor: `2026-07-24 07:46:36` — separador espacio, sin zona, sin fracción.
  Convive hoy con filas nuevas en formato RFC3339Nano. Verificado sobre la base real.
- **Varias rutinas paginadas ordenan por `created_at` en SQL** (`internal/schema/server_dual.go`,
  entre ellas la de contenido dinámico). Ese `ORDER BY` compara el **texto crudo guardado**, no el
  canónico.

## T1 — Comparar instantes, no cadenas

FIX/OBJETIVO: toda comparación de un valor de la familia `timestamp` que hoy se hace como texto
debe comparar **instantes**. Un timestamp es un momento, no una cadena; que se transporte como
texto es un detalle del portador.

Aplicá el mismo criterio en todos los ordenamientos en Go que usen un timestamp como clave (los de
`internal/dual` y sus llamadores). Si el texto no parsea, la comparación debe fallar de forma
visible o degradar de una manera que esté **documentada y testeada** — no quedar en un
comportamiento accidental.

## T2 — El `ORDER BY` de SQL, decidido con evidencia

Un `ORDER BY` en SQL no puede parsear: compara el texto guardado. Para que ese orden sea correcto,
el texto tiene que ser uniformemente ordenable, y hoy **no lo es**: conviven el formato viejo del
motor y el nuevo de la aplicación, y el nuevo recorta ceros salvo donde ya se usa ancho fijo.

FIX/OBJETIVO: **analizá y decidí, con evidencia medida, no con razonamiento**. Las preguntas
concretas:

1. ¿Qué orden produce hoy la mezcla de formatos en una tabla real con filas de los dos? Medilo
   contra los dos motores, con datos que incluyan los dos formatos.
2. ¿Es seguro por construcción (una tabla donde `created_at` solo crece hace que las filas viejas
   sean siempre las más antiguas) o hay un caso donde se rompe?
3. Si hace falta uniformar el texto guardado, ¿alcanza con que la aplicación escriba siempre ancho
   fijo de acá en adelante, o exige además normalizar las filas existentes?

Si tu conclusión es que hace falta normalizar datos existentes, **NO la implementes**: describila
en el reporte como paso operativo con su SQL, y el orquestador decide. Cambiar datos de producción
no se delega.

Si tu conclusión es que es seguro sin tocar datos, tiene que estar respaldada por una prueba que
**falle** si alguien rompe la condición que lo hace seguro.

## T3 — Verificación

- `go build ./...`, `go vet ./...`, `gofmt -l .` vacío, `go test ./... -count=1` verde dos veces.
- Un test que fije el defecto: valores construidos a propósito (un nanosegundo terminado en cero,
  un instante sin fracción, y el mismo segundo con y sin fracción) ordenados **correctamente**. Ese
  test tiene que FALLAR con el código de `main` — mostralo en el reporte, como en `CONTRACT-20B`.
- Las baterías dual-motor de `CONTRACT-19` y `CONTRACT-20` verdes contra los dos motores reales.
- **El orden observable en SQLite no cambia** para datos que no disparan el defecto. Donde sí lo
  disparan, cambia de incorrecto a correcto: enumerá esos casos en el reporte.

## Criterios de aceptación

- [ ] build/vet/gofmt limpios; `go test ./... -count=1` verde dos veces.
- [ ] T1: comparaciones por instante; test del defecto, con su fallo contra `main` documentado.
- [ ] T2: decisión tomada con evidencia MEDIDA contra ambos motores; si pide normalizar datos, va
  descrita y no implementada.
- [ ] Baterías del 19 y del 20 verdes contra los dos motores reales.
- [ ] Los cambios de orden observable, enumerados y justificados uno por uno.

## Restricciones

- Tocar SOLO archivos dentro de `librarian`. NO toques `sqlite-postgres-compat`.
- Sin dependencias nuevas. NO commitear.
- NO cambies `store.Open` ni agregues elección de motor: es el `CONTRACT-21`.
- NO migres sentencias de `internal/store`: también es el 21.
- NO modifiques datos de producción ni escribas código que lo haga.
- El contrato público de las rutas HTTP no cambia.

## Checklist antes de delegar

- [ ] RECON corrido: entendido que compat re-renderiza recortado al leer, y que por eso el ancho
  fijo en la escritura no alcanza para el orden en Go.
- [ ] Red-team: ¿un timestamp con zona distinta de UTC? ¿Un texto vacío o nulo como clave de
  orden? ¿Dos instantes exactamente iguales — qué desempata, y es total? ¿El formato viejo con
  espacio parsea igual en el camino nuevo? ¿Qué pasa con un valor que compat aceptaría pero Go no?
- [ ] Perímetro: un solo dev, un solo perímetro.
- [ ] Infra PostgreSQL 17 con pgvector provista por el orquestador; password enmascarado.

# librarian — Definición

## Qué es

Sistema de administración headless (backend + API, sin panel visual en v1) para gestionar usuarios, roles, permisos, tipos de contenido y metadatos — el rol de datos/administración de WordPress, sin su capa de theming ni renderizado público.

## Arquitectura

Un único servicio en Go que expone una API JSON. Consume [`sqlite-postgres-compat`](https://github.com/MauricioPerera/sqlite-postgres-compat) como librería Go directa (no como subproceso ni CLI externo): el esquema de `librarian` (usuarios, roles, permisos, tipos de contenido) se declara con el modelo canónico de `compat` desde el día uno, para heredar su validación, compilación DDL dual-motor y su capacidad de exportación sin ventana de corte.

Almacenamiento primario: libSQL embebido en el proceso, corriendo en el VPS propio (ardf.dev). No hay motor de base de datos separado que administrar en v1.

Camino de exportación: cuando se requiera PostgreSQL, se usa el CLI `compat` (`audit` → `copy`/`cutover`) ya construido y auditado, contra la instancia libSQL existente — sin rearquitecturar `librarian` para ese momento.

No hay otros componentes en v1 (sin servicio de auth separado, sin UI separada): toda la lógica vive en el único binario.

## Capacidades objetivo

- Gestión de usuarios (alta, edición, estado).
- Roles y permisos: catálogo predefinido en código (no editable en runtime en v1), consistente con el modelo de tipos de contenido fijo.
- Tipos de contenido declarados en código: cada uno es una tabla real con columnas tipadas, `CHECK`, claves foráneas — usando la gramática canónica completa de `compat` (columnas generadas `STORED`, dominios, índices de expresión donde aplique).
- Metadatos extensibles sin migración: cada tipo de contenido incluye una columna `metadata JSON` de escape para campos no previstos en el esquema tipado (equivalente a `wp_postmeta`).
- Columnas de tipo `vector(N)` para almacenar embeddings que el cliente calcula y envía (sin pipeline de generación de embeddings ni búsqueda semántica integrada en v1).
- Autenticación dual: API keys por rol/servicio (integraciones servicio-a-servicio) y JWT con usuario/contraseña (usuarios humanos).
- Exportación bajo demanda a PostgreSQL vía `compat`, sin ventana de corte, cuando el crecimiento lo exija.

## Por qué es un caso válido / motivación real

Necesidad real de infraestructura: un backend de administración de contenido que arranca sin costo operativo (libSQL embebido, sin servidor de base de datos que gestionar) y puede migrar a PostgreSQL sin reescritura cuando la escala lo exija. Es además la validación de producción del caso de uso que motivó gran parte del trabajo reciente sobre `sqlite-postgres-compat`: un esquema diseñado dentro de la gramática canónica desde el día uno, no una base heredada evaluada después.

## Fuera de alcance

- Panel de administración / UI web (fase futura, planeada en htmx + CSS moderno; no se construye en v1).
- Tipos de contenido definidos dinámicamente en runtime por el admin (se descartó el modelo tipo Strapi/Directus; agregar un tipo de contenido nuevo implica deploy).
- Pipeline de generación de embeddings o búsqueda semántica integrada (v1 solo almacena vectores que el cliente ya calculó).
- PostgreSQL como almacenamiento primario o por defecto — libSQL es el motor primario; Postgres es un destino de exportación bajo demanda, no un requisito de v1.
- Hosting gestionado (Turso, Cloudflare D1/Workers) — v1 corre en el VPS propio (ardf.dev).
- Theming, renderizado público o cualquier capa de sitio publicable (nunca estuvo en alcance; `librarian` es puramente el backend de administración/API).
- Nombres exactos de rutas de API, formato exacto de tokens JWT, layout de tablas — decisiones de implementación que se cierran en el primer contrato de ejecución (PLAN), no acá.

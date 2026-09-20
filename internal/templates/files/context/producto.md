# Estado actual del producto

> Mantengan esta lista al día después de cada ciclo. Es lo que el Product Owner
> usa para decidir si una petición de los docentes ya está cubierta.
>
> Levantado el 2026-09-18 y reconciliado el 2026-09-19 contra `yanai` en
> `2d9d41d`, leyendo el repositorio y los `SPEC.md` de cada carpeta.
>
> **Este documento separa cuatro cosas y nunca las mezcla:**
>
> | | Qué es | Dónde vive |
> |---|---|---|
> | **Alcance previsto** | lo que decidimos construir | `alcance.md` |
> | **Implementación observada** | lo que el código hace hoy, leído y fechado | las tablas de abajo |
> | **Comportamiento probado** | lo que una prueba automatizada garantiza | hoy: **nada** |
> | **Evidencia de uso** | que un docente real lo use | hoy: **nada** |
>
> Que algo esté construido no es evidencia de que se use. Que algo compile no es
> evidencia de que funcione. Las dos últimas columnas están vacías a propósito y
> el alcance prohíbe publicar cifras no medidas.
>
> **La ausencia de entrevistas en este checkout no es evidencia de que no
> existan.** Si tienes notas de campo, entran por `interviews/`; hasta entonces
> lo correcto es decir que no las tenemos a la vista, no que no las hay.

## Cómo leer esto contra el alcance

`alcance.md` declara que la unidad atómica del producto es la **observación**: un
hecho fechado, atado a un estudiante y a una competencia. **El código no tiene
una entidad "observación".** Lo más cercano es la **nota de voz** atada a una
sesión y a un estudiante. La superficie de observaciones por sesión existió y fue
retirada (`internal/httpapi/SPEC.md`). Toda propuesta que hable de
"observaciones" hay que traducirla primero a lo que existe: nota de voz,
evidencia (assessment), criterio, o nivel de logro.

## Funcionalidades que ya existen

| # | Funcionalidad | Implementación observada | Comportamiento probado | Evidencia de uso |
|---|---------------|--------------------------|------------------------|------------------|
| 1 | Autenticación real del docente: usuario + contraseña bcrypt, cookie de sesión HttpOnly, tabla `user_sessions`, bloqueo por intentos fallidos, expiración absoluta y por inactividad | construido | ninguno | — |
| 2 | **Captura de nota de voz** por presionar y sostener, máximo 60s, normalizada a MP3 con ffmpeg y guardada en base de datos; atada a la sesión y al estudiante que el docente tocó | construido | ninguno | — |
| 3 | **Transcripción automática** de la nota de voz al español vía OpenAI (`gpt-4o-mini-transcribe`), en cola de trabajos con reintentos; el transcrito se muestra junto al audio | construido | ninguno | — |
| 4 | Borrado suave y guardado de notas de voz, con autoguardado (sin botón de enviar) | construido | ninguno | — |
| 5 | **Registro del nivel de logro (AD/A/B/C)** por estudiante, competencia y periodo — el registro oficial MINEDU (`competency_term_levels`); solo lo escribe un acto explícito del docente | construido | ninguno | — |
| 6 | **Conclusión descriptiva** por estudiante y competencia: el docente la **escribe a mano**, con estado borrador/final | construido | ninguno | — |
| 7 | El servidor deriva **si la conclusión descriptiva es obligatoria y por qué** (`level_c`, `level_b`, `cycle_i_mandatory`, `consecutive_c`, `inclusion_support`, `optional`) a partir del nivel vigente y el grado; nunca se confía en el cliente | construido | ninguno | — |
| 8 | **Detección de C consecutivo**: si la competencia estuvo en C en dos o más periodos seguidos, la conclusión queda clasificada `consecutive_c` — obligatoria y debe justificar la falta de progreso | construido | ninguno | — |
| 9 | **Criterios de evaluación** escritos por el docente por competencia (autoría); son la vara contra la que se juzga la evidencia | construido | ninguno | — |
| 10 | **Registro de evidencia** (prueba calificada, examen de recuperación, tarea) para toda la clase o un estudiante, etiquetada con la competencia y los criterios que evidencia | construido | ninguno | — |
| 11 | Notas/marcas sobre la evidencia según `value_scale` (numérica, ordinal o ninguna), con corrección que supersede en lugar de sobrescribir (historial append-only) | construido | ninguno | — |
| 12 | Notas de práctica no oficiales (`practice_scores`), crear y eliminar | construido | ninguno | — |
| 13 | **Home: agenda del día en rejilla horaria proporcional** (1 hora = 64px, ventana 07:00–17:00), tira lunes–viernes, marcador de hora actual; cada sesión abre su pantalla de grabación | construido | ninguno | — |
| 14 | Pantalla de **área/clase**: sesiones pasadas, en curso y próximas, más criterios y evidencia de clase | construido | ninguno | — |
| 15 | Pantalla de **alumnos** de la clase, acotada a una competencia, con el nivel de cada estudiante y su conteo de evidencia | construido | ninguno | — |
| 16 | Pantalla del **estudiante para evaluar**: la evidencia del periodo con sus valores, y debajo la decisión del nivel de logro | construido | ninguno | — |
| 17 | Pantalla de **notas del estudiante** (lectura completa): registro oficial primero, luego evidencia, luego el promedio interno opcional colapsado | construido | ninguno | — |
| 18 | **Sistema interno ponderado opcional del colegio** (D.S. 009-2006-ED art. 37.4.f): esquemas, versiones, pesos, descartar-la-más-baja, políticas de redondeo y recuperación; calculado en cola y **amurallado** del registro oficial | construido | ninguno | — |
| 19 | Feed de **notificaciones** con marcar-leída y marcar-todas; distingue alertas de recordatorios | construido (solo lectura) | ninguno | — |
| 20 | Perfil del docente, y su lista "MIS CLASES" como entrada a un área sin depender de la fecha | construido | ninguno | — |
| 21 | **Multi-inquilino con RLS forzado** por colegio; toda consulta corre en transacción de inquilino | construido | ninguno | — |
| 22 | **Auditoría de acceso a datos de menores**: evento `student.read` en toda respuesta que devuelve filas identificables; tabla `events` particionada | construido | ninguno | — |
| 23 | Cola de trabajos en Postgres in-process (`SELECT … FOR UPDATE SKIP LOCKED`), con backoff exponencial; dos tipos: `grade.recompute` y `voice_note.transcribe` | construido | ninguno | — |
| 24 | Esquema de privacidad (`privacy`) para descubrir y auditar columnas con datos personales (Ley N° 29733) | construido | ninguno | — |
| 25 | Despliegue: imagen distroless, Caddy, `docker-compose.prod.yml`, CI que construye la imagen | construido | ninguno | — |

## Lo que el alcance pide y todavía NO existe

Esto es lo más importante para el PO: si un docente pide algo de esta lista, **no
está cubierto**, aunque suene parecido a algo de la tabla de arriba.

| Petición del alcance | Qué falta exactamente |
|----------------------|------------------------|
| Nota de voz **estructurada automáticamente** (estudiante, fecha, curso, competencia, evidencia textual) | La transcripción es texto plano. **No hay ningún paso que extraiga competencia ni evidencia del transcrito.** El estudiante, la fecha y el curso se conocen por la sesión que el docente tocó, no por el análisis del audio. |
| Captura manual de observaciones **por texto** | No existe. No hay entidad de observación ni campo de texto libre equivalente a la nota de voz. |
| **Generación de la conclusión descriptiva** a partir de las observaciones | No existe. El docente la escribe entera. No hay llamada a un modelo de lenguaje en el servidor: la única integración de IA es la transcripción de audio. |
| Mostrar **las observaciones que sustentan** el texto propuesto | No existe, porque no hay texto propuesto. |
| **Edición y aprobación explícita** de texto generado antes de guardarlo | Hay estado borrador/final en la conclusión, que sirve de andamio, pero no hay nada generado que aprobar. |
| **Alerta** de C consecutivo | La *clasificación* existe (#8) y se muestra como etiqueta al abrir la competencia. Pero **nada inserta notificaciones**: la tabla `notifications` solo se lee. El docente no recibe aviso; tiene que entrar a mirar. |
| **Carga de evidencias (foto, archivo)** vinculadas a estudiante y competencia | No existe. La única subida multipart del servidor es audio de nota de voz. "Evidencia" en el código es un assessment con notas, no un archivo adjunto. |
| **Exportación del registro** del docente | No existe. Ninguna ruta, ningún CSV, ningún formato de salida. |

## Decisiones de producto ya tomadas

- **La evaluación es por criterios, no ponderada.** El nivel de logro por
  competencia sale del juicio del docente sobre la evidencia contra los
  criterios que él escribió (RVM 094-2020, mod. 048-2024). Las notas de pruebas
  y exámenes son evidencia; **nunca alimentan aritméticamente** el registro
  oficial.
- **El promedio ponderado es el sistema interno opcional del colegio**, no el
  registro oficial, y está explícitamente separado y etiquetado como tal.
  Se activa con `tenant.uses_internal_grading`.
- **Un nivel de logro solo lo escribe un acto explícito del docente.** Ningún
  trabajo en cola, ningún cálculo y ninguna sugerencia puede escribirlo ni
  insinuarlo. `suggested_level_id` y `computed_grade_id` quedan nulos a
  propósito.
- **Nada predictivo.** No hay proyecciones, ni banderas de riesgo, ni pronóstico
  de situación final. El subsistema predictivo se retiró completo.
- **Un criterio nunca recibe un nivel de logro.** El criterio es la vara, no el
  juicio oficial. El nivel de logro de una competencia se afirma solo por acto
  explícito del docente (`competency_term_levels`) y no se deriva de nada; no
  existe ruta que registre un nivel de logro para un criterio y no se permite
  añadir una.
  Eso **no** quiere decir que un criterio no se marque. Un criterio sí lleva una
  marca por evidencia dentro de una assessment, en `criterion_scores`: numérica
  (`raw_value`/`max_value`) o por nivel de rúbrica (`scale_level_id`, la misma
  escala AD/A/B/C). Las rutas existen (`POST /assessments/{id}/criterion-scores`
  y su corrección) y están vivas. Esa marca es evidencia y nunca se agrega para
  producir el nivel oficial.
  ⚠️ Esa marca **no tiene cliente en `yanai-ui`**: capacidad del servidor sin
  consumidor. Decidir construirlo o retirarlo es una pregunta abierta.
- **El registro de calificaciones es append-only.** Una corrección supersede la
  fila anterior (`superseded_at`); nada se actualiza en sitio ni se borra.
- **No hay asistencia en la app.** Las dos rutas de asistencia siguen vivas en el
  servidor pero la interfaz que las usaba se retiró; el cliente no las llama.
- **No hay integración técnica con SIAGIE** (ni lectura ni escritura), por
  decisión de alcance.
- **Una fila tocada en Home es la sesión en la que se graba.** Home nunca enlaza
  al área directamente; el área se alcanza desde la pantalla de grabación o
  desde la lista de clases del perfil.
- **Lo generado por el asistente se marca por forma, no por color**: insignia
  rellena para lo generado, delineada para lo que la app deriva.
- **El audio vive en la base de datos**, no en almacenamiento de objetos, y el
  transcrito es purgable por separado del audio.
- **Sin interfaz de proveedor para STT ni para media**: una sola implementación,
  llamada directo. La abstracción se gana cuando aparece el segundo proveedor.
- **Nada corre periódicamente**: no hay ticker ni planificador; todo trabajo se
  dispara por una acción del docente.

## Peticiones postergadas

| Petición | De qué ciclo viene | Por qué se postergó |
|----------|--------------------|---------------------|
| — | — | Todavía no hay ciclos de entrevistas. Esta tabla se llena al cerrar el primero. |

## Deuda conocida

- **El backend ya tiene pruebas; la UI no.** Desde el 2026-09-20 `yanai-server`
  corre una suite contra un PostgreSQL 16 desechable, bajo el rol real
  `yanai_app`, y CI la ejecuta en cada push con `-race -shuffle=on`. Cubre el
  aislamiento por colegio, la autorización por clase, el protocolo de
  supersesión de notas, las reglas de las marcas por criterio, que un nivel de
  logro no se deriva de nada, el login y la resolución de sesión, la cola de
  trabajos y el recálculo interno. `yanai-ui` sigue **sin ninguna prueba**.
  La columna "Comportamiento probado" de la tabla de arriba sigue vacía a
  propósito: dice qué garantiza una prueba *para esa funcionalidad*, y la suite
  cubre contratos transversales, no una fila por pantalla. Llenarla exige
  pruebas por funcionalidad, que todavía no existen.
- **El squash de migraciones ya está cerrado.** `migrations/` tiene un solo
  archivo, `00001_initial_schema.sql` (~2900 líneas, 43 tablas), y
  `migrations/SPEC.md` existe. La regla vigente es: toda obra de esquema de aquí
  en adelante es una **migración goose numerada nueva**, nunca una reescritura
  de la anterior.
- **La tabla `notifications` no tiene productor.** La pantalla y las rutas de
  lectura existen; nada escribe filas. El feed está vacío por construcción.
- **`yanai-ui` está desactualizado en el contrato de descarte de nota de voz**:
  `discardVoiceNote()` sigue tipando el retorno como
  `Promise<{status: VoiceNoteStatus}>` cuando el servidor responde `204`. No
  rompe hoy porque nadie lee el valor, pero está marcado para cerrarse.
- **El digest del recálculo ignora ediciones en sitio del esquema** de
  calificación: si se edita un esquema sin cambiar de versión, el recálculo lo
  salta.
- **No hay segador de arrendamientos** en la cola: un trabajo cuyo worker muere
  se queda en `running`; hay que diagnosticarlo por `attempted_by`/`attempted_at`.
- **`grade_scales.passing_value` y `grade_scale_levels.is_passing` ya no
  existen.** Codificaban "nota 11 aprobatoria", vigente solo para Secundaria en
  2020, y fueron retiradas del esquema; esta nota decía que seguían ahí sin uso,
  y es falso (verificado el 2026-09-20 contra el esquema real). "Aprobado" no es
  una propiedad de un nivel de logro: AD/A/B/C no llevan nota.
- **La sesión no rota por petición**, solo se emite al iniciar sesión, para
  evitar cierres intermitentes cuando el layout dispara `/me` y el sondeo de
  notificaciones en paralelo.
- **Dos transacciones por petición autenticada** (resolver sesión, luego
  resolver docente), costo asumido del orden del problema: no se puede conocer
  el inquilino antes que la sesión.
- **Sin uso medido.** No hay analítica, ni entrevistas, ni evidencia de que
  ninguna de las 25 funcionalidades construidas se use en aula. Es la deuda más
  grande para decidir producto.

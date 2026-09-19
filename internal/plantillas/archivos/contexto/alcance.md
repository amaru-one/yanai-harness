# Alcance del proyecto

> Este documento lo mantienen **ustedes**, no los agentes. Es la vara con la que
> el Product Owner acepta o rechaza propuestas. Mientras más concreto, mejor
> protege el alcance.

## Qué estamos construyendo

Una aplicación para **docentes** de educación básica en escuelas peruanas que
convierte la observación cotidiana del aula en un registro estructurado por
estudiante, y usa ese registro para generar conclusiones descriptivas alineadas
al CNEB.

La unidad atómica del producto es la **observación**: un hecho fechado, atado a
un estudiante y a una competencia. Todo lo que el producto hace es capturar
observaciones o leerlas. Una propuesta que no capture ni lea observaciones
está, por defecto, fuera del alcance.

## Quién la usa en esta etapa

Solo docentes de aula. Las vistas de **estudiante** y **coordinador** están
planificadas para más adelante y **quedan fuera de este esfuerzo**.

## Dentro del alcance

- Captura de observaciones por **nota de voz**, transcritas y estructuradas
  automáticamente (estudiante, fecha, curso, competencia, evidencia textual).
- Captura manual de observaciones por texto, como alternativa a la voz.
- Registro de **nivel de logro** (AD / A / B / C) por estudiante y competencia,
  por periodo.
- Carga de evidencias (foto, archivo) vinculadas a un estudiante y a una
  competencia.
- Definición por el docente de los criterios de evaluación del periodo.
- **Generación de la conclusión descriptiva** por estudiante y competencia, a
  partir de las observaciones registradas, con las observaciones que la
  sustentan visibles junto al texto propuesto.
- Edición y **aprobación explícita del docente** antes de que cualquier texto
  generado quede guardado en el registro.
- Alerta de **C consecutivo** cuando un estudiante recibe C en dos o más
  periodos seguidos en la misma competencia, con apoyo para la justificación
  formal.
- Exportación del registro del docente en un formato que pueda copiarse o
  cargarse fuera de la app.

## Fuera del alcance

- Cualquier pantalla dirigida a estudiantes o apoderados.
- Carga o autocorrección de evidencias por parte del estudiante.
- Reportes institucionales o de gestión para el director o el coordinador.
- Estadística comparativa entre secciones, docentes o colegios.
- Seguimiento del desempeño del docente por parte del colegio.
- Planificación de sesiones, unidades o currículas.
- Generación o recomendación de material didáctico.
- Integración técnica con SIAGIE (lectura o escritura automática).
- Comunicación con familias, mensajería, asistencia.
- Chatbot de consulta libre sobre los datos.
- Personalizaciones pedidas por un colegio en particular que no apliquen a
  cualquier otro colegio (pilotear ≠ acomodar).

## Restricciones que no se negocian

- Equipos modestos y celulares.
- Cumplimiento del CNEB y de la RVM N° 094-2020-MINEDU, modificada por la
  RVM N° 048-2024-MINEDU.
- Protección de datos personales de menores (Ley N° 29733).
- El docente es dueño de su registro y decide qué se comparte.
- Ningún texto generado entra al registro sin aprobación explícita del docente.
- Ningún texto generado puede afirmar algo que no esté sustentado en una
  observación registrada.
- Ninguna cifra de impacto (tiempo ahorrado, mejora de resultados) se publica
  sin haber sido medida.

## Definición de "listo"

- Registrar una observación por voz toma **3 pasos o menos** desde la pantalla
  de inicio.
- Toda conclusión descriptiva generada **muestra las observaciones que la
  sustentan**, y el docente puede rastrear cada afirmación hasta su origen.
- El docente puede editar el texto generado antes de aprobarlo, y nada se
  guarda hasta que apruebe.
- La funcionalidad se probó en un **celular de gama media**, no solo en
  escritorio.
- Los datos de estudiantes están cifrados en tránsito y en reposo, y el acceso
  está restringido al docente que los registró.
- Existen pruebas automatizadas del esquema de datos y del circuito de
  generación y aprobación.
- Un docente que no participó en el diseño completa la tarea sin que nadie del
  equipo le explique cómo.
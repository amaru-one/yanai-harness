# yanai — equipo de agentes para la aplicación docente

Un binario en Go que corre cuatro agentes sobre **OpenRouter** con tus claves.
Todo lo que producen queda en markdown, en carpetas que puedes leer y versionar
en git. **Nada se implementa sin que una persona apruebe el plan.**

El paquete `internal/workflow` añade contratos tipados, validación de
dependencias y estado durable SQLite para nuevos flujos. El CLI existente
mantiene sus ciclos legibles y la compuerta humana; consulta `WORKFLOW.md` para
los límites y la migración.

## El equipo

| Rol | Identificador | Qué hace |
|-----|---------------|----------|
| Product Owner | `product-owner` | Lee las entrevistas, saca insights, valida lo existente, protege el alcance y reparte tareas |
| Arquitecto de BD | `arquitecto-bd` | Modelo de datos alineado al CNEB y a la RVM N° 094-2020-MINEDU, diagramas ER y DDL |
| Software Engineer | `ingeniero` | Implementación en Go y Svelte |
| Diseñador UI/UX | `disenador` | Maquetas HTML, simplicidad como requisito |

## El flujo

```
  entrevistas
       │
       ▼
  yanai analyze ──► VEREDICTO: SUFICIENTE ──► reporte y fin del ciclo
       │
       └──────────► VEREDICTO: NUEVO_PLAN
                         │
                         ▼
                   yanai discuss
              (arquitecto → diseñador → ingeniero,
               cada uno ve lo que dijo el anterior;
               luego el PO consolida y asigna tareas)
                         │
                         ▼
                ⏸  awaiting_approval   ◄── AQUÍ SE DETIENE
                    │              │
        yanai approve          yanai reject --note "..."
                    │              │
                    ▼              └──► vuelve a 'yanai discuss'
              yanai run             con tu motivo en el contexto
        (cada agente entrega sus archivos)
```

La compuerta es real: `run` falla con error si el ciclo no está aprobado.

## Instalación

```bash
go build -o yanai ./cmd/yanai
sudo mv yanai /usr/local/bin/     # o déjalo donde quieras
```

Sin dependencias externas. Go 1.24 o superior.

## Puesta en marcha

```bash
yanai init --repo /ruta/a/tu/app-docente --ws ./yanai-workspace
export OPENROUTER_API_KEY=sk-or-...
```

Antes del primer ciclo, edita dos archivos. Son los que hacen la diferencia
entre un equipo útil y uno que inventa:

- **`contexto/alcance.md`** — la vara con la que el PO rechaza propuestas. Si
  está vacío, el PO no puede proteger nada.
- **`contexto/producto.md`** — qué existe hoy en la app. Es lo que el PO usa
  para decidir si una petición del docente ya está cubierta.

Actualiza `producto.md` al cerrar cada ciclo.

## Uso

```bash
yanai analyze entrevistas/docente-01.md    # abre el ciclo
yanai discuss                              # mesa de trabajo + plan
# lee ciclos/001/04-plan.md con calma
yanai approve                               # o: yanai reject --note "..."
yanai run                                   # solo corre si aprobaste
yanai status                                # en qué va todo
```

Puedes pasarle varias entrevistas juntas:

```bash
cat entrevistas/*.md | yanai analyze -
```

## Probar sin gastar la clave

```bash
YANAI_MOCK=1 yanai analyze entrevistas/docente-01.md
```

Recorre el flujo completo con respuestas simuladas. Útil para ver la forma de
las carpetas y para probar cambios en el harness.

## Qué queda en disco

```
yanai-workspace/
  yanai.config.json          modelos, repo, parámetros
  prompts/                   el prompt de cada agente — edítalos, son tuyos
  context/
    alcance.md               lo mantienen ustedes
    producto.md              lo mantienen ustedes
  interviews/                tus notas de campo
  cycles/001/
    00-entrada.md            las entrevistas tal como entraron
    02-propuesta.md          insights + propuesta del PO
    02-reporte-suficiencia.md   (si el veredicto fue SUFICIENTE)
    03-discusion.md          lo que dijo cada especialista
    04-plan.md               decisión, conflictos resueltos, recortes, tareas
    05-aprobacion.md         tu decisión y tu motivo
    entregables/
      arquitecto-bd/T-001/
        respuesta.md         razonamiento del agente
        archivos/            los archivos que produjo
      disenador/T-002/...
      ingeniero/T-003/...
    state.json               fase, tareas y bitácora
```

Los entregables **no se escriben en tu repositorio**. Quedan en el ciclo para
que los revises y los muevas tú. Esa fricción es a propósito.

## Configuración

`yanai.config.json`:

- `repo.path` — ruta al repositorio de la app docente.
- `repo.extensions`, `repo.exclude_dirs`, `repo.priority` — qué ve el equipo.
- `repo.max_bytes_total` — tope del contexto. Súbelo si tu repo crece y tu
  presupuesto de tokens lo aguanta.
- `agents.<rol>.model` — cualquier modelo de OpenRouter. Puedes usar uno fuerte
  para el PO y el arquitecto y uno más barato para el resto.
- `agents.<rol>.temperature` — baja para el arquitecto y el ingeniero, más alta
  para el diseñador.
- `discussion_order` — el orden importa: quien habla último responde a todos.

Variables de entorno:

| Variable | Para qué |
|----------|----------|
| `OPENROUTER_API_KEY` | tu clave |
| `YANAI_MOCK=1` | simula respuestas, no llama a la API |
| `YANAI_NO_REPO=1` | omite la lectura del repositorio |
| `YANAI_WS` | espacio de trabajo por defecto |

## Cosas que conviene saber

- **Los prompts son el producto.** El código solo mueve texto. Cuando un agente
  entregue algo flojo, edita su archivo en `prompts/` antes de cambiar el modelo.
- **Un ciclo, un problema.** Si el plan tiene ocho tareas, probablemente el PO
  no recortó lo suficiente. Rechaza con ese motivo.
- **El rechazo es barato y es la herramienta principal.** `yanai reject --note`
  guarda tu motivo y el equipo rehace el plan teniéndolo a la vista.
- **Revisa las citas.** El PO cita las entrevistas; verifica que las citas
  existan de verdad en `00-entrada.md`.
- **Verifica lo normativo.** El prompt del arquitecto le exige marcar con
  `⚠️ VERIFICAR CON LA NORMA VIGENTE` todo lo que no pueda confirmar. Busca esas
  marcas y confírmalas contra el texto oficial antes de construir.
- **Versiona el workspace en git.** Los ciclos son el registro de por qué el
  producto es como es.

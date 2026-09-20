# yanai — equipo de agentes para la aplicación docente

Un binario en Go que corre cuatro agentes sobre **OpenRouter** con tus claves.
Todo lo que producen queda en markdown, en carpetas que puedes leer y versionar
en git. **Nada se implementa sin que una persona apruebe el plan.**

El paquete `internal/workflow` trae contratos tipados, validación de
dependencias y estado durable en SQLite. **Todavía no está conectado al CLI**:
existe y está probado, y el flujo actual sigue corriendo sobre `internal/team`.
Conectarlo es trabajo pendiente.

## El equipo

| Rol | Identificador | Qué hace |
|-----|---------------|----------|
| Product Owner | `product-owner` | Lee las entrevistas, saca insights, valida lo existente, protege el alcance y reparte tareas |
| Arquitecto de BD | `arquitecto-bd` | Modelo de datos alineado al CNEB y a la RVM N° 094-2020-MINEDU, diagramas ER y DDL |
| Software Engineer | `ingeniero` | Implementación del backend en Go (chi, pgx) |
| Diseñador UI/UX | `disenador` | Revisa el recorrido del docente; simplicidad como requisito |

**En esta etapa el trabajo es de backend.** `yanai-ui` queda fuera: no se lee,
no se modifica y no se generan archivos de frontend. El diseñador sigue siendo
parte del equipo y revisa necesidades y requisitos de interacción sin entregar
maquetas.

## El flujo

```
  entrevistas
       │
       ▼
  yanai analyze ──► NO_CHANGE_NEEDED ──► reporte y fin del ciclo
       │            NEEDS_EVIDENCE    ──► reporte: falta averiguar
       │            OUT_OF_SCOPE      ──► reporte: fuera de alcance
       │            BLOCKED_BY_BASELINE ► reporte: falta base probada
       │
       └──────────► VEREDICTO: PROPOSE_CHANGE
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

Go 1.24 o superior y Git en `PATH`. La compuerta de ejecución usa bloqueos
del sistema operativo en macOS y Linux.

## Puesta en marcha

```bash
cd /ruta/a/yanai-harness
yanai init --ws ./yanai-workspace   # descubre ../yanai y guarda la ruta canónica
export OPENROUTER_API_KEY=sk-or-...
```

Los dos repositorios viven al mismo nivel:

```text
proyectos/
  yanai-harness/
    yanai-workspace/        configuración y estado del equipo
  yanai/
    yanai-server/           módulo Go de la aplicación
    yanai-ui/              fuera de alcance
```

También puedes indicar `yanai init --repo ../yanai --ws ./yanai-workspace`.
`--repo` se resuelve desde el directorio donde invocas el comando. El valor
guardado es absoluto y canónico. Si editas `repo.path` a mano para usar una ruta
relativa, se resuelve desde la carpeta de `yanai.config.json`, nunca desde el
directorio del siguiente comando. Desde otro directorio, pasa el mismo `--ws`.

Sin `--repo`, un workspace existente conserva su destino. Para uno nuevo se
busca el checkout hermano desde el directorio de invocación o el del binario;
si no se reconoce el layout, `init` pide una ruta explícita. El destino debe ser
la raíz Git de Yanai y contener `yanai-server/go.mod` con módulo `yanai-server`.
El workspace no puede contener el destino ni estar dentro de él.

Antes del primer ciclo, edita dos archivos. Son los que hacen la diferencia
entre un equipo útil y uno que inventa:

- **`context/alcance.md`** — la vara con la que el PO rechaza propuestas. Si
  está vacío, el PO no puede proteger nada.
- **`context/producto.md`** — qué existe hoy en la app. Es lo que el PO usa
  para decidir si una petición del docente ya está cubierta.

Actualiza `producto.md` al cerrar cada ciclo.

## Uso

```bash
yanai analyze interviews/docente-01.md    # abre el ciclo
yanai discuss                              # mesa de trabajo + plan
# lee cycles/001/04-plan.md con calma
yanai approve                               # o: yanai reject --note "..."
yanai run                                   # solo corre si aprobaste
yanai status                                # en qué va todo
```

Puedes pasarle varias entrevistas juntas:

```bash
cat interviews/*.md | yanai analyze -
```

## Probar sin gastar la clave

```bash
YANAI_MOCK=1 yanai analyze interviews/docente-01.md
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
    02-reporte-suficiencia.md   (si el ciclo cerró sin plan)
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

**Hoy** los entregables no se escriben en tu repositorio: quedan en el ciclo
para que los revises y los muevas tú.

Eso es una limitación del estado actual, no la meta. El objetivo del proyecto es
que una tarea aprobada produzca el cambio real en `yanai`, con sus
verificaciones corridas ahí y revisión independiente antes de darla por
terminada. Mientras el ejecutor controlado no exista, mover los archivos a mano
es el paso que falta — y revisarlos antes de moverlos sigue siendo tuyo.

El enlace de Step 3 ya valida que las rutas de los archivos candidatos
pertenezcan al backend. Rechaza rutas absolutas, `..`, enlaces simbólicos,
archivos ignorados, rutas ocultas y archivos de credenciales reconocibles,
incluido `.env`. El índice y las lecturas explícitas comparten esa política;
pedir un archivo con `context --files` no evita los filtros.

`run` exige un checkout limpio (también sin archivos no rastreados), y toma un
bloqueo exclusivo en los metadatos Git durante toda la ejecución. Workspaces
distintos y worktrees del mismo repositorio comparten el bloqueo. Al terminar
el proceso se libera, incluso si murió; el archivo del bloqueo se conserva y
no se debe borrar para desbloquear una ejecución activa. El harness nunca hace
stash, reset ni limpieza de tus cambios.

La planificación puede leer un checkout sucio, pero lo marca `DIRTY` y esa base
no permite ejecutar. Tras resolver los cambios, vuelve a generar y aprobar el
plan. La base incluye la identidad local del checkout, HEAD y su estado de
limpieza: cambiar a otro clon con el mismo commit también invalida la base.
Los planes anteriores a Step 3 deben regenerarse. El contrato completo de
aprobación y el ejecutor de patches siguen pendientes en Steps 7 y 8.

## Cómo se actualiza un workspace existente

`yanai init` se puede volver a correr sobre un workspace que ya existe. Compara
tres versiones de cada archivo de plantilla — la que tienes en disco, la que
este binario te entregó la última vez (anotada en
`.yanai-template-manifest.json`) y la que trae embebida — y te dice qué pasó:

- **creado** — no estaba, se escribió.
- **actualizado** — cambió río arriba y tú no lo habías tocado: se actualiza
  solo.
- **cambió río arriba — se conserva el tuyo** — lo editaste y además cambió río
  arriba. Tu archivo queda intacto y el nuevo se escribe al lado como
  `<nombre>.new` para que los juntes tú. El aviso se repite en cada `init`
  hasta que borres el `.new`.
- **sin cambios** — no tenemos nada nuevo que ofrecerte; lo que hayas hecho con
  ese archivo es asunto tuyo.

**Un archivo que modificaste nunca se sobrescribe.** Un workspace anterior a
este mecanismo no tiene manifiesto: en ese caso se asume que editaste todo y no
se toca nada.

La excepción explícita es `init --repo`: cambia únicamente `repo.path` mediante
JSON estructurado, conserva modelos y campos desconocidos, y también repara
configuraciones antiguas con `../app-docente`. Sin ese argumento, una ruta
antigua inválida produce un error; no se sustituye por otro repositorio por
suposición. Un `init` normaliza a ruta canónica el destino ya configurado.

## Configuración

`yanai.config.json`:

- `schema_version` — la generación de plantillas con que se creó el workspace.
- `repo.path` — raíz Git de la aplicación, enlazada por `init`. La plantilla
  embebida la deja vacía hasta que se valida el destino.
- `repo.allowed_paths` — por defecto `AGENTS.md` y `yanai-server`. Puede acotar
  estos caminos; en esta etapa no puede ampliarlos al frontend u otros proyectos.
  Las configuraciones anteriores que omiten el campo reciben el mismo límite.
- `repo.extensions`, `repo.exclude_dirs`, `repo.priority` — qué ve el equipo.
  `yanai-ui` viene excluido: el frontend está fuera de alcance en esta etapa.
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
| `YANAI_NO_REPO=1` | omite contexto al planificar; no evita validar el destino y está prohibido en `run` |
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

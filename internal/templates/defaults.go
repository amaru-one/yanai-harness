package templates

// defaultConfig is a public starter configuration. Repository binding is filled
// by init; operators review the remaining settings before planning work.
// Agents start empty: the parent proposes the worker for each ticket.
const defaultConfig = `{
  "schema_version": 10,
  "project": "Engineering project",
  "repo": {
    "path": "",
    "allowed_paths": ["."],
    "extensions": [],
    "exclude_dirs": [],
    "priority": [],
    "max_file_bytes": 24000,
    "max_bytes_total": 400000
  },
  "openrouter": {
    "base_url": "https://openrouter.ai/api/v1",
    "api_key_env": "OPENROUTER_API_KEY",
    "referer": "https://github.com/yanai/yanai-harness",
    "title": "Yanai - agent team",
    "timeout_seconds": 600,
    "retries": 3
  },
  "orchestrator": {
    "model": "",
    "temperature": 0.2,
    "max_tokens": 16000,
    "max_steps": 20,
    "budget": {
      "max_tokens": 0,
      "max_cost_usd": 0,
      "max_active_seconds": 0,
      "max_calls": 0,
      "prices": {}
    }
  },
  "execution": {
    "max_tokens": 0,
    "max_cost_usd": 0,
    "max_active_seconds": 0,
    "max_calls": 0,
    "max_repairs": 0,
    "prices": {},
    "checks": [],
    "commit": false,
    "merge": false,
    "deploy": false,
    "destructive_db": false,
    "tools": {}
  },
  "models": {},
  "agents": {}
}
`

// alcanceTemplate is the project scope document. It changes rarely.
const alcanceTemplate = `# Project scope

## Vision and purpose

## Domain and users

## Stack and architecture

## Modules and boundaries
<!-- directory → responsibility -->

## Conventions
<!-- style, tests, naming, commits; prompts and Markdown must be in English -->

## Out of scope

## Constraints
<!-- security, data, compatibility -->
`

// estadoTemplate is the current project state. The parent proposes an update
// after every ticket and a human accepts it with 'yanai close'.
const estadoTemplate = `# Project state

## What exists and works
<!-- by module -->

## Partially implemented

## Technical debt and known bugs

## Latest resolved tickets
<!-- ticket, type, branch, commits, summary -->

## Recent decisions

## Open risks
`

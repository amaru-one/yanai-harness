package templates

// defaultConfig is a public starter configuration. Repository binding is filled
// by init; operators review the remaining settings before planning work.
const defaultConfig = `{
  "schema_version": 9,
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
  "agents": {
    "arquitecto-bd": {
      "name": "Arquitecto de BD",
      "model": "google/gemini-2.5-pro",
      "temperature": 0.2,
      "max_tokens": 16000,
      "prompt": "prompts/arquitecto-bd.md"
    },
    "ingeniero": {
      "name": "Software Engineer",
      "model": "qwen/qwen3-coder-plus",
      "temperature": 0.2,
      "max_tokens": 16000,
      "prompt": "prompts/ingeniero.md"
    },
    "disenador": {
      "name": "Diseñador UI/UX (revisión)",
      "model": "openai/gpt-5.1",
      "temperature": 0.5,
      "max_tokens": 16000,
      "prompt": "prompts/disenador.md"
    }
  }
}
`

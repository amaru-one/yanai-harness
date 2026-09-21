// Package config loads and validates yanai.config.json.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yanai/yanai-harness/internal/workflow"
)

// Agent describes a member of the team.
type Agent struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	Prompt      string  `json:"prompt"` // relative path within the workspace
}

// Repo describes how to read the source code of the teaching app.
type Repo struct {
	Path          string   `json:"path"`
	AllowedPaths  []string `json:"allowed_paths,omitempty"`
	Extensions    []string `json:"extensions"`
	ExcludeDirs   []string `json:"exclude_dirs"`
	Priority      []string `json:"priority"`
	MaxBytesFile  int      `json:"max_file_bytes"`
	MaxBytesTotal int      `json:"max_bytes_total"`
	// MaxSelectedFiles caps how many files an agent can request after seeing
	// the Index (repoctx.Index), regardless of how many it asks for in its
	// response. It's the safety net for the two-pass selection: without
	// this, a runaway response could request half the repository.
	MaxSelectedFiles int `json:"max_selected_files"`
}

// OpenRouter groups the model provider configuration.
type OpenRouter struct {
	BaseURL    string `json:"base_url"`
	APIKeyEnv  string `json:"api_key_env"`
	Referer    string `json:"referer"`
	Title      string `json:"title"`
	TimeoutSec int    `json:"timeout_seconds"`
	Retries    int    `json:"retries"`
}

// Config is the full file.
type Config struct {
	Execution       workflow.ExecutionPolicy `json:"execution"`
	Project         string                   `json:"project"`
	Repo            Repo                     `json:"repo"`
	OpenRouter      OpenRouter               `json:"openrouter"`
	Agents          map[string]Agent         `json:"agents"`
	DiscussionOrder []string                 `json:"discussion_order"`

	path string
}

// Fixed team roles. The flow depends on these identifiers. The values
// match the (unchanged, Spanish) prompt filenames and config keys they
// map to, so they are intentionally not translated.
const (
	RolePO        = "product-owner"
	RoleArchitect = "arquitecto-bd"
	RoleEngineer  = "ingeniero"
	RoleDesigner  = "disenador"
)

// ValidRoles lists the identifiers the flow recognizes.
var ValidRoles = []string{RolePO, RoleArchitect, RoleEngineer, RoleDesigner}

// Load reads the config from the workspace and applies default values.
func Load(ws string) (*Config, error) {
	abs, err := filepath.Abs(ws)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	path := filepath.Join(abs, "yanai.config.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s (did you run 'yanai init'?): %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s has invalid JSON: %w", path, err)
	}
	c.path = path
	c.applyDefaults()
	if c.Repo.Path != "" && !filepath.IsAbs(c.Repo.Path) {
		c.Repo.Path = filepath.Join(abs, c.Repo.Path)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.OpenRouter.BaseURL == "" {
		c.OpenRouter.BaseURL = "https://openrouter.ai/api/v1"
	}
	if c.OpenRouter.APIKeyEnv == "" {
		c.OpenRouter.APIKeyEnv = "OPENROUTER_API_KEY"
	}
	if c.OpenRouter.TimeoutSec == 0 {
		c.OpenRouter.TimeoutSec = 300
	}
	if c.OpenRouter.Retries == 0 {
		c.OpenRouter.Retries = 3
	}
	if c.Repo.MaxBytesFile == 0 {
		c.Repo.MaxBytesFile = 24000
	}
	if c.Repo.MaxBytesTotal == 0 {
		c.Repo.MaxBytesTotal = 400000
	}
	if c.Repo.MaxSelectedFiles == 0 {
		c.Repo.MaxSelectedFiles = 15
	}
	if len(c.DiscussionOrder) == 0 {
		c.DiscussionOrder = []string{RoleArchitect, RoleDesigner, RoleEngineer}
	}
	for id, a := range c.Agents {
		if a.ID == "" {
			a.ID = id
		}
		if a.MaxTokens == 0 {
			a.MaxTokens = 8000
		}
		if a.Prompt == "" {
			a.Prompt = filepath.Join("prompts", id+".md")
		}
		c.Agents[id] = a
	}
}

func (c *Config) validate() error {
	for _, r := range ValidRoles {
		a, ok := c.Agents[r]
		if !ok {
			return fmt.Errorf("missing agent %q in %s", r, c.path)
		}
		if a.MaxTokens <= 0 || a.MaxTokens > 10000000 {
			return fmt.Errorf("agent %q max_tokens must be between 1 and 10000000", r)
		}
		if a.Model == "" {
			return fmt.Errorf("agent %q has no 'model' (e.g. anthropic/claude-sonnet-4.5)", r)
		}
	}
	return nil
}

// Agent returns the configuration for a role.
func (c *Config) Agent(role string) (Agent, error) {
	a, ok := c.Agents[role]
	if !ok {
		return Agent{}, fmt.Errorf("unknown role: %s", role)
	}
	return a, nil
}

// APIKey reads the key from the configured environment variable.
func (c *Config) APIKey() string { return os.Getenv(c.OpenRouter.APIKeyEnv) }

// SetRepoPath updates just the binding, preserving model choices and unknown
// fields from newer configurations. The caller supplies a validated absolute path.
func SetRepoPath(ws, path string) error {
	name := filepath.Join(ws, "yanai.config.json")
	data, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	var repo map[string]json.RawMessage
	if err := json.Unmarshal(doc["repo"], &repo); err != nil {
		return err
	}
	if repo == nil {
		return fmt.Errorf("configuration requires a repo object")
	}
	repo["path"], err = json.Marshal(path)
	if err != nil {
		return err
	}
	doc["repo"], err = json.Marshal(repo)
	if err != nil {
		return err
	}
	data, err = json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(ws, ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}

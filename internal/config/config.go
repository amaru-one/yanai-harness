// Package config loads and validates yanai.config.json.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yanai/yanai-harness/internal/workflow"
)

// Agent describes a worker the parent created (or an operator declared).
// Worker IDs are data, not code: nothing in the harness branches on them.
type Agent struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Purpose     string  `json:"purpose,omitempty"`
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	// MaxSteps bounds the worker's model turns in one run. Every turn,
	// including an invalid answer, consumes a step.
	MaxSteps int    `json:"max_steps,omitempty"`
	Prompt   string `json:"prompt"` // relative path within the workspace
	// ModelCategory names the catalog category the parent chose Model from.
	ModelCategory string `json:"model_category,omitempty"`
}

// ModelCategory is one kind of role and the models the parent may choose
// from for it. The operator owns the catalog; the parent can only choose.
type ModelCategory struct {
	Description string        `json:"description"`
	Options     []ModelOption `json:"options"`
}

// ModelOption is one model the parent may choose, with the operator's notes on
// what it is good at. The parent reasons only from these notes.
type ModelOption struct {
	Model string `json:"model"`
	// Difficulty lists the task levels (see Difficulties) this model handles.
	Difficulty []string `json:"difficulty"`
	Strengths  string   `json:"strengths"`
	Weaknesses string   `json:"weaknesses,omitempty"`
	// ContextTokens is the model's context window: prompt, history, tool
	// results and answer together.
	ContextTokens int `json:"context_tokens"`
	// ReasoningMaxTokens, when set, caps the model's reasoning per call
	// (OpenRouter's reasoning.max_tokens). Leave it unset for models that do
	// not support a cap, or where sending it would switch thinking on.
	ReasoningMaxTokens int `json:"reasoning_max_tokens,omitempty"`
}

// Difficulties are the task levels, from easiest to hardest.
var Difficulties = []string{"low", "medium", "high"}

// ValidDifficulty reports whether d is one of Difficulties.
func ValidDifficulty(d string) bool {
	for _, x := range Difficulties {
		if x == d {
			return true
		}
	}
	return false
}

// Covers reports whether the option declares difficulty d.
func (o ModelOption) Covers(d string) bool {
	for _, x := range o.Difficulty {
		if x == d {
			return true
		}
	}
	return false
}

// MaxModelOptions bounds the choices offered per category.
const MaxModelOptions = 5

// Orchestrator configures the parent agent. Its budget is separate from the
// worker's execution budget and is captured when a cycle starts, so nothing
// the parent proposes can raise the limits it is currently running under.
type Orchestrator struct {
	Model       string                  `json:"model"`
	Temperature float64                 `json:"temperature"`
	MaxTokens   int                     `json:"max_tokens"`
	MaxSteps    int                     `json:"max_steps"`
	Budget      workflow.PlanningBudget `json:"budget"`
}

// Repo describes how to read the source code of the target project.
type Repo struct {
	Path          string   `json:"path"`
	ModuleDir     string   `json:"module_dir,omitempty"`
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
	SchemaVersion int                      `json:"schema_version,omitempty"`
	Execution     workflow.ExecutionPolicy `json:"execution"`
	Project       string                   `json:"project"`
	Repo          Repo                     `json:"repo"`
	OpenRouter    OpenRouter               `json:"openrouter"`
	Orchestrator  Orchestrator             `json:"orchestrator"`
	// Models is the operator's model catalog, by role category.
	Models map[string]ModelCategory `json:"models,omitempty"`
	Agents map[string]Agent         `json:"agents"`
	// Commands holds the operator's command policy; only the operator edits
	// it (a proposal's config_changes cannot).
	Commands Commands `json:"commands,omitempty"`

	path string
}

// Commands configures agent commands (run_command).
type Commands struct {
	// AutoApprove lists commands that run without asking the human.
	AutoApprove []workflow.AutoApproveRule `json:"auto_approve,omitempty"`
}

// FileName is the workspace configuration file.
const FileName = "yanai.config.json"

// Reserved IDs belong to the harness and can never name a worker.
var reserved = map[string]bool{"parent": true, "engine": true, "human": true}

var workerID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// ValidWorkerID reports whether id can name a worker: lowercase letters,
// digits and hyphens, starting with a letter, at most 64 characters, and not
// reserved for the harness.
func ValidWorkerID(id string) error {
	if !workerID.MatchString(id) || reserved[id] {
		return fmt.Errorf("invalid worker id %q: use lowercase letters, digits and hyphens, start with a letter, at most 64 characters; parent, engine and human are reserved", id)
	}
	return nil
}

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
	path := filepath.Join(abs, FileName)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s (did you run 'yanai init'?): %w", path, err)
	}
	c, err := Parse(b, abs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.path = path
	return c, nil
}

// Parse validates configuration bytes exactly as Load does, relative to the
// workspace root. It is how a parent's proposed configuration is checked
// before a human ever sees it, without touching the live file.
func Parse(data []byte, workspace string) (*Config, error) {
	var c Config
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&c); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	c.path = filepath.Join(workspace, FileName)
	c.applyDefaults()
	if c.Repo.Path != "" && !filepath.IsAbs(c.Repo.Path) {
		c.Repo.Path = filepath.Join(workspace, c.Repo.Path)
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
	if c.Agents == nil {
		c.Agents = map[string]Agent{}
	}
	for id, a := range c.Agents {
		if a.ID == "" {
			a.ID = id
		}
		if a.MaxTokens == 0 {
			a.MaxTokens = 8000
		}
		if a.MaxSteps == 0 {
			a.MaxSteps = DefaultWorkerSteps
		}
		if a.Prompt == "" {
			a.Prompt = filepath.ToSlash(filepath.Join("prompts", id+".md"))
		}
		c.Agents[id] = a
	}
	if c.Orchestrator.MaxTokens == 0 {
		c.Orchestrator.MaxTokens = 16000
	}
	if c.Orchestrator.MaxSteps == 0 {
		c.Orchestrator.MaxSteps = DefaultParentSteps
	}
}

// Starter step limits: parent turns per planning or closing phase, and worker
// turns per run.
const (
	DefaultParentSteps = 20
	DefaultWorkerSteps = 100
)

func (c *Config) validate() error {
	for _, rule := range c.Commands.AutoApprove {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("commands: %w", err)
		}
	}
	if c.Orchestrator.MaxTokens < 0 || c.Orchestrator.MaxTokens > 10000000 || c.Orchestrator.MaxSteps < 0 || c.Orchestrator.MaxSteps > 1000 {
		return fmt.Errorf("orchestrator max_tokens must be between 1 and 10000000 and max_steps between 1 and 1000")
	}
	for key, a := range c.Agents {
		if err := validateAgent(key, a); err != nil {
			return err
		}
	}
	for key, category := range c.Models {
		if err := validateCategory(key, category); err != nil {
			return err
		}
	}
	return nil
}

func validateCategory(key string, category ModelCategory) error {
	if err := ValidWorkerID(key); err != nil {
		return fmt.Errorf("model category %q: use lowercase letters, digits and hyphens, starting with a letter", key)
	}
	if strings.TrimSpace(category.Description) == "" {
		return fmt.Errorf("model category %q needs a description", key)
	}
	if len(category.Options) == 0 || len(category.Options) > MaxModelOptions {
		return fmt.Errorf("model category %q needs between 1 and %d options", key, MaxModelOptions)
	}
	seen := map[string]bool{}
	covered := map[string]bool{}
	for _, o := range category.Options {
		m := o.Model
		if strings.TrimSpace(m) == "" || m != strings.TrimSpace(m) || seen[m] {
			return fmt.Errorf("model category %q has a blank or repeated option %q", key, m)
		}
		seen[m] = true
		if strings.TrimSpace(o.Strengths) == "" {
			return fmt.Errorf("model %s (category %s) needs \"strengths\"", m, key)
		}
		if o.ContextTokens <= 0 {
			return fmt.Errorf("model %s (category %s) needs \"context_tokens\": its context window in tokens", m, key)
		}
		if o.ReasoningMaxTokens < 0 {
			return fmt.Errorf("model %s (category %s) has a negative \"reasoning_max_tokens\"", m, key)
		}
		if len(o.Difficulty) == 0 {
			return fmt.Errorf("model %s (category %s) needs \"difficulty\": some of %s", m, key, strings.Join(Difficulties, ", "))
		}
		levels := map[string]bool{}
		for _, d := range o.Difficulty {
			if !ValidDifficulty(d) || levels[d] {
				return fmt.Errorf("model %s (category %s) has an invalid or repeated difficulty %q; use %s", m, key, d, strings.Join(Difficulties, ", "))
			}
			levels[d] = true
			covered[d] = true
		}
	}
	for _, d := range Difficulties {
		if !covered[d] {
			return fmt.Errorf("model category %q has no option for difficulty %q; together its options must cover %s", key, d, strings.Join(Difficulties, ", "))
		}
	}
	return nil
}

// Option returns the catalog entry for model within category.
func (c *Config) Option(category, model string) (ModelOption, bool) {
	for _, o := range c.Models[category].Options {
		if o.Model == model {
			return o, true
		}
	}
	return ModelOption{}, false
}

// CategoryOf reports whether model is an option of category.
func (c *Config) CategoryOf(category, model string) bool {
	_, ok := c.Option(category, model)
	return ok
}

func validateAgent(key string, a Agent) error {
	if err := ValidWorkerID(key); err != nil && !legacyRole(key) {
		return err
	}
	if a.ID != key {
		return fmt.Errorf("agent %q declares a different id %q", key, a.ID)
	}
	if a.MaxTokens <= 0 || a.MaxTokens > 10000000 {
		return fmt.Errorf("agent %q max_tokens must be between 1 and 10000000", key)
	}
	if a.MaxSteps <= 0 || a.MaxSteps > 10000 {
		return fmt.Errorf("agent %q max_steps must be between 1 and 10000", key)
	}
	if strings.TrimSpace(a.Model) == "" {
		return fmt.Errorf("agent %q has no 'model' (e.g. z-ai/glm-5)", key)
	}
	if a.Temperature < 0 || a.Temperature > 2 {
		return fmt.Errorf("agent %q temperature must be between 0 and 2", key)
	}
	p := filepath.ToSlash(filepath.Clean(a.Prompt))
	if a.Prompt == "" || filepath.IsAbs(a.Prompt) || p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsAny(a.Prompt, "\\\x00\r\n") {
		return fmt.Errorf("agent %q prompt must be a workspace-relative path", key)
	}
	return nil
}

// legacyRole keeps historical workspaces readable: their role names carry no
// special behavior any more, but a config that still lists them must load.
func legacyRole(id string) bool {
	return id == "ingeniero" || id == "arquitecto-bd" || id == "disenador"
}

// ValidateOrchestrator reports whether the parent can make paid calls.
func (c *Config) ValidateOrchestrator() error {
	o := c.Orchestrator
	if strings.TrimSpace(o.Model) == "" {
		return fmt.Errorf("configure orchestrator.model in %s before planning", FileName)
	}
	if err := o.Budget.Validate(); err != nil {
		return fmt.Errorf("orchestrator.budget: %w", err)
	}
	if _, ok := o.Budget.Prices[o.Model]; !ok {
		return fmt.Errorf("configure orchestrator.budget.prices upper bounds for %s before paid calls", o.Model)
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("configure the \"models\" catalog in %s before planning: the parent chooses the worker's model from it", FileName)
	}
	for key, category := range c.Models {
		for _, o := range category.Options {
			if _, ok := c.Execution.Prices[o.Model]; !ok {
				return fmt.Errorf("model %s (category %s) needs an execution.prices upper bound before planning", o.Model, key)
			}
		}
	}
	return nil
}

// Path is where this configuration was loaded from.
func (c *Config) Path() string { return c.path }

// Agent returns the configuration for a role.
func (c *Config) Agent(role string) (Agent, error) {
	a, ok := c.Agents[role]
	if !ok {
		return Agent{}, fmt.Errorf("unknown role: %s", role)
	}
	return a, nil
}

// APIKey reads the key from the configured environment variable.

// SetRepoPath updates just the binding, preserving model choices and unknown
// fields from newer configurations. The caller supplies a validated absolute path.
func SetRepoPath(ws, path string) error { return SetRepoBinding(ws, path, "", nil) }

func SetRepoBinding(ws, path, module string, allowed []string) error {
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
	if module != "" {
		repo["module_dir"], _ = json.Marshal(module)
	}
	if allowed != nil {
		repo["allowed_paths"], _ = json.Marshal(allowed)
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

// Package local performs Pando's AI functions with a model served on your own
// hardware, by any server that speaks the OpenAI Chat Completions API: Ollama,
// LM Studio, llama.cpp's server, vLLM.
//
// The prompts, tools, budgeted reader and answer shapes are aikit's, shared
// with every AI adapter. What is here is how a conversation is held with a
// server that may be small, slow, and uneven at calling tools: a longer
// timeout, and an answer written as JSON in the reply accepted where a tool
// call was asked for. Core validates every answer regardless (R-027), so a
// weaker model can be wrong but cannot be trusted further than any other.
//
// Nothing leaves the network it is on, which is the reason to choose it. The
// audit event still records what was read (R-337): the server is somebody's
// machine, and what was sent to it is still worth knowing.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/secret"
)

// Kind is what adapter_configs stores.
const Kind = "local"

// DefaultBaseURL is Ollama on the machine Pando's container runs on. [P]
// Pando usually runs in Docker, where localhost is the container itself, and
// Docker Desktop names the host host.docker.internal; on Linux the Compose
// file maps that name to the host gateway.
const DefaultBaseURL = "http://host.docker.internal:11434/v1"

// Defaults for a model on local hardware.
const (
	DefaultMaxFiles       = 30
	DefaultMaxBytes       = 192 << 10
	DefaultTimeoutSeconds = 300
	maxIterations         = 24
	maxOutputTokens       = 4096
)

// Config is the adapter's own configuration.
type Config struct {
	// Credentials are supplied by core, decrypted at startup (O-20). Most local
	// servers take no key; vLLM and a proxy in front of one may.
	Credentials Credentials `json:"credentials,omitzero"`
	APIKeyEnv   string      `json:"api_key_env,omitempty"`

	// BaseURL is the server's OpenAI-compatible root, ending in /v1.
	BaseURL string `json:"base_url,omitempty"`

	// Model is required: there is no model every local server has.
	Model string `json:"model,omitempty"`

	// ScreenPlans defaults to true (R-336). False turns off repairing plans,
	// answering questions and revising plans.
	ScreenPlans *bool `json:"screen_plans,omitempty"`

	MaxFiles int   `json:"max_files,omitempty"`
	MaxBytes int64 `json:"max_bytes,omitempty"`

	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Credentials is what this adapter needs kept secret (R-194).
type Credentials struct {
	APIKey secret.Value `json:"api_key,omitzero"`
}

// Adapter is the local model adapter.
type Adapter struct {
	cfg    Config
	client openai.Client
	ready  bool
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryAI }

// Configure reads the adapter's configuration.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg Config
	if len(raw) > 0 {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			return fmt.Errorf("local: reading configuration: %w", err)
		}
		if _, inline := top["api_key"]; inline {
			return errors.New("local: api_key is in this adapter's stored configuration, which is " +
				"unencrypted; set it as a credential instead")
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("local: reading configuration: %w", err)
		}
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return errors.New("local: no model is set — set model to one your server serves, such as qwen2.5:7b " +
			"(ollama list shows them)")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = DefaultMaxFiles
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = DefaultTimeoutSeconds
	}

	key := cfg.Credentials.APIKey
	if key.IsZero() && cfg.APIKeyEnv != "" {
		key = secret.New(os.Getenv(cfg.APIKeyEnv))
	}
	cfg.Credentials.APIKey = key
	// The SDK sends a key whatever happens; a server that takes none ignores
	// this one. Never OPENAI_API_KEY from the environment: that is a key for
	// OpenAI, and sending it to somebody's local server would be a leak.
	sent := "local"
	if !key.IsZero() {
		sent = key.Reveal()
	}

	a.cfg = cfg
	a.client = openai.NewClient(
		option.WithAPIKey(sent),
		option.WithBaseURL(cfg.BaseURL),
		option.WithRequestTimeout(time.Duration(cfg.TimeoutSeconds)*time.Second),
		// Local servers restart; a transient refusal is worth one more try.
		option.WithMaxRetries(1),
	)
	a.ready = true
	return nil
}

// HealthCheck asks the server for its models and checks the configured one
// is among them, so "the server is down" and "the model was never pulled"
// each say which.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	if !a.ready {
		return errors.New("local: not configured")
	}
	page, err := a.client.Models.List(ctx)
	if err != nil {
		return fmt.Errorf("local: the server at %s did not answer: %w", a.cfg.BaseURL, err)
	}
	var served []string
	for _, m := range page.Data {
		if m.ID == a.cfg.Model {
			return nil
		}
		served = append(served, m.ID)
	}
	sort.Strings(served)
	return fmt.Errorf("local: the server at %s does not serve %s; it serves: %s",
		a.cfg.BaseURL, a.cfg.Model, strings.Join(served, ", "))
}

// Capabilities reports what this adapter does, as data (R-254, R-259).
func (a *Adapter) Capabilities(_ context.Context) (api.AICapabilities, error) {
	caps := api.AICapabilities{
		Model:        a.cfg.Model,
		ChoosesModel: true,
		MaxFiles:     a.cfg.MaxFiles,
		MaxBytes:     a.cfg.MaxBytes,

		// LooksUp is left off. [P] A small model is uneven at calling tools,
		// and a lookup loop multiplies the rounds a slow server serves; core
		// searches the words of the request instead and sends the matches
		// (O-54, design 10 §10.6).
	}
	if a.cfg.ScreenPlans == nil || *a.cfg.ScreenPlans {
		caps.Functions = append(caps.Functions,
			api.AIFunctionRepairPlan, api.AIFunctionAnswerQuestions, api.AIFunctionRevisePlan)
	}
	caps.Functions = append(caps.Functions,
		api.AIFunctionDraftAccess, api.AIFunctionDraftPolicy,
		api.AIFunctionSearchAudit, api.AIFunctionAnswerReference)
	return caps, nil
}

func (a *Adapter) model(assigned string) string {
	if assigned != "" {
		return assigned
	}
	return a.cfg.Model
}

var _ api.AIAdapter = (*Adapter)(nil)

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category: api.CategoryAI,
		Kind:     Kind,
		Name:     "Local model",
		Description: "Performs the AI functions assigned to it with a model on your own hardware, through any " +
			"server that speaks the OpenAI API: Ollama, LM Studio, llama.cpp or vLLM. Nothing is sent to a provider.",
		IDPrefix: "ai_",
		Fields: []api.Field{
			{Key: "base_url", Label: "Server URL", Type: "string", Help: "The server’s OpenAI-compatible address, ending in /v1. The default is Ollama on the machine running Pando’s container.", Default: DefaultBaseURL},
			{Key: "model", Label: "Model", Type: "string", Required: true, Help: "The model each function uses unless its assignment names another, as the server names it.", Placeholder: "qwen2.5:7b"},
			{Key: "api_key", Label: "API key", Type: "string", Help: "Only if your server asks for one. Stored encrypted and never shown again.", Credential: true},
			{Key: "timeout_seconds", Label: "Timeout, in seconds", Type: "int", Help: "How long one request may take. Local models are slower than hosted ones.", Default: "300", Advanced: true},
		},
	}
}

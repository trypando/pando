// Package openai performs Pando's AI functions with OpenAI's models, through
// the Responses API.
//
// The second AI adapter. The prompts, tools, budgeted reader and answer shapes
// are aikit's, shared with every AI adapter; what is here is how a
// conversation is held with OpenAI. It returns amendments and drafts core
// validates again, because an adapter proposes and core decides (R-027).
//
// Nothing here writes. The repository is read through a SourceView, which has
// no write method, and the tools a model is given are a listing and a read.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/secret"
)

// Kind is what adapter_configs stores.
const Kind = "openai"

// DefaultModel is what an install gets without choosing. [P] OpenAI's newest
// general model when this was written; an install sets its own, and each
// function may run on another (R-259).
const DefaultModel = "gpt-5.5"

// Budget ceilings this adapter will not exceed regardless of what it is asked
// for. Core lowers them further; neither side raises the other's.
const (
	DefaultMaxFiles = 40
	DefaultMaxBytes = 256 << 10
	maxIterations   = 24
	maxOutputTokens = 8192
)

// Config is the adapter's own configuration.
type Config struct {
	// Credentials are supplied by core, decrypted from adapter_credentials at
	// startup (O-20). Never part of the stored configuration: the database
	// refuses a `credentials` key in adapter_configs.config.
	Credentials Credentials `json:"credentials,omitzero"`

	// APIKeyEnv names an environment variable holding the key. When neither
	// this nor a stored credential is set, OPENAI_API_KEY is read — the SDK's
	// own convention.
	APIKeyEnv string `json:"api_key_env,omitempty"`

	Model string `json:"model,omitempty"`

	// BaseURL points at a gateway or a proxy. Empty is the OpenAI API.
	BaseURL string `json:"base_url,omitempty"`

	// ScreenPlans defaults to true (R-336). False turns off repairing plans,
	// answering questions and revising plans, for an install that wants the
	// adapter for the administrative functions only.
	ScreenPlans *bool `json:"screen_plans,omitempty"`

	MaxFiles int   `json:"max_files,omitempty"`
	MaxBytes int64 `json:"max_bytes,omitempty"`

	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Credentials is what this adapter needs kept secret. A secret.Value, so it
// renders [redacted] in every marshaler and cannot reach a log line (R-194).
type Credentials struct {
	APIKey secret.Value `json:"api_key,omitzero"`
}

// Adapter is the OpenAI adapter.
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
		// A key at the top level was stored in the clear. The create handler
		// and the database both refuse one; refusing it here too means a row
		// written some other way does not quietly work.
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			return fmt.Errorf("openai: reading configuration: %w", err)
		}
		if _, inline := top["api_key"]; inline {
			return errors.New("openai: api_key is in this adapter's stored configuration, which is " +
				"unencrypted; set it as a credential instead")
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("openai: reading configuration: %w", err)
		}
	}

	key := resolveKey(cfg, os.Getenv)
	if key.IsZero() {
		return errors.New("openai: no API key was found — set one as this adapter's api_key " +
			"credential, or name an environment variable in api_key_env")
	}
	cfg.Credentials.APIKey = key
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = DefaultMaxFiles
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}

	opts := []option.RequestOption{option.WithAPIKey(cfg.Credentials.APIKey.Reveal())}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.TimeoutSeconds > 0 {
		opts = append(opts, option.WithRequestTimeout(time.Duration(cfg.TimeoutSeconds)*time.Second))
	}

	a.cfg = cfg
	a.client = openai.NewClient(opts...)
	a.ready = true
	return nil
}

// HealthCheck asks the API whether the configured model exists: a key that
// does not work and a model nobody serves both answer here as they would to
// a real call, and the lookup costs nothing.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	if !a.ready {
		return errors.New("openai: not configured")
	}
	if _, err := a.client.Models.Get(ctx, a.cfg.Model); err != nil {
		return fmt.Errorf("openai: %w", err)
	}
	return nil
}

// Capabilities reports what this adapter does, as data (R-254, R-259).
func (a *Adapter) Capabilities(_ context.Context) (api.AICapabilities, error) {
	caps := api.AICapabilities{
		Model:        a.cfg.Model,
		ChoosesModel: true,
		MaxFiles:     a.cfg.MaxFiles,
		MaxBytes:     a.cfg.MaxBytes,
	}
	if a.screensPlans() {
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

func (a *Adapter) screensPlans() bool {
	return a.cfg.ScreenPlans == nil || *a.cfg.ScreenPlans
}

// resolveKey finds the credential: the stored one, then the named variable,
// then the SDK's default variable.
func resolveKey(cfg Config, getenv func(string) string) secret.Value {
	if !cfg.Credentials.APIKey.IsZero() {
		return cfg.Credentials.APIKey
	}
	if cfg.APIKeyEnv != "" {
		return secret.New(getenv(cfg.APIKeyEnv))
	}
	return secret.New(getenv("OPENAI_API_KEY"))
}

var _ api.AIAdapter = (*Adapter)(nil)

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category: api.CategoryAI,
		Kind:     Kind,
		Name:     "OpenAI",
		Description: "Performs the AI functions assigned to it with OpenAI's models. Uses OpenAI's Responses API, " +
			"which stores each conversation on OpenAI's servers under OpenAI's retention terms; Pando does not store it. " +
			"Needs an OpenAI API key.",
		IDPrefix: "ai_",
		Fields: []api.Field{
			{Key: "api_key", Label: "API key", Type: "string", Help: "An OpenAI API key. Stored encrypted and never shown again. Leave empty to use OPENAI_API_KEY from Pando’s environment.", Credential: true, Placeholder: "sk-…"},
			{Key: "model", Label: "Model", Type: "string", Help: "The model each function uses unless its assignment names another.", Default: DefaultModel},
			{Key: "base_url", Label: "Base URL", Type: "string", Help: "A gateway or proxy in front of the OpenAI API. Empty is the API itself.", Default: "https://api.openai.com/v1", Advanced: true},
			{Key: "api_key_env", Label: "API key variable", Type: "string", Help: "The environment variable to read the key from, instead of a stored one.", Default: "OPENAI_API_KEY", Advanced: true},
		},
	}
}

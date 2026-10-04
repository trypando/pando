// Package anthropic screens deployment plans with Claude (design 10 §6).
//
// The first AI adapter, and the one the interface in adapter/api was shaped
// against. It talks to the Messages API through the official Go SDK, reads the
// repository through the SourceView it is handed, and returns amendments drawn
// from the closed set — which core then validates again, because an adapter
// proposes and core decides (R-027).
//
// Nothing here writes. SourceView has no write method, and the tools this
// adapter exposes to the model are a listing and a read.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/secret"
)

// Kind is what adapter_configs stores.
const Kind = "anthropic"

// DefaultModel is what an install gets without choosing.
//
// [P] It runs at most once per detection, and only when detection failed or
// asked something (R-336), on a repository somebody is about to deploy — what
// is being bought is whether the app comes up without a person stepping in. This is not a high-volume path where a cheaper model pays for itself,
// and the cost of a wrong amendment is a person's afternoon. Opus 5.5 rather
// than Opus 5: newer, and cheaper per token besides.
const DefaultModel = "claude-opus-5-5"

// Budget ceilings this adapter will not exceed regardless of what it is asked
// for. Core lowers them further; neither side raises the other's.
const (
	DefaultMaxFiles = 40
	DefaultMaxBytes = 256 << 10
	maxIterations   = 24

	// maxTokens is generous because the answer is a tool call carrying
	// amendments and their reasons, and a truncated one is an amendment with
	// half a reason. Well under the model's ceiling; this is not a long answer.
	maxTokens = 8192
)

// Config is the adapter's own configuration (design 10 §7).
type Config struct {
	// Credentials are supplied by core, decrypted from adapter_credentials at
	// startup (O-20). They are never part of the stored configuration: the
	// database refuses a `credentials` key in adapter_configs.config, so a value
	// here can only have come from encrypted storage.
	Credentials Credentials `json:"credentials,omitzero"`

	// APIKeyEnv names an environment variable holding the key, for an operator
	// who keeps credentials in the environment. When neither this nor a stored
	// credential is set, ANTHROPIC_API_KEY is read — the SDK's own convention.
	APIKeyEnv string `json:"api_key_env,omitempty"`

	Model string `json:"model,omitempty"`

	// BaseURL points at a gateway or a proxy. Empty is the Anthropic API.
	BaseURL string `json:"base_url,omitempty"`

	// ScreenPlans defaults to true (R-336): configuring this adapter meant
	// supplying a credential, and that was the decision. False turns off both
	// functions — repairing a failed plan and answering questions — for an
	// install that wants the adapter for something else. The key keeps its
	// original name so existing configurations still read.
	ScreenPlans *bool `json:"screen_plans,omitempty"`

	MaxFiles int   `json:"max_files,omitempty"`
	MaxBytes int64 `json:"max_bytes,omitempty"`

	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Credentials is what this adapter needs kept secret.
//
// A secret.Value, so it renders [redacted] in every marshaler and cannot reach a
// log line (R-194).
type Credentials struct {
	APIKey secret.Value `json:"api_key,omitzero"`
}

// Adapter is the AI adapter.
type Adapter struct {
	cfg    Config
	client anthropic.Client
	ready  bool
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryAI }

// Configure reads the adapter's row from adapter_configs.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg Config
	if len(raw) > 0 {
		// A key at the top level is a key that was stored in the clear. The
		// create handler and the database both refuse one; refusing it here as
		// well means a row written some other way does not quietly work.
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			return fmt.Errorf("anthropic: reading configuration: %w", err)
		}
		if _, inline := top["api_key"]; inline {
			return errors.New("anthropic: api_key is in this adapter's stored configuration, which is " +
				"unencrypted; set it as a credential instead")
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("anthropic: reading configuration: %w", err)
		}
	}

	key := resolveKey(cfg, os.Getenv)
	if key.IsZero() {
		// Not a warning. An adapter with no credential can do nothing, and
		// registering it would put a permanently unhealthy adapter in the
		// console with no way to tell it from a provider outage.
		return errors.New("anthropic: no API key was found — set one as this adapter's api_key " +
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
	a.client = anthropic.NewClient(opts...)
	a.ready = true
	return nil
}

// HealthCheck asks the API whether the configured model exists.
//
// A real call rather than a ping, because the failure this has to catch is a
// credential that does not work or a model name nobody can serve — and both
// answer a models lookup the same way they would answer a screening. It costs
// no tokens.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	if !a.ready {
		return errors.New("anthropic: not configured")
	}
	if _, err := a.client.Models.Get(ctx, a.cfg.Model, anthropic.ModelGetParams{}); err != nil {
		return fmt.Errorf("anthropic: %w", err)
	}
	return nil
}

// Capabilities reports what this adapter does, as data (R-254, R-259).
func (a *Adapter) Capabilities(_ context.Context) (api.AICapabilities, error) {
	caps := api.AICapabilities{
		Model: a.cfg.Model,

		// Any model the Messages API serves, for one function at a time
		// (R-259). A name it does not serve fails that call, and the health
		// check reports on the adapter's own model only.
		ChoosesModel: true,

		MaxFiles: a.cfg.MaxFiles,
		MaxBytes: a.cfg.MaxBytes,
	}
	if a.screensPlans() {
		caps.Functions = append(caps.Functions,
			api.AIFunctionRepairPlan, api.AIFunctionAnswerQuestions, api.AIFunctionRevisePlan)
	}
	// Advertised, not performed: each runs only when an administrator assigns
	// it to this adapter (R-259).
	caps.Functions = append(caps.Functions,
		api.AIFunctionDraftAccess, api.AIFunctionDraftPolicy,
		api.AIFunctionSearchAudit, api.AIFunctionAnswerReference)
	return caps, nil
}

// resolveKey finds the credential: the stored one, then the named variable,
// then the SDK's default variable. getenv is injected so the order is testable
// without touching the process environment.
func resolveKey(cfg Config, getenv func(string) string) secret.Value {
	if !cfg.Credentials.APIKey.IsZero() {
		return cfg.Credentials.APIKey
	}
	if cfg.APIKeyEnv != "" {
		return secret.New(getenv(cfg.APIKeyEnv))
	}
	return secret.New(getenv("ANTHROPIC_API_KEY"))
}

// model is the model one call runs on: the assignment's, else the adapter's.
func (a *Adapter) model(assigned string) string {
	if assigned != "" {
		return assigned
	}
	return a.cfg.Model
}

func (a *Adapter) screensPlans() bool {
	if a.cfg.ScreenPlans == nil {
		return true // R-336
	}
	return *a.cfg.ScreenPlans
}

var _ api.AIAdapter = (*Adapter)(nil)

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryAI,
		Kind:        Kind,
		Name:        "Anthropic",
		Description: "Performs the AI functions assigned to it: repairing a failed plan, answering detection's questions, revising a plan on request, drafting access and policy, searching the audit log, and answering from the reference. Needs an Anthropic API key.",
		IDPrefix:    "ai_",
		Fields: []api.Field{
			{Key: "api_key", Label: "API key", Type: "string", Help: "An Anthropic API key. Stored encrypted and never shown again. Leave empty to use ANTHROPIC_API_KEY from Pando’s environment.", Credential: true, Placeholder: "sk-ant-…"},
			{Key: "model", Label: "Model", Type: "string", Help: "The Claude model each function uses unless its assignment names another.", Default: DefaultModel},
			{Key: "base_url", Label: "Base URL", Type: "string", Help: "A gateway or proxy in front of the Anthropic API. Empty is the API itself.", Default: "https://api.anthropic.com", Advanced: true},
			{Key: "api_key_env", Label: "API key variable", Type: "string", Help: "The environment variable to read the key from, instead of a stored one.", Default: "ANTHROPIC_API_KEY", Advanced: true},
		},
	}
}

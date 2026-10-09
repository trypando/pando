package errs

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable machine-readable error code.
//
// Stable is the operative word: clients branch on these, so a code is part of
// the API surface and renaming one is a breaking change. The human Message may
// be reworded freely.
type Code string

// The taxonomy from design 00 §3.2. The prefix determines the HTTP status, so
// a new code gets the right status by being named correctly.
const (
	// AUTH_* — authentication failed or absent. 401.
	AuthRequired     Code = "AUTH_REQUIRED"
	AuthInvalid      Code = "AUTH_INVALID"
	AuthTokenInvalid Code = "AUTH_TOKEN_INVALID"
	// AuthTokenOrphaned is R-059: a delegated token whose owning user was
	// suspended or deleted. Resolved live on every request, never by cascade.
	AuthTokenOrphaned Code = "AUTH_TOKEN_ORPHANED"

	// PERM_* — authenticated, not permitted. 403.
	PermDenied       Code = "PERM_DENIED"
	PermVerbRequired Code = "PERM_VERB_REQUIRED"

	// PermPasscodeRequired: the app is shared with everyone who knows its
	// passcode, and this request has not shown it (R-075a).
	PermPasscodeRequired Code = "PERM_PASSCODE_REQUIRED"

	// PermCrossOrigin: a write carried by the session cookie, from a page on
	// another origin (issue #78).
	PermCrossOrigin Code = "PERM_CROSS_ORIGIN"

	// RateLimited: too many attempts in too short a time; wait and try again.
	RateLimited Code = "RATE_LIMITED"

	// POLICY_* — blocked by host policy. 403.
	// Policy is evaluated before grants and is a floor, not an override (R-272).
	PolicySourceNotAllowed        Code = "POLICY_SOURCE_NOT_ALLOWED"        // R-092, raised before clone
	PolicyExecDisabled            Code = "POLICY_EXEC_DISABLED"             // R-085
	PolicyAnonymousGrantForbidden Code = "POLICY_ANONYMOUS_GRANT_FORBIDDEN" // R-076
	PolicyWebhookPrivateAddress   Code = "POLICY_WEBHOOK_PRIVATE_ADDRESS"   // R-372
	PolicyAppLimitReached         Code = "POLICY_APP_LIMIT_REACHED"         // R-244
	PolicyBackupRequired          Code = "POLICY_BACKUP_REQUIRED"           // R-284

	// VALID_* — malformed request or spec. 400.
	ValidInvalid         Code = "VALID_INVALID"
	SourceUnreadable     Code = "VALID_SOURCE_UNREADABLE" // R-091: no source connection can read the repository
	ValidUnknownEvent    Code = "VALID_UNKNOWN_EVENT"     // R-364: a subscription names no catalogued event
	ValidPrimaryWorkload Code = "VALID_PRIMARY_WORKLOAD"
	ValidDanglingMount   Code = "VALID_DANGLING_MOUNT"
	ValidEnvAmbiguous    Code = "VALID_ENV_AMBIGUOUS"
	ValidDanglingSlotRef Code = "VALID_DANGLING_SLOT_REF"
	ValidDependencyCycle Code = "VALID_DEPENDENCY_CYCLE"

	// PLAN_* — deployment cannot proceed as specified. 409.
	// Every one of these must be reachable from :plan before anything is created.
	PlanSlotUnfilled             Code = "PLAN_SLOT_UNFILLED"           // R-132
	PlanNoAdapterMeetsPolicy     Code = "PLAN_NO_ADAPTER_MEETS_POLICY" // R-024, R-114
	PlanCapabilityUnsupported    Code = "PLAN_CAPABILITY_UNSUPPORTED"  // R-254
	PlanAdapterNotConfigured     Code = "PLAN_ADAPTER_NOT_CONFIGURED"
	PlanComposeConstructRejected Code = "PLAN_COMPOSE_CONSTRUCT_REJECTED" // R-099

	// PlanSecurityBelowThreshold is R-314: this installation requires a
	// security score and this app does not have one, or does not clear it.
	PlanSecurityBelowThreshold Code = "PLAN_SECURITY_BELOW_THRESHOLD"

	// PlanEgressLooseningForbidden is R-183: the app loosens the install's
	// egress rules and host policy says no app may.
	PlanEgressLooseningForbidden Code = "PLAN_EGRESS_LOOSENING_FORBIDDEN"
	// PlanImagePlatformUnsupported is an image with no build for the operating
	// system and CPU the runtime runs (issue #41), refused before it is pulled.
	PlanImagePlatformUnsupported Code = "PLAN_IMAGE_PLATFORM_UNSUPPORTED"

	// STATE_* — object in the wrong state for this action. 409.
	StateInvalid                Code = "STATE_INVALID"
	StateBackupDecisionRequired Code = "STATE_BACKUP_DECISION_REQUIRED" // R-204/205

	// StateAppExited: the deploy started the app and its primary workload
	// stopped rather than serving (issue #55).
	StateAppExited Code = "STATE_APP_EXITED"

	// StateAIFunctionAssigned: another AI adapter already handles this
	// function, and each function has one adapter at a time (R-259).
	StateAIFunctionAssigned Code = "STATE_AI_FUNCTION_ASSIGNED"

	// StateSetAtStartup: the startup configuration declares this, so it
	// cannot be changed from the API while it does (R-271).
	StateSetAtStartup Code = "STATE_SET_AT_STARTUP"

	// StateAddressTaken: another app is already reached at this hostname or
	// path, or at one this path would sit inside or around.
	StateAddressTaken Code = "STATE_ADDRESS_TAKEN"

	// ADAPTER_* — adapter failed or is unavailable. 502.
	AdapterUnavailable Code = "ADAPTER_UNAVAILABLE"
	AdapterFailed      Code = "ADAPTER_FAILED"

	// AdapterRegistryRateLimited: a registry refused an image because this
	// server, or the app's registry account, has downloaded too many in its
	// window — Docker Hub's pull limit (internal/registrylimit).
	AdapterRegistryRateLimited Code = "ADAPTER_REGISTRY_RATE_LIMITED"

	// BUILD_* — build failed. 422.
	BuildFailed  Code = "BUILD_FAILED"
	BuildTimeout Code = "BUILD_TIMEOUT" // R-119

	// BuildListensOnLoopback: the built app listens only on 127.0.0.1 inside
	// its container, so it cannot be reached (issue #55).
	BuildListensOnLoopback Code = "BUILD_LISTENS_ON_LOOPBACK"

	// CAPACITY_* — insufficient host resources. 409.
	CapacityWouldOversubscribe Code = "CAPACITY_WOULD_OVERSUBSCRIBE" // R-242

	// CapacityNoFreePort: port-mode routing has run out of range. A limit of
	// the install's configuration rather than of the machine, so the remedy
	// names the setting to change.
	CapacityNoFreePort Code = "CAPACITY_NO_FREE_PORT"

	// BACKUP_* — backup and restore. 422.
	BackupDecryptFailed Code = "BACKUP_DECRYPT_FAILED"
	BackupIncomplete    Code = "BACKUP_INCOMPLETE" // R-215, verify before applying

	NotFound Code = "NOT_FOUND"
	Internal Code = "INTERNAL"
)

// Error is the envelope every error crossing an API boundary carries.
//
// R-242 and R-254 both promise "fail at plan time with a readable error", and
// that promise needs a type rather than a convention. Message text is held to
// the R-105 standard wherever a user might act on it: self-contained, pasteable
// into an assistant, no undefined terms.
type Error struct {
	Code      Code           `json:"code"`
	Message   string         `json:"message"`
	Remedy    string         `json:"remedy,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"request_id,omitempty"`

	// wrapped is not serialized. Internal causes belong in logs, not in a
	// response body where they leak implementation detail to a caller.
	wrapped error
}

func (e *Error) Error() string {
	if e.wrapped != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.wrapped)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.wrapped }

// New builds an error. Message should read as something a person can act on.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Newf builds an error with a formatted message.
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches an underlying cause, which is logged but never serialized.
func Wrap(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, wrapped: cause}
}

// WithRemedy attaches a hint about what to do next. Required wherever the
// requirements promise one — PLAN_SLOT_UNFILLED without a remedy naming the
// three ways to fill a slot fails R-132's intent.
func (e *Error) WithRemedy(remedy string) *Error {
	e.Remedy = remedy
	return e
}

// WithDetail attaches structured context. Several requirements specify exactly
// what belongs here: the unfilled slots for R-132, the requested and allocated
// amounts for R-242, the capability and adapter for R-254.
//
// Never put a secret value in Details. secret.Value redacts itself, but a
// revealed string does not.
func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = make(map[string]any, 4)
	}
	e.Details[key] = value
	return e
}

// WithRequestID stamps the envelope. Set by the HTTP layer, not by callers.
func (e *Error) WithRequestID(id string) *Error {
	e.RequestID = id
	return e
}

// Status maps a code to its HTTP status, by prefix.
func (e *Error) Status() int { return statusFor(e.Code) }

func statusFor(c Code) int {
	switch {
	case c == NotFound:
		return http.StatusNotFound
	case c == Internal:
		return http.StatusInternalServerError
	case c == RateLimited:
		// R-075a: a visitor who must wait is told so, not shown a server
		// fault.
		return http.StatusTooManyRequests
	case hasPrefix(c, "AUTH_"):
		return http.StatusUnauthorized
	case hasPrefix(c, "PERM_"), hasPrefix(c, "POLICY_"):
		return http.StatusForbidden
	case hasPrefix(c, "VALID_"):
		return http.StatusBadRequest
	case hasPrefix(c, "PLAN_"), hasPrefix(c, "STATE_"), hasPrefix(c, "CAPACITY_"):
		return http.StatusConflict
	case hasPrefix(c, "ADAPTER_"):
		return http.StatusBadGateway
	case hasPrefix(c, "BUILD_"), hasPrefix(c, "BACKUP_"):
		return http.StatusUnprocessableEntity
	default:
		// An unrecognized code is a bug in the caller, not a client error.
		// Failing to 500 here would hide it behind a plausible status.
		return http.StatusInternalServerError
	}
}

func hasPrefix(c Code, prefix string) bool {
	return len(c) >= len(prefix) && string(c)[:len(prefix)] == prefix
}

// As extracts an *Error from a chain, or nil.
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// CodeOf returns the code of err, or Internal if it carries none. An error that
// reaches an API boundary without an envelope is an internal failure by
// definition — the alternative is guessing a status for it.
func CodeOf(err error) Code {
	if e := As(err); e != nil {
		return e.Code
	}
	return Internal
}

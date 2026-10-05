package errs

// The catalog: every code a client can branch on, with what it means.
//
// A code is part of the API surface — clients branch on these and renaming one
// is a breaking change — so the list of them is documentation the same way the
// route table is, and it is generated from here rather than written twice.
//
// `TestEveryCodeIsInTheCatalog` parses this package's own source for constants
// of type Code and fails when one is missing from this map. Go cannot enumerate
// constants at runtime, and a list that has to be remembered is a list that
// goes stale.

// CodeDoc is one code as the reference publishes it.
type CodeDoc struct {
	Code    Code   `json:"code"`
	Status  int    `json:"status"`
	Meaning string `json:"meaning"`
}

var meanings = map[Code]string{
	AuthRequired:      "No credential was presented, or the session has expired.",
	AuthInvalid:       "The credential presented is not valid.",
	AuthTokenInvalid:  "The token is unknown, revoked or expired.",
	AuthTokenOrphaned: "The token's owner was suspended or deleted, so the token no longer resolves to anyone (R-059).",

	PermDenied:       "Authenticated, but not permitted to do this.",
	PermVerbRequired: "The caller holds no grant carrying the verb this action needs.",
	RateLimited:      "Too many attempts in a short time — at a passcode, for example. Wait a few minutes and try again.",
	PermPasscodeRequired: "The app is shared with everyone who knows its passcode, and this request has not shown it. " +
		"A browser is sent to the passcode page; entering it there lets the visitor in.",

	PolicySourceNotAllowed:        "Host policy does not allow apps from this source (R-092).",
	PolicyExecDisabled:            "Host policy has turned off terminal access, including for an app's owner (R-085).",
	PolicyWebhookPrivateAddress:   "A webhook points at a private, loopback or link-local address, and host policy does not allow that (R-372).",
	ValidUnknownEvent:             "A subscription names an event, or a pattern, that matches no event in the catalog (R-364).",
	PolicyAnonymousGrantForbidden: "Host policy does not allow apps to be shared with everyone (R-076).",

	ValidInvalid:         "The request or spec is malformed.",
	ValidPrimaryWorkload: "A spec must name exactly one primary workload.",
	ValidDanglingMount:   "A workload mounts a volume the spec does not declare.",
	ValidEnvAmbiguous:    "An environment variable is set twice with different values.",
	ValidDanglingSlotRef: "The spec refers to a slot it does not declare.",
	ValidDependencyCycle: "The workloads depend on each other in a cycle.",

	PlanSlotUnfilled:             "A required dependency has nothing filling it, so the deploy would start an app that cannot connect (R-132).",
	PlanNoAdapterMeetsPolicy:     "No configured adapter can satisfy this spec under host policy (R-024, R-114).",
	PlanCapabilityUnsupported:    "The spec asks for something the chosen adapter does not do (R-254).",
	PlanAdapterNotConfigured:     "The spec names an adapter this installation does not have.",
	PlanComposeConstructRejected: "The compose file uses a construct Pando will not translate (R-099).",
	PlanSecurityBelowThreshold:   "This installation requires a security score, and this app is below it or has never been scanned (R-314).",
	PlanEgressLooseningForbidden: "The app loosens the installation's egress rules, and host policy says no app may (R-183).",

	StateInvalid:                "The object is in a state this action does not apply to.",
	StateAIFunctionAssigned:     "Another AI adapter already handles this AI function. Remove it from that adapter first.",
	StateSetAtStartup:           "This is declared in Pando's startup configuration and cannot be changed through the API while it is.",
	StateAddressTaken:           "Another app is already reached at this address, or at a path this one would sit inside or around.",
	StateAppExited:              "The app started and then stopped, so the deploy has nothing to send traffic to.",
	StateBackupDecisionRequired: "The app has storage and the request did not say whether to keep a final backup of it (R-204, R-205).",

	AdapterUnavailable: "The adapter needed for this is not configured or not reachable.",
	AdapterFailed:      "The adapter was reached and failed.",

	BuildFailed:            "The build ran and did not succeed. Its log is the answer.",
	BuildTimeout:           "The build exceeded the time allowed for it (R-119).",
	BuildListensOnLoopback: "The built app listens only on 127.0.0.1 inside its container, where nothing outside it can reach it.",

	CapacityWouldOversubscribe: "Running this would commit more of the host than is left (R-242).",
	CapacityNoFreePort:         "Port-mode routing has no free port in the configured range.",

	BackupDecryptFailed: "The backup could not be decrypted with the key supplied.",
	BackupIncomplete:    "The backup is missing part of what it claims to hold, so it was not applied (R-215).",

	NotFound: "No such object, or none the caller may see.",
	Internal: "Pando failed in a way it did not expect. The request ID finds the log line.",
}

// Catalog returns every documented code with its HTTP status.
//
// Sorted by status then code, so the generated reference is stable and a diff
// of it shows what changed rather than how the map iterated.
func Catalog() []CodeDoc {
	out := make([]CodeDoc, 0, len(meanings))
	for code, meaning := range meanings {
		out = append(out, CodeDoc{Code: code, Status: statusFor(code), Meaning: meaning})
	}
	sortCatalog(out)
	return out
}

func sortCatalog(in []CodeDoc) {
	// Insertion sort: this runs once per reference build over a few dozen
	// entries, and pulling in sort for it is not worth the import.
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && less(in[j], in[j-1]); j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

func less(a, b CodeDoc) bool {
	if a.Status != b.Status {
		return a.Status < b.Status
	}
	return a.Code < b.Code
}

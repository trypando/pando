package api

import "github.com/trypando/pando/internal/secret"

// --- edge ------------------------------------------------------------------
//
// An edge is an install-scoped workload a routing adapter needs running in
// front of Pando: Traefik terminating :80 and :443, or cloudflared holding a
// tunnel open (R-174, design 03 §4.4). The routing adapter says what runs; the
// runtime adapter says how; core joins them. Neither adapter reaches into the
// other.
//
// It is deliberately not a BundlePlan. An edge publishes host ports, which
// R-026 forbids for a workload, and it is in no app's network. A flag on the
// app path would be a loophole every app plan could reach.

// EdgeRequest is what core tells a routing adapter when asking for its edge.
type EdgeRequest struct {
	// Ref is the adapter configuration's ID. An edge is named for it, so two
	// configured Traefiks would be two edges rather than one fought over.
	Ref string

	// ProxyUpstream is Pando's proxy, as the edge must reach it (R-023) — the
	// same value every RouteRequest carries.
	ProxyUpstream string

	// EdgeConfig is how the runtime that will run the edge lets it receive
	// routes (RuntimeCapabilities.EdgeConfig). Empty means the runtime did not
	// say, which is read as EdgeConfigSharedMount.
	EdgeConfig []EdgeConfig
}

// EdgeConfig is one way an edge receives its routes.
type EdgeConfig string

const (
	// EdgeConfigSharedMount: a directory Pando writes and the edge mounts
	// (EdgeMount.SharedWithPando). Single-host Docker.
	EdgeConfigSharedMount EdgeConfig = "shared_mount"
	// EdgeConfigKubernetesAPI: objects in the cluster's API that the edge
	// watches — Traefik's IngressRoutes. The Kubernetes runtime.
	EdgeConfigKubernetesAPI EdgeConfig = "kubernetes_api"
)

// Offers reports whether a list of edge configurations includes c. An empty
// list offers only the shared mount, which is what every runtime offered
// before the field existed.
func Offers(list []EdgeConfig, c EdgeConfig) bool {
	if len(list) == 0 {
		return c == EdgeConfigSharedMount
	}
	for _, x := range list {
		if x == c {
			return true
		}
	}
	return false
}

// EdgePlan is the workload a routing adapter needs running.
type EdgePlan struct {
	// Name is unique within the install. The runtime names what it creates
	// from it and finds it again by it.
	Name string

	Image string
	Args  []string

	// Env reaches the edge's configuration and nowhere else. Credentials —
	// a DNS provider's key, a tunnel token — are secret.Value so they cannot
	// reach a log line on the way (R-194).
	Env map[string]secret.Value

	// Ports are host ports bound to the edge. They are the only ports Pando
	// ever publishes: an app's workloads publish none (R-026).
	Ports []EdgePort

	Mounts []EdgeMount

	// ProxyAlias is the host name the edge dials to reach Pando's proxy — the
	// host part of ProxyUpstream. The runtime makes Pando answer to it on the
	// network it shares with the edge, and joins the edge to nothing else.
	ProxyAlias string

	// ReadsRoutesFrom is how this edge receives its routes, when it reads them
	// from the runtime's API (EdgeConfigKubernetesAPI): the runtime then gives
	// it the identity that may read them and nothing else. Empty for an edge
	// that reads a shared mount or holds its configuration remotely
	// (cloudflared), which is given no API identity at all.
	ReadsRoutesFrom EdgeConfig

	// Issue is the certificates this edge needs Pando to issue, when it does
	// not issue its own (notes-kubernetes-runtime-issue-72.md: on Kubernetes
	// several replicas share one set, so Pando's leader is the one issuer).
	// Nil for an edge that issues its own or serves none.
	Issue *CertificateIssue

	// Certificates are the issued certificates the edge serves, filled in by
	// core from Issue before the plan reaches the runtime. The runtime
	// stores them where the edge reads them, under each one's Name.
	Certificates []EdgeCertificate
}

// Challenge types an ACME order is proved with (R-169).
const (
	ChallengeHTTP01 = "http-01"
	ChallengeDNS01  = "dns-01"
)

// CertificateIssue is what a routing adapter asks Pando to issue for its
// edge, in the terms its settings already use (R-169).
type CertificateIssue struct {
	// Email is the ACME account's address.
	Email string

	// Challenge is ChallengeHTTP01, answered by Pando's proxy on every
	// hostname it issues for, or ChallengeDNS01 through DNSProvider.
	Challenge string

	// DNSProvider is the DNS-01 provider's code, as Traefik and lego name it,
	// and DNSCredentials its variables. secret.Value so they cannot reach a
	// log line (R-194).
	DNSProvider    string
	DNSCredentials map[string]secret.Value

	// Orders are the certificates wanted. Each is kept under its Name.
	Orders []CertificateOrder
}

// CertificateOrder is one certificate: the name the edge's routes refer to it
// by, and the names it covers, the first being its subject.
type CertificateOrder struct {
	Name    string
	Domains []string
}

// EdgeCertificate is an issued certificate and its key, as the edge serves
// it.
type EdgeCertificate struct {
	Name    string
	CertPEM []byte
	KeyPEM  secret.Value
}

// EdgePort publishes one port of the edge on the host.
type EdgePort struct {
	Host      int
	Container int
	Protocol  string // "tcp" when empty
}

// EdgeMount is storage the edge sees at Path. Exactly one of SharedWithPando
// and Volume is set.
type EdgeMount struct {
	Path string

	// SharedWithPando is a path in Pando's own filesystem whose storage the
	// edge mounts too — the directory the Traefik adapter writes routes into.
	// The runtime resolves what that storage is (R-251); core never learns.
	SharedWithPando string

	// Volume is storage the edge owns, kept when the edge is recreated — an
	// ACME certificate store, so a restart does not re-issue every certificate.
	Volume string

	ReadOnly bool
}

// EdgeState is an edge as found.
type EdgeState struct {
	Present bool
	Running bool

	// Detail says what is wrong in words an operator can act on, when Present
	// or Running is false for a reason the runtime can see.
	Detail string
}

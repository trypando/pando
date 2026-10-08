package api

import "context"

// The image registry category (R-252, issue #153).
//
// The twelfth category. An image registry adapter is where Pando pushes a
// build when the runtime pulls images rather than taking one directly — a
// cluster has no single daemon to load a tarball into (issue #72, PR 5). It
// answers four questions: where does this build go (Target), may Pando pull
// what it pushed and with what (Owns, PullAuth), and what goes when an app is
// deleted (DeleteApp, R-224).
//
// It was install configuration until issue #153, on the argument that the
// planner asks it nothing. The providers' differences say otherwise: ECR does
// not create a repository on push and mints a password from an AWS key, a
// Distribution registry does both the other way, and which one an install has
// decides how images are laid out and whether a per-app layout can work at
// all. Those are capabilities (R-254), and design 03 §8.1's test passes.
//
// The registry Pando probes for an image a project already publishes is not
// this: that is a step of detection (internal/detect/registryprobe). Nor is
// the credential an image app pulls with, which belongs to the app (issue #41,
// design 03 "Registry authentication is not an adapter category").
//
// Core builds the install's registry adapter from its stored row each time it
// is used (core/imageregistry), as it does source connections, so a password
// rotated on one replica is what every replica pushes with next.

// ImageRegistryAdapter is a registry Pando pushes builds to.
type ImageRegistryAdapter interface {
	Adapter

	ImageRegistryCapabilities() ImageRegistryCapabilities

	// Target is where one build is pushed, with the credential for that push.
	// An empty workload is the app-wide image. The tag is unique to the
	// deployment, so an image refused after it was built is never mistaken
	// for the one that runs (R-146).
	Target(ctx context.Context, appID, workload, deploymentID string) (PushTarget, error)

	// Owns reports whether an image reference is one this registry holds
	// under Pando's path: a build Pando pushed. Pure: no network.
	Owns(ref string) bool

	// PullAuth is the credential for pulling an image the registry Owns,
	// minted fresh where the provider issues short-lived passwords. Nil for a
	// registry Pando reaches anonymously.
	PullAuth(ctx context.Context) (*RegistryAuth, error)

	// DeleteApp deletes every manifest of a deleted app's builds (R-224).
	// workloads names the app's separately built workloads; the app-wide
	// image is always included. It returns how many manifests were deleted.
	// The layers they referenced are freed by the registry's own garbage
	// collection (O-38).
	DeleteApp(ctx context.Context, appID string, workloads []string) (int, error)
}

// ImageRegistryCapabilities is what the registry does and how Pando uses it,
// as data (R-254).
type ImageRegistryCapabilities struct {
	// Host is the registry's host and port, for messages.
	Host string `json:"host"`

	// CreatesRepositoriesOnPush says a push to a repository that does not
	// exist creates it. Without it every build goes to one repository the
	// operator created, tagged by app and deployment, and a repository per
	// app is refused when the adapter is configured rather than at the first
	// push.
	CreatesRepositoriesOnPush bool `json:"creates_repositories_on_push"`

	// RepositoryPerApp says each app's builds have a repository of their own,
	// so a deleted app's images are a few listings to delete.
	RepositoryPerApp bool `json:"repository_per_app"`

	// SendsEveryBuild says builds go through the registry even for a runtime
	// that can take one directly, such as Docker on one host. Off by default
	// (O-34).
	SendsEveryBuild bool `json:"sends_every_build"`
}

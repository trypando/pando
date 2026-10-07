// Package imageregistry is the install's image registry: where a build is
// pushed when the runtime pulls rather than imports (issue #72, PR 5,
// docs/design/notes-image-registry-issue-72.md).
//
// The registry is configuration, like the database (O-34 – O-38). Pando does
// not start it or run it: the multi-machine topologies supply one, or setup
// points at the organization's own. Registry authentication is outside the
// adapter categories, for design 03 §2.1's reason — the planner asks it
// nothing but whether it is there.
package imageregistry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Layout is how built images are arranged in the registry.
type Layout string

const (
	// LayoutPerApp is <prefix>/apps/<app>[/<workload>], tagged with the
	// deployment: one repository per workload, and per app, so a deleted app's
	// images are a few listings to delete. The default.
	LayoutPerApp Layout = "per_app"

	// LayoutSingle puts every image in <prefix>, tagged
	// <app>-<workload>-<deployment>, for a registry where a repository must
	// exist before a push (ECR, unless the account creates them on push).
	LayoutSingle Layout = "single"
)

// Config is the startup configuration (PANDO_REGISTRY_*). Startup only, like
// the database URL: the credential is held in memory as a secret.Value from
// the moment it is read and is never stored (R-190, R-194).
type Config struct {
	// URL is the registry and an optional path prefix:
	// https://registry.internal:5000, or
	// 123456789012.dkr.ecr.us-east-1.amazonaws.com/pando.
	URL string

	// Username and Password are the one credential Pando pushes and pulls
	// with (O-36). For Kind ecr they are an AWS access key ID and its secret.
	Username string
	Password secret.Value

	// Kind is basic (the default) or ecr, the kinds core/oci mints for image
	// apps.
	Kind string

	// Layout is per_app (the default) or single.
	Layout string

	// Insecure permits plain HTTP. Off unless the operator sets it (O-35).
	Insecure bool

	// Always sends builds through the registry even when the runtime can
	// import them. Off by default: single-host Docker imports (O-34). For an
	// install that wants one delivery path, and for exercising the push path on
	// one host.
	Always bool
}

// Registry is a configured install registry. A nil *Registry is "none
// configured", and every method answers accordingly.
type Registry struct {
	host     string
	prefix   string
	layout   Layout
	insecure bool
	always   bool
	cred     *oci.Credential
	resolver oci.Resolver
}

// New reads the configuration. An empty URL is no registry: (nil, nil).
func New(c Config) (*Registry, error) {
	raw := strings.TrimSpace(c.URL)
	if raw == "" {
		return nil, nil
	}
	// A credential in the URL would be stored and shown in the clear (R-190,
	// R-194). Not quoted back, for the same reason.
	if host, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://"), "/"); strings.Contains(host, "@") {
		return nil, errs.New(errs.ValidInvalid, "The registry URL carries a username or password, which Pando does not keep in a URL.").
			WithRemedy("Give the registry's address alone, and the username and password as their own settings.")
	}
	r := &Registry{insecure: c.Insecure, always: c.Always}

	switch {
	case strings.HasPrefix(raw, "http://"):
		if !c.Insecure {
			return nil, errs.Newf(errs.ValidInvalid,
				"PANDO_REGISTRY_URL is %q, which is plain HTTP, and PANDO_REGISTRY_INSECURE is not set.", raw).
				WithRemedy("Give the registry a certificate every host trusts and use https://, or set PANDO_REGISTRY_INSECURE=true to permit plain HTTP on a private network.")
		}
		raw = strings.TrimPrefix(raw, "http://")
	case strings.HasPrefix(raw, "https://"):
		raw = strings.TrimPrefix(raw, "https://")
	case strings.Contains(raw, "://"):
		return nil, errs.Newf(errs.ValidInvalid,
			"PANDO_REGISTRY_URL is %q, and Pando reaches a registry only over https:// (or http:// with PANDO_REGISTRY_INSECURE=true).", c.URL)
	}
	raw = strings.Trim(raw, "/")
	host, prefix, _ := strings.Cut(raw, "/")
	r.host = strings.ToLower(host)
	r.prefix = strings.ToLower(strings.Trim(prefix, "/"))
	if r.host == "" {
		return nil, errs.Newf(errs.ValidInvalid, "PANDO_REGISTRY_URL is %q, which names no registry host.", c.URL).
			WithRemedy("Set it to the registry's address, such as https://registry.internal:5000.")
	}
	if _, err := name.NewRepository(r.join("apps/check"), r.nameOpts()...); err != nil {
		return nil, errs.Wrap(errs.ValidInvalid,
			fmt.Sprintf("PANDO_REGISTRY_URL is %q, which is not a registry address and path Pando can push to.", c.URL), err).
			WithRemedy("Use a host, an optional port and an optional lowercase path, such as registry.internal:5000/pando.")
	}

	switch Layout(c.Layout) {
	case "", LayoutPerApp:
		r.layout = LayoutPerApp
	case LayoutSingle:
		if r.prefix == "" {
			return nil, errs.New(errs.ValidInvalid,
				"PANDO_REGISTRY_LAYOUT is single, and PANDO_REGISTRY_URL names no repository to put every image in.").
				WithRemedy("Add the repository's path to PANDO_REGISTRY_URL, such as 123456789012.dkr.ecr.us-east-1.amazonaws.com/pando.")
		}
		r.layout = LayoutSingle
	default:
		return nil, errs.Newf(errs.ValidInvalid,
			"PANDO_REGISTRY_LAYOUT is %q. Valid answers: per_app (a repository per app) or single (one repository for every image).", c.Layout)
	}

	kind := oci.CredentialKind(c.Kind)
	if kind == "" {
		kind = oci.CredentialBasic
	}
	if kind != oci.CredentialBasic && kind != oci.CredentialECR {
		return nil, errs.Newf(errs.ValidInvalid,
			"PANDO_REGISTRY_KIND is %q. Valid answers: basic (a username and password) or ecr (an AWS access key).", c.Kind)
	}
	if c.Username != "" || !c.Password.IsZero() || kind == oci.CredentialECR {
		cred := oci.Credential{Kind: kind}
		switch kind {
		case oci.CredentialECR:
			cred.AccessKeyID, cred.SecretAccessKey = c.Username, c.Password
		default:
			cred.Username, cred.Password = c.Username, c.Password
		}
		if err := cred.Validate(); err != nil {
			return nil, errs.New(errs.ValidInvalid,
				"The install registry's credential is incomplete: it needs both a username and a password.").
				WithRemedy("Set both (PANDO_REGISTRY_USERNAME and PANDO_REGISTRY_PASSWORD, or the image registry settings in the console), or neither for a registry Pando reaches anonymously. For ECR the username is the AWS access key ID and the password its secret access key.")
		}
		r.cred = &cred
	}
	return r, nil
}

// Configured reports whether the install has a registry.
func (r *Registry) Configured() bool { return r != nil }

// Always reports whether builds go through the registry even for a runtime
// that can import them.
func (r *Registry) Always() bool { return r != nil && r.always }

// Host is the registry's host and port, for messages.
func (r *Registry) Host() string {
	if r == nil {
		return ""
	}
	return r.host
}

func (r *Registry) join(path string) string {
	if r.prefix == "" {
		return r.host + "/" + path
	}
	return r.host + "/" + r.prefix + "/" + path
}

func (r *Registry) nameOpts() []name.Option {
	if r.insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

var notNameChar = regexp.MustCompile(`[^a-z0-9._-]+`)

// component makes an app ID or a workload name a repository path component
// and a tag fragment: lowercase, [a-z0-9._-], starting and ending with a
// letter or digit.
func component(s string) string {
	s = notNameChar.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "._-")
}

// Repository is where an app's workload's builds go, and the tag a deployment
// is pushed under. An empty workload is the app-wide image.
func (r *Registry) Repository(appID, workload, deploymentID string) (repository, tag string) {
	app, wl, dep := component(appID), component(workload), component(deploymentID)
	if r.layout == LayoutSingle {
		tag = app + "-"
		if wl != "" {
			tag += wl + "-"
		}
		tag += dep
		if len(tag) > 128 {
			tag = tag[len(tag)-128:]
		}
		return r.host + "/" + r.prefix, strings.TrimLeft(tag, "._-")
	}
	repository = r.join("apps/" + app)
	if wl != "" {
		repository += "/" + wl
	}
	return repository, dep
}

// Owns reports whether an image reference is in this registry, under Pando's
// prefix: a built image Pando pushed.
func (r *Registry) Owns(ref string) bool {
	if r == nil {
		return false
	}
	base := r.host + "/"
	if r.prefix != "" {
		base += r.prefix + "/"
	}
	if r.layout == LayoutSingle {
		return strings.HasPrefix(ref, r.host+"/"+r.prefix+"@") || strings.HasPrefix(ref, r.host+"/"+r.prefix+":")
	}
	return strings.HasPrefix(ref, base+"apps/")
}

// Auth is the credential for one push or pull, minted fresh for ECR. Nil for
// a registry Pando reaches anonymously.
func (r *Registry) Auth(ctx context.Context) (*api.RegistryAuth, error) {
	if r == nil || r.cred == nil {
		return nil, nil
	}
	a, err := r.resolver.Resolve(ctx, *r.cred, r.join("apps/check"))
	if err != nil {
		return nil, err
	}
	return &api.RegistryAuth{Registry: r.host, Username: a.Username, Password: a.Password, IdentityToken: a.IdentityToken}, nil
}

// Target is where one build is pushed, with the credential for it.
func (r *Registry) Target(ctx context.Context, appID, workload, deploymentID string) (*api.PushTarget, error) {
	if r == nil {
		return nil, errs.New(errs.Internal, "No install registry is configured.")
	}
	auth, err := r.Auth(ctx)
	if err != nil {
		return nil, err
	}
	repo, tag := r.Repository(appID, workload, deploymentID)
	return &api.PushTarget{Repository: repo, Tag: tag, Auth: auth, Insecure: r.insecure}, nil
}

// DeleteApp deletes every manifest of a deleted app's builds through the
// registry API (R-224). workloads names the app's separately built
// workloads; the app-wide image is always included. It returns how many
// manifests were deleted.
//
// Manifests only: the layers they referenced are freed by the registry's own
// garbage collection, which the topology runs (O-38).
func (r *Registry) DeleteApp(ctx context.Context, appID string, workloads []string) (int, error) {
	if r == nil {
		return 0, nil
	}
	opts, err := r.remoteOpts(ctx)
	if err != nil {
		return 0, err
	}

	type listing struct {
		repo   string
		prefix string // tags to take; empty takes every tag
	}
	var listings []listing
	if r.layout == LayoutSingle {
		listings = append(listings, listing{repo: r.host + "/" + r.prefix, prefix: component(appID) + "-"})
	} else {
		seen := map[string]bool{}
		for _, w := range append([]string{""}, workloads...) {
			repo, _ := r.Repository(appID, w, "x")
			if !seen[repo] {
				seen[repo] = true
				listings = append(listings, listing{repo: repo})
			}
		}
	}

	deleted := 0
	for _, l := range listings {
		repo, err := name.NewRepository(l.repo, r.nameOpts()...)
		if err != nil {
			return deleted, errs.Wrap(errs.Internal, "Pando could not name a repository in the registry.", err)
		}
		tags, err := remote.List(repo, opts...)
		if err != nil {
			if notFound(err) {
				continue
			}
			return deleted, errs.Wrap(errs.AdapterFailed,
				fmt.Sprintf("Pando could not list a deleted app's images in the registry %s.", r.host), err).
				WithRemedy("Check that the registry is reachable from Pando and accepts its credential. Pando tries again at its next teardown pass.")
		}
		digests := map[string]bool{}
		for _, tag := range tags {
			if l.prefix != "" && !strings.HasPrefix(tag, l.prefix) {
				continue
			}
			desc, err := remote.Head(repo.Tag(tag), opts...)
			if err != nil {
				if notFound(err) {
					continue
				}
				return deleted, errs.Wrap(errs.AdapterFailed,
					fmt.Sprintf("Pando could not read a deleted app's image in the registry %s.", r.host), err)
			}
			digests[desc.Digest.String()] = true
		}
		for d := range digests {
			if err := remote.Delete(repo.Digest(d), opts...); err != nil {
				if notFound(err) {
					continue
				}
				return deleted, errs.Wrap(errs.AdapterFailed,
					fmt.Sprintf("The registry %s refused to delete a deleted app's image.", r.host), err).
					WithRemedy("A registry Pando runs needs storage.delete.enabled: true in its configuration, as the shipped one has. For your organization's registry, give Pando's credential permission to delete images.")
			}
			deleted++
		}
	}
	return deleted, nil
}

func (r *Registry) remoteOpts(ctx context.Context) ([]remote.Option, error) {
	opts := []remote.Option{remote.WithContext(ctx)}
	auth, err := r.Auth(ctx)
	if err != nil {
		return nil, err
	}
	if auth != nil {
		opts = append(opts, remote.WithAuth(authn.FromConfig(authn.AuthConfig{
			Username:      auth.Username,
			Password:      auth.Password.Reveal(),
			IdentityToken: auth.IdentityToken.Reveal(),
		})))
	}
	return opts, nil
}

// notFound reports a registry answer that means "nothing there": an app that
// never pushed, or a manifest already deleted.
func notFound(err error) bool {
	var te *transport.Error
	if errors.As(err, &te) {
		if te.StatusCode == http.StatusNotFound {
			return true
		}
		for _, d := range te.Errors {
			if d.Code == transport.NameUnknownErrorCode || d.Code == transport.ManifestUnknownErrorCode {
				return true
			}
		}
	}
	return false
}

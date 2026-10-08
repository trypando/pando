// Package registrykit is what the image registry adapters share: reading a
// registry address, laying built images out in it, and deleting a deleted
// app's (R-224). Every registry Pando pushes to speaks the OCI distribution
// API; what differs between providers is how Pando signs in and whether a
// repository has to exist before a push, and each kind supplies those.
package registrykit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Layout is how built images are arranged in the registry.
type Layout string

const (
	// LayoutPerApp is <prefix>/apps/<app>[/<workload>], tagged with the
	// deployment: one repository per workload, and per app, so a deleted app's
	// images are a few listings to delete. The default where the registry
	// creates repositories on push.
	LayoutPerApp Layout = "per_app"

	// LayoutSingle puts every image in <prefix>, tagged
	// <app>-<workload>-<deployment>, for a registry where a repository must
	// exist before a push (ECR).
	LayoutSingle Layout = "single"
)

// Settings are the settings every kind shares, as stored in
// adapter_configs.config.
type Settings struct {
	// URL is the registry and an optional path prefix:
	// https://registry.internal:5000, or
	// 123456789012.dkr.ecr.us-east-1.amazonaws.com/pando.
	URL string `json:"url"`

	// Layout is per_app or single. Empty is the kind's default.
	Layout string `json:"layout"`

	// Insecure permits plain HTTP. Off unless the operator sets it (O-35).
	Insecure bool `json:"insecure"`

	// Always sends builds through the registry even when the runtime can
	// take them directly. Off by default: single-host Docker imports (O-34).
	Always bool `json:"always"`
}

// AuthFunc is the credential for one push or pull. Nil for anonymous.
type AuthFunc func(ctx context.Context, host string) (*api.RegistryAuth, error)

// Registry is a configured registry. Each kind embeds one and adds Kind,
// Configure and Info.
type Registry struct {
	host       string
	prefix     string
	layout     Layout
	insecure   bool
	always     bool
	createsOn  bool
	auth       AuthFunc
	configured bool
}

// Open reads the shared settings. createsOnPush is the provider's answer to
// whether a push creates its repository; where it does not, the layout is
// single and a per_app one is refused here rather than at the first push.
func Open(s Settings, createsOnPush bool, auth AuthFunc) (Registry, error) {
	raw := strings.TrimSpace(s.URL)
	if raw == "" {
		return Registry{}, errs.New(errs.ValidInvalid, "The image registry needs an address.").
			WithRemedy("Set url to the registry Pando should push built images to, such as https://registry.internal:5000.")
	}
	// A credential in the URL would be stored and shown in the clear (R-190,
	// R-194). Not quoted back, for the same reason.
	if host, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://"), "/"); strings.Contains(host, "@") {
		return Registry{}, errs.New(errs.ValidInvalid, "The image registry's address carries a username or password, which Pando does not keep in an address.").
			WithRemedy("Give the registry's address alone, and the credential in its own settings.")
	}
	r := Registry{insecure: s.Insecure, always: s.Always, createsOn: createsOnPush, auth: auth}

	switch {
	case strings.HasPrefix(raw, "http://"):
		if !s.Insecure {
			return Registry{}, errs.Newf(errs.ValidInvalid,
				"The image registry's address is %q, which is plain HTTP, and plain HTTP is not allowed for it.", raw).
				WithRemedy("Give the registry a certificate every host trusts and use https://, or turn on Allow plain HTTP (insecure) for a registry on a private network.")
		}
		raw = strings.TrimPrefix(raw, "http://")
	case strings.HasPrefix(raw, "https://"):
		raw = strings.TrimPrefix(raw, "https://")
	case strings.Contains(raw, "://"):
		return Registry{}, errs.Newf(errs.ValidInvalid,
			"The image registry's address is %q, and Pando reaches a registry only over https://, or http:// with Allow plain HTTP (insecure) turned on.", s.URL)
	}
	raw = strings.Trim(raw, "/")
	host, prefix, _ := strings.Cut(raw, "/")
	r.host = strings.ToLower(host)
	r.prefix = strings.ToLower(strings.Trim(prefix, "/"))
	if r.host == "" {
		return Registry{}, errs.Newf(errs.ValidInvalid, "The image registry's address is %q, which names no registry host.", s.URL).
			WithRemedy("Set url to the registry's address, such as https://registry.internal:5000.")
	}
	if _, err := name.NewRepository(r.join("apps/check"), r.nameOpts()...); err != nil {
		return Registry{}, errs.Wrap(errs.ValidInvalid,
			fmt.Sprintf("The image registry's address is %q, which is not a registry address and path Pando can push to.", s.URL), err).
			WithRemedy("Use a host, an optional port and an optional lowercase path, such as registry.internal:5000/pando.")
	}

	layout := Layout(s.Layout)
	if layout == "" {
		layout = LayoutPerApp
		if !createsOnPush {
			layout = LayoutSingle
		}
	}
	switch layout {
	case LayoutPerApp:
		if !createsOnPush {
			return Registry{}, errs.New(errs.ValidInvalid,
				"This registry does not create a repository when Pando pushes to one that does not exist, so it cannot hold a repository per app.").
				WithRemedy("Use one repository for everything (layout single), and add that repository's path to the address, such as 123456789012.dkr.ecr.us-east-1.amazonaws.com/pando.")
		}
	case LayoutSingle:
		if r.prefix == "" {
			return Registry{}, errs.New(errs.ValidInvalid,
				"The image registry puts every image in one repository, and its address names no repository.").
				WithRemedy("Add the repository's path to the address, such as 123456789012.dkr.ecr.us-east-1.amazonaws.com/pando.")
		}
	default:
		return Registry{}, errs.Newf(errs.ValidInvalid,
			"The image registry's layout is %q. Valid answers: per_app (a repository per app) or single (one repository for every image).", s.Layout)
	}
	r.layout = layout
	r.configured = true
	return r, nil
}

// Host is the registry's host and port.
func (r *Registry) Host() string { return r.host }

// Capabilities is the shared half of the kind's capabilities.
func (r *Registry) ImageRegistryCapabilities() api.ImageRegistryCapabilities {
	return api.ImageRegistryCapabilities{
		Host:                      r.host,
		CreatesRepositoriesOnPush: r.createsOn,
		RepositoryPerApp:          r.configured && r.layout == LayoutPerApp,
		SendsEveryBuild:           r.always,
	}
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
	if !r.configured {
		return false
	}
	if r.layout == LayoutSingle {
		return strings.HasPrefix(ref, r.host+"/"+r.prefix+"@") || strings.HasPrefix(ref, r.host+"/"+r.prefix+":")
	}
	base := r.host + "/"
	if r.prefix != "" {
		base += r.prefix + "/"
	}
	return strings.HasPrefix(ref, base+"apps/")
}

// PullAuth is the credential for one push or pull, minted fresh by the kind
// where its provider issues short-lived passwords. Nil for anonymous.
func (r *Registry) PullAuth(ctx context.Context) (*api.RegistryAuth, error) {
	if !r.configured || r.auth == nil {
		return nil, nil
	}
	return r.auth(ctx, r.host)
}

// Target is where one build is pushed, with the credential for it.
func (r *Registry) Target(ctx context.Context, appID, workload, deploymentID string) (api.PushTarget, error) {
	if !r.configured {
		return api.PushTarget{}, errs.New(errs.AdapterUnavailable, "This image registry is not configured.")
	}
	auth, err := r.PullAuth(ctx)
	if err != nil {
		return api.PushTarget{}, err
	}
	repo, tag := r.Repository(appID, workload, deploymentID)
	return api.PushTarget{Repository: repo, Tag: tag, Auth: auth, Insecure: r.insecure}, nil
}

// HealthCheck signs in to the registry's API, so a registry Pando cannot
// reach or whose credential it refuses is reported on the adapters screen
// rather than by the first deploy that needs it.
func (r *Registry) HealthCheck(ctx context.Context) error {
	if !r.configured {
		return errs.New(errs.AdapterUnavailable, "This image registry is not configured.")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	opts, err := r.remoteOpts(ctx)
	if err != nil {
		return err
	}
	repo, err := name.NewRepository(r.join("apps/check"), r.nameOpts()...)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not name a repository in the registry.", err)
	}
	if _, err := remote.List(repo, opts...); err != nil && !notFound(err) {
		return errs.Wrap(errs.AdapterUnavailable,
			fmt.Sprintf("Pando could not sign in to the image registry %s.", r.host), err).
			WithRemedy("Check that the registry is reachable from Pando and accepts its credential.")
	}
	return nil
}

// DeleteApp deletes every manifest of a deleted app's builds through the
// registry API (R-224).
func (r *Registry) DeleteApp(ctx context.Context, appID string, workloads []string) (int, error) {
	if !r.configured {
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
	auth, err := r.PullAuth(ctx)
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

// Shared fields, for each kind's Info.

// URLField is the registry's address.
func URLField(placeholder, help string) api.Field {
	return api.Field{Key: "url", Label: "Registry address", Type: "string", Required: true,
		Placeholder: placeholder, Help: help}
}

// AlwaysField sends every build through the registry.
func AlwaysField() api.Field {
	return api.Field{Key: "always", Label: "Send every build through the registry", Type: "bool", Advanced: true,
		Default: "false", Help: "Even on a runtime that can take a built image directly, such as Docker on one host."}
}

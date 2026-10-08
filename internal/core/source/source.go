// Package source fetches an app's source and exposes it read-only.
//
// R-020: the source tree is strictly read-only input. Pando looks at it and
// never asks it for permission — there is no pando.yaml, and nothing in a repo
// can change how Pando behaves.
package source

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Checkout is a fetched source tree on disk.
type Checkout struct {
	// Dir is the root of the working tree.
	Dir string

	// Commit is the resolved SHA. R-120: Ref is what the user asked for, Commit
	// is what runs, and it is written back into the spec so a redeploy of the
	// same revision builds the same code.
	Commit string

	// Digest names an uploaded source, which has no commit: the SHA-256 of the
	// archive it was unpacked from, as "sha256:<hex>". Never written into the
	// spec as a commit (R-120) — it is how a scan knows the same archive when
	// it sees it again (R-312).
	Digest string

	cleanup func()
}

// Identity is what names this exact source: the commit, or for an upload the
// archive's digest. Empty when neither is known.
func (c *Checkout) Identity() string {
	if c.Commit != "" {
		return c.Commit
	}
	return c.Digest
}

// Close removes the checkout.
func (c *Checkout) Close() {
	if c.cleanup != nil {
		c.cleanup()
	}
}

// View returns a read-only view rooted at the source, honoring Subdir.
func (c *Checkout) View(subdir string) api.SourceView {
	// A published image has no checkout. A view rooted at "" resolved every
	// name against the filesystem root of the server itself, so anything that
	// read "the app's source" — a detector, the scanner, a screener that sends
	// files to a provider — read the server's own files instead.
	if c.Dir == "" {
		return emptyView{}
	}
	root := c.Dir
	if subdir != "" {
		root = filepath.Join(c.Dir, filepath.Clean("/"+subdir))
	}
	return &dirView{root: root}
}

// Sources fetches an app's source, and keeps the sources that were uploaded
// rather than cloned (R-262).
//
// A value handed to each caller rather than a package variable: the upload
// directory used to be one, and two servers in one process — which is what a
// parallel test run is — wrote their uploads into each other's (issue #31).
//
// The zero value fetches git and image sources. An upload needs UploadDir.
type Sources struct {
	// UploadDir is where uploaded sources are kept.
	UploadDir string

	// WorkDir is where a clone or an unpacked upload is made while a deploy or
	// a detection reads it: server.work_dir, beside Pando's other working
	// files, rather than the system temporary directory, which is often a
	// small tmpfs that a few concurrent clones fill (issue #72). Empty means
	// the system temporary directory, for tests and embeddings.
	WorkDir string

	// Credentials finds what a repository is read with, from the install's
	// source connections (R-091, issue #127). Nil reads every repository
	// anonymously, which is what a public one needs.
	Credentials Credentials
}

// Purpose is why a repository is being read, so a use that matters — a clone
// whose code will run — can be audited and a poll for new commits is not.
type Purpose int

const (
	// PurposeClone fetches the code.
	PurposeClone Purpose = iota

	// PurposeCheck only asks the repository what it has: whether it can be
	// read, or what a branch points at.
	PurposeCheck
)

// Access is what a repository is read with.
type Access struct {
	Credential api.GitCredential

	// Connection names the source connection it came from, for messages:
	// "the source connection 'GitHub (acme)'".
	Connection string
}

// Credentials finds what a repository is read with. Nil Access with no error
// means read it anonymously.
type Credentials interface {
	Access(ctx context.Context, src spec.Source, purpose Purpose) (*Access, error)
}

// access resolves what src is read with, as go-git wants it. The returned
// function removes anything written to disk to build it.
func (s Sources) access(ctx context.Context, src spec.Source, purpose Purpose) (*gitAccess, error) {
	out := &gitAccess{url: src.URL}
	if s.Credentials == nil || src.Type != spec.SourceGit {
		return out, nil
	}
	a, err := s.Credentials.Access(ctx, src, purpose)
	if err != nil || a == nil {
		return out, err
	}
	out.connection = a.Connection
	if a.Credential.URL != "" {
		out.url = a.Credential.URL
	}
	out.caBundle = a.Credential.CABundle
	out.auth, err = authMethod(a.Credential, out.url)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// gitAccess is a resolved Access: the address to use and go-git's
// authentication for it.
type gitAccess struct {
	url        string
	auth       transport.AuthMethod
	caBundle   []byte
	connection string
}

// tempDir makes a working directory for one fetch under WorkDir, or under the
// system temporary directory when WorkDir is unset or cannot be made — a
// development run without /var/lib/pando, say — since a fetch that fails over
// where it would have put the files is worse than one in a small tmpfs.
func (s Sources) tempDir(pattern string) (string, error) {
	if s.WorkDir != "" {
		if err := os.MkdirAll(s.WorkDir, 0o700); err == nil {
			return os.MkdirTemp(s.WorkDir, pattern)
		}
	}
	return os.MkdirTemp("", pattern)
}

// Fetch clones an app's source.
//
// The caller must have checked the source allowlist first (R-092) — by the time
// this runs, disk has been written to, which is exactly why that check belongs
// before it and not inside it.
func (s Sources) Fetch(ctx context.Context, src spec.Source) (*Checkout, error) {
	switch src.Type {
	case spec.SourceGit:
		acc, err := s.access(ctx, src, PurposeClone)
		if err != nil {
			return nil, err
		}
		return fetchGit(ctx, src, acc, s.tempDir)
	case spec.SourceImage:
		// Nothing to fetch: a prebuilt image is run as it is.
		return &Checkout{Dir: "", Commit: src.Digest}, nil
	case spec.SourceUpload:
		return s.fetchUpload(ctx, src)
	default:
		return nil, errs.Newf(errs.ValidInvalid,
			"Pando does not know how to fetch source of type %q.", src.Type)
	}
}

// fetchAttempts is how many times a clone is tried when the connection fails
// under it.
const fetchAttempts = 3

// fetchGit clones, trying again when the connection rather than the repository
// was the problem.
//
// A pooled connection that has died is found out by the HTTP/2 health check
// (transport.go) and fails the request in seconds instead of hanging it for
// ten minutes — but it still fails that request. The dead connection has left
// the pool by then, so another attempt opens a new one. A wrong address, a
// missing branch or a commit that is not there fails the same way every time
// and is reported at once.
func fetchGit(ctx context.Context, src spec.Source, acc *gitAccess, mkdir func(string) (string, error)) (*Checkout, error) {
	if acc == nil {
		acc = &gitAccess{url: src.URL}
	}
	var err error
	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		var co *Checkout
		co, err = fetchGitOnce(ctx, src, acc, mkdir)
		if err == nil || !transient(err) || ctx.Err() != nil || attempt == fetchAttempts {
			return co, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	return nil, err
}

// transient reports a failure of the connection rather than of the request.
func transient(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		msg := strings.ToLower(e.Error())
		for _, s := range []string{
			"connection lost", "connection reset", "unexpected eof", "broken pipe",
			"timeout awaiting response headers", "i/o timeout", "tls handshake timeout",
			"server sent goaway", "stream error",
		} {
			if strings.Contains(msg, s) {
				return true
			}
		}
	}
	return false
}

func fetchGitOnce(ctx context.Context, src spec.Source, acc *gitAccess, mkdir func(string) (string, error)) (*Checkout, error) {
	if mkdir == nil {
		mkdir = func(pattern string) (string, error) { return os.MkdirTemp("", pattern) }
	}
	dir, err := mkdir("pando-src-")
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not make room to fetch the source.", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	opts := &git.CloneOptions{
		// The credential is go-git's, in this process (R-112): it is not
		// written into the clone's config, and the remote recorded there is
		// the address without it. Nothing the builder is given carries it.
		URL:      acc.url,
		Auth:     acc.auth,
		CABundle: acc.caBundle,

		// Submodules are not initialized. R-021: Pando fills declared slots and
		// never invents topology, and silently pulling in another repository's
		// contents is the same class of decision.
		RecurseSubmodules: git.NoRecurseSubmodules,
	}

	// A commit takes precedence over a ref: redeploying a pinned revision must
	// build the same code, not whatever the branch points at now (R-120).
	if src.Commit == "" && src.Ref != "" {
		opts.ReferenceName = referenceFor(src.Ref)
		opts.SingleBranch = true
	}

	// Shallow only when nothing is pinned.
	//
	// Depth 1 fetches the tip commit and no history, so checking out any other
	// SHA fails with "object not found" — which made R-120 true only when the
	// pinned commit happened to still be the tip. Every rollback to an earlier
	// spec revision pins one that is not, and every one of them failed at
	// clone. The cost of the full history is paid on the deploys that need it
	// and nowhere else.
	if src.Commit == "" {
		opts.Depth = 1
	}

	repo, err := git.PlainCloneContext(ctx, dir, false, opts)
	if err != nil {
		cleanup()
		if denied := accessError(err, src.URL, acc.connection); denied != nil {
			return nil, denied
		}
		return nil, errs.Wrap(errs.ValidInvalid,
			"Pando could not fetch this app's source.", err).
			WithDetail("url", src.URL).
			WithRemedy("Check that the repository address and branch are correct, and that this installation can reach it.")
	}

	if src.Commit != "" {
		worktree, err := repo.Worktree()
		if err != nil {
			cleanup()
			return nil, errs.Wrap(errs.Internal, "Could not read the fetched source.", err)
		}
		if err := worktree.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(src.Commit)}); err != nil {
			cleanup()
			return nil, errs.Wrap(errs.ValidInvalid,
				"Pando could not find that commit in this app's repository.", err).
				WithDetail("commit", src.Commit)
		}
	}

	head, err := repo.Head()
	if err != nil {
		cleanup()
		return nil, errs.Wrap(errs.Internal, "Could not read the fetched source.", err)
	}

	return &Checkout{Dir: dir, Commit: head.Hash().String(), cleanup: cleanup}, nil
}

// emptyView is the source of an app that has none: every name is absent.
type emptyView struct{}

func (emptyView) Open(name string) (io.ReadCloser, error) {
	return nil, errs.Newf(errs.NotFound, "%q is not in the app's source; this app runs a published image.", name)
}

func (emptyView) Stat(name string) (api.FileInfo, error) {
	return api.FileInfo{}, errs.Newf(errs.NotFound, "%q is not in the app's source; this app runs a published image.", name)
}

func (emptyView) Glob(string) ([]string, error) { return nil, nil }

// referenceFor guesses whether a ref names a branch or a tag.
func referenceFor(ref string) plumbing.ReferenceName {
	if strings.HasPrefix(ref, "refs/") {
		return plumbing.ReferenceName(ref)
	}
	return plumbing.NewBranchReferenceName(ref)
}

// dirView is a read-only view of a directory.
//
// It has no write methods, structurally (R-020), and every path is resolved
// inside the root so a traversal cannot reach the host filesystem.
type dirView struct{ root string }

func (v *dirView) resolve(name string) (string, error) {
	clean := filepath.Clean("/" + name)
	full := filepath.Join(v.root, clean)
	if !strings.HasPrefix(full, filepath.Clean(v.root)) {
		return "", errs.Newf(errs.ValidInvalid, "%q is outside the app's source.", name)
	}
	return full, nil
}

// Root exposes the directory the view is rooted at.
//
// A builder that needs the source on a filesystem — BuildKit mounts it rather
// than reading it file by file — asks for this through a narrow interface
// assertion. It is deliberately not part of api.SourceView: a view is read-only
// by construction, and a builder that can reach the path can write to it. The
// assertion makes that capability explicit at the one call site that needs it.
func (v *dirView) Root() string { return v.root }

func (v *dirView) Open(name string) (io.ReadCloser, error) {
	full, err := v.resolve(name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, errs.Wrap(errs.NotFound, "That file is not in the app's source.", err)
	}
	return f, nil
}

func (v *dirView) Stat(name string) (api.FileInfo, error) {
	full, err := v.resolve(name)
	if err != nil {
		return api.FileInfo{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return api.FileInfo{}, errs.Wrap(errs.NotFound, "That file is not in the app's source.", err)
	}
	return api.FileInfo{Name: info.Name(), Size: info.Size(), IsDir: info.IsDir()}, nil
}

func (v *dirView) Glob(pattern string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(v.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			//nolint:nilerr // Returning nil continues the walk. One unreadable
			// entry in a repository must not fail the whole glob — detection
			// would then be defeated by a single bad symlink.
			return nil
		}
		rel, relErr := filepath.Rel(v.root, path)
		if relErr != nil {
			//nolint:nilerr // Same: skip this entry, keep walking.
			return nil
		}
		if match, _ := filepath.Match(pattern, rel); match {
			out = append(out, rel)
		}
		if match, _ := filepath.Match(pattern, d.Name()); match && !contains(out, rel) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the app's source.", err)
	}
	return out, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

var _ api.SourceView = (*dirView)(nil)

// ResolveRef returns the commit a ref currently points at, without cloning.
//
// Auto-deploy runs this every five minutes for every app tracking a branch, and
// the answer is usually "the same as last time". Cloning to find that out would
// make Pando's largest source of network traffic a question it almost never
// needs to act on.
func (s Sources) ResolveRef(ctx context.Context, src spec.Source) (string, error) {
	if src.Type != spec.SourceGit || src.URL == "" {
		return "", nil
	}

	acc, err := s.access(ctx, src, PurposeCheck)
	if err != nil {
		return "", err
	}
	refs, err := listRefs(ctx, acc)
	if err != nil {
		if denied := accessError(err, src.URL, acc.connection); denied != nil {
			return "", denied
		}
		return "", errs.Wrap(errs.ValidInvalid,
			"Pando could not reach this app's source to check for new commits.", err).
			WithDetail("url", src.URL)
	}

	wanted := referenceFor(src.Ref)
	for _, ref := range refs {
		if ref.Name() == wanted {
			return ref.Hash().String(), nil
		}
	}
	return "", nil
}

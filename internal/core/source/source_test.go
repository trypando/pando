package source_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

func ctx() context.Context { return context.Background() }

// --- git -------------------------------------------------------------------

// repo builds a real git repository on disk and returns its path plus the SHAs
// of the commits it made, oldest first.
func repo(t *testing.T, commits ...map[string]string) (string, []string) {
	t.Helper()
	dir := t.TempDir()

	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := r.Worktree()
	require.NoError(t, err)

	var shas []string
	for i, files := range commits {
		for name, body := range files {
			full := filepath.Join(dir, name)
			require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
			require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
			_, err := wt.Add(name)
			require.NoError(t, err)
		}
		hash, err := wt.Commit("commit", &git.CommitOptions{
			Author: &object.Signature{Name: "Test", Email: "t@example", When: time.Unix(int64(1700000000+i), 0)},
		})
		require.NoError(t, err)
		shas = append(shas, hash.String())
	}
	return dir, shas
}

func TestFetchGitClonesTheWorkingTree(t *testing.T) {
	dir, shas := repo(t, map[string]string{"main.go": "package main", "src/app.js": "x"})

	co, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceGit, URL: dir})
	require.NoError(t, err)
	defer co.Close()

	require.Equal(t, shas[0], co.Commit, "R-120: the resolved SHA is what runs")
	require.FileExists(t, filepath.Join(co.Dir, "main.go"))
	require.FileExists(t, filepath.Join(co.Dir, "src", "app.js"))
}

// R-120: a pinned commit takes precedence over a ref, so redeploying a pinned
// revision builds the same code rather than whatever the branch points at now.
func TestR120_APinnedCommitWinsOverTheBranchHead(t *testing.T) {
	dir, shas := repo(t,
		map[string]string{"version.txt": "one"},
		map[string]string{"version.txt": "two"})
	require.Len(t, shas, 2)

	co, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceGit, URL: dir, Commit: shas[0]})
	require.NoError(t, err)
	defer co.Close()

	require.Equal(t, shas[0], co.Commit)
	body, err := os.ReadFile(filepath.Join(co.Dir, "version.txt"))
	require.NoError(t, err)
	require.Equal(t, "one", string(body), "the old revision, not the branch head")
}

func TestFetchGitReportsACommitThatIsNotInTheRepository(t *testing.T) {
	dir, _ := repo(t, map[string]string{"main.go": "package main"})

	_, err := source.Sources{}.Fetch(ctx(), spec.Source{
		Type: spec.SourceGit, URL: dir,
		Commit: "1234567890abcdef1234567890abcdef12345678",
	})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "could not find that commit")
	require.Equal(t, "1234567890abcdef1234567890abcdef12345678", errs.As(err).Details["commit"])
}

func TestFetchGitReportsAnUnreachableRepository(t *testing.T) {
	_, err := source.Sources{}.Fetch(ctx(), spec.Source{
		Type: spec.SourceGit, URL: filepath.Join(t.TempDir(), "not-a-repo"),
	})
	require.Equal(t, errs.SourceUnreadable, errs.CodeOf(err))
	require.NotEmpty(t, errs.As(err).Remedy)
}

func TestFetchGitReportsABranchThatDoesNotExist(t *testing.T) {
	dir, _ := repo(t, map[string]string{"main.go": "package main"})

	_, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceGit, URL: dir, Ref: "no-such-branch"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Equal(t, dir, errs.As(err).Details["url"])
}

// A prebuilt image is run as it is: there is nothing to fetch.
func TestFetchOfAnImageClonesNothing(t *testing.T) {
	co, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceImage, Digest: "sha256:abc"})
	require.NoError(t, err)
	require.Empty(t, co.Dir)
	require.Equal(t, "sha256:abc", co.Commit)
	co.Close() // a checkout with nothing to clean up closes without panicking
}

func TestFetchRefusesASourceTypeItDoesNotKnow(t *testing.T) {
	_, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceType("carrier-pigeon")})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "carrier-pigeon")
}

func TestCloseRemovesTheCheckout(t *testing.T) {
	dir, _ := repo(t, map[string]string{"main.go": "package main"})

	co, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceGit, URL: dir})
	require.NoError(t, err)
	require.DirExists(t, co.Dir)

	co.Close()
	require.NoDirExists(t, co.Dir, "a checkout does not outlive the deploy that made it")
}

// Auto-deploy asks this every five minutes per app, and the answer is usually
// "the same as last time". Cloning to find that out would make Pando's largest
// source of traffic a question it almost never needs to act on.
func TestResolveRefAnswersWithoutCloning(t *testing.T) {
	dir, shas := repo(t, map[string]string{"main.go": "package main"})

	head, err := git.PlainOpen(dir)
	require.NoError(t, err)
	ref, err := head.Head()
	require.NoError(t, err)
	branch := ref.Name().Short()

	got, err := source.Sources{}.ResolveRef(ctx(), spec.Source{Type: spec.SourceGit, URL: dir, Ref: branch})
	require.NoError(t, err)
	require.Equal(t, shas[0], got)

	// A fully qualified ref resolves too.
	got, err = source.Sources{}.ResolveRef(ctx(), spec.Source{
		Type: spec.SourceGit, URL: dir, Ref: "refs/heads/" + branch,
	})
	require.NoError(t, err)
	require.Equal(t, shas[0], got)
}

func TestResolveRefOfAnUnknownBranchIsEmptyRatherThanAnError(t *testing.T) {
	dir, _ := repo(t, map[string]string{"main.go": "package main"})

	got, err := source.Sources{}.ResolveRef(ctx(), spec.Source{Type: spec.SourceGit, URL: dir, Ref: "no-such-branch"})
	require.NoError(t, err, "nothing to deploy is not a failure to check")
	require.Empty(t, got)
}

func TestResolveRefIgnoresSourcesThatCannotHaveOne(t *testing.T) {
	for _, src := range []spec.Source{
		{Type: spec.SourceImage, Digest: "sha256:abc"},
		{Type: spec.SourceUpload, UploadID: "app_01HQ8"},
		{Type: spec.SourceGit, URL: ""},
	} {
		got, err := source.Sources{}.ResolveRef(ctx(), src)
		require.NoError(t, err)
		require.Empty(t, got)
	}
}

func TestResolveRefReportsARepositoryItCannotReach(t *testing.T) {
	_, err := source.Sources{}.ResolveRef(ctx(), spec.Source{
		Type: spec.SourceGit, URL: filepath.Join(t.TempDir(), "gone"), Ref: "main",
	})
	require.Equal(t, errs.SourceUnreadable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "does not exist")
}

// --- the read-only view ----------------------------------------------------

func TestTheViewReadsFilesAndReportsTheirShape(t *testing.T) {
	dir, _ := repo(t, map[string]string{"package.json": `{"name":"notes"}`, "src/app.js": "x"})
	co, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceGit, URL: dir})
	require.NoError(t, err)
	defer co.Close()
	v := co.View("")

	f, err := v.Open("package.json")
	require.NoError(t, err)
	body, err := io.ReadAll(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.JSONEq(t, `{"name":"notes"}`, string(body))

	info, err := v.Stat("package.json")
	require.NoError(t, err)
	require.Equal(t, "package.json", info.Name)
	require.False(t, info.IsDir)
	require.NotZero(t, info.Size)

	info, err = v.Stat("src")
	require.NoError(t, err)
	require.True(t, info.IsDir)
}

func TestTheViewSaysWhenAFileIsNotInTheSource(t *testing.T) {
	dir, _ := repo(t, map[string]string{"main.go": "package main"})
	co, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceGit, URL: dir})
	require.NoError(t, err)
	defer co.Close()
	v := co.View("")

	_, err = v.Open("package.json")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	_, err = v.Stat("package.json")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
}

// R-020: the view is read-only structurally, and a traversal cannot reach the
// host filesystem.
func TestR020_TheViewCannotReachOutsideTheSource(t *testing.T) {
	dir, _ := repo(t, map[string]string{"main.go": "package main"})
	co, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceGit, URL: dir})
	require.NoError(t, err)
	defer co.Close()
	v := co.View("")

	// ../ is cleaned against the root rather than followed, so a traversal
	// resolves back inside the source and finds nothing.
	for _, escape := range []string{"../../../etc/passwd", "/etc/passwd", "../../../../etc/hosts"} {
		_, openErr := v.Open(escape)
		require.Error(t, openErr, escape)

		_, statErr := v.Stat(escape)
		require.Error(t, statErr, escape)
	}

	// ".." cleans to the root itself, so it resolves to the source directory
	// rather than its parent. Reaching the parent is what must not happen.
	info, err := v.Stat("..")
	require.NoError(t, err)
	require.True(t, info.IsDir)
	require.Equal(t, filepath.Base(co.Dir), info.Name, "the root, not the directory above it")

	// Nothing on the interface can write.
	require.NotContains(t, methodsOf(v), "Write")
	require.NotContains(t, methodsOf(v), "Create")
	require.NotContains(t, methodsOf(v), "Remove")
}

func TestTheViewHonorsASubdirectory(t *testing.T) {
	dir, _ := repo(t, map[string]string{
		"README.md":           "root",
		"services/api/go.mod": "module api",
	})
	co, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceGit, URL: dir})
	require.NoError(t, err)
	defer co.Close()

	v := co.View("services/api")
	_, err = v.Stat("go.mod")
	require.NoError(t, err, "the subdirectory is the root")

	_, err = v.Stat("README.md")
	require.Error(t, err, "the repository root is not visible through a subdirectory view")
}

func TestGlobMatchesByFullPathAndByBaseName(t *testing.T) {
	dir, _ := repo(t, map[string]string{
		"Dockerfile":      "FROM scratch",
		"go.mod":          "module x",
		"src/index.js":    "x",
		"src/helper.js":   "y",
		"deep/a/b/go.mod": "module y",
	})
	co, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceGit, URL: dir})
	require.NoError(t, err)
	defer co.Close()
	v := co.View("")

	got, err := v.Glob("Dockerfile")
	require.NoError(t, err)
	require.Equal(t, []string{"Dockerfile"}, got)

	// A base-name pattern finds the file however deep it is, which is what a
	// detector asking "is there a go.mod anywhere" needs.
	got, err = v.Glob("go.mod")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"go.mod", "deep/a/b/go.mod"}, got)

	got, err = v.Glob("src/*.js")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"src/index.js", "src/helper.js"}, got)

	got, err = v.Glob("*.rb")
	require.NoError(t, err)
	require.Empty(t, got)
}

// One unreadable entry must not fail the whole glob — detection would then be
// defeated by a single bad symlink.
func TestGlobSurvivesABrokenSymlink(t *testing.T) {
	dir, _ := repo(t, map[string]string{"go.mod": "module x"})
	co, err := source.Sources{}.Fetch(ctx(), spec.Source{Type: spec.SourceGit, URL: dir})
	require.NoError(t, err)
	defer co.Close()

	require.NoError(t, os.Symlink(filepath.Join(co.Dir, "nowhere"), filepath.Join(co.Dir, "dangling")))

	got, err := co.View("").Glob("go.mod")
	require.NoError(t, err)
	require.Contains(t, got, "go.mod")
}

// --- uploads ---------------------------------------------------------------

// archive builds a gzipped tar from a name->content map, plus any extra
// headers a test needs to express something a normal packer would not write.
func archive(t *testing.T, files map[string]string, extra ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	for _, h := range extra {
		require.NoError(t, tw.WriteHeader(h))
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// uploads keeps uploaded source in a temporary directory for one test.
func uploads(t *testing.T) source.Sources {
	t.Helper()
	return source.Sources{UploadDir: t.TempDir()}
}

// An empty upload directory is refused rather than read as the working
// directory, which is where every upload would otherwise land.
func TestAnUploadNeedsAnUploadDirectory(t *testing.T) {
	var none source.Sources

	_, err := none.StoreUpload("app_01HQ8", bytes.NewReader(nil))
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	_, err = none.Fetch(ctx(), spec.Source{Type: spec.SourceUpload, UploadID: "app_01HQ8"})
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Equal(t, errs.Internal, errs.CodeOf(none.DiscardUpload("app_01HQ8")))
}

// R-262: an agent that just generated an app cannot commit and push, but it can
// run a command. The upload is the source of record for that app's next deploy.
func TestR262_AnUploadIsStoredThenExpandedIntoACheckout(t *testing.T) {
	src := uploads(t)

	packed := archive(t, map[string]string{"main.go": "package main", "src/app.js": "x"})
	path, err := src.StoreUpload("app_01HQ8", bytes.NewReader(packed))
	require.NoError(t, err)
	require.FileExists(t, path)

	co, err := src.Fetch(ctx(), spec.Source{Type: spec.SourceUpload, UploadID: "app_01HQ8"})
	require.NoError(t, err)
	defer co.Close()

	require.Empty(t, co.Commit, "an upload has no revision, and none is invented")
	body, err := os.ReadFile(filepath.Join(co.Dir, "main.go"))
	require.NoError(t, err)
	require.Equal(t, "package main", string(body))
	require.FileExists(t, filepath.Join(co.Dir, "src", "app.js"))
}

// An uploaded app's checkout is removed like a clone's. It had no cleanup, so
// every detection and deploy of an uploaded app left a copy of its source in
// the temporary directory for as long as the server ran (issue #55).
func TestR262_AnUploadedCheckoutIsRemovedOnClose(t *testing.T) {
	src := uploads(t)
	_, err := src.StoreUpload("app_01HQ8", bytes.NewReader(archive(t, map[string]string{"main.go": "package main"})))
	require.NoError(t, err)

	co, err := src.Fetch(ctx(), spec.Source{Type: spec.SourceUpload, UploadID: "app_01HQ8"})
	require.NoError(t, err)
	require.DirExists(t, co.Dir)

	co.Close()
	require.NoDirExists(t, co.Dir)
}

// Written to a temporary name and renamed, so a deploy running while an upload
// is in flight reads the previous archive rather than half of the new one.
func TestStoreUploadReplacesTheArchiveAtomically(t *testing.T) {
	src := uploads(t)

	_, err := src.StoreUpload("app_01HQ8", bytes.NewReader(archive(t, map[string]string{"v": "one"})))
	require.NoError(t, err)
	_, err = src.StoreUpload("app_01HQ8", bytes.NewReader(archive(t, map[string]string{"v": "two"})))
	require.NoError(t, err)

	entries, err := os.ReadDir(src.UploadDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no .partial is left behind")
	require.Equal(t, "app_01HQ8.tar.gz", entries[0].Name())

	co, err := src.Fetch(ctx(), spec.Source{Type: spec.SourceUpload, UploadID: "app_01HQ8"})
	require.NoError(t, err)
	defer co.Close()
	body, err := os.ReadFile(filepath.Join(co.Dir, "v"))
	require.NoError(t, err)
	require.Equal(t, "two", string(body))
}

func TestStoreUploadReportsAReadThatFails(t *testing.T) {
	src := uploads(t)

	_, err := src.StoreUpload("app_01HQ8", failingReader{})
	require.Equal(t, errs.Internal, errs.CodeOf(err))

	entries, err := os.ReadDir(src.UploadDir)
	require.NoError(t, err)
	require.Empty(t, entries, "a failed upload leaves nothing behind")
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestAMissingUploadSaysHowToSendItAgain(t *testing.T) {
	src := uploads(t)

	_, err := src.Fetch(ctx(), spec.Source{Type: spec.SourceUpload, UploadID: "app_gone"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "pando deploy")
}

func TestAnUploadThatIsNotAGzippedTarIsRefused(t *testing.T) {
	src := uploads(t)

	_, err := src.StoreUpload("app_01HQ8", bytes.NewReader([]byte("not gzip at all")))
	require.NoError(t, err, "storing succeeds; it is unpacking that judges the bytes")

	_, err = src.Fetch(ctx(), spec.Source{Type: spec.SourceUpload, UploadID: "app_01HQ8"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "gzipped tar")
}

func TestATruncatedArchiveIsRefused(t *testing.T) {
	src := uploads(t)

	packed := archive(t, map[string]string{"main.go": "package main"})
	_, err := src.StoreUpload("app_01HQ8", bytes.NewReader(packed[:len(packed)-20]))
	require.NoError(t, err)

	_, err = src.Fetch(ctx(), spec.Source{Type: spec.SourceUpload, UploadID: "app_01HQ8"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
}

// Path traversal in a tar is the oldest trick there is, and this archive is
// whatever a user or an agent chose to send.
func TestAnUploadCannotWriteOutsideItsDirectory(t *testing.T) {
	src := uploads(t)

	for _, name := range []string{"../escaped.txt", "../../etc/passwd", "a/../../../escaped"} {
		packed := archive(t, map[string]string{name: "owned"})
		_, err := src.StoreUpload("app_01HQ8", bytes.NewReader(packed))
		require.NoError(t, err)

		_, err = src.Fetch(ctx(), spec.Source{Type: spec.SourceUpload, UploadID: "app_01HQ8"})
		if err == nil {
			// filepath.Clean("/"+name) collapses the traversal, so the entry
			// lands inside rather than escaping. Either outcome is safe; what
			// must never happen is a write outside the directory.
			require.NoFileExists(t, filepath.Join(os.TempDir(), "escaped.txt"))
			continue
		}
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), name)
	}
}

// A source tree with a symlink in it is normal and should still deploy; a
// symlink Pando followed while unpacking is how an archive writes outside its
// directory.
func TestSymlinksAndDevicesAreDroppedRatherThanUnpacked(t *testing.T) {
	src := uploads(t)

	packed := archive(t, map[string]string{"main.go": "package main"},
		&tar.Header{Name: "escape", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink, Mode: 0o777},
		&tar.Header{Name: "hard", Linkname: "main.go", Typeflag: tar.TypeLink, Mode: 0o644},
		&tar.Header{Name: "dev", Typeflag: tar.TypeChar, Mode: 0o666},
		&tar.Header{Name: "nested/", Typeflag: tar.TypeDir, Mode: 0o755},
	)
	_, err := src.StoreUpload("app_01HQ8", bytes.NewReader(packed))
	require.NoError(t, err)

	co, err := src.Fetch(ctx(), spec.Source{Type: spec.SourceUpload, UploadID: "app_01HQ8"})
	require.NoError(t, err, "the rest of the archive still unpacks")
	defer co.Close()

	require.FileExists(t, filepath.Join(co.Dir, "main.go"))
	require.DirExists(t, filepath.Join(co.Dir, "nested"))
	require.NoFileExists(t, filepath.Join(co.Dir, "escape"))
	require.NoFileExists(t, filepath.Join(co.Dir, "dev"))
}

// Extracting everything 0600 produces a source tree only root can read, and a
// build that then runs as a different user answers 403 for every file — a
// failure that appears at runtime, in the app, looking like the app's fault.
func TestFileModesSurviveExtractionWithoutSetuid(t *testing.T) {
	src := uploads(t)

	packed := archive(t, nil,
		&tar.Header{Name: "build.sh", Mode: 0o755, Size: 0, Typeflag: tar.TypeReg},
		&tar.Header{Name: "readme.md", Mode: 0o644, Size: 0, Typeflag: tar.TypeReg},
		&tar.Header{Name: "locked", Mode: 0o000, Size: 0, Typeflag: tar.TypeReg},
		&tar.Header{Name: "sneaky", Mode: 0o4755, Size: 0, Typeflag: tar.TypeReg},
	)
	_, err := src.StoreUpload("app_01HQ8", bytes.NewReader(packed))
	require.NoError(t, err)

	co, err := src.Fetch(ctx(), spec.Source{Type: spec.SourceUpload, UploadID: "app_01HQ8"})
	require.NoError(t, err)
	defer co.Close()

	mode := func(name string) os.FileMode {
		info, statErr := os.Stat(filepath.Join(co.Dir, name))
		require.NoError(t, statErr)
		return info.Mode()
	}

	require.Equal(t, os.FileMode(0o755), mode("build.sh").Perm(), "a build script keeps its executable bit")
	require.Equal(t, os.FileMode(0o644), mode("readme.md").Perm())
	require.Equal(t, os.FileMode(0o644), mode("locked").Perm(), "a zero mode falls back to something readable")

	require.Zero(t, mode("sneaky")&os.ModeSetuid, "setuid cannot arrive in an upload")
	require.Zero(t, mode("sneaky")&os.ModeSetgid)
	require.NotZero(t, mode("build.sh").Perm()&0o600, "Pando can read back what it just wrote")
}

// methodsOf is the crude but honest way to assert a type has no write surface.
func methodsOf(v any) []string {
	var names []string
	t := reflect.TypeOf(v)
	for i := range t.NumMethod() {
		names = append(names, t.Method(i).Name)
	}
	return names
}

package source_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// emptyHTTP serves a repository with no commits: it advertises no refs.
func emptyHTTP(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/acme/empty.git/info/refs", func(w http.ResponseWriter, _ *http.Request) {
		ar := packp.NewAdvRefs()
		ar.Prefix = [][]byte{[]byte("# service=git-upload-pack"), pktline.Flush}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_ = ar.Encode(w)
	})
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return hs.URL + "/acme/empty.git"
}

// refusing is a connection that cannot give a credential.
type refusing struct{}

func (refusing) Access(context.Context, spec.Source, source.Purpose) (*source.Access, error) {
	return nil, errs.New(errs.SourceUnreadable, "the connection was removed")
}

// TestR091_ProbeRefusesAnEmptyRepository asserts that an app is not created
// from a repository with nothing in it to deploy, and says what to do (R-105).
func TestR091_ProbeRefusesAnEmptyRepository(t *testing.T) {
	url := emptyHTTP(t)
	err := source.Sources{}.Probe(ctx(), spec.Source{Type: spec.SourceGit, URL: url})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "%v", err)
	require.Contains(t, err.Error(), "is empty")
	require.Contains(t, errs.As(err).Remedy, "Push a commit")
}

// TestR091_ProbeReportsAConnectionThatCannotBeUsed asserts that a probe
// passes on why its connection gave no credential, and that a source other
// than git is not probed.
func TestR091_ProbeReportsAConnectionThatCannotBeUsed(t *testing.T) {
	err := source.Sources{Credentials: refusing{}}.Probe(ctx(),
		spec.Source{Type: spec.SourceGit, URL: "https://git.example.com/a/b.git", CredentialRef: "src_gone"})
	require.Equal(t, errs.SourceUnreadable, errs.CodeOf(err))

	require.NoError(t, source.Sources{Credentials: refusing{}}.Probe(ctx(),
		spec.Source{Type: spec.SourceImage, URL: "registry.example.com/a:1"}))
}

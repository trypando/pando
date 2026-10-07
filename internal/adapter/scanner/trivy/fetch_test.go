package trivy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// fakeDaemon is the slice of the Docker API a scan's fetch talks to: whether
// an image is present, and a pull. It records the pulls it was asked for and
// the credential each carried.
type fakeDaemon struct {
	mu      sync.Mutex
	present map[string]bool
	pullErr bool
	pulls   []string
	auths   []string
}

func (d *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Api-Version", "1.47")
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	if i := strings.Index(path, "/images/"); i >= 0 {
		path = path[i:]
	} else if i := strings.Index(path, "/volumes/"); i >= 0 {
		path = path[i:]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/_ping"):
		_, _ = w.Write([]byte("OK"))
	case strings.HasPrefix(path, "/volumes/"):
		_, _ = w.Write([]byte(`{"Name":"` + cacheVolume + `"}`))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		if d.present[ref] {
			_, _ = w.Write([]byte(`{"Id":"sha256:present"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such image: ` + ref + `"}`))
	case r.Method == http.MethodPost && path == "/images/create":
		d.pulls = append(d.pulls, r.URL.Query().Get("fromImage"))
		d.auths = append(d.auths, r.Header.Get("X-Registry-Auth"))
		if d.pullErr {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"unauthorized: authentication required"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"status":"Downloaded newer image"}` + "\n"))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not faked: ` + r.Method + " " + r.URL.Path + `"}`))
	}
}

func adapterOn(t *testing.T, d *fakeDaemon) *Adapter {
	t.Helper()
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	a := New()
	raw, err := json.Marshal(Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	require.NoError(t, err)
	require.NoError(t, a.Configure(context.Background(), raw))
	return a
}

const built = "registry.internal:5000/apps/app_01hq8@sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var registryAuth = &api.RegistryAuth{Registry: "registry.internal:5000", Username: "pando",
	Password: secret.New("registry-password-9")}

// TestR311_ABuiltImageIsFetchedFromTheRegistryWithTheRequestsCredential
// asserts that a built image this host does not have — one a pull-only
// runtime ran from the install's registry (issue #72) — is pulled for the scan
// with the credential the request carries, for that pull only, and that an
// image already here, or one with no credential, is not pulled.
func TestR311_ABuiltImageIsFetchedFromTheRegistryWithTheRequestsCredential(t *testing.T) {
	t.Parallel()
	d := &fakeDaemon{present: map[string]bool{"pando/app:dep_1": true}}
	a := adapterOn(t, d)
	ctx := context.Background()

	require.NoError(t, a.fetch(ctx, built, nil), "no credential: left to the save, which says what is missing")
	require.NoError(t, a.fetch(ctx, "pando/app:dep_1", registryAuth), "imported, so already here")
	require.Empty(t, d.pulls)

	require.NoError(t, a.fetch(ctx, built, registryAuth))
	require.Len(t, d.pulls, 1)
	require.Contains(t, d.pulls[0], "registry.internal:5000/apps/app_01hq8")

	raw, err := base64.URLEncoding.DecodeString(d.auths[0])
	require.NoError(t, err)
	var sent struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		ServerAddress string `json:"serveraddress"`
	}
	require.NoError(t, json.Unmarshal(raw, &sent))
	require.Equal(t, "pando", sent.Username)
	require.Equal(t, "registry-password-9", sent.Password)
	require.Equal(t, "registry.internal:5000", sent.ServerAddress)
}

// TestR318_AnImageThatCannotBeFetchedFailsTheScanSayingWhatToCheck asserts
// that a refused pull fails the scan with the image named and a remedy, rather
// than scanning nothing and reporting a clean result.
func TestR318_AnImageThatCannotBeFetchedFailsTheScanSayingWhatToCheck(t *testing.T) {
	t.Parallel()
	d := &fakeDaemon{pullErr: true}
	a := adapterOn(t, d)

	_, err := a.Scan(context.Background(), api.ScanRequest{Image: built, PullAuth: registryAuth})
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	e := errs.As(err)
	require.Contains(t, e.Message, built)
	require.Contains(t, e.Remedy, "accepts Pando's credential")
	require.NotContains(t, e.Message, "registry-password-9")
}

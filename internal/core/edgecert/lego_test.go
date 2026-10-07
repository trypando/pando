package edgecert

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR169_APrivateACMEServerIsTrustedThroughItsCAFile asserts O-49: a CA
// whose certificate no public root signs is reached once its certificate is
// in PANDO_ACME_CA_FILE, and not without it.
func TestR169_APrivateACMEServerIsTrustedThroughItsCAFile(t *testing.T) {
	ca := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ca.Close()

	plain, err := ClientTrusting("")
	require.NoError(t, err)
	require.Nil(t, plain, "no CA file: lego's own client, the system roots")
	resp, err := (&http.Client{}).Get(ca.URL)
	if err == nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "the system roots do not trust it")

	file := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate().Raw}), 0o600))
	client, err := ClientTrusting(file)
	require.NoError(t, err)
	resp, err = client.Get(ca.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()

	empty := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))
	_, err = ClientTrusting(empty)
	require.ErrorContains(t, err, "no PEM certificate")
	_, err = ClientTrusting(filepath.Join(t.TempDir(), "missing.pem"))
	require.ErrorContains(t, err, "could not read")
}

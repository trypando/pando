package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func skewClient(t *testing.T, serverVersion string) (*Client, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if serverVersion != "" {
			w.Header().Set(versionHeader, serverVersion)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	var warned bytes.Buffer
	return &Client{BaseURL: srv.URL, HTTP: srv.Client(), Warn: &warned}, &warned
}

// TestR353_ACLIOutOfStepWithItsServerSaysSoOnce asserts R-353.
func TestR353_ACLIOutOfStepWithItsServerSaysSoOnce(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })

	cases := []struct {
		name, mine, theirs, want string
	}{
		{"server newer", "0.3.1", "0.4.0", "Upgrade the CLI"},
		{"server older", "0.4.0", "0.3.1", "pando updates"},
		{"patch apart", "0.3.0", "0.3.1", ""},
		{"development CLI", "dev", "0.4.0", ""},
		{"anonymous, no header", "0.3.1", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skewOnce = sync.Once{}
			Version = tc.mine
			c, warned := skewClient(t, tc.theirs)
			require.NoError(t, c.Do("GET", "/me", nil, nil))
			require.NoError(t, c.Do("GET", "/me", nil, nil))
			if tc.want == "" {
				require.Empty(t, warned.String())
				return
			}
			require.Contains(t, warned.String(), tc.want)
			require.Equal(t, 1, bytes.Count(warned.Bytes(), []byte("\n")), "said once per run")
		})
	}
}

// The upgrade command matches how this CLI was installed (R-352).
func TestTheCLIUpgradeCommandFollowsHowItWasInstalled(t *testing.T) {
	none := func(string) bool { return false }
	noFile := func(string, string) bool { return false }

	require.Equal(t, "brew upgrade --cask trypando/tap/pando",
		upgradeFor("/opt/homebrew/Caskroom/pando/0.3.1/pando", "0.4.0", none, noFile))

	dpkg := func(p string) bool { return p == "/var/lib/dpkg/info/pando.list" }
	require.Contains(t, upgradeFor("/usr/bin/pando", "0.4.0", dpkg, noFile),
		"sudo apt install ./pando_0.4.0_linux_"+runtime.GOARCH+".deb")

	apk := func(_, s string) bool { return s == "\nP:pando\n" }
	require.Contains(t, upgradeFor("/usr/bin/pando", "0.4.0", none, apk), "apk add --allow-untrusted")
	require.Contains(t, upgradeFor("/usr/bin/pando", "0.4.0", none, noFile), "dnf install")

	require.Equal(t, "go install github.com/trypando/pando/cmd/pando@v0.4.0",
		upgradeFor("/home/me/go/bin/pando", "0.4.0", none, noFile))
	require.Contains(t, upgradeFor("/home/me/tools/pando", "0.4.0", none, noFile),
		"releases/download/v0.4.0/")
}

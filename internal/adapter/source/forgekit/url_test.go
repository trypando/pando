package forgekit

import (
	"strings"
	"testing"
)

func TestParseRepoURL(t *testing.T) {
	for raw, want := range map[string]RepoURL{
		"https://github.com/acme/api":                    {Host: "github.com", Path: "acme/api"},
		"https://GitHub.com/acme/api.git":                {Host: "github.com", Path: "acme/api"},
		"https://gitlab.com/acme/platform/api.git/":      {Host: "gitlab.com", Path: "acme/platform/api"},
		"http://git.example.com:8443/acme/api":           {Host: "git.example.com", Port: "8443", Path: "acme/api"},
		"ssh://git@git.example.com:2222/acme/api.git":    {SSH: true, Host: "git.example.com", Port: "2222", User: "git", Path: "acme/api"},
		"git+ssh://deploy@git.example.com/acme/api":      {SSH: true, Host: "git.example.com", User: "deploy", Path: "acme/api"},
		"git@github.com:acme/api.git":                    {SSH: true, Host: "github.com", User: "git", Path: "acme/api"},
		"  git@GitLab.com:acme/platform/api.git  ":       {SSH: true, Host: "gitlab.com", User: "git", Path: "acme/platform/api"},
		"github.com:acme/api":                            {SSH: true, Host: "github.com", Path: "acme/api"},
		"git@ssh.dev.azure.com:v3/acme/Payments/api":     {SSH: true, Host: "ssh.dev.azure.com", User: "git", Path: "v3/acme/Payments/api"},
		"https://dev.azure.com/acme/Payments/_git/api":   {Host: "dev.azure.com", Path: "acme/Payments/_git/api"},
		"https://user@bitbucket.org/acme/api.git":        {Host: "bitbucket.org", Path: "acme/api"},
		"https://codeberg.org/acme/api.git?ref=whatever": {Host: "codeberg.org", Path: "acme/api"},
	} {
		got, err := ParseRepoURL(raw)
		if err != nil {
			t.Errorf("ParseRepoURL(%q): %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("ParseRepoURL(%q) = %+v, want %+v", raw, got, want)
		}
	}
	for _, raw := range []string{"", "   ", "not an address", "ftp://example.com/acme/api", "https:///acme/api", "acme/api"} {
		if _, err := ParseRepoURL(raw); err == nil {
			t.Errorf("ParseRepoURL(%q) succeeded", raw)
		}
	}
	if got, _ := ParseRepoURL("https://github.com/acme/api"); strings.Join(got.Segments(), ",") != "acme,api" {
		t.Errorf("segments = %v", got.Segments())
	}
	if (RepoURL{}).Segments() != nil {
		t.Error("an empty path has segments")
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"github.com":                       "github.com",
		"  GitHub.com ":                    "github.com",
		"https://gitlab.acme.internal":     "gitlab.acme.internal",
		"https://gitlab.acme.internal/x/y": "gitlab.acme.internal",
		"gitlab.acme.internal:8443":        "gitlab.acme.internal",
		"gitlab.acme.internal/path":        "gitlab.acme.internal",
		"":                                 "",
	} {
		if got := HostOf(in); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScopeSegments(t *testing.T) {
	if got := ScopeSegments(" /acme/platform/ "); strings.Join(got, ",") != "acme,platform" {
		t.Errorf("got %v", got)
	}
	if ScopeSegments("  ") != nil {
		t.Error("an empty scope has segments")
	}
}

func TestCovers(t *testing.T) {
	hosts := []string{"dev.azure.com", "ssh.dev.azure.com"}
	cases := []struct {
		hosts []string
		scope []string
		host  string
		path  []string
		want  int
	}{
		{[]string{"github.com"}, nil, "github.com", []string{"acme", "api"}, 1},
		{[]string{"github.com"}, nil, "GITHUB.com", []string{"acme", "api"}, 1},
		{[]string{"github.com"}, nil, "gitlab.com", []string{"acme", "api"}, 0},
		{[]string{"github.com"}, []string{"acme"}, "github.com", []string{"ACME", "api"}, 2},
		{[]string{"github.com"}, []string{"acme"}, "github.com", []string{"other", "api"}, 0},
		{[]string{"github.com"}, []string{"acme"}, "github.com", []string{"acme"}, 0},
		{[]string{"gitlab.com"}, []string{"acme", "platform"}, "gitlab.com", []string{"acme", "platform", "team", "api"}, 3},
		{[]string{"gitlab.com"}, []string{"acme", "platform"}, "gitlab.com", []string{"acme", "api"}, 0},
		{hosts, []string{"acme"}, "ssh.dev.azure.com", []string{"acme", "Payments", "api"}, 2},
		{[]string{""}, nil, "", []string{"acme", "api"}, 0},
	}
	for _, c := range cases {
		if got := Covers(c.hosts, c.scope, c.host, c.path); got != c.want {
			t.Errorf("Covers(%v, %v, %q, %v) = %d, want %d", c.hosts, c.scope, c.host, c.path, got, c.want)
		}
	}
}

func TestHTTPSAndSCPURL(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{HTTPSURL("github.com", "", "acme/api"), "https://github.com/acme/api.git"},
		{HTTPSURL("github.com", "443", "acme/api"), "https://github.com/acme/api.git"},
		{HTTPSURL("git.example.com", "8443", "acme/api"), "https://git.example.com:8443/acme/api.git"},
		{SCPURL("", "github.com", "", "acme/api"), "git@github.com:acme/api.git"},
		{SCPURL("deploy", "github.com", "22", "acme/api"), "deploy@github.com:acme/api.git"},
		{SCPURL("git", "git.example.com", "2222", "acme/api"), "ssh://git@git.example.com:2222/acme/api.git"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

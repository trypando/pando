package forgekit

import "testing"

func TestNextLinkKeepsCommasInTheAddress(t *testing.T) {
	for header, want := range map[string]string{
		`<https://api.github.com/user/repos?affiliation=owner,collaborator&page=2>; rel="next", <https://api.github.com/user/repos?affiliation=owner,collaborator&page=5>; rel="last"`: "https://api.github.com/user/repos?affiliation=owner,collaborator&page=2",
		`<https://x/1>; rel="prev", <https://x/3>; rel="next"`:  "https://x/3",
		`<https://x/3>; rel=next`:                               "https://x/3",
		`<https://x/1>; rel="first", <https://x/9>; rel="last"`: "",
		``: "",
	} {
		if got := NextLink(header); got != want {
			t.Errorf("NextLink(%q) = %q, want %q", header, got, want)
		}
	}
}

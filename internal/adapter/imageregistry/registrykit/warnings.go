package registrykit

import (
	"encoding/json"
	"fmt"
	"strings"
)

// dockerHub reports the hosts Docker Hub answers on.
func dockerHub(host string) bool {
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return true
	}
	return false
}

// pathDepth counts the components of a registry path prefix.
func pathDepth(prefix string) int {
	if prefix == "" {
		return 0
	}
	return strings.Count(prefix, "/") + 1
}

// Warnings says what in an OCI registry's settings is accepted but will not
// do what the operator likely expects. Shown when the settings are saved;
// nothing here refuses them, Open does that.
func Warnings(raw json.RawMessage) []string {
	var s Settings
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return nil
	}
	addr := strings.TrimSpace(s.URL)
	for _, scheme := range []string{"https://", "http://"} {
		addr = strings.TrimPrefix(addr, scheme)
	}
	host, prefix, _ := strings.Cut(strings.Trim(addr, "/"), "/")
	host, prefix = strings.ToLower(host), strings.Trim(prefix, "/")
	single := s.Layout == string(LayoutSingle) || (s.Layout == "" && dockerHub(host))

	var out []string
	switch {
	case dockerHub(host):
		out = append(out, "Docker Hub does not let Pando delete images through the registry, so a deleted app's images stay there until you delete them on Docker Hub. "+
			"Every pull also counts against Docker Hub's download limits.")
	case host == ghcrHost:
		// ghcr.io/<owner>/<image>: the owner is the first component, and in
		// one repository the image is the second.
		if single && pathDepth(prefix) < 2 {
			out = append(out, missingPath(host, "owner and image name", "ghcr.io/acme/pando"))
		} else if pathDepth(prefix) < 1 {
			out = append(out, missingPath(host, "owner", "ghcr.io/acme"))
		}
	case strings.HasSuffix(host, "-docker.pkg.dev"):
		// <location>-docker.pkg.dev/<project>/<repository>/<image>.
		if single && pathDepth(prefix) < 3 {
			out = append(out, missingPath(host, "project, repository and image name", host+"/my-project/my-repository/pando"))
		} else if pathDepth(prefix) < 2 {
			out = append(out, missingPath(host, "project and repository", host+"/my-project/my-repository"))
		}
	}
	return out
}

func missingPath(host, what, example string) string {
	return fmt.Sprintf("The address names no %s on %s, so pushes will be refused or go somewhere you did not choose. Add it to the address, such as %s.",
		what, host, example)
}

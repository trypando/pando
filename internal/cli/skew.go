package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/trypando/pando/internal/core/update"
)

// Version is this CLI's version, set by main from the release build's stamp.
var Version = "dev"

// versionHeader is httpapi.VersionHeader. Not imported: the CLI is a client of
// the API's wire format, not of its package.
const versionHeader = "Pando-Version"

var skewOnce sync.Once

// warnSkew says once per run that this CLI and the server it reached differ in
// MAJOR or MINOR version (R-353), and how to bring them into step. A warning,
// never a refusal: most commands work across a minor version.
func warnSkew(w io.Writer, server string, resp *http.Response) {
	if w == nil {
		return
	}
	theirs := resp.Header.Get(versionHeader)
	if !update.Skewed(Version, theirs) {
		return
	}
	skewOnce.Do(func() {
		mine := strings.TrimPrefix(Version, "v")
		if update.Newer(mine, theirs) {
			_, _ = fmt.Fprintf(w, "This pando CLI is %s and the server at %s runs %s. Some commands may not match what the "+
				"server accepts. Upgrade the CLI: %s\n", mine, server, theirs, cliUpgrade(theirs))
			return
		}
		_, _ = fmt.Fprintf(w, "This pando CLI is %s and the server at %s runs %s, which is older. Some commands may not "+
			"match what the server accepts. Run `pando updates` to see how to upgrade the server, or use a %s CLI.\n",
			mine, server, theirs, theirs)
	})
}

// cliUpgrade is the command that moves this CLI to version, for the way it
// was installed (R-352): the Homebrew cask, a Linux package, go install, or a
// downloaded archive.
func cliUpgrade(version string) string {
	exe, err := os.Executable()
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
	}
	return upgradeFor(exe, version, fileExists, fileContains)
}

func upgradeFor(exe, version string, exists func(string) bool, contains func(path, s string) bool) string {
	version = strings.TrimPrefix(version, "v")
	download := "https://github.com/trypando/pando/releases/download/v" + version + "/"
	pkg := func(format string) string { return "pando_" + version + "_linux_" + runtime.GOARCH + "." + format }

	switch {
	case strings.Contains(exe, "/Caskroom/") || strings.Contains(exe, "/Cellar/"):
		return "brew upgrade --cask trypando/tap/pando"
	case exe == "/usr/bin/pando" && exists("/var/lib/dpkg/info/pando.list"):
		return "curl -fsSLO " + download + pkg("deb") + " && sudo apt install ./" + pkg("deb")
	case exe == "/usr/bin/pando" && contains("/lib/apk/db/installed", "\nP:pando\n"):
		return "curl -fsSLO " + download + pkg("apk") + " && sudo apk add --allow-untrusted ./" + pkg("apk")
	case exe == "/usr/bin/pando":
		return "curl -fsSLO " + download + pkg("rpm") + " && sudo dnf install ./" + pkg("rpm")
	case filepath.Base(filepath.Dir(exe)) == "bin" && strings.Contains(exe, "/go/bin/"):
		return "go install github.com/trypando/pando/cmd/pando@v" + version
	}
	return "download pando_" + version + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz from " + download +
		" and replace " + exe
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileContains(path, s string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains("\n"+string(b), s)
}

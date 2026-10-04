package update

import (
	"os"
	"strings"
)

// How the server was installed, which decides how it is upgraded (R-352).
//
// The server ships as the container image, run by the release's Compose file
// (internal/reference/install.go); packages and the Homebrew cask carry the
// CLI. So a server is either in a container, or a binary somebody started by
// hand — and the CLI's own upgrade is the CLI's business (internal/cli).
const (
	InstallContainer = "container"
	InstallBinary    = "binary"
)

// Image is the published server image.
const Image = "trypando/pando"

// Install is how this server was installed.
type Install struct {
	Kind string `json:"kind"`
}

// DetectInstall reads how this process was started. Docker writes /.dockerenv
// into every container it runs; a PANDO_ variable would be read as
// configuration.
func DetectInstall() Install {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return Install{Kind: InstallContainer}
	}
	return Install{Kind: InstallBinary}
}

// Upgrade is how to move this installation to a version, in the words and
// commands somebody would paste.
type Upgrade struct {
	Version string `json:"version"`
	// Image is the image to run, for a container install.
	Image string `json:"image,omitempty"`
	// Command upgrades an installation run from the release's Compose file,
	// from the directory that file is in.
	Command string `json:"command"`
	// Instructions says the same in a sentence, including the case the
	// command does not cover.
	Instructions string `json:"instructions"`
}

const download = "https://github.com/trypando/pando/releases/download/v"

// Upgrade says how to move this installation to version.
func (i Install) Upgrade(version string) Upgrade {
	version = strings.TrimPrefix(version, "v")
	if i.Kind == InstallContainer {
		image := Image + ":" + version
		return Upgrade{
			Version: version,
			Image:   image,
			Command: "curl -fsSLO " + download + version + "/docker-compose.yml && docker compose up -d",
			Instructions: "In the directory holding the docker-compose.yml Pando runs from, download Pando " + version +
				"'s docker-compose.yml over it and run docker compose up -d. If you changed that file, set the pando " +
				"service's image to " + image + " instead. If infrastructure-as-code deploys Pando, change the image " +
				"there, or the next apply puts the old version back. Take a backup first: an older version does not " +
				"start against a database a newer one migrated, so going back means restoring one.",
		}
	}
	return Upgrade{
		Version: version,
		Command: "curl -fsSLO " + download + version + "/pando_" + version + "_linux_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz",
		Instructions: "Download Pando " + version + " from " + download + version + "/, replace the pando binary this " +
			"server runs, and start it again. Take a backup first: going back to an older version means restoring one.",
	}
}

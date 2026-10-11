package reference

// How to get the CLI.
//
// The server is installed with the Compose file — it needs Postgres, a
// container runtime and Railpack, none of which a package manager supplies.
// What ships as a package is the CLI, which is the same binary (R-253) pointed
// at an installation over its API.
//
// These are constants rather than a read of `.goreleaser.yaml`, because the
// server serves this document at runtime and there is no release config next to
// a running container. `install_test.go` reads the release config and go.mod and
// fails when they and these disagree — so the page describes what a tag
// actually publishes, and renaming the tap or dropping a package format breaks
// the build rather than the instructions.
type Install struct {
	// Repo is where releases are published.
	Repo string `json:"repo"`
	// Module is the `go install` path.
	Module string `json:"module"`
	// Homebrew is the cask, tap included.
	Homebrew string `json:"homebrew"`
	// Packages are the Linux package formats each release attaches.
	Packages []string `json:"packages"`
	// Archive is how a tarball is named, with the parts a person substitutes.
	Archive string `json:"archive"`
	// Package is how a Linux package is named. Same shape as Archive, so one
	// download line covers every format.
	Package string `json:"package"`
	// Download is where a release's files are, with the version substituted.
	Download string `json:"download"`
	// Platforms are the os/arch pairs built.
	Platforms []string `json:"platforms"`
	// InContainer runs the copy already inside a Compose installation, for
	// somebody who would rather install nothing.
	InContainer string `json:"in_container"`
}

func install() Install {
	return Install{
		Repo:        "https://github.com/trypando/pando",
		Module:      "github.com/trypando/pando/cmd/pando",
		Homebrew:    "trypando/tap/pando",
		Packages:    []string{"deb", "rpm", "apk"},
		Archive:     "pando_<version>_<os>_<arch>.tar.gz",
		Package:     "pando_<version>_linux_<arch>.<format>",
		Download:    "https://github.com/trypando/pando/releases/download/v<version>/",
		Platforms:   []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"},
		InContainer: "docker compose exec pando pando",
	}
}

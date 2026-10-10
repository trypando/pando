//go:build tools

// Package pins raises modules in Pando's module graph that Pando never
// compiles, past versions with known vulnerabilities. A dependency's go.mod can
// require an old version of a module Pando does not build; the dependency scan
// reads the whole graph and reports it, and go mod tidy drops a plain
// requirement of a module no package imports. An import here, behind the tools
// tag that no build sets, keeps the requirement through tidy.
//
// Remove an entry once the dependency that pulls the module in requires a fixed
// version itself (go mod graph shows who requires it).
package pins

import (
	// CVE-2026-10722 (GO-2026-6238), fixed in v0.22.0. Required at v0.17.3 by
	// containerd v2.4.1, which comes in through the BuildKit client.
	_ "github.com/cilium/ebpf"
)

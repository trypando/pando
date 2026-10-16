package source_test

import "github.com/trypando/pando/internal/core/source"

// These tests clone repositories they build in a temporary directory, which
// Pando itself refuses to fetch (transport.go).
func init() { source.AllowLocalRepositories() }

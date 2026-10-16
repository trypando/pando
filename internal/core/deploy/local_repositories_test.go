package deploy_test

import "github.com/trypando/pando/internal/core/source"

// These tests deploy repositories they build in a temporary directory, which
// Pando itself refuses to fetch (source/transport.go).
func init() { source.AllowLocalRepositories() }

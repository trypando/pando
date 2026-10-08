//go:build kwokscale

package traefik

import "k8s.io/client-go/dynamic"

// UseDynamicForScaleTest replaces the adapter's API client with the scale
// harness's (test/kwok, notes-kubernetes-scale-issue-72.md), which records
// every call and lifts client-go's default limit of 5 requests a second.
// Compiled only with the kwokscale build tag, which no release build and no
// CI job sets.
func (a *Adapter) UseDynamicForScaleTest(dyn dynamic.Interface) { a.dyn = dyn }

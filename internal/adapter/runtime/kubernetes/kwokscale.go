//go:build kwokscale

package kubernetes

import (
	"context"
	"time"

	"k8s.io/client-go/kubernetes"
)

// NewForScaleTest is the adapter on a kwok cluster, for the scale harness in
// test/kwok (notes-kubernetes-scale-issue-72.md). Compiled only with the
// kwokscale build tag, which no release build and no CI job sets.
//
// It differs from Configure in two ways, both forced by kwok: the client is
// the harness's, so it can record every call and lift client-go's default
// limit of 5 requests a second; and the NetworkPolicy canary is recorded as
// passed without running, because a kwok pod has no network to probe (O-43).
// poll is how often a wait looks again; zero keeps the adapter's 2 seconds.
func NewForScaleTest(ctx context.Context, cs kubernetes.Interface, cfg Config, poll time.Duration) (*Adapter, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	a := &Adapter{
		probe: func(context.Context) (bool, error) { return true, nil },
		poll:  poll,
	}
	a.use(cs, cfg)
	if _, err := a.networkPolicyEnforced(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

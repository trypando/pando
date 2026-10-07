package traefik

import "k8s.io/client-go/dynamic"

// UseDynamic makes Configure connect to d rather than to a cluster.
func UseDynamic(d dynamic.Interface) func() {
	prev := newDynamic
	newDynamic = func(Config) (dynamic.Interface, error) { return d, nil }
	return func() { newDynamic = prev }
}

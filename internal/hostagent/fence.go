package hostagent

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
)

// The labels the Docker runtime puts on what it creates
// (internal/adapter/runtime/docker). Repeated here, not imported: this package
// imports no adapter.
const (
	labelManaged = "io.pando.managed"
	labelApp     = "io.pando.app"
)

// fenceStopSeconds is how long an app container is given to stop before it is
// killed. Short: a fenced host is cut off, its requests already reach nobody,
// and what fencing is for is ending its background writes before the
// standby starts, two periods later (R-421).
const fenceStopSeconds = 10

// ContainerAPI is the slice of the Docker client a fence uses.
type ContainerAPI interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
}

// DockerFencer stops every running app container on this host, through the
// host's own container runtime, when the host is cut off from most of its
// cluster (R-421). It only stops: nothing here starts a container, and the
// Docker runtime creates app containers with no restart policy, so a fenced
// host never restarts its apps by itself. Pando starts them again, from the
// control plane, once the host is reached.
//
// Only containers Pando made for an app: the edge and Pando's own containers
// carry no app label and are left running.
type DockerFencer struct {
	Docker ContainerAPI
}

// Fence stops the running app containers and returns how many it stopped. It
// tries every one before reporting the first failure, so one container that
// will not stop does not keep the rest running.
func (f DockerFencer) Fence(ctx context.Context) (int, error) {
	list, err := f.Docker.ContainerList(ctx, client.ContainerListOptions{
		Filters: make(client.Filters).Add("label", labelManaged+"=true").Add("label", labelApp),
	})
	if err != nil {
		return 0, fmt.Errorf("could not list this host's app containers: %w", err)
	}
	timeout := fenceStopSeconds
	stopped := 0
	var first error
	for _, c := range list.Items {
		if c.Labels[labelApp] == "" {
			continue
		}
		if _, err := f.Docker.ContainerStop(ctx, c.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
			if first == nil {
				first = fmt.Errorf("could not stop app container %s: %w", c.ID, err)
			}
			continue
		}
		stopped++
	}
	return stopped, first
}

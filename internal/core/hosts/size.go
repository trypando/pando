package hosts

import (
	"context"
	"slices"

	"github.com/trypando/pando/internal/core/state"
)

// Count is how many hosts the cluster has, this install's own included: it
// records itself at its first start (RecordInstallHost, R-414). An install is
// never fewer than one host, also before that record is made.
func (s *Service) Count(ctx context.Context) (int, error) {
	hosts, err := s.Store.Hosts(ctx)
	if err != nil {
		return 0, err
	}
	return max(len(hosts), 1), nil
}

// ControlCount is how many hosts hold the control role, at least one: the
// install's own first host does, also before it is recorded (R-414). Each
// runs an edge (R-417).
func (s *Service) ControlCount(ctx context.Context) (int, error) {
	hosts, err := s.Store.Hosts(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range hosts {
		if slices.Contains(h.Roles, state.HostRoleControl) {
			n++
		}
	}
	return max(n, 1), nil
}

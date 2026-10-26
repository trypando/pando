package hosts

import "context"

// Count is how many hosts the cluster has, this install's own included: a
// cluster of one has no rows until another host joins (DefaultRoles).
func (s *Service) Count(ctx context.Context) (int, error) {
	hosts, err := s.Store.Hosts(ctx)
	if err != nil {
		return 0, err
	}
	return len(hosts) + 1, nil
}

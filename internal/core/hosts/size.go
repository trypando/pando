package hosts

import "context"

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

package main

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/hostagent"
)

// wireClusterRuntime gives the cluster's host service what keeps the runtime
// adapter's host list in line with the cluster's app hosts (R-256, R-418):
// the check a join makes before an app host is let in, and the sync that runs
// after a host joins or is removed.
func wireClusterRuntime(svc *hosts.Service, rt *hosts.Runtime, cfg *config.Config, logger *zap.Logger) {
	rt.Authority = svc.Authority
	rt.LocalAgentAddress = localAgentAddress(cfg.Server.AdvertiseURL)
	declared := map[string]bool{}
	for _, d := range cfg.Adapters {
		declared[d.ID] = true
	}
	rt.Declared = func(id string) bool { return declared[id] }
	svc.Runtime = rt
	svc.OnChange = func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		res, err := rt.Sync(ctx)
		if err != nil {
			logger.Error("the runtime's host list could not be brought in line with the cluster", zap.Error(err))
			return
		}
		if res.Changed {
			logger.Info("the runtime's host list follows the cluster; every replica restarts to run it",
				zap.String("adapter_id", res.AdapterID), zap.Bool("converted", res.Converted))
		}
	}
}

// localAgentAddress is where other hosts reach this machine's agent: the
// host of the advertise address and the agent port. Empty when no advertise
// address is set, since the default one names a container only its own
// Compose network knows; an app-role join then says to set it.
func localAgentAddress(advertise string) string {
	if advertise == "" {
		return ""
	}
	u, err := url.Parse(advertise)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return net.JoinHostPort(u.Hostname(), strconv.Itoa(hostagent.DefaultPort))
}

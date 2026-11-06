package main

import (
	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/webhookroute"
	"github.com/trypando/pando/internal/httpapi"
	"github.com/trypando/pando/internal/proxy"
)

// webhookRouteWiring is what webhook routes are built from.
type webhookRouteWiring struct {
	db         *state.DB
	apps       *state.Apps
	secrets    api.SecretsAdapter
	secretsRef string
	policy     webhookroute.Policy
	authz      webhookroute.Authorizer
	auditor    *audit.Writer
}

// wireWebhookRoutes builds what the proxy decides a delivery with and the service
// the API changes routes through, over one store of secrets (R-425, R-432).
func wireWebhookRoutes(w webhookRouteWiring) (proxy.StateWebhooks, *webhookroute.Service) {
	secrets := state.NewWebhookRouteSecrets(w.db, w.secrets, w.secretsRef)
	store := proxy.StateWebhooks{
		WebhookRouteSecrets: secrets,
		WebhookDeliveries:   state.NewWebhookDeliveries(w.db),
	}
	return store, &webhookroute.Service{
		Apps: w.apps, Secrets: secrets, Policy: w.policy, Authz: w.authz,
		Audit: httpapi.AuditFunc(w.auditor),
	}
}

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR432_TheProxyAndTheAPIShareOneStoreOfSecrets asserts a secret the API
// writes is the one the proxy reads.
func TestR432_TheProxyAndTheAPIShareOneStoreOfSecrets(t *testing.T) {
	store, service := wireWebhookRoutes(webhookRouteWiring{secretsRef: "sec_local"})
	require.Same(t, store.WebhookRouteSecrets, service.Secrets)
	require.NotNil(t, store.WebhookDeliveries)
}

package kubernetes

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestR096_AWorkloadAnswersToItsAliasesInTheCluster asserts R-096: a compose
// service's container_name resolves inside the app, as under compose, so a
// workload's alias is a headless Service selecting its pod. A name a Service
// cannot carry is left out rather than failing the deploy.
func TestR096_AWorkloadAnswersToItsAliasesInTheCluster(t *testing.T) {
	ctx := context.Background()
	a, cs := testAdapter(t, nil)
	plan := webPlan()
	plan.Workloads[1].Aliases = []string{"app-postgres", "app_postgres"}
	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)

	svc, err := cs.CoreV1().Services(testNS).Get(ctx, "app-postgres", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]string{labelWorkload: "db"}, svc.Spec.Selector)
	require.Equal(t, "None", svc.Spec.ClusterIP, "headless, and never reachable from outside (R-026)")

	_, err = cs.CoreV1().Services(testNS).Get(ctx, "app_postgres", metav1.GetOptions{})
	require.Error(t, err)
}

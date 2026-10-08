package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/trypando/pando/internal/errs"
)

// kubeconfigFor is a kubeconfig whose cluster is server.
func kubeconfigFor(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: %q}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
`, server)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// TestTheRuntimesClientIsLimitedAsConfigured: the configured rate reaches the
// client every call goes through, a default far above client-go's 5 a second
// applies when none is set, and the built-in kinds are asked for as protobuf
// (notes-kubernetes-scale-issue-72.md).
func TestTheRuntimesClientIsLimitedAsConfigured(t *testing.T) {
	var (
		mu     sync.Mutex
		accept []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		accept = append(accept, r.Header.Get("Accept"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
	}))
	defer srv.Close()
	kc := kubeconfigFor(t, srv.URL)

	for _, c := range []struct {
		extra string
		qps   float32
		burst int
	}{
		{"", 200, 400},
		{`,"api_qps":25,"api_burst":50`, 25, 50},
		{`,"api_qps":1000`, 1000, 1000},
	} {
		a := New()
		raw := fmt.Sprintf(`{"kubeconfig":%q,"pod_cidr":"10.244.0.0/16","service_cidr":"10.96.0.0/12"%s}`, kc, c.extra)
		require.NoError(t, a.Configure(context.Background(), []byte(raw)), raw)
		require.Equal(t, c.qps, a.rest.QPS, raw)
		require.Equal(t, c.burst, a.rest.Burst, raw)
		require.Equal(t, c.qps, a.cs.CoreV1().RESTClient().GetRateLimiter().QPS(), "the limit reaches the client: %s", raw)
	}

	a := New()
	require.NoError(t, a.Configure(context.Background(), []byte(fmt.Sprintf(`{"kubeconfig":%q,"pod_cidr":"10.244.0.0/16","service_cidr":"10.96.0.0/12"}`, kc))))
	_, err := a.cs.CoreV1().Namespaces().Get(context.Background(), "x", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "%v", err)
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, accept)
	require.True(t, strings.HasPrefix(accept[len(accept)-1], "application/vnd.kubernetes.protobuf"), accept[len(accept)-1])

	for _, bad := range []string{`"api_qps":-1`, `"api_qps":50,"api_burst":10`} {
		cfg := fmt.Sprintf(`{"kubeconfig":%q,"pod_cidr":"10.244.0.0/16","service_cidr":"10.96.0.0/12",%s}`, kc, bad)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(New().Configure(context.Background(), []byte(cfg))), bad)
	}
}

// accountsAfter is a fake cluster whose controller manager makes a
// namespace's default ServiceAccount only after the namespace's first n reads
// of it.
func accountsAfter(n int) (*fake.Clientset, func() int) {
	cs := fake.NewClientset()
	var mu sync.Mutex
	reads := 0
	cs.PrependReactor("get", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		if reads <= n {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, "default")
		}
		return true, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: action.GetNamespace()}}, nil
	})
	return cs, func() int { mu.Lock(); defer mu.Unlock(); return reads }
}

func waitingAdapter(t *testing.T, cs *fake.Clientset, wait time.Duration) *Adapter {
	t.Helper()
	cfg := Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12"}
	require.NoError(t, cfg.validate())
	a := &Adapter{
		probe: func(context.Context) (bool, error) { return true, nil }, poll: 2 * time.Millisecond,
		scheduleWait: -1, accountWait: wait,
	}
	a.use(cs, cfg)
	require.NoError(t, a.HealthCheck(context.Background()))
	return a
}

// TestADeployWaitsForItsNewNamespacesServiceAccount: the cluster refuses
// every pod in a namespace until its default ServiceAccount exists, so a
// first deploy creates its pods only after seeing it.
func TestADeployWaitsForItsNewNamespacesServiceAccount(t *testing.T) {
	cs, reads := accountsAfter(3)
	created := false
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		require.Greater(t, reads(), 3, "no pod before the namespace's ServiceAccount exists")
		created = true
		return false, nil, nil
	})
	a := waitingAdapter(t, cs, time.Second)

	_, err := a.Apply(context.Background(), webPlan())
	require.NoError(t, err)
	require.True(t, created)
	// Four until it appeared, and one more when the plan's volume makes sure of
	// the namespace again (CreateVolume).
	require.Equal(t, 5, reads())
}

// TestANamespaceThatNeverGetsItsServiceAccountFailsReadably: the wait is
// bounded, and what it reports says what is missing and where to look.
func TestANamespaceThatNeverGetsItsServiceAccountFailsReadably(t *testing.T) {
	cs, _ := accountsAfter(1 << 30)
	a := waitingAdapter(t, cs, 20*time.Millisecond)

	start := time.Now()
	_, err := a.Apply(context.Background(), webPlan())
	require.Less(t, time.Since(start), 5*time.Second)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	var e *errs.Error
	require.True(t, errors.As(err, &e))
	require.Contains(t, e.Message, testNS)
	require.Contains(t, e.Message, "no default ServiceAccount")
	require.Contains(t, e.Remedy, "kube-controller-manager")
	pods, lerr := cs.CoreV1().Pods(testNS).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, lerr)
	require.Empty(t, pods.Items, "no pod was tried")
}

// TestWaitingForAServiceAccountStopsWithItsContext: a canceled deploy stops
// waiting at once.
func TestWaitingForAServiceAccountStopsWithItsContext(t *testing.T) {
	cs, _ := accountsAfter(1 << 30)
	a := waitingAdapter(t, cs, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := a.awaitServiceAccount(ctx, testNS)
	require.Less(t, time.Since(start), 5*time.Second)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestAServiceAccountThatCannotBeReadIsReported: an error other than "not
// there yet" is not waited out.
func TestAServiceAccountThatCannotBeReadIsReported(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("get", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, "default", errors.New("no"))
	})
	a := waitingAdapter(t, cs, time.Hour)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(a.awaitServiceAccount(context.Background(), testNS)))
}

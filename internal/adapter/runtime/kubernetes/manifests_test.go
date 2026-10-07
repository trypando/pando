package kubernetes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"
)

const manifestDir = "../../../../deploy/kubernetes"

// manifests decodes every object in deploy/kubernetes, failing on any the
// cluster's own schema would not read.
func manifests(t *testing.T) []runtime.Object {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(manifestDir, "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	var out []runtime.Object
	for _, f := range files {
		if filepath.Base(f) == "kustomization.yaml" {
			continue
		}
		body, err := os.ReadFile(f) //nolint:gosec // a file in this repository
		require.NoError(t, err)
		for _, doc := range strings.Split(string(body), "\n---") {
			if strings.TrimSpace(stripComments(doc)) == "" {
				continue
			}
			obj, _, err := scheme.Codecs.UniversalDeserializer().Decode([]byte(doc), nil, nil)
			require.NoError(t, err, f)
			out = append(out, obj)
		}
	}
	return out
}

func stripComments(doc string) string {
	var keep []string
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

// TestR023_TheManifestsGivePandosPodsTheLabelsAppNamespacesAdmit asserts the
// shipped manifests and the adapter agree: Pando's server pods carry exactly
// the labels every app namespace admits (R-023), run as several replicas that
// each advertise their own pod address, share /var/lib/pando on a
// ReadWriteMany claim (O-39), and the names the adapter's defaults assume
// exist. No Service in them reaches outside the cluster (R-026).
func TestR023_TheManifestsGivePandosPodsTheLabelsAppNamespacesAdmit(t *testing.T) {
	var (
		pando     *appsv1.Deployment
		claims    = map[string]*corev1.PersistentVolumeClaim{}
		roles     = map[string]bool{}
		accounts  = map[string]bool{}
		spaces    = map[string]bool{}
		configMap *corev1.ConfigMap
	)
	for _, obj := range manifests(t) {
		switch o := obj.(type) {
		case *appsv1.Deployment:
			if o.Name == "pando" {
				pando = o
			}
		case *corev1.PersistentVolumeClaim:
			claims[o.Name] = o
		case *rbacv1.ClusterRole:
			roles[o.Name] = true
		case *corev1.ServiceAccount:
			accounts[o.Namespace+"/"+o.Name] = true
		case *corev1.Namespace:
			spaces[o.Name] = true
		case *corev1.ConfigMap:
			configMap = o
		case *corev1.Service:
			require.NotContains(t, []corev1.ServiceType{corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer}, o.Spec.Type, o.Name)
		}
	}

	require.NotNil(t, pando)
	require.GreaterOrEqual(t, *pando.Spec.Replicas, int32(2))
	labels := pando.Spec.Template.Labels
	require.Equal(t, "pando", labels[labelName])
	require.Equal(t, componentSrv, labels[labelComponent])
	require.Equal(t, defaultServiceAccount, pando.Spec.Template.Spec.ServiceAccountName)

	env := map[string]corev1.EnvVar{}
	for _, e := range pando.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	require.Equal(t, "status.podIP", env["POD_IP"].ValueFrom.FieldRef.FieldPath)
	require.Equal(t, "http://$(POD_IP):8080", env["PANDO_SERVER_ADVERTISE_URL"].Value)
	require.Equal(t, "http://pando-proxy:8080", env["PANDO_SERVER_PROXY_UPSTREAM"].Value)

	data := claims["pando-data"]
	require.NotNil(t, data)
	require.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, data.Spec.AccessModes)
	mounted := false
	for _, m := range pando.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "data" && m.MountPath == "/var/lib/pando" {
			mounted = true
		}
	}
	require.True(t, mounted)

	require.True(t, roles[defaultAppRole])
	require.True(t, accounts[defaultPandoNamespace+"/"+defaultServiceAccount])
	require.True(t, accounts[defaultEdgeNamespace+"/"+edgeServiceAccount])
	require.True(t, spaces[defaultPandoNamespace])
	require.True(t, spaces[defaultEdgeNamespace])

	// The runtime declared in the ConfigMap is one this adapter accepts.
	require.NotNil(t, configMap)
	var file struct {
		Adapters map[string]struct {
			Kind   string         `json:"kind"`
			Config map[string]any `json:"config"`
		} `json:"adapters"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(configMap.Data["pando.yaml"]), &file))
	decl := file.Adapters["rt_kubernetes"]
	require.Equal(t, Kind, decl.Kind)
	raw, err := json.Marshal(decl.Config)
	require.NoError(t, err)
	var cfg Config
	require.NoError(t, json.Unmarshal(raw, &cfg))
	require.NoError(t, cfg.validate())
	require.Equal(t, "kubernetes_api", file.Adapters["rte_traefik"].Config["delivery"])
}

// TestR112_OnlyTheBuilderLeavesBaseline: Pod Security baseline refuses a pod
// asking for an Unconfined seccomp or AppArmor profile, which rootless
// BuildKit needs. So BuildKit runs alone in pando-build, which enforces
// privileged (O-48); every other namespace the manifests make enforces
// baseline, and no other pod asks for Unconfined. On a kind cluster the
// builder was in "pando" and its Deployment never made a pod.
func TestR112_OnlyTheBuilderLeavesBaseline(t *testing.T) {
	enforce := map[string]string{}
	var deployments []*appsv1.Deployment
	for _, obj := range manifests(t) {
		switch o := obj.(type) {
		case *corev1.Namespace:
			enforce[o.Name] = o.Labels["pod-security.kubernetes.io/enforce"]
		case *appsv1.Deployment:
			deployments = append(deployments, o)
		}
	}
	for ns, level := range enforce {
		if ns == "pando-build" {
			require.Equal(t, "privileged", level)
			continue
		}
		require.Equal(t, "baseline", level, ns)
	}

	unconfined := func(sec *corev1.SeccompProfile, aa *corev1.AppArmorProfile) bool {
		return (sec != nil && sec.Type == corev1.SeccompProfileTypeUnconfined) ||
			(aa != nil && aa.Type == corev1.AppArmorProfileTypeUnconfined)
	}
	builder := false
	for _, d := range deployments {
		spec := d.Spec.Template.Spec
		loose := spec.SecurityContext != nil && unconfined(spec.SecurityContext.SeccompProfile, spec.SecurityContext.AppArmorProfile)
		for _, c := range spec.Containers {
			if sc := c.SecurityContext; sc != nil {
				loose = loose || unconfined(sc.SeccompProfile, sc.AppArmorProfile)
				require.True(t, sc.Privileged == nil || !*sc.Privileged, "%s is privileged", d.Name)
			}
		}
		if d.Namespace == "pando-build" {
			require.Equal(t, "buildkit", d.Name, "only BuildKit runs in pando-build")
			builder = true
			continue
		}
		require.False(t, loose, "%s asks for an Unconfined profile in %s, which baseline refuses", d.Name, d.Namespace)
	}
	require.True(t, builder, "BuildKit runs in pando-build")
}

// TestR174_TraefikMayWatchNodes: Traefik's CRD provider watches nodes and
// serves no route until it can; on a kind cluster every request to the edge
// was a 404 until its ServiceAccount could list them.
func TestR174_TraefikMayWatchNodes(t *testing.T) {
	roles := map[string]*rbacv1.ClusterRole{}
	var bound []string
	for _, obj := range manifests(t) {
		switch o := obj.(type) {
		case *rbacv1.ClusterRole:
			roles[o.Name] = o
		case *rbacv1.ClusterRoleBinding:
			for _, s := range o.Subjects {
				if s.Kind == "ServiceAccount" && s.Namespace == defaultEdgeNamespace && s.Name == edgeServiceAccount {
					bound = append(bound, o.RoleRef.Name)
				}
			}
		}
	}
	verbs := map[string]bool{}
	for _, name := range bound {
		require.Contains(t, roles, name)
		for _, r := range roles[name].Rules {
			for _, res := range r.Resources {
				if res == "nodes" {
					for _, v := range r.Verbs {
						verbs[v] = true
					}
				}
			}
		}
	}
	require.True(t, verbs["list"] && verbs["watch"], "Traefik's ServiceAccount can list and watch nodes")
}

// TestR112_TheClusterBuilderMountsNoRuntimeSocket asserts R-112 for the
// shipped BuildKit: no hostPath at all, so no container runtime socket, and
// nothing privileged.
func TestR112_TheClusterBuilderMountsNoRuntimeSocket(t *testing.T) {
	found := false
	for _, obj := range manifests(t) {
		d, ok := obj.(*appsv1.Deployment)
		if !ok {
			continue
		}
		for _, v := range d.Spec.Template.Spec.Volumes {
			require.Nil(t, v.HostPath, "%s mounts a host path", d.Name)
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if sc := c.SecurityContext; sc != nil && sc.Privileged != nil {
				require.False(t, *sc.Privileged, d.Name)
			}
		}
		if d.Name == "buildkit" {
			found = true
		}
	}
	require.True(t, found)
}

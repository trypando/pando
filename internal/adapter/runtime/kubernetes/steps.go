package kubernetes

import "github.com/trypando/pando/internal/adapter/api"

// steps walk through setting up this kind (api.KindInfo.Steps).
func steps() []api.Step {
	return []api.Step{
		{ID: "cluster", Label: "Cluster", Title: "Prepare the cluster",
			Do: []api.Action{
				{Text: "Use a network plugin that enforces NetworkPolicy, such as Calico or Cilium."},
				{Text: "Grant the ServiceAccount `pando` in the namespace `pando` these permissions, as `deploy/kubernetes/rbac.yaml` does:", Table: &api.Table{
					Columns: []string{"Where", "Resources", "Verbs"},
					Rows: [][]string{
						{"Cluster", "namespaces", "get, list, create, delete"},
						{"Cluster", "serviceaccounts", "get"},
						{"Cluster", "rolebindings", "create"},
						{"Cluster", "clusterroles " + defaultAppRole, "bind"},
						{"Cluster", "nodes, pods", "get, list, watch"},
						{"Cluster", "persistentvolumes", "get, patch"},
						{"Cluster", "runtimeclasses", "get"},
						{"Cluster", "metrics.k8s.io pods, nodes", "list"},
						{"Each app namespace, through ClusterRole " + defaultAppRole, "pods, pods/log, pods/exec, services, configmaps, secrets, persistentvolumeclaims, resourcequotas, networkpolicies", "get, list, watch, create, update, patch, delete"},
						{"Namespace " + defaultEdgeNamespace, "deployments, services, secrets, poddisruptionbudgets, networkpolicies", "get, list, create, update, delete"},
						{"Namespace " + defaultEdgeNamespace, "pods", "list"},
					},
				}},
				{Text: "Run `kubectl get servicecidr kubernetes` and copy **CIDRS**."},
				{Text: "Run `kubectl cluster-info dump | grep -m1 -- --cluster-cidr` and copy the pod range. On a managed cluster, copy it from the cluster's network settings."},
			},
			Links: []api.Link{
				{Label: "Open rbac.yaml", URL: "https://github.com/trypando/pando/blob/main/deploy/kubernetes/rbac.yaml"},
				{Label: "Open the RBAC guide", URL: "https://kubernetes.io/docs/reference/access-authn-authz/rbac/"},
			},
			Fields: []string{"pod_cidr", "service_cidr"}},
		{ID: "egress", Label: "Egress gateway", Title: "Choose an egress gateway image",
			Fields: []string{"egress_gateway_image"}},
	}
}

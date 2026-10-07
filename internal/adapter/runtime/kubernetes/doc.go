// Package kubernetes runs apps on a Kubernetes cluster (issue #72, PR 6, O-33).
//
// Design: docs/design/notes-kubernetes-runtime-issue-72.md. In short:
//
//   - One namespace per app, "pando-<app id>", with a default-deny
//     NetworkPolicy that admits only Pando's server pods and the app's own
//     pods (R-023, R-025). Nothing in it is reachable from outside the
//     cluster: no NodePort, LoadBalancer, Ingress, hostPort or hostNetwork
//     (R-026).
//   - A headless Service per workload, so the workload's name resolves inside
//     the namespace as it does on a Docker network, and so Pando's proxy can
//     reach it by a name Upstream computes without asking the API.
//   - Bare pods with restartPolicy Never, owned by no controller. Nothing but
//     Pando's reconciler starts a workload again, so a failed app stays failed
//     (R-151).
//   - A PersistentVolumeClaim per volume, its PersistentVolume patched to
//     Retain, kept when the app is destroyed unless the volumes are asked for
//     too (R-204).
//   - The edge (R-174) in its own namespace, as a Deployment of at least two
//     replicas with a disruption budget, behind the only LoadBalancer or
//     NodePort Service Pando creates.
//
// Core never learns any of this vocabulary (R-251): it sees a runtime adapter
// whose capabilities say what it can do (R-254).
package kubernetes

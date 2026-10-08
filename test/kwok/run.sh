#!/usr/bin/env bash
# `make test-kwok-scale`: a kwok cluster of fake nodes, the scale harness
# against it, and the cluster deleted whether or not anything failed
# (notes-kubernetes-scale-issue-72.md). kwokctl runs etcd, the API server, the
# controller manager, the scheduler and kwok's controller as containers of
# whichever Docker DOCKER_CONTEXT names; the nodes and the kubelet are kwok's.
#
# The kubeconfig is written to a file of its own: ~/.kube/config and kubectl's
# current context are left alone.
set -euo pipefail

CLUSTER=${KWOK_CLUSTER:-pando-scale}
NODES=${KWOK_NODES:-300}
APPS=${APPS:-2500,5000,10000,20000}
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
WORK=${KWOK_WORKDIR:-$(mktemp -d)}
KUBECONFIG_FILE=$WORK/kubeconfig
OUT=${KWOK_OUT:-$WORK/results.md}
KWOKCTL=${KWOKCTL:-$(command -v kwokctl || echo "$(go env GOPATH)/bin/kwokctl")}

if [ ! -x "$KWOKCTL" ]; then
  echo "kwokctl is not installed. Install it with: go install sigs.k8s.io/kwok/cmd/kwokctl@v0.7.0" >&2
  exit 1
fi

cleanup() {
  if [ -z "${KWOK_KEEP:-}" ]; then
    "$KWOKCTL" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    # kwokctl has been seen to forget a cluster whose containers it had not
    # finished removing; remove what is left of this one by name.
    for c in $(docker ps -aq --filter "name=^kwok-$CLUSTER-"); do docker rm -f "$c" >/dev/null || true; done
    docker network rm "kwok-$CLUSTER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# Client QPS limits lifted on the controller manager and the scheduler, so
# that placing tens of thousands of pods is not what is measured.
"$KWOKCTL" create cluster --name "$CLUSTER" --runtime docker \
  --kubeconfig "$KUBECONFIG_FILE" --disable-qps-limits --wait 5m
# kwok's default node: 32 CPUs, 256 GiB, 110 pods, which is Kubernetes'
# default pod limit per node.
"$KWOKCTL" scale node --name "$CLUSTER" --replicas "$NODES"
kubectl --kubeconfig "$KUBECONFIG_FILE" apply -f "$HERE/cluster.yaml"
kubectl --kubeconfig "$KUBECONFIG_FILE" wait --for condition=established --timeout=60s crd/ingressroutes.traefik.io

cd "$ROOT"
PANDO_KWOK_KUBECONFIG=$KUBECONFIG_FILE PANDO_KWOK_APPS=$APPS PANDO_KWOK_OUT=$OUT \
  go test -count=1 -timeout=8h -tags=kwokscale -run TestKwokScale -v ./test/kwok/
echo "Results: $OUT"

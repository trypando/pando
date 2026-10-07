#!/usr/bin/env bash
# Brings up Pando on a kind cluster for `make test-kubernetes`: the cluster,
# the image built from this checkout, Traefik's CRDs, and deploy/kubernetes
# with Postgres and a registry beside it (manifests/). Idempotent: run again,
# it reuses what exists.
set -euo pipefail

CLUSTER=${KIND_CLUSTER:-pando-k8s}
CTX=kind-$CLUSTER
IMAGE=${PANDO_K8S_IMAGE:-pando-k8s-test/pando:dev}
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
TRAEFIK_CRDS=https://raw.githubusercontent.com/traefik/traefik/v3.2/docs/content/reference/dynamic-configuration/kubernetes-crd-definition-v1.yml
REGISTRY_NAME=registry.pando.svc.cluster.local:5000
REGISTRY_IP=10.96.0.50
k() { kubectl --context "$CTX" "$@"; }

if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --config "$HERE/kind.yaml"
fi

# kind's local-registry pattern: each node's containerd sends the registry's
# cluster name to its ClusterIP, over plain HTTP.
for node in $(kind get nodes --name "$CLUSTER"); do
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/$REGISTRY_NAME"
  printf '[host."http://%s:5000"]\n  capabilities = ["pull", "resolve"]\n' "$REGISTRY_IP" |
    docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/$REGISTRY_NAME/hosts.toml"
done

# The shared /var/lib/pando (manifests/shared-volume.yaml), emptied: the
# directory outlives a cluster, and a new install starts with none.
CP=$(kind get nodes --name "$CLUSTER" | grep control-plane)
docker exec "$CP" sh -c 'rm -rf /pando-shared/pando-data && mkdir -p /pando-shared/pando-data && chmod 0777 /pando-shared/pando-data'

# Pebble's validation authority resolves app hostnames through cluster DNS:
# every *.pando.test answers with the edge's Service (manifests/pebble.yaml).
corefile=$(k -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')
if ! grep -q pando.test <<<"$corefile"; then
  corefile=$(sed 's|^\( *\)ready$|\1ready\
\1rewrite name regex (.+)\\.pando\\.test\\.$ pando-edge-rte-traefik.pando-edge.svc.cluster.local. answer auto|' <<<"$corefile")
  k -n kube-system create configmap coredns --from-literal=Corefile="$corefile" --dry-run=client -o yaml | k apply -f -
  k -n kube-system rollout restart deployment/coredns
  k -n kube-system rollout status deployment/coredns --timeout=3m
fi

if [ -z "${SKIP_BUILD:-}" ]; then
  docker build -t "$IMAGE" "$ROOT"
fi
kind load docker-image --name "$CLUSTER" "$IMAGE"

k apply --server-side -f "$TRAEFIK_CRDS" >/dev/null
k apply -f "$ROOT/deploy/kubernetes/namespaces.yaml"

# The two Secrets deploy/kubernetes/pando.yaml says to create first.
if ! k -n pando get secret pando-database >/dev/null 2>&1; then
  k -n pando create secret generic pando-database \
    --from-literal=url='postgres://pando:pando-k8s-test@postgres.pando.svc.cluster.local:5432/pando?sslmode=disable'
fi
if ! k -n pando get secret pando-keys >/dev/null 2>&1; then
  keys=$(mktemp -d)
  head -c 32 /dev/urandom >"$keys/secrets.key"
  head -c 32 /dev/urandom >"$keys/token.key"
  k -n pando create secret generic pando-keys \
    --from-file="$keys/secrets.key" --from-file="$keys/token.key"
  rm -rf "$keys"
fi

# The CA Pebble's own TLS certificate is signed by, which Pando trusts
# through PANDO_ACME_CA_FILE.
if ! k -n pando get configmap pebble-ca >/dev/null 2>&1; then
  ca=$(mktemp -d)
  curl -fsSL -o "$ca/pebble.minica.pem" https://raw.githubusercontent.com/letsencrypt/pebble/main/test/certs/pebble.minica.pem
  k -n pando create configmap pebble-ca --from-file="$ca/pebble.minica.pem"
  rm -rf "$ca"
fi

k apply -k "$HERE/manifests"
k -n pando rollout status deployment/pebble --timeout=5m
k -n pando rollout status deployment/postgres --timeout=5m
k -n pando rollout status deployment/registry --timeout=5m
k -n pando-build rollout status deployment/buildkit --timeout=5m
k -n pando rollout status deployment/pando --timeout=10m

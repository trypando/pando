# A Kubernetes runtime adapter (issue #72, PR 6, O-33)

The sixth PR of the stack in `notes-multiple-replicas-issue-72.md`. O-33 asked whether Pando should
have a Kubernetes runtime adapter; the issue's author answered yes **[D]**, as one of two ways to run
apps on more than one machine (the other is PR 7). Placement is Kubernetes' scheduler's, behind the
adapter. Core places nothing (R-256), and R-010 holds: Pando delegates to something that schedules.
R-010 was amended for this runtime (O-44): the cluster recreating an app's pod on another node after
a node fails is the runtime placing a workload, not Pando scheduling.

This note maps the runtime interface onto Kubernetes, decides what is decidable, and lists what is not.
It depends on PR 5 (`notes-image-registry-issue-72.md`): Kubernetes pulls every image from a registry.

## As built (PR 6)

`internal/adapter/runtime/kubernetes`, registered as runtime kind `kubernetes`; IngressRoute delivery in
`internal/adapter/routing/traefik/kubernetes.go`; manifests in `deploy/kubernetes`. The design below is
what was built, with these differences, each a **[P]** the owner may override:

| What | As built | Why |
|---|---|---|
| Interface additions | `RuntimeCapabilities.ImageDelivery` and `EdgeConfig`, `EdgeRequest.EdgeConfig`, `EdgePlan.ReadsRoutesFrom`, `Capacity.LargestFit` (design 03). The planner refuses a workload larger than `LargestFit` | PR 5's branch did not exist when this was written; `ImageDelivery` follows its note and is the field PR 5 adds. Docker reports `[import]` and `[shared_mount]`, and `LargestFit` equal to its totals |
| Services | Headless, as designed, with `publishNotReadyAddresses: true` | A workload's name resolves while its readiness probe fails, as a Docker container name does. Whether to send traffic to an unhealthy app is Pando's decision |
| Quota | Always refuses NodePort and LoadBalancer Services (`services.nodeports: 0`, `services.loadbalancers: 0`). CPU and memory ceilings at twice the plan, only when every workload has limits | A pod and its replacement exist together during a recreate; a CPU quota refuses any pod that does not state its own limits |
| The canary | Three pods in a namespace of the run's own, `pando-canary-<random>` (each replica runs its own; a shared one let one replica's cleanup remove the policy under the other's run, which then recorded the cluster as not enforcing, seen on kind), swept when older than two runs: a server behind a policy admitting one label, an admitted client that must connect, and a refused client that must not. An admitted client that cannot connect is "could not check", not a pass. Passed results last an hour, failed ones five minutes | Without the admitted client a broken network would read as an enforcing one. `Capabilities` reports `SupportsPrivateNetwork` only after a pass, so the planner refuses the runtime until `HealthCheck` has run it |
| Edge certificates | As designed (below): the leader's edge pass (a `cluster.Job`) orders with lego (`internal/core/edgecert`), renews 30 days before expiry, waits an hour after a failed order and keeps serving the certificate it would replace. The account key and each certificate with its key are sealed by the secrets adapter in migration `000052_edge_certificates` (numbered past 000051, which PR 7 may take). The Traefik adapter asks for them through `EdgePlan.Issue`, core fills `EdgePlan.Certificates`, and the runtime writes `kubernetes.io/tls` Secrets the IngressRoutes name in `tls.secretName`. HTTP-01 answers are written to `edge_acme_challenges` and served by whichever replica's router the CA reaches, before the proxy fallback, only for a token Pando is waiting on. Port 80 redirects to HTTPS except `/.well-known/acme-challenge/`. DNS-01 uses the adapter's provider and credentials for the five named providers; another code is refused at configure on this delivery | The CA is a setting (O-49): `PANDO_ACME_DIRECTORY_URL`, Let's Encrypt by default, and `PANDO_ACME_CA_FILE` to trust a private ACME server. `test/kubernetes` issues and renews against Pebble |
| Path prefixes | No `Middleware`; the prefix reaches Pando's proxy, which strips it (R-167) | The same as the file delivery on Docker. Stripping it at the edge would hand the proxy a path it cannot resolve |
| Usage | CPU and memory per workload from `metrics.k8s.io` (`k8s.io/metrics`); `ReportsUsage` is false, and the console says use is not reported, when the cluster serves no metrics API. Disk and volume sizes are -1 | Volume sizes need `nodes/proxy`, which the adapter does not ask for |
| `LargestFit` | The eligible node with the most memory (then CPU) left after every pod's requests, Pando's included; `LargestFitFor` adds back the bundle's own pods' requests, so a redeploy that fits where it runs is allowed (merged with PR 7's planner rule) | The planner compares an app's summed workloads with one node, which is stricter than the scheduler needs; Apply turns a `FailedScheduling` refusal into the deploy's error |
| Keys | `pando-keys` Secret mounted by `subPath` over `/var/lib/pando/secrets.key` and `token.key` on the shared volume | No setting changes, and no replica can write a key of its own. The secrets-key canary and `token_key_check` still run at every start and refuse a replica whose mounted key differs from the one the database was set up with |
| RBAC | The cluster role adds `pods: get, list, watch` cluster-wide, read-only, and `metrics.k8s.io` `pods`, `nodes`: `get, list` | Capacity subtracts what other pods request on each node; usage reads metrics. Pando cannot change, exec into or read logs of a pod outside its own namespaces |
| Trial | Writes are not observed; an observer container in the trial pod reads `/proc/net/tcp` through `pods/exec` | As designed, through exec rather than the pod's log |

**Run on a kind cluster.** `make test-kubernetes` (`test/kubernetes`) creates a kind cluster of one
control plane and two workers with kind's own network plugin, kindnet, builds Pando's image from the
checkout, applies `deploy/kubernetes` with Postgres, a CNCF Distribution registry, a ReadWriteMany volume
every node shares and Pebble as the edge's certificate authority beside it (`test/kubernetes/manifests`),
and drives Pando's API through a port-forward. kindnet enforces
NetworkPolicy (kind v0.24 and later), so the canary passes on it; the note's expectation that it would
refuse predates that. What the run proves, each a `TestR###_` in that package: two replicas against one
Postgres with one leader; the canary; an image app as a bare pod with `restartPolicy: Never` in its own
namespace under default deny, reached only through the proxy, with forged `X-Pando-*` headers and
`pando_*` cookies stripped and an assertion added; a pod in `default` or in another app's namespace
timing out against an app pod, carrying Pando's labels or not, while Pando's pods connect; exec and
logs through the API; a crash loop reaching `failed` and nothing restarting it; a volume as a Retain
claim surviving its pod's replacement, and a delete keeping a final backup taken through the volume
helper; a build pushed by in-cluster BuildKit to the registry and pulled by the node by digest; the edge
at two replicas with a PodDisruptionBudget, every IngressRoute naming `pando-proxy`, and a request into
the edge's Service reaching an app through the proxy while the edge's own pods cannot reach it; and a
deploy resumed on the other replica when its pod is force-deleted. Also: a certificate for an app's
hostname issued by the leader from Pebble over HTTP-01, served by every edge replica with a chain to
Pebble's root, and renewed ahead of expiry (`TestR169_…` in `certificates_test.go`); and an uploaded
source stored by one replica and built by both from the shared `/var/lib/pando`
(`TestR262_AnUploadedSourceIsBuiltByEitherReplica`).

Found and fixed by that run: an app's IngressRoute was named with the app ID as is (`pando-app_01…`),
which the API refuses, so every deploy failed at routing — it is lowercased with a hyphen now; Traefik
3.2's CRD provider watches nodes and serves no route until it can, so `pando-edge-traefik` gets a
ClusterRole reading nodes; a deleted app without storage left its namespace behind, and now goes in one
delete; `Stop` deleted finished pods too, so the reconciler stopping an app as it gave up took the log
of the crash that failed it — it now deletes only pods still running; and BuildKit's Deployment asked
for `Unconfined` profiles, which `baseline` refuses, so it never made a pod. It now runs alone in
`pando-build`, which enforces `privileged` (O-48, decided). And every replica ran the NetworkPolicy
canary in one namespace, `pando-canary`: when two ran at once, one's cleanup deleted the namespace
under the other, whose refused client then connected while the policy was being removed, and that
replica recorded the cluster as not enforcing — every deploy refused for five minutes. Each run now has
a namespace of its own, and the test waits for every replica's canary, not one.

Still not run against a real cluster: DNS-01 issuance; a CNI that does not enforce NetworkPolicy; the ValidatingAdmissionPolicy refusals;
restricted egress through the gateway; `fsGroup` on a network
ReadWriteMany volume (the test's is a directory every kind node mounts); usage from metrics-server; and O-40's 20,000 namespaces through the load harness.

## Running Pando on Kubernetes

`deploy/kubernetes`, applied with `kubectl apply -k deploy/kubernetes`:

| File | What |
|---|---|
| `namespaces.yaml` | `pando` and `pando-edge`, both Pod Security `baseline`, and `pando-build`, `privileged`, for BuildKit alone (O-48) |
| `rbac.yaml` | The tables under "RBAC, minimal" below: Pando's ClusterRole, `pando-app-manager`, the edge Role, and `pando-edge-traefik` (a Role in `pando-edge`, and a ClusterRole reading nodes, which Traefik 3.2 watches) |
| `admission-policy.yaml` | The ValidatingAdmissionPolicies: namespace deletes only of Pando's, no NodePort or LoadBalancer Service but the edge's, no `ExternalName` outside `pando-edge`, no host network or host port, every IngressRoute to `pando-proxy` |
| `networkpolicy.yaml` | Default deny in `pando`; Pando's server pods admit the edge and each other on 8080; BuildKit admits only Pando and reaches only outside the cluster |
| `buildkit.yaml` | Rootless BuildKit alone in `pando-build`, which enforces Pod Security `privileged`: seccomp and AppArmor `Unconfined`, nothing privileged, no host path or runtime socket (O-48, R-112). It admits only Pando's server pods and reaches DNS, the in-cluster registry and outside the cluster (R-113) |
| `pando.yaml` | A ConfigMap declaring `rt_kubernetes` and `rte_traefik` (`delivery: kubernetes_api`); the ReadWriteMany `pando-data` claim (O-39); Deployment `pando`, 2 replicas, `PANDO_SERVER_ADVERTISE_URL=http://$(POD_IP):8080` from the downward API, `PANDO_SERVER_PROXY_UPSTREAM=http://pando-proxy:8080`; Service `pando`; a PodDisruptionBudget |

Before applying: create Secrets `pando-database` (key `url`) and `pando-keys` (`secrets.key` and
`token.key`, 32 random bytes each, kept offsite), set `pod_cidr` and `service_cidr` in the ConfigMap and
the BuildKit policy, choose a ReadWriteMany storage class, and install Traefik's CRDs. Postgres is
external (`PANDO_DATABASE_URL`), as design 00 §1.1 allows. The registry PR 5 adds is not in these
manifests yet; until it is, the Kubernetes runtime runs image apps and refuses a deploy that needs a build
(`PLAN_CAPABILITY_UNSUPPORTED`).

## Topology

```
        :80 / :443 ── Service type LoadBalancer or NodePort (the edge's only)
                                 │
          ┌─────────── namespace pando-edge ────────────┐
          │  Deployment traefik (2+ replicas, spread)    │
          │    or cloudflared (2+ replicas, one tunnel)  │
          │  IngressRoutes, all to Service pando-proxy   │
          └──────────────────────┬───────────────────────┘
                                 │ to Pando's proxy only
          ┌────────────── namespace pando ──────────────┐
          │  Deployment pando (N replicas) ── Service    │
          │  Deployment buildkit (rootless)              │
          │  Deployment registry  (or the org's)         │
          │  Postgres (StatefulSet, or external)         │
          └──────────────────────┬───────────────────────┘
                                 │ only Pando's pods may open connections
          ┌──── namespace pando-app-<id> ───┐   ┌──── namespace pando-app-<id> ───┐
          │ Pod per workload                 │   │ ...                             │
          │ headless Service per workload    │   │                                 │
          │ NetworkPolicy: default deny      │   │                                 │
          │ PVC per volume                   │   │                                 │
          └──────────────────────────────────┘   └─────────────────────────────────┘
```

**[P] Pando runs inside the cluster it deploys to.** Its proxy reaches a workload by a cluster DNS name
and a pod address, which exist only inside the cluster network. A Pando outside the cluster would need
the API server's `services/proxy` path (slow, and a much broader RBAC grant) or a tunnel; neither is
built. One cluster per runtime adapter configuration, and a Pando reaches only the cluster it runs in.

## Namespace per app [D] (O-40)

Each app is a namespace, `pando-app-<app id, lowercased>`, labeled `app.kubernetes.io/managed-by=pando`
and `pando.dev/app=<id>`.

Reasons:

- **Workload names resolve as they do on Docker.** A compose app's web service reaches `db` by that
  name; on Docker it is a network alias (`applyWorkload` sets `Aliases: []string{w.Name}`). In a
  namespace of its own, a Service named `db` resolves as `db` through the pod's DNS search path, with
  nothing rewritten. In a shared namespace, two apps' `db` collide, and making `db` resolve per app
  needs `hostAliases` pinned to ClusterIPs or an environment rewrite, which R-028 forbids.
- **Isolation is one policy per namespace.** A default-deny NetworkPolicy and one allow rule per
  namespace are short enough to audit. In a shared namespace every rule is a label selector, and a
  label mistake on one pod opens it to every other app.
- **Pod Security Admission, ResourceQuota and LimitRange are namespace-scoped**, and each is wanted per
  app (below).
- **Teardown is one delete** when no volume is kept.

Costs, stated:

- **Pando needs cluster-scoped rights to create and delete namespaces.** RBAC cannot limit a delete to
  namespaces carrying a label. A ValidatingAdmissionPolicy shipped with the manifests refuses a
  namespace delete by Pando's ServiceAccount unless the namespace carries
  `app.kubernetes.io/managed-by=pando`.
- **Scale.** Kubernetes' published scalability thresholds put tested limits around 10,000 namespaces
  and 10,000 Services per cluster. The cluster tier is 20,000 apps. Headless Services (below) avoid the
  kube-proxy rules that make Service count expensive, but the namespace count is past what upstream
  tests. **[D] (O-40)** Namespace per app is kept, and the PR 2 load harness proves 20,000 app
  namespaces on the cluster tier before this PR is done, with the result documented. If the harness
  finds a limit, the fallback is several clusters per install, which needs the proxy to reach pods in a
  cluster it does not run in (PR 7's host agent would do it).

A shared namespace was the alternative; the rest of this note marks where it would differ.

## How the proxy reaches an app (R-023)

- **Each workload with a port gets a headless Service** (`clusterIP: None`) in the app's namespace,
  named for the workload. Headless because the proxy needs a name that resolves to the one pod, not a
  virtual IP: kube-proxy installs no rules for a headless Service, which matters at 20,000 apps.
- **`Upstream` returns `http://<workload>.pando-app-<id>.svc.<cluster domain>:<port>`**, computed from
  the reference without asking the API, as the Docker adapter computes a container name. The cluster
  domain is the adapter's setting, default `cluster.local`.
- **Pando's replicas run in the cluster**, so the name resolves and the pod address routes.

**How nothing else reaches it.** Each app namespace gets one NetworkPolicy that selects every pod:

```yaml
policyTypes: [Ingress, Egress]
ingress:
  - from: [{ podSelector: {} }]                     # the app's own workloads
  - from:
      - namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: pando } }
        podSelector:       { matchLabels: { app.kubernetes.io/name: pando, app.kubernetes.io/component: server } }
egress:
  - to: [{ podSelector: {} }]
  - to: [{ namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: kube-system } },
           podSelector: { matchLabels: { k8s-app: kube-dns } } }]
    ports: [{ port: 53, protocol: UDP }, { port: 53, protocol: TCP }]
  - to: [{ ipBlock: { cidr: 0.0.0.0/0, except: [<pod CIDR>, <service CIDR>] } }]   # unrestricted egress only
```

The namespace and pod selectors in the second `from` entry are one element, so both must match: a pod
in Pando's namespace carrying Pando's server labels. Every app namespace denies ingress from every
other app namespace, which is R-025 enforced at the destination, so a misconfigured egress rule in one
app cannot reach another.

**This is only true where the CNI enforces NetworkPolicy.** Flannel, and kubenet, accept the objects and
enforce nothing. The API cannot tell Pando which it has. So **[P]** `HealthCheck` runs a canary on first
configure and every hour: two pods in a throwaway namespace under the same policy, and a connection
from one to the other that must be refused. If it connects, the adapter reports
`SupportsPrivateNetwork: false`, which makes the adapter unusable (design 03 §1.1), and the message
says why: *"This cluster does not enforce NetworkPolicy, so Pando cannot keep apps from reaching each
other. Install a network plugin that enforces it, such as Calico or Cilium."* **[D] (O-43)** An
enforcing network plugin is required, verified by this canary.

**What NetworkPolicy does not cover.** Traffic from a node's own network (host-network pods, the
kubelet) is admitted by most CNIs regardless of policy. Anything in the cluster running with
`hostNetwork: true` can reach app pods directly. Inside Pando's threat model that is the host operator
(R-087); a cluster shared with other teams' workloads is not. **[D] (O-43)** A cluster dedicated to
Pando is recommended, not required: dedication cannot be checked, so the operator documentation states
the host-network gap and recommends it.

**Who else could carry Pando's labels.** Anyone allowed to create pods in the `pando` namespace can
create one with Pando's server labels and reach every app. That right belongs to cluster
administrators and to nothing Pando creates.

## No route in from outside (R-026)

The adapter creates no `NodePort` or `LoadBalancer` Service, no Ingress, no Gateway route, and no
`hostPort` or `hostNetwork` pod for an app. That is the adapter's code; as a second check, the same
ValidatingAdmissionPolicy refuses those objects in any namespace labeled `managed-by=pando`, so a bug
in the adapter fails loudly. App pods get no ServiceAccount token (`automountServiceAccountToken:
false`), so nothing in an app can create anything through the API.

Each app namespace is labeled for Pod Security Admission `baseline`, enforced: no privileged pods, no
host namespaces, no `hostPath`, no added capabilities beyond the default set. `restricted` would
refuse every image that runs as root, which is most of them; Docker runs those today, so `baseline`
matches what the Docker adapter allows.

## The edge Pando runs (R-174) [D] (O-42)

**R-174 holds on Kubernetes as written.** Turning on an edge is a setting in Pando, and Pando creates,
configures, reconciles and removes it through this runtime adapter — Traefik as well as `cloudflared`.
The cluster's own ingress controller is not a substitute, and Pando running Traefik does not depend on
the operator choosing to. The owner rejected the narrower reading this note first proposed, in which
Traefik ran `managed: false` against a cluster ingress Pando did not run.

`SupportsEdge` is true. `ApplyEdge`, `ObserveEdge`, `RemoveEdge` and `Edges` act on one namespace,
`pando-edge`, which Pando's manifests create and which holds nothing but edges and their objects. It is
separate from `pando` so that an edge's ServiceAccount, which Traefik needs to read its routes, can read
no Secret of Pando's — the secrets key is a Secret in `pando`.

### What an `EdgePlan` becomes

| `EdgePlan` field | On Kubernetes |
|---|---|
| `Name`, `Image`, `Args` | A Deployment `pando-edge-<name>` of at least two replicas spread across nodes, with a PodDisruptionBudget (below), labeled `app.kubernetes.io/component: edge`. `restartPolicy: Always`: an edge restarts itself, as it does on Docker (`unless-stopped`); R-149 – R-151 are about apps |
| `Env` | A Secret in `pando-edge`, mounted by `envFrom` (DNS-01 credentials, a tunnel token). Values stay `secret.Value` until the API call that writes them (R-194) |
| `Ports` | One Service for the edge, type `LoadBalancer` by default or `NodePort` (below). The only Service of either type Pando ever creates |
| `Mounts` with `Volume` | Not supported on this runtime: edge replicas are stateless and share nothing. On `kubernetes_api` the Traefik adapter's plan has no ACME volume, because certificates come from Pando (below). A plan that asks for an owned volume is refused at configure |
| `Mounts` with `SharedWithPando` | Not supported on this runtime. A plan that asks for one is refused at configure with a message naming the routing adapter's setting (below) |
| `ProxyAlias` | A Service `pando-proxy` in `pando-edge` of type `ExternalName`, resolving to `pando.pando.svc.<cluster domain>`. The edge reaches Pando's proxy by that name |

**Several replicas, spread across nodes [P].** Issue #72 is about the install surviving the loss of any
one process, and every app is behind the edge, so the edge cannot be one pod. The Deployment runs
`edge_replicas` replicas (an adapter setting, default 2, minimum 2) with a `topologySpreadConstraint` on
`kubernetes.io/hostname` (`maxSkew: 1`, `whenUnsatisfiable: ScheduleAnyway`): a soft spread, so the
scheduler puts replicas on different nodes when it can and on the same node when it cannot. A PodDisruptionBudget with `minAvailable: 1` keeps a drain from
taking every replica at once, and updates roll with `maxUnavailable: 0` and `maxSurge: 1`. On a
one-node cluster both replicas run on that node. `ObserveEdge` then reports the edge healthy with a
warning, *"The edge's replicas share a node, so losing that node takes the edge offline. Add a node
to the cluster to spread them."* It is a warning, never a blocker (CLAUDE.md §4).

With the CRD provider, Traefik holds no state: routes come from the API and certificates from Secrets
(below), so replicas need nothing shared and any replica serves any request. `cloudflared` runs the
same way: several replicas of one connector against the one tunnel, which Cloudflare treats as
redundant connections and balances across.

What remains: a replica being replaced, or one on a node that fails, drops the connections open on it —
websockets and in-flight requests — and those clients reconnect to another replica. Until the load
balancer's health check notices a dead node, some new connections to that node fail.

**Service type [P]: `LoadBalancer` by default, `NodePort` as a setting.** `LoadBalancer` is what managed
clusters and MetalLB provide, and it publishes `:80` and `:443` as the plan asks. A cluster with no load
balancer implementation leaves such a Service pending; the adapter reports that through `ObserveEdge`,
and the message names the setting. With `edge_service_type: NodePort`, the plan's ports map to node
ports from the adapter's settings (default 30080 and 30443; Kubernetes' node port range does not include
80 and 443), and the operator points an external balancer at them. `externalTrafficPolicy: Local`, so
the client address the proxy records is the visitor's and not a node's; a node without an edge replica
then fails the balancer's health check and receives no traffic, which is why replicas are spread.

### Certificates with several replicas [P]

**Pando's leader is the one issuer, and every Traefik replica reads the result from Secrets.** The edge's
ACME settings are unchanged (R-169: HTTP-01 or DNS-01, an email, one of the five named DNS providers or
"other", credentials stored per R-190). On this runtime core carries them out instead of Traefik:

- **Issuance and renewal run in the leader's edge pass**, which already runs on the leader only
  (replicas note), using lego — the ACME library Traefik itself uses, so the same provider codes and
  credential variables work. A hostname added by `Ensure` on any replica is issued at the next pass.
- **Certificates and the ACME account key are kept in Postgres, sealed with the secrets key**, and
  written out as `kubernetes.io/tls` Secrets in `pando-edge`, one per hostname (or one wildcard). The
  `IngressRoute`s name them in `tls.secretName`, and Traefik's CRD provider loads them on every replica.
  Keeping them in Postgres means a DR bundle carries them, as the Docker edge's ACME volume is in the
  full-host bundle, and a deleted Secret is rewritten rather than re-issued.
- **HTTP-01 challenges are answered by Pando's proxy**, not by Traefik. The leader stores the pending
  token in Postgres; an `IngressRoute` for `/.well-known/acme-challenge/` on each hostname goes to
  `pando-proxy` like every other route, and whichever Pando replica receives the CA's request answers
  from the database. That path is then Pando's on every hostname it issues for, as it already is on
  Docker, where Traefik takes it.

Alternatives considered:

| Option | Why not |
|---|---|
| Each Traefik replica runs its own ACME | Every replica orders the same certificates, multiplying Let's Encrypt rate-limit use, and an HTTP-01 challenge fails whenever the CA's request lands on a replica other than the one that ordered it |
| Traefik's ACME with one elected issuer | Traefik's open-source edition has no leader election for ACME; that is a Traefik Enterprise feature |
| cert-manager issuing Secrets | A second controller with cluster-scoped CRDs and webhooks that Pando would configure but not run, which is the split R-174 rejects. Its HTTP-01 solver publishes through Ingress or Gateway objects, which a Traefik limited to the CRD provider does not serve. Porkbun and Namecheap are available only through third-party webhooks, so R-169's five named providers would not all work |

### How Pando writes Traefik's dynamic configuration

**[P] Traefik's Kubernetes CRD provider, with Pando writing `IngressRoute` objects in `pando-edge`.**
On Docker the Traefik adapter writes one file per app into a directory Pando and the edge share. That
does not carry over. Four ways were weighed:

| Option | Why not, or why |
|---|---|
| Files on the ReadWriteMany volume (O-39), mounted into Traefik | Traefik's file provider learns of changes through inotify, which does not report a write made by another node over NFS. Routes would apply only when Traefik restarts. It also ties the edge to the volume O-39's second step removes |
| One ConfigMap holding every route, mounted into Traefik | A ConfigMap holds at most 1 MiB. At about 400 bytes per app that is roughly 2,500 apps; the cluster tier is 20,000. One ConfigMap per app does not help, since adding a mounted ConfigMap changes the pod spec and restarts the edge. A change also takes up to a minute to reach the pod |
| Traefik's HTTP provider, polling Pando for its configuration | Works on every runtime, but it is a new Pando endpoint that has to be kept off the public listener and documented as a surface (R-261), for a problem the cluster already solves |
| **Traefik's Kubernetes CRD provider** | Traefik watches the API and applies a change within seconds. No size limit, no shared storage, no new Pando surface. Pando writes one `IngressRoute` per app, and a `Middleware` where a path prefix is stripped (R-167). Costs: Traefik's CRDs installed with the manifests, and the RBAC below |

**Every `IngressRoute` names one backend: Service `pando-proxy`** (R-023, design 03 §4: an adapter is
told where to point, and that destination is always the proxy). Three things keep it so:

1. **The adapter's code.** `RouteRequest.ProxyUpstream` is the only backend it writes.
2. **Traefik's static configuration**, which the routing adapter generates into `EdgePlan.Args`:
   `--providers.kubernetescrd.namespaces=pando-edge`, so routes written anywhere else are ignored, and
   `allowCrossNamespace=false`, so a route in `pando-edge` cannot name a Service in an app namespace.
   `allowExternalNameServices=true` is set for `pando-proxy`, the one `ExternalName` Service in the
   namespace.
3. **The network, as a second check.** Edge pods carry `component: edge`, not Pando's server labels, so
   every app namespace's NetworkPolicy refuses them. An `IngressRoute` that named an app's Service would
   connect to nothing. The edge's own NetworkPolicy allows egress to Pando's server pods on the proxy
   port, to the API server (the address is an adapter setting), to DNS, and to addresses outside the pod
   and Service CIDRs (ACME and DNS-01 provider APIs).

The same ValidatingAdmissionPolicy that refuses a `NodePort` Service in an app namespace also refuses,
for Pando's ServiceAccount, an `IngressRoute` whose services name anything but `pando-proxy`, a Service
of type `ExternalName` other than `pando-proxy`, and a `LoadBalancer` or `NodePort` Service anywhere but
`pando-edge` or selecting anything but edge pods. A bug in the adapter fails at the API.

**How the routing adapter learns which to write [P].** Core joins the two adapters, as it does for
`EdgePlan` (design 03 §4.4). `RuntimeCapabilities` gains `EdgeConfig`, a list of the ways an edge on this
runtime can receive configuration: `shared_mount` on Docker, `kubernetes_api` here (R-254: data, not a
type assertion). Core hands the default runtime's list to the routing adapter in `EdgeRequest` and at
`Configure`. The Traefik adapter writes files for `shared_mount` and `IngressRoute` objects for
`kubernetes_api`, through the in-cluster client with Pando's ServiceAccount; its `Ensure`, `Remove` and
`Observe` are unchanged in meaning. A routing adapter that supports neither delivery the runtime offers
is refused at configure with a message that says so.

`cloudflared` needs none of this. It holds its configuration remotely, so its plan has no shared mount,
no ports and no Service: a Deployment and a Secret in `pando-edge`, and egress to Cloudflare and to
`pando-proxy`.

**The console's own address.** With an edge configured, the edge is the way into the cluster for the
console as well as the apps: Traefik's lowest-priority catch-all route goes to `pando-proxy`, exactly as
on Docker (design 03 §4.4). Without one, the operator points a load balancer or Ingress at Service
`pando`, which is install topology, as before.

## Egress (R-180 – R-187)

- **Unrestricted** (`Egress.Restricted()` false): the policy above, with the last egress rule present.
  Pod and Service CIDRs are excluded so an app cannot reach cluster-internal addresses it could not
  reach on Docker; they are the adapter's settings, because the API does not report them reliably.
  Nothing else is in the app's path (R-186).
- **Restricted:** the same mechanism as Docker, one namespace over. A per-app egress gateway pod
  (`pando egress-gateway`, Pando's image, `PANDO_EGRESS_RULES`) runs in the app's namespace. The app's
  workloads may reach the gateway, each other and DNS, and nothing else; the gateway alone may reach
  `0.0.0.0/0`, less the same cluster ranges. Workloads get `HTTP_PROXY` / `HTTPS_PROXY` naming the gateway's Service. The gateway
  decides names and resolved addresses with `egress.Compiled`, including `BlockPrivate`.
- **The gaps R-187 names are the same:** raw TCP and clients that ignore the proxy variables do not
  leave. The DNS gap is wider than on Docker: cluster DNS answers any name, so a restricted workload
  can leak data through lookups. The plan note already says traffic outside the gateway does not leave;
  it should also say that lookups do.
- `SupportsEgressRestriction` is true only when the canary passed and the gateway image is known.

## Volumes (R-200 – R-206)

- **`CreateVolume`** creates a PersistentVolumeClaim in the app's namespace, `ReadWriteOnce`, sized by
  `SizeBytes` or the adapter's default, from the adapter's StorageClass (default: the cluster's
  default class).
- **The bound PersistentVolume is patched to `persistentVolumeReclaimPolicy: Retain`.** Deleting a PVC
  under the usual `Delete` policy deletes the data. Retain means a PV outlives an accidental PVC or
  namespace delete, which is the storage-side half of R-204's mechanism (`ON DELETE RESTRICT` is the
  state-side half).
- **`Destroy` with `KeepVolumes`** deletes pods, Services, the gateway and policies, and leaves the
  namespace and its PVCs. The namespace is deleted only when no volume is kept. `DestroyVolume` deletes
  the PVC and then the retained PV.
- **`SnapshotVolume` / `RestoreVolume` [P]** run a helper pod in the app's namespace with the PVC mounted
  (read-only for a snapshot) and stream `tar` through `pods/exec`, as the Docker adapter does with a
  helper container. The helper must land on the node the PVC is attached to while the app runs; pod
  affinity to the app's pod does that for `ReadWriteOnce`. A `ReadWriteOncePod` claim cannot be shared,
  so the app is stopped for the snapshot; the adapter does not create such claims.
- **CSI VolumeSnapshots are not the backup.** A snapshot lives in the cluster's storage system, so it
  does not survive losing that system, and R-217 sends backups to a destination adapter as bytes. A
  snapshot can make the tar consistent later (snapshot, clone, tar the clone); that is an improvement,
  not v1.
- **R-206, in place only:** a restore writes into the PVC of the app recreated from the same spec,
  found by the app's ID in the namespace name.
- **An app's volumes are where the storage class puts them.** With network storage (EBS, Persistent
  Disk, Ceph) a pod can be recreated on another node and its volume follows within a zone. With local
  volumes the pod is tied to its node. Either is the cluster's placement, not Pando's.

## The runtime interface, method by method

| Method or capability | Kubernetes |
|---|---|
| `Apply` | Namespace, policies, quota, Services, PVCs, a Secret for environment, a ConfigMap for carried files, the namespace's pull Secret (below), then one Pod per workload in `DependsOn` order. Idempotent by comparing a digest annotation, as the Docker adapter compares labels. |
| `Observe` | Pods and their status in the namespace. `Running` from phase and container state, `Healthy` from the readiness condition (nil when no probe), `ImageDigest` from `status.containerStatuses[].imageID`, `ExitCode` from the terminated state. `RestartCount` is always 0 (below). Never remediates. |
| `Stop` | Deletes the pods; keeps everything else. A stopped workload is observed as present and not running from its Service, which remains. |
| `Destroy` | Above, under Volumes. |
| `Logs` | `pods/log` with `follow`, `sinceTime`, `tailLines`. |
| `Exec` | `pods/exec` over the WebSocket protocol (Kubernetes 1.30+), with TTY and resize. Exit code from the status channel. Authorization and the audit event stay in core (R-084, R-228). |
| `Upstream` | Computed Service DNS name, above. |
| `Trial` | A namespace per trial under default-deny, the image's pod, and a busybox container in the same pod reading `/proc/net/tcp` (containers in a pod share the network namespace, which is what the Docker adapter's sidecar joins). Removed afterwards. |
| `SupportsPortObservation` | True, by the sidecar above. |
| `SupportsWriteObservation` | False. Kubernetes has nothing like `ContainerDiff`, and reading the node's overlay directories would need host access. The R-201 warning stays generic on this runtime. |
| `SupportsCarriedFiles` | True: a ConfigMap mounted with `subPath` at each file's path, which can sit over a file in the image. ConfigMaps hold at most 1 MiB, which the planner checks. |
| `SupportsMultipleWorkloads` | True, a pod per workload. One pod holding every workload was rejected: the workloads would restart together and could not be recreated one at a time. |
| `SupportsResourceLimits` | True. Requests equal limits, so the scheduler's arithmetic and R-242's agree and the pod is in the Guaranteed QoS class. |
| `SupportsStartThenSwap` | False, as on Docker, until the proxy can point at two bundles (R-145). |
| `ReportsUsage`, `Usage`, `InUse` | CPU and memory from `metrics.k8s.io` when metrics-server is installed; otherwise false. Volume sizes are not reported: the kubelet's stats need `nodes/proxy`, which is close to node-level access, and the adapter does not ask for it. The console says the runtime does not report it (R-245). |
| `LogRetention` | `SupportsSizeCap: false`. The kubelet rotates every container's log at a node-wide size (`containerLogMaxSize`), not a per-app one. The planner and console say logs are bounded by the cluster's setting, with the value in `Capacity.Details` where the adapter can read it. |
| `IsolationClass` | `container`, or `sandboxed` when the adapter's `runtime_class` names a RuntimeClass whose handler is gVisor (`runsc`) or Kata. The adapter checks the RuntimeClass exists; it cannot check what the handler really is, which is the same trust the Docker adapter places in a configured OCI runtime name (R-114, R-115). |
| `ImageDelivery` | `[registry]` (PR 5). `ImportImage` is not offered. |
| `Platform` | The architecture of the eligible nodes, when they agree. Mixed nodes: empty, and the pod gets a `kubernetes.io/arch` selector for the image's platform. |
| `SupportsEdge` | True. `pando-edge`, above. `EdgeConfig` is `[kubernetes_api]`. |
| `SupportsSelfUpgrade` | False. Pando is upgraded by changing the image on its Deployment, which rolls the replicas (R-352). |
| `Capacity` | Below. |

**Environment goes in a Secret per workload** (`envFrom`), not the pod spec, so it is readable only by
something holding `get` on Secrets in that namespace. Either way it is stored in etcd; installs should
enable etcd encryption at rest. That is the cluster's version of R-191's statement that Pando's own
storage protects a copied disk and not a compromised host.

**Private images [D] (O-41).** Kubernetes has no per-pull credential. Each app namespace gets one
`imagePullSecret` holding the credentials its pods pull with — the install registry's pull credential
(PR 5) for built images, and the app's own registry credential for an image app — rewritten at every
`Apply` (an ECR password lasts twelve hours) and deleted at `Destroy`. It stores in etcd a credential the
Docker runtime never stores (design 03 §2.1); no app can read it, because app pods get no ServiceAccount
token. Copying every image app's image into the install registry, so pods pull only from there, is the
cleaner alternative and costs registry storage for every image app.

**Workload names** must be DNS labels to be Service names. The planner refuses a name that is not one,
on this runtime, with the name it would accept.

## Restarts, backoff and "failed stays failed" (R-148 – R-151)

Every Kubernetes controller that keeps a pod running — Deployment, StatefulSet, ReplicaSet — requires
`restartPolicy: Always`. The kubelet then restarts a crashing container on its own, with exponential
backoff capped at five minutes, forever. That is the loop design 05 removed from Docker
(`RestartPolicyDisabled`, `applyWorkload`'s comment): the app reaches `failed`, Pando says it has
stopped trying, and the runtime keeps restarting it. On Kubernetes it is worse, because the pod reports
`Running` most of the time and the reconciler cannot see the loop.

**[P] Bare pods with `restartPolicy: Never`, owned by no controller.** A pod that exits stays exited.
The reconciler sees it in its next pass, decides under R-149's backoff whether to recreate it, and stops
at R-150's threshold. `failed` is then true on this runtime for the same reason it is on Docker: nothing
restarts the workload, and the reconciler has no code path that touches a failed app.

Consequences:

- **"Start a stopped workload" is delete and create.** A finished pod cannot be restarted. The adapter
  keeps the most recent finished pod of each workload until the next one is running, so the logs of the
  crash that sent an app to `failed` are still readable by `Logs` (they go when the pod does).
- **A node that dies takes its pods with it.** Kubernetes marks the pods of a node that stays
  unreachable for deletion (after five minutes by default); they are gone once the node is removed,
  returns, or is tainted out-of-service. The adapter reports a pod being deleted as not running, and
  the reconciler recreates the workload under a new pod name. The scheduler puts it on another node,
  where it starts once its volume can attach there — for a `ReadWriteOnce` volume, after the storage
  system detaches it from the dead node. That is recovery from node failure by way of R-148, and it is
  accepted **[D] (O-44)**: R-010 now says a runtime that spans machines moving a workload after a
  failure is not Pando scheduling. Pando decided only that the workload should exist; the scheduler
  decided where. The adapter does not pin pods to the node they first ran on.
- **`kubectl drain` refuses bare pods without `--force`.** The operator documentation says so: draining a
  node that runs apps needs `--force`, and the reconciler recreates the evicted pods elsewhere within a
  tick. PodDisruptionBudgets do not apply to bare pods.
- **Only a readiness probe.** `HealthPlan` becomes a readiness probe (HTTP, TCP or exec), which only
  marks the pod ready or not. A liveness probe makes the kubelet kill the container, which is
  remediation by the runtime (design 03 §2.2: an adapter never remediates). `Healthy` comes from
  readiness.

## Capacity (R-240 – R-245)

- **`Capacity`** sums `status.allocatable` CPU and memory over eligible nodes: schedulable, not
  cordoned, matching the adapter's `node_selector` if set. `TotalDiskBytes` is 0 (not checked), because
  PVC storage comes from a storage system the API does not size. Per-node figures go in `Details`.
- **R-242 is checked by core against those totals**, as now. A cluster also runs things that are not
  Pando's, so `Capacity` subtracts the requests of pods outside Pando's namespaces on those nodes, and
  says so in `Details`.
- **The total can fit when no node can.** 6 GB free across three nodes does not place a 4 GB workload.
  Multi-host Docker has the same problem (PR 7). **[P] `Capacity` gains `LargestFit`** — the CPU and memory
  of the roomiest single place a workload could go — and the planner refuses a workload larger than it
  with a readable message. Without it, the pod stays `Pending` and `Apply` turns the scheduler's
  `FailedScheduling` event into the error: *"No machine in the cluster has 4 GB of memory free for
  'web'. The largest free space is 2.5 GB on one machine."*
- **A ResourceQuota per app namespace** equal to the plan's limits, plus the gateway and helper pods.
  Not the enforcement — R-242 is Pando's — but a ceiling that makes a bug in the adapter fail at the API.

## Pando in the cluster

| Object | Notes |
|---|---|
| `Deployment pando` | N replicas, the image Pando ships. `PANDO_SERVER_ADVERTISE_URL=http://$(POD_IP):8080` (replicas note). A readiness probe on `/healthz`. PodDisruptionBudget `minAvailable: 1`. |
| `Service pando` | ClusterIP. The edge reaches it through `pando-proxy` in `pando-edge`. With no edge configured, the operator's load balancer or Ingress points at it; that is install topology, like the balancer in the replicas note. |
| `Secret pando-secrets-key` | The secrets key, mounted read-only into every replica (R-190). The canary check at start (replicas note) refuses a replica with a different key. |
| Postgres | External (`PANDO_DATABASE_URL`), or a StatefulSet in the manifests. |
| `Deployment buildkit` | Below. |
| `Deployment registry` | PR 5, or the organization's registry. |
| NetworkPolicy in `pando` | Postgres, BuildKit and the registry admit connections only from Pando's server pods. Pando's server pods admit the proxy port from edge pods in `pando-edge`. |
| Namespace `pando-edge` | Edges (above). Pando's ServiceAccount has a Role here, not cluster-wide rights. |
| Traefik's CRDs | `IngressRoute`, `Middleware` and the rest of `traefik.io`, installed with the manifests. Cluster-scoped, so applied once by whoever applies the manifests; Pando holds no rights over CRDs. |

**RBAC, minimal [P].** One ClusterRole bound to Pando's ServiceAccount:

| Resource | Verbs | Why |
|---|---|---|
| `namespaces` | get, list, watch, create, delete | One per app; delete guarded by the admission policy |
| `rolebindings` (any namespace) | create, delete | Binds `pando-app-manager` in each new app namespace |
| `clusterroles` | `bind`, resourceNames `pando-app-manager` | Lets Pando bind that role without holding its rights cluster-wide |
| `nodes` | get, list, watch | Capacity |
| `persistentvolumes` | get, patch, delete | Retain policy; `DestroyVolume` |
| `runtimeclasses` | get | Isolation check |
| `pods.metrics.k8s.io` | get, list | Usage, when installed |

**Added for the edge [P].** A Role in `pando-edge`, bound to Pando's ServiceAccount:

| Resource | Verbs | Why |
|---|---|---|
| `deployments` (apps) | get, list, watch, create, update, patch, delete | The edge's Deployment |
| `services` | get, list, watch, create, update, patch, delete | The edge's `LoadBalancer` or `NodePort` Service, and `pando-proxy`; the admission policy limits which |
| `secrets` | get, list, watch, create, update, patch, delete | Certificates; DNS-01 credentials and tunnel tokens |
| `poddisruptionbudgets` (policy) | get, create, update, delete | The edge's disruption budget |
| `networkpolicies` | get, create, update, delete | The edge's egress policy |
| `ingressroutes.traefik.io`, `middlewares.traefik.io` | get, list, watch, create, update, patch, delete | One route per app, and prefix stripping (R-167) |
| `pods`, `pods/log` | get, list, watch | `ObserveEdge`, and the edge's logs for an operator |

A ServiceAccount `pando-edge-traefik`, used only by a Traefik edge, with a Role in `pando-edge` that
reads what Traefik's CRD provider watches: `get`, `list`, `watch` on the `traefik.io` resources,
`services`, `endpointslices` and `secrets` (the certificates), in `pando-edge` only. A `cloudflared` edge runs with no
ServiceAccount token. Neither can read anything in `pando` or in an app namespace.

`pando-app-manager`, bound only inside app namespaces: pods, `pods/log`, `pods/exec`, services,
persistentvolumeclaims, configmaps, secrets, networkpolicies, resourcequotas, limitranges — create, get,
list, watch, update, delete. Nothing in `kube-system`, no `nodes/proxy`, no `pods/exec` outside app
namespaces. With a shared namespace, all of it would be one Role and no cluster-scoped rights except
nodes.

**`/var/lib/pando` [D] (O-39): a ReadWriteMany PVC first.** The replicas note lists what lives there:

| Item | On Kubernetes |
|---|---|
| Secrets key | A Secret (above). Not on the volume. |
| Uploaded sources | Needs shared storage, or Postgres (`bytea` or large objects), or the registry as an OCI artifact |
| Local backups | Use a remote backup destination adapter (R-217). `local` on an RWX volume works but is the disk-failure case R-217 warns about |
| Build cache | Moves into the registry as BuildKit `type=registry` cache, one cache reference per app (R-117) |
| Audit archives | Exported to the backup destination (R-347 already permits it) |
| Traefik's dynamic configuration | `IngressRoute` objects in `pando-edge` (above). Not on the volume |

An RWX volume needs a storage class that offers it (NFS, EFS, Filestore, CephFS). Moving uploads and the
build cache out removes the last reasons for it; that is the next step after this PR, and it also
lets Pando's replicas run on more than one host under multi-host Docker (O-47).

**Builder in the cluster.** Rootless BuildKit (`moby/buildkit:rootless`) as a Deployment, which needs
`seccompProfile: Unconfined` and `appArmorProfile: Unconfined` to create its own user namespaces. Pod
Security `baseline` refuses both, so BuildKit runs alone in `pando-build`, a namespace enforcing
`privileged` **[D] (O-48)**; app namespaces and `pando` stay `baseline`, and the pod
is not privileged. No container runtime socket is mounted anywhere (R-112); the integration test that
reads the build container's mounts gets a Kubernetes version reading the pod spec. Builds push to the
registry (PR 5). Its NetworkPolicy allows egress to the internet and the registry, and denies the pod
and Service CIDRs, which is R-113's "no route to the internal network or other bundles". More replicas
of BuildKit share the registry cache, so a build need not land on the same replica as the last one.

## What is not supported

- Pando outside the cluster it deploys to.
- More than one cluster per Pando (see O-40).
- The in-place upgrade (R-359).
- Write observation in the trial run.

## Requirements this touches

- **R-010** is amended in the same change as this note's decisions (O-44): the cluster moving a
  workload after a node failure is not Pando scheduling. Nothing else here needs a change.
- **R-174 holds as written (O-42).** Pando runs and configures the edge, Traefik included.
- **R-023, R-025, R-026** hold by NetworkPolicy, the absence of any Service type that leaves the
  cluster, and the admission policy. They depend on the CNI (O-43).
- **R-151** holds by bare pods with `restartPolicy: Never`.
- **R-222** is met by the cluster's node-wide log rotation rather than a per-app cap, which the
  capability says.

## Tests this PR is done with

Against a kind cluster with Calico in CI (`testcontainers-go` can start one; the four sequences of design
07 run on it):

- `TestR023_AnAppPodIsReachableOnlyFromPandosServerPods` — a pod in another app namespace, and a pod in
  `pando` without Pando's labels, are both refused.
- `TestR026_AnAppNamespaceRefusesANodePortService` — the admission policy.
- `TestR151_AFailedAppsPodIsNotRestartedByKubernetes` — a crashing workload reaches `failed`, and its
  pod stays terminated for the next ten minutes.
- `TestR204_AVolumeIsRetainedAndTheDeleteKeepsAFinalBackup` — PV `Retain` while the app runs; the delete keeps a final backup (R-204) and then removes the claim, as every delete of an app with storage settles it.
- `TestR187_ARestrictedAppReachesOnlyItsGateway`.
- `TestR112_TheBuildPodMountsNoRuntimeSocket`.
- `TestR174_TurningOnTraefikRunsItInTheCluster` — the edge's Deployment and `LoadBalancer` Service are
  created, and an app's `IngressRoute` serves the app through the proxy.
- `TestR174_TheEdgeSurvivesLosingAReplica` — with one edge pod deleted, apps keep answering through the
  other, and both replicas serve the same certificate. It holds whether or not the replicas share a
  node, so it runs on a one-node kind cluster.
- `TestR174_EdgeReplicasSharingANodeIsAWarning` — on one node, both replicas run and the edge is
  reported healthy with the shared-node warning.
- `TestR023_TheEdgeCannotReachAnAppPod` — a connection from the edge pod to an app pod is refused, and
  an `IngressRoute` naming an app's Service is refused by the admission policy.
- `TestR026_NoLoadBalancerServiceOutsidePandoEdge` — the admission policy.
- The canary refusing a cluster without an enforcing CNI (kind with `disableDefaultCNI` and flannel; kindnet
  enforces NetworkPolicy since kind v0.24).

## Decisions

All six were decided by the owner **[D]**: five as recommended, and O-42 against the recommendation. The table keeps the options that were weighed.


| ID | Question | Options | Decided |
|---|---|---|---|
| **O-39** | What replaces the shared `/var/lib/pando` in a cluster? | (a) A ReadWriteMany PVC, required. (b) Move uploads to Postgres or the registry and the build cache to the registry, so no shared volume is needed. | **[D]** (a) for the first release, (b) next. (b) also removes the prerequisite for multi-host Docker with replicas on several hosts (PR 7, O-47). |
| **O-40** | Namespace per app, given 20,000 apps exceeds the tested namespace count? | (a) Namespace per app; prove 20,000 with the PR 2 load harness and document the result. (b) Shared namespaces holding many apps each, with hostAliases for workload names. (c) Several clusters per install, which needs the proxy to reach pods in a cluster it does not run in (PR 7's host agent would do it). | **[D]** (a), proven by the load harness. (c) is the fallback if the harness finds a limit. (b) loses per-app isolation boundaries for a scale problem that may not exist. |
| **O-41** | How does a pod pull an app's private image? Kubernetes has no per-pull credential. | (a) An `imagePullSecret` in the app's namespace, rewritten at each `Apply` (ECR passwords last twelve hours) and deleted at `Destroy`. (b) Pando copies the image into the install registry at deploy and pods pull only from there. | **[D]** (a). It stores a credential in etcd that design 03 §2.1 says the Docker runtime never stores; no app can read it (no ServiceAccount token), and it is the same for the install registry's own pull credential (O-36). (b) is cleaner and costs registry storage for every image app. |
| **O-42** | How does R-174's edge work on Kubernetes? | (a) `SupportsEdge` for plans with no `SharedWithPando` mount — `cloudflared` runs as a Deployment; Traefik runs `managed: false` against Pando's Service. (b) Run Traefik too, sharing route files through a ConfigMap. (c) A Gateway API routing adapter that writes routes to Pando's Service. | Recommended (a), which read R-174 narrowly for Traefik. **Not approved. [D] R-174 holds on Kubernetes:** Pando runs and configures the edge itself, Traefik included, with routes written as `IngressRoute` objects (the edge section above). (c) remains possible later as another routing adapter. |
| **O-43** | Does the adapter require a NetworkPolicy-enforcing CNI, and a cluster dedicated to Pando? | (a) Require an enforcing CNI (canary-verified), recommend a dedicated cluster, and document the host-network gap. (b) Require both. | **[D]** (a). An enforcing CNI is checkable; dedication is not, so it is documented rather than enforced. |
| **O-44** | Kubernetes recreates an app on another node after node failure, by way of R-148's "workload missing, recreate it". R-010 says "no rescheduling on node failure". | (a) Accept: amend R-010 to say Pando itself does not reschedule, and an adapter that spans machines places a recreated workload where it places any workload. (b) Forbid: pin each pod to the node it first ran on, so a dead node leaves the app down until a person acts. | **[D]** (a). R-010 is amended in this change. It follows R-256 ("Pando delegates to something that schedules"), and (b) would require the adapter to fight the scheduler. |

The bare-pod design for R-151 is a [P] rather than an open question, but it changes how operators drain
nodes, so it is worth confirming alongside these.

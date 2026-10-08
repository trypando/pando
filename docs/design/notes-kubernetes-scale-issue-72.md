# The Kubernetes runtime at 20,000 apps (issue #72)

O-40 kept a namespace per app on the condition that a load harness show 20,000 app namespaces working
on one cluster, with the result documented (`notes-kubernetes-runtime-issue-72.md`, "Namespace per
app"). Kubernetes' published scalability thresholds stop at about 10,000 namespaces and 10,000
Services per cluster, and the Kubernetes runtime makes one namespace per app and one headless Service
per workload. This note is that measurement: what one app is in the cluster, what the control plane
did at 2,500, 5,000, 10,000 and 20,000 apps, where it degraded and why, and the options. Nothing in
it is implemented; the recommendation at the end needs the owner's decision.

`make test-kwok-scale` reproduces it (`test/kwok`).

## What one app is in the cluster

What `Apply` and the Traefik routing adapter write for one app, read from
`internal/adapter/runtime/kubernetes/apply.go` and `internal/adapter/routing/traefik/kubernetes.go`:

| Object | Where | How many | Written by |
|---|---|---|---|
| Namespace `pando-app-<id>`, labeled `managed-by`, `pando.dev/app`, `pando.dev/bundle` and Pod Security `enforce: baseline` | cluster | 1 | Pando |
| RoleBinding `pando-app-manager` to Pando's ServiceAccount | app namespace | 1 | Pando |
| NetworkPolicy `pando-default` (ingress from the namespace and Pando's server pods; egress to the namespace, DNS, and outside the cluster) | app namespace | 1 | Pando |
| ResourceQuota `pando-quota` (no NodePort or LoadBalancer Service; CPU and memory at twice the plan when every workload has limits) | app namespace | 1 | Pando |
| Secret `pando-pull`, the registry credential | app namespace | 1 when any workload pulls with one, which every built image does | Pando |
| Headless Service named for the workload | app namespace | 1 per workload | Pando |
| Secret `pando-env-<workload>`, the workload's environment | app namespace | 1 per workload | Pando |
| Pod `<workload>-<random>`, bare, `restartPolicy: Never` | app namespace | 1 per workload | Pando |
| ConfigMap `pando-files-<workload>` | app namespace | 1 per workload that carries files | Pando |
| PersistentVolumeClaim (and the PersistentVolume the provisioner makes) | app namespace | 1 per volume | Pando |
| Egress gateway pod, its Service and a second NetworkPolicy | app namespace | 1 each when egress is restricted | Pando |
| IngressRoute `pando-<app id>` | `pando-edge` | 1 | Pando (routing adapter) |
| TLS Secret `pando-tls-<hostname>` | `pando-edge` | 1 per custom hostname with certificates `http` | Pando (certificate issuer) |
| ServiceAccount `default` | app namespace | 1 | controller manager |
| ConfigMap `kube-root-ca.crt` | app namespace | 1 | controller manager |
| Endpoints and EndpointSlice | app namespace | 1 each per Service | controller manager |
| Events (`Scheduled` and others), kept for an hour | app namespace | about 1 per pod start | scheduler, kubelet |

So a typical one-workload app built by Pando is **9 objects Pando writes and 4 the cluster adds**, and a
two-workload app (web and database) 12 and 6. The harness's mix (seven in ten apps with one workload,
three in ten with two, eight in ten with a pull Secret) averages 1.3 workloads and about 15 objects an app.

What each operation costs in API calls, as recorded by the harness:

| Operation | Calls | Which |
|---|---:|---|
| First deploy of an app | about 25 for one workload, 34 for two | a get and a create each for the namespace, policy, quota and pull Secret, and a create for the role binding; three deletes that find nothing (the egress gateway's pod, Service and policy, removed whenever egress is unrestricted); per workload a get and create of the Service and environment Secret, a ConfigMap delete that finds nothing, a pod list, the pod create, and gets until the pod is placed; a claim list; get and create of the IngressRoute |
| Observing one app | 5 | get the namespace; list its pods, Services and claims; get its IngressRoute |
| The planner's capacity read, twice per plan (`Capacity`, `LargestFitFor`) | 3 each | list nodes, list Pando's namespaces, list **every pod in the cluster** |

## How it was measured

`test/kwok` drives the real adapters, not copies of them: the Kubernetes runtime's `Apply` and
`Observe` and the Traefik adapter's `Ensure` and `Observe`, through a client whose transport records the
time of every request. Two hooks, compiled only under the `kwokscale` build tag, let the harness hand
the adapters that client and record the NetworkPolicy canary as passed (a kwok pod has no network to
probe, O-43).

- **Cluster.** kwokctl 0.7.0: etcd 3.5.21, Kubernetes 1.33.0 API server, controller manager and
  scheduler, each a container on Docker Desktop (12 CPUs and 15.6 GiB shared by all of them and the
  harness), with the components' client rate limits lifted. 300 fake nodes of 32 CPUs, 256 GiB and 110
  pods, Kubernetes' default pod limit; pods are placed by the real scheduler and reported Running by
  kwok.
- **Load.** Apps are added in steps to each size, 64 deploys at once, each the bundle and then its
  route. At each size the harness waits for every pod to be Running, then times one observe pass over
  every app at 8 at once (the reconciler's `Concurrency`), one capacity read, and one more deploy on
  its own, and reads etcd's database size and the API server's memory from the API server's metrics.
- **The client's limiter.** The harness's client has client-go's rate limiter lifted, so the times are
  the API server's. Pando's own client does not (below), and the harness measures that separately.

## Results

Run on 7 October 2026. Each row is the cluster after filling to that many apps; the next step adds to
it. "Calls" are API requests; latencies are client-side, request to response.

| Apps | Fill (wall, 64 at once) | Deploy p50 / p95 / p99 | Fill calls; p50 / p95 / p99 | Observe pass (8 at once) | Observe calls; p50 / p95 / p99 | Capacity read | One more deploy (calls, API time) | etcd database | API server memory |
|---:|---:|---|---|---:|---|---:|---|---:|---:|
| 2,500 | 1m41s | 2.2 s / 4.5 s / 5.6 s | 66,514; 38 / 179 / 1,136 ms | 24.2 s | 12,500; 2.8 / 87 / 105 ms | 0.71 s | 6.4 s (34, 2.4 s) | 118 MiB | 975 MiB |
| 5,000 | 2m01s | 2.4 s / 9.0 s / 14 s | 68,022; 36 / 276 / 1,251 ms | 20.3 s | 25,000; 2.5 / 20 / 85 ms | 0.24 s | 4.1 s (34, 0.13 s) | 217 MiB | 1,333 MiB |
| 10,000 | 3m20s | 2.2 s / 6.2 s / 12 s | 135,829; 6.7 / 134 / 542 ms | 7.2 s | 50,000; 0.9 / 1.4 / 3.7 ms | 0.28 s | 4.1 s (34, 0.06 s) | 369 MiB | 2,239 MiB |
| 20,000 | 6m50s | 2.2 s / 4.4 s / 5.4 s | 275,928; 2.5 / 73 / 150 ms | 2m13s | 100,000; 4.9 / 37 / 87 ms | 2.5 s | 4.2 s (34, 0.16 s) | 703 MiB | 3,976 MiB |

No deploy and no call failed at any size, and every pod was Running when each step finished. The
run did not stop early: 20,000 apps was reached.

Objects in the cluster at each size:

| Apps | Namespaces | Pods | Services | Endpoints | EndpointSlices | Secrets | ConfigMaps | ServiceAccounts | NetworkPolicies | ResourceQuotas | RoleBindings | IngressRoutes | Events |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 2,500 | 2,507 | 3,252 | 3,253 | 3,253 | 3,253 | 5,252 | 2,509 | 2,507 | 2,501 | 2,501 | 2,508 | 2,501 | 3,554 |
| 5,000 | 5,007 | 6,502 | 6,503 | 6,503 | 6,503 | 10,502 | 5,009 | 5,007 | 5,001 | 5,001 | 5,008 | 5,001 | 6,804 |
| 10,000 | 10,007 | 13,002 | 13,003 | 13,003 | 13,003 | 21,002 | 10,009 | 10,007 | 10,001 | 10,001 | 10,008 | 10,001 | 13,304 |
| 20,000 | 20,007 | 26,002 | 26,003 | 26,003 | 26,003 | 42,002 | 20,009 | 20,007 | 20,001 | 20,001 | 20,008 | 20,001 | 26,304 |

About 300,000 objects at 20,000 apps, twice upstream's tested namespace and Service counts. The
other components at 20,000 apps: controller manager 1.8 GiB, scheduler 0.66 GiB, etcd 0.59 GiB
resident.

Three further measurements on the 20,000-app cluster (`TestKwokObservePass`, `TestKwokPodCreateCost`):

| Measurement | Result |
|---|---|
| The observe pass, repeated three times | 76 s, 59 s, 49 s (100,000 calls each; p99 83, 49, 10 ms). The first pass's 2m13s above came right after filling, with the controllers still catching up. |
| Creating 500 pods, 64 at once, in fresh namespaces without a ResourceQuota | p50 192 ms, p95 305 ms, p99 397 ms |
| The same in namespaces with the adapter's ResourceQuota | p50 1,478 ms, p95 2,203 ms, p99 2,356 ms |
| Observing 200 apps through a client with client-go's default limits, which is Pando's (below) | 1,000 calls in 2m45s: 6.1 calls a second, whatever the cluster's size |

An earlier run of the same harness, before it retried the case below, had 224 of the first 2,500
deploys refused with `pods "web-…" is forbidden: error looking up service account
pando-app-…/default: serviceaccount "default" not found`. In the run above the harness retried such a
refusal after two seconds, as the reconciler's next pass would, and needed 0, 84, 1 and 0 retries at
the four steps.

## Where it degrades, and why

**The cluster did not stop.** Through 20,000 apps, per-call latency stayed at a few milliseconds at
the median and under 200 ms at p99 while filling, a deploy's own API time stayed under 200 ms, and
nothing failed. Upstream's 10,000-namespace and 10,000-Service figures are the sizes its tests cover,
not limits the control plane hits there; on kwok, namespaces and headless Services are ordinary
objects, and count against memory and etcd like any other.

What grows with the number of apps, and is the real cost at 20,000:

- **API server memory: about 175 KiB an app**, 4 GiB at 20,000. The API server's watch cache holds
  every object, and a production control plane runs this on each of its API server replicas.
- **etcd: about 34 KiB an app**, 703 MiB at 20,000, under etcd's default 2 GiB quota by about three
  times. Churn grows it between compactions: every redeploy writes a new pod and deletes the old one,
  and every pod start writes events kept for an hour.
- **Observing every app: five calls an app, about a minute for 20,000** with the limiter lifted —
  four times the reconciler's 15-second `Interval`. This is per-app polling, not the cluster: the time
  is linear in apps by construction. O-52 replaces it with watches and a slow sweep.
- **The capacity read: 2.5 s at 20,000 apps**, against a quarter of a second at 5,000 and 10,000. It
  lists every pod in the cluster (26,000 here, with full specs) to sum requests per node, and the
  planner calls it twice per plan (`Capacity`, then `LargestFitFor`), so every deploy at 20,000 apps
  spends about 5 s and a transient 26,000 decoded pods in Pando's memory before it starts.

And three things found on the way, none of them a limit of the cluster:

1. **Pando's own client is the first limit, by a wide margin.** `Configure` builds its client with
   neither QPS nor Burst set, which is client-go's default of 5 requests a second with bursts of 10;
   the Traefik adapter's client is the same. Measured: 6.1 calls a second, so about 1.2 app observes a
   second. **A 15-second reconcile pass can observe about 18 apps**; a pass over 2,500 apps takes 34
   minutes, and over 20,000 about 4.5 hours, and every deploy's 25 to 34 calls wait in the same queue. This
   binds at tens of apps, long before anything measured above. It needs fixing whichever option is
   chosen: QPS and Burst set from configuration, and, with O-52, Observe and the capacity read served
   from informers (a watch-fed cache of the pods, Services and namespaces Pando labels), which also
   turns the observe pass and the capacity read into no API calls at all.
2. **A new namespace's first pod can be refused.** The ServiceAccount admission plugin refuses a pod
   until the controller manager has created the namespace's `default` ServiceAccount, even with token
   automounting off. `Apply` creates the namespace and reaches the pod within milliseconds, so under
   load (here, 64 deploys at once) the first deploy of a new app fails with an `ADAPTER_FAILED` error
   that the next pass would not hit. Proposed fix: `ensureNamespace` waits, polling as `waitScheduled`
   does and for a few seconds at most, until `default` exists. Not made here.
3. **The ResourceQuota on every app namespace makes each pod create several times slower** (7.7 times
   at the median for 500 creates at once; in the fill, pod creates were 18 ms at the median against
   2–3 ms for every other create). The quota admission plugin checks the namespace's quota on every
   pod create and writes the quota's usage back, a second etcd write per pod. It is a cost per deploy,
   not a limit, and the quota is what refuses a NodePort Service if the adapter ever makes one (R-026),
   so it stays.


## What kwok does not measure

kwok replaces the kubelet and nothing else, so everything above the API is real and everything below
it is absent:

- **No kubelet, container runtime or image pulls.** A pod is Running the moment kwok says so. A real
  node starts containers at a rate of its own, pulls images, runs readiness probes and reports status
  through the API (the status writes kwok makes stand in for these, at a similar volume).
- **No network plugin.** Nothing programs 20,000 NetworkPolicies into a dataplane. With Calico or
  Cilium every namespace's policy is per-node state, and policy count is a known cost for both: a
  namespace selector in every policy (Pando's names `kube-system` and `pando`) is evaluated on every
  node. This is the part most likely to fail first on a real cluster and the part this run says least
  about.
- **No kube-proxy and no cluster DNS.** Headless Services install no kube-proxy rules, so that is
  likely fine; CoreDNS watching 26,000 Services and EndpointSlices is not measured.
- **Not production hardware.** etcd ran on a laptop's SSD, inside a VM shared with the API server and
  the harness. Upstream's thresholds assume dedicated control-plane machines; absolute latencies here
  are pessimistic, and the shape of the curves is the useful part.
- **One API server.** A production control plane has three or more behind a load balancer, which
  spreads reads but not etcd's writes.
- **No other tenants.** The cluster held only Pando's apps. A shared cluster adds its own objects to
  every count here.
- **Not measured: deleting many apps at once.** Removing a namespace makes the namespace controller
  list and delete every kind in it; thousands at once is a known slow path and was not timed.

## Options

The question O-40 left open was whether namespace per app holds at 20,000. On the control plane it
does, so these are weighed against what the run found: memory and etcd that grow with objects,
polling that grows with apps, and the dataplane kwok cannot see.

### (a) Fewer objects per app

Where objects could go, and what each saves at the harness's mix (1.3 workloads an app, about 15
objects):

- **One headless Service per app instead of one per workload.** Each Service also brings an Endpoints
  and an EndpointSlice, so a second workload costs three objects. Pods would set `hostname` to the
  workload and `subdomain` to the app's Service, and add `<service>.<namespace>.svc.<domain>` to
  `dnsConfig.searches`, so `db` still resolves to the database pod with nothing in the app's
  environment changed (R-028). Saves about 0.9 objects an app (6%); the proxy's name for a workload
  becomes `<workload>.<service>.<namespace>.svc`, so `Upstream` changes shape.
- **One environment Secret per app instead of per workload**, with each workload's values under
  prefixed keys and `envFrom` replaced by per-key references. Saves 0.3 an app (2%).
- **Calls rather than objects:** the three deletes of an egress gateway that is not there and the
  ConfigMap delete for a workload with no files are a sixth of a typical deploy's calls, and a get
  precedes every create. One list of the namespace's managed objects at the start of `Apply` would
  replace most of them.

Tradeoff: little risk, little gain. Objects were not where the cluster strained; at most this is a
tenth less memory and etcd. The call trimming is worth doing on its own while the client is limited.

### (b) One namespace per team, with per-app NetworkPolicy isolation

Namespaces would drop from 20,000 to the number of teams. Nothing else would: Services, Secrets and
pods are per workload, and policies would become per app.

- **Pando has no team.** Groups (R-078) grant access; they do not own apps, and an app may be shared
  between several. Choosing an app's namespace would be a new concept and a new decision per app,
  which is a requirement to write, not a default to pick.
- **Isolation becomes label selectors** (R-025, R-180): one policy per app selecting its pods by
  `pando.dev/app`, against a default-deny for the namespace. A pod created without the label, or with
  another app's, is open to that app. This is the failure O-40 chose namespaces to avoid.
- **Names collide.** Two apps' `db` in one namespace cannot both be a Service called `db`, so
  workload names would need per-app Service names and the `dnsConfig.searches` arrangement above.
- **Pod Security, the quota and the role binding become per team**, so one app's quota no longer
  bounds that app, and deleting an app is a list of deletes rather than one.

Tradeoff: a large change to the isolation model for a limit this run did not find on the control
plane, and no evidence that it helps the dataplane: Calico's and Cilium's cost grows with policies
and selectors, which (b) keeps per app while moving them into fewer namespaces.

### (c) Several clusters, each app assigned to one when it is created

One runtime spanning several clusters, as the Docker hosts runtime spans hosts
(`notes-multi-host-docker-issue-72.md`): an app is assigned to a cluster at its first deploy, by a
rule declared in configuration (the cluster with the most room, or one named by a label the
administrator sets), and stays there. Placement at creation is placing, which R-010 allows; the app
never moves (O-46), and there is no rebalancing and no reaction to load, so it does not become
scheduling. The assignment lives inside the adapter, as the Docker hosts runtime's does, so core
models no clusters (R-256).

- **What it costs.** The proxy must reach pods in a cluster it does not run in (R-023): a forwarding
  agent per cluster accepting only Pando's client certificate, as O-45 gives each Docker host. Each
  cluster needs a route from the edge, the registry reachable, its own canary (O-43) and its own
  capacity. An app's namespace and volumes live in one cluster, which is why the app cannot move.
- **What it buys.** No ceiling from any one cluster: each holds as many apps as its real dataplane
  takes, and a cluster's failure takes only its apps. It is also the only option that addresses the
  part kwok cannot measure.

Tradeoff: the most work of the three, and only needed once a real cluster shows a limit.

## Recommendation (needs the owner's decision)

**Keep namespace per app (O-40 (a)) and do not adopt (b).** The control plane held 20,000 apps —
20,007 namespaces, 26,003 Services, about 300,000 objects — with no failures and flat per-call
latency. (b) gives up the isolation boundary for a limit the run did not find.

**Hold (c) as the scale-out path, unbuilt**, as O-40 already says, until a test on a real cluster
with the network plugin the cluster tier names shows a limit below 20,000. kwok says nothing about
20,000 NetworkPolicies in Calico or Cilium, CoreDNS watching 26,000 Services, or real etcd under
churn, and that is where a limit would come from.

**Fix what binds first, whichever option is chosen:** the clients' rate limit and per-app polling
(finding 1, with O-52's watches), the capacity read that lists every pod twice per plan, the
ServiceAccount race on a new namespace (finding 2), and the no-op calls in (a). Without the first,
the runtime cannot keep up with a few dozen apps, whatever the cluster can hold.

Proposed text for `docs/plan/open-decisions.md`, for the owner:

> **O-??** Whether the Kubernetes runtime changes its per-app shape to reach 20,000 apps on one
> cluster (O-40's load harness). Measured on kwok (`notes-kubernetes-scale-issue-72.md`): 20,000
> apps — 20,007 namespaces and 26,003 Services — held with no failures; API server 4 GiB, etcd
> 703 MiB. **Proposed:** keep namespace per app and a Service per workload; no namespace per team.
> Several clusters per runtime, each app assigned to one at creation and never moved (R-010, O-46),
> stays the fallback, built only if a real cluster with the cluster tier's network plugin shows a
> limit below 20,000. Before then: set the Kubernetes clients' QPS and Burst (client-go's default of
> 5 a second limits a replica to about 18 app observes per 15-second pass), serve Observe and the
> capacity read from watches (O-52), wait for a new namespace's default ServiceAccount before its
> first pod, and drop `Apply`'s no-op calls.

## Reproducing it

```
go install sigs.k8s.io/kwok/cmd/kwokctl@v0.7.0
DOCKER_CONTEXT=desktop-linux make test-kwok-scale APPS=2500,5000,10000,20000
```

About 20 minutes for these four sizes on Docker with 12 CPUs and 16 GiB. `KWOK_OUT` names the
results file. `KWOK_KEEP=1` keeps the cluster; then
`go test -tags kwokscale -run 'TestKwokObservePass|TestKwokPodCreateCost' ./test/kwok/`, with
`PANDO_KWOK_KUBECONFIG` set to the kubeconfig in `KWOK_WORKDIR`, repeats the further measurements.
A step stops the run when more than one deploy in a hundred fails, or when it takes longer than
`PANDO_KWOK_STEP_MINUTES` (60).

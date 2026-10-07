# Docker on several hosts (issue #72, PR 7)

The seventh PR of the stack in `notes-multiple-replicas-issue-72.md`. The issue's author decided
**[D]** that several machines without Kubernetes are supported, by a runtime adapter that drives Docker
on several hosts, with placement inside the adapter (R-256). This note designs it. It depends on PR 5
(`notes-image-registry-issue-72.md`) for getting built images to the hosts, and shares two decisions
with PR 6 (`notes-kubernetes-runtime-issue-72.md`): `Capacity.LargestFit` and the shared-storage
question (O-39).

The hard part is R-023: on one host, Pando reaches an app by joining its container to the app's bridge
network (`attachProxy`). A bridge network exists on one host. A Pando on host A cannot join a network
on host B.

## Shape

```
                        load balancer / edge
                               │
            ┌──── control host ─────────────────────┐
            │ pando (replicas), postgres, registry, │
            │ buildkit, the edge                    │
            └──────┬────────────────┬───────────────┘
      Docker API   │ (TLS or SSH)   │  mTLS to the host agent (port 7443)
            ┌──────┴─────┐   ┌──────┴─────┐
            │ app host 1 │   │ app host 2 │   ...
            │ pando-agent│   │ pando-agent│   joined to every app network on its host
            │ app nets   │   │ app nets   │   nothing published but the agent's port
            └────────────┘   └────────────┘
```

- **One adapter configuration lists the hosts.** Each entry: a name, a Docker endpoint, its
  credential, and whether new apps may be placed there. The control host may also be an app host, and
  on a small install is the only one at first.
- **The adapter talks to each host's Docker API** over TLS (`tcp://host:2376`, client certificate) or
  SSH (`ssh://pando@host`, which the Docker client supports through its SSH connection helper). Either
  credential is root on that host, and is stored as an adapter credential, encrypted (R-190).
- **Every per-host operation is the existing Docker adapter's code**, run against that host's client:
  networks, subnets, egress gateways, volumes, helper containers for backups, `Logs`, `Exec`,
  `Observe`. The multi-host adapter is a router in front of N single-host adapters, plus placement and
  the agent below. Subnet pools are per host, because bridge networks are host-local; each host has its
  own `10.213.0.0/16`.

## Reaching an app on another host (R-023, R-026)

Three options were evaluated.

### (a) A Pando proxy replica on every app host

Each app host runs a full Pando replica joined to its local app networks, as on one host today. The
load balancer, or the routing adapter, sends each app's traffic to a replica on that app's host.

- The proxy and the app are on the same host, so there is no extra hop.
- **Routing adapters become placement-aware.** `RouteRequest.ProxyUpstream` would have to name a
  different replica per app and change when an app's host changes. Path routing on one hostname
  (R-164's proxy mode, the recommended enterprise topology) puts every app behind one hostname, so the
  balancer has to route by path to hosts — a second routing table outside Pando that must agree with
  Pando's. Port mode needs the same per port. A Cloudflare tunnel would need a `cloudflared` per host.
- **A request that lands on the wrong replica cannot be served** — during a move, after a stale route,
  or for the console on an app's hostname — unless replicas can reach each other's apps, which is the
  original problem.
- Every host is a full control-plane member: a database pool per host, a leader candidate per host, and
  the shared `/var/lib/pando` on every host (O-39).

### (b) An overlay network with Pando attached

Docker Swarm's attachable overlay networks (or a WireGuard mesh with routes to each host's bridges):
one overlay per app, spanning hosts, with Pando's containers attached to all of them. This keeps
today's structural argument word for word.

- It needs Swarm mode on every host — a Raft quorum of managers and three more ports between hosts — to
  use only its networking. That is a second distributed system to operate for one feature, and Swarm is
  a scheduler, which the install would then be running beside Pando.
- **Pando's container would join every app's network.** At one host that is up to about 4,000 after PR 3.
  At the cluster tier it is 20,000 overlay networks on one container, each with a VXLAN device in the
  network namespace of every host where a member lives. Docker is not built for that, and nothing
  published says it works.
- A WireGuard mesh routing to every bridge subnet makes every app subnet reachable from every host,
  which turns R-025 from a topology property into a firewall rule set Pando would have to maintain on
  every host.

### (c) A forwarding agent on each app host [D]

Each app host runs one Pando-owned container, `pando-agent` (Pando's binary, `pando host-agent serve`,
as the egress gateway is). The adapter joins it to every app network on its host — the same
`attachProxy` call, naming the agent instead of Pando's own container (`ProxyContainer` is already a
setting). The agent publishes one port, and accepts only mutual TLS with a client certificate that only
Pando's replicas hold. A replica's proxy, having decided a request (design 06 §4, steps 1–10), opens a
connection to the agent on the app's host, names the target container and port in a one-line header,
and from then on the connection is a TCP stream to the workload. HTTP, websockets and server-sent events
pass through it unchanged.

Reasons:

- **Routing adapters do not change.** They still point at Pando's proxy (design 00 §1.3), any replica
  can serve any app, and the load balancer needs no knowledge of hosts. Proxy mode, path routing, port
  mode and Cloudflare work as they do now.
- **The agent decides nothing.** Authentication, `CheckData`, assertion minting and header and cookie
  stripping stay in the proxy, in core (R-027). The agent is a transport that will carry a connection
  only for a holder of Pando's client certificate, and only to a container on a network it was joined
  to.
- **It scales with hosts.** Each agent is on its own host's app networks only: 20,000 apps on twenty
  hosts is about 1,000 networks per agent, inside what one Docker host does today.
- **It is the `Upstream` extension design 03 §2 anticipated** ("a remote host … will need to hand the
  proxy a way to dial as well, and gets a field here then").

Costs:

- **One more hop** on the host network for every proxied request, and a TLS handshake per new connection
  to an agent. PR 2's upstream connection reuse makes the handshake per connection rather than per
  request; the agent keeps no pool of its own.
- **The client key is the key to every app.** Anyone holding it can open a connection to any workload
  without passing the proxy. It is generated by Pando, kept sealed with the secrets key in Postgres
  (so every replica has it and a database dump alone does not), and rotated by overlap like the
  assertion keys. Its exposure is comparable to the secrets key's, and R-191 is the threat model that
  covers it: it protects against a copied disk or dump, not a compromised Pando.
- **The agent's port is a published host port.** R-026 is about app workloads, and this port reaches no
  app without the certificate; the documentation also says to restrict it to the control host's
  address with the host firewall.

**Approved by the owner (O-45) [D].** Design 06 §4 now says it: on one host, the set of networks Pando's
container belongs to is the set of apps it can reach; on several hosts, the set of networks each host's
agent belongs to is, and the agent forwards only for Pando's proxy. There is still no route to an app
that does not pass enforcement, now with a certificate check as part of the route.

**What it requires of the interface.**

```go
type Upstream struct {
    URL string
    // Dial opens the connection the proxy sends this request over. Nil: dial
    // URL's host directly, as on one host. Supplied by the runtime so the
    // agent's protocol stays the adapter's vocabulary (R-251).
    Dial func(ctx context.Context) (net.Conn, error)
    // PoolKey groups connections the proxy may reuse: the same key, the same
    // destination. Empty: URL's host.
    PoolKey string
}
```

The routing adapters need nothing.

## Placement [P]

Inside the adapter, deliberately simple, and documented in the adapter's settings form:

1. **Sticky.** An app already on a host stays there. The record is on the host: the app's network
   (`io.pando.bundle` label) is created first in `Apply` and is the marker. At start the adapter lists
   labeled networks on every host and builds its map in memory; it keeps it current as it creates and
   destroys. The adapter does not write Pando's state (R-027) and does not need to: the hosts are the record.
2. **New apps go to the host with the most free memory that fits** every workload of the plan, among
   hosts open to placement, by committed limits rather than measured use (R-242's arithmetic, per host,
   from the containers' own limits). Ties go to the host with fewer apps.
3. **A bundle never spans hosts.** Its workloads share a bridge network, and R-153 says one app, one
   place.
4. **No host fits:** `Apply` fails with a `CAPACITY_*` error naming the largest free space, and the
   planner, given `LargestFit`, refuses first in the common case (PR 6 note, Capacity).

Because placement is sticky and volumes are host-local, an app does not move unless a person moves it
(below). That keeps R-010's "no rescheduling on node failure" literally true on this runtime.

**Two apps placed at once on different replicas** both read the same free space. Docker does not
reserve memory, so both are created and the host is overcommitted by one app. The adapter serializes
placement per adapter configuration with an in-process lock, and `Apply` re-checks the chosen host's
committed limits after creating the network; two replicas can still race. **[P]** accept it: the
reconciler's lease (PR 3) and the deploy queue (PR 4) make concurrent first deploys rare, and the
result is an overcommitted host, which R-242's per-install check bounds.

## Volumes and moving an app (R-204, R-206, R-257)

Volumes are Docker volumes on the app's host. Consequences:

- **Snapshots and restores** run the existing helper container on that host (`volumes_backup.go`).
- **R-204** holds as now: `Destroy` with `KeepVolumes` removes containers and networks and leaves the
  volumes. The kept volumes keep the app's host as its home, so a re-created app returns to its data.
  The marker for a deleted app with kept volumes is the volume label rather than the network.
- **An app cannot follow its data to another host**, because the data does not move. Moving an app is:
  stop, snapshot each volume to the backup destination, destroy on the old host with volumes discarded,
  apply on the new host, restore. Every step exists today. **[D] Pando does not offer it as an action
  in the first release (O-46).** A host is emptied by deleting and re-creating its apps, with backups
  (R-205). Offering a move later — one audited action behind a new install-scoped verb, which is a
  change to R-080's table — is a separate decision when someone needs it.

## Host failure

An app host that stops answering is a runtime problem, not an app failure (design 05 §2): `Observe` for
its apps returns an error, the reconciler records `unobservable_since`, and nothing counts toward
`failed`. The console shows those apps as last known, with a notice that their host cannot be reached.

The adapter does not move them (R-010). Recovery is a person's:

- **The host comes back:** the reconciler observes again and restarts anything stopped (R-148).
- **The host is gone:** the operator removes it from the adapter's configuration. Its apps are then
  observed as absent, with volumes that previously existed missing — which design 05 §2.1 classes as not
  reconcilable, so the reconciler reports and does not recreate empty volumes (R-203). The app's owner or
  an administrator restores from the last rolling backup (R-206, in place: the same app, re-created from
  its spec on whatever host placement chooses). What changed since that backup is lost; R-013 says this
  is not a DR product, and the console should say so in the restore confirmation.

Apps with no volumes have nothing to lose and are recreated on another host by the reconciler once the
dead host is removed from the configuration. Removing a host is the human act; the adapter does not do it
on a timeout.

## Capacity (R-243)

- **`Capacity`** sums the hosts open to placement — `NCPU`, `MemTotal` and the data root's size from each
  daemon's `info` — with each host's figures in `Details`.
- **`LargestFit`** is the largest free CPU and memory on any one host, by committed limits.
- **`InUse` and `Usage`** ask each host, as the single-host adapter asks its daemon. `InUse` runs the
  hosts in parallel, because each takes about a second.
- **R-224** applies per host, and a host's disk holds its apps' logs, volumes and images. The per-app log
  caps already apply per container. Images are pulled by digest from the registry and pruned per host
  by the existing teardown.

## Images and builds

- **Builds run on the control host**, rootless BuildKit as today, with no runtime socket (R-112), and
  push to the registry (PR 5). Each host pulls by digest, with the registry credential sent per pull
  (`X-Registry-Auth`) and stored on no host.
- **`ImageDelivery` is `[registry]`.** A tarball streamed to whichever host placement chooses would
  work for a first deploy, but the reconciler re-creating a workload later needs the image on that host
  again, and a registry is where it comes from.
- **Trial runs** go to the control host [P]. They are throwaway and need no placement.
- **The egress gateway** runs Pando's image, pulled from the registry Pando's own image comes from,
  on the app's host.

## Pando's own replicas, the edge, and self-upgrade

- **[P] Pando's replicas, the edge, BuildKit and the registry run on the control host.** That is the
  replicas note's supported topology (one host, a shared volume), with the app hosts added. The edge joins
  `pando-edge` with Pando's containers on the control host, exactly as now, so `SupportsEdge` is true
  for the control host and the edge does not move.
- **[D] Replicas run on the control host only (O-47)** until the shared `/var/lib/pando` is gone —
  O-39's second step, which moves uploads and the build cache out of it. Replicas on several hosts with
  an NFS mount of that directory are not supported. Whether the edge then runs on more than one host is
  decided with that change.
- **The in-place upgrade (R-359)** stays as on one host: allowed with a single replica on the control
  host, refused otherwise. The agents run Pando's image and are re-created at the new version by the
  adapter at start, one host at a time; an agent being replaced drops that host's open connections, and
  clients reconnect.

## The runtime interface, briefly

Every method routes to the app's host and runs the single-host code there, except:

| Method or capability | Difference |
|---|---|
| `Apply` | Places first if the app has no host |
| `Upstream` | Returns the container URL and a `Dial` through that host's agent |
| `Capacity` | Summed, with `LargestFit` |
| `Trial`, `ApplyEdge` and the edge methods | Control host |
| `ImportImage` | Not offered |
| `SupportsSelfUpgrade` | Only with one replica, on the control host |
| `Platform` | The hosts' common platform; mixed architectures are refused at configure in v1 [P] |
| `RejoinNetworks`, `ReclaimNetworks` | Per host, for the agent, by the leader. Pando's own replicas join no app network |

## Requirements this touches

- **R-010, R-153, R-256** hold: one app on one host, placed by the adapter, never moved by Pando on its
  own, and not moved by a person through Pando either in the first release (O-46).
- **R-023** holds by the agent's certificate check and the proxy being the only holder of the
  certificate. Design 06 §4 describes the mechanism (O-45).
- **R-025, R-026, R-180 – R-187** hold per host exactly as on one host.
- **R-013** is what the host-failure section relies on.

## As built (PR 7)

What was built, and where it differs from the design above.

| Part | As built |
|---|---|
| Adapter | `internal/adapter/runtime/multidocker`, kind `docker-hosts`. One `docker.Adapter` per host, made with `docker.NewWithClient` on that host's client, with `ProxyContainer: "pando-agent"` so `attachProxy` and `RejoinNetworks` join the agent where they joined Pando's container. The control host has a second one, joined as Pando's own container, for the edge, trial runs and self-upgrade. The single-host adapter gained only `NewWithClient`, `Committed`, `Bundles`, limit labels on containers (`io.pando.limit.cpu`, `io.pando.limit.memory`) and `LargestFit` in its `Capacity`. |
| Configuration | `hosts`, a JSON list (or a string holding one, for a form's text area): `name`, `endpoint`, `agent_address`, `control`, `no_placement`, `ssh_host_key`, and the per-host totals. Exactly one control host. `network_pool` may not be `off`: the agent's check depends on it. Credentials, sealed in `adapter_credentials` like every adapter's (R-190): `agent_authority`, `docker_tls` (client certificate, key and CA for `tcp://`), `ssh_key` (for `ssh://`, through `golang.org/x/crypto/ssh` to the remote socket, with the host key pinned). |
| Placement | As described. The map is a cache rebuilt from the hosts' bundle-labeled networks and volumes, on a miss (at most every 2 s) and on every rejoin pass (15 s). **While a host does not answer**, an app the map does not know is reported as an observation error rather than absent, and an app that has deployed before is not placed, because either could put a second copy beside one on the silent host. An app's first deploy (`BundlePlan.FirstDeploy`, set by core when the app has no successful deploy) is placed among the hosts that answer: nothing of it can be on the silent one. If a first deploy that failed partway left a network on a host that later goes silent and returns, the bundle is found on both hosts and the first one listed wins. |
| Plan-time capacity | `RuntimeAdapter.LargestFitFor(bundleID)`: for a placed app, its host's free space plus what the app's running containers hold there (`docker.RoomFor`); for a new one, the roomiest open host that answers. The planner refuses a bundle whose workload limits sum to more, at plan time (design 03 §2.4). `Committed` counts running containers only, as R-242 counts running apps. |
| Deleted apps' networks | On an app host, `Destroy` then disconnects the agent from the app's networks and removes them (`docker.DetachProxy`), touching only networks labeled as that bundle's — never the agent's own, which has no bundle label — and refusing outright for Pando's own container. Not on the control host, which may be Docker Desktop, where disconnecting a running container drops its published ports. |
| The agent | `pando host-agent serve`, from `agent_image` (default: the image Pando's container on the control host was started from, by reference so other hosts can pull it). On its own `pando-agent` bridge network, made with Docker's addresses; the adapter refuses one that overlaps the app range. Read-only root, all capabilities dropped, one published port. Protocol: after TLS 1.3, `PANDO-AGENT/1 <container> <port>\n`, answered `OK` or `NO <reason>`, then a byte stream. Re-created when its image, port, range or authority changes, and every 30 days for a new certificate. |
| What the agent forwards to | A name matching Pando's container names (`pando-…`), resolved by Docker's DNS on the agent's networks, dialed by address, and only if the address is inside `network_pool`, on a network the agent is joined to that lies within the pool, and not that network's address, bridge (`.1`, the host), broadcast or the agent's own address (`hostagent.Permitted`). |
| Certificates | Not generated into Postgres by core, as design 06 §4 first said: the authority is the adapter's `agent_authority` credential, made by `pando host-agent new-authority`, which is sealed by the secrets adapter in Postgres all the same. Each replica issues its own client certificate (`CN=pando-proxy`, client authentication only) from it in memory at start, and each agent's server certificate (`pando-agent.<host>.invalid`, server authentication only) when the agent is created; an agent's certificate cannot open another agent. Replaced by overlap: the credential holds several authorities, the first issues and all are trusted. |
| Proxy | `api.Upstream` gained `Dial` and `PoolKey`. When `Dial` is set the proxy sends the request on a second transport, with no environment HTTP proxy, whose connections are pooled under a host derived from `PoolKey` (`<hash>.dial.pando.invalid`) and opened by the registered `Dial`. Steps 1–10 are unchanged and happen before any dial. |
| Capabilities | `ImageDelivery: [registry]`, `SupportsImageImport: false` (`ImportImage` refuses). `api.RuntimeCapabilities.ImageDelivery` was added here with the shape PR 5's note gives, ahead of PR 5; the single-host adapter reports `[import]`. Until PR 5 lands, only apps that run a published image can be deployed on this runtime. Platform is the hosts' common one, and HealthCheck refuses a mix. |
| Capacity | Summed over hosts open to new apps; `LargestFit` the roomiest one; per-host figures and reachability in `Details["hosts"]`. |

**Not done here:**

- **On the control host, a deleted app's network is not reclaimed until the agent is re-created**
  (see "Deleted apps' networks"). `Apply`'s placement refusal remains for two replicas racing for the
  last room on a host.
- **The agent's key is in its container's environment** on its own host. It is a server key and
  opens nothing, and that host's root can reach its apps anyway.
- **The SSH Docker client is not run against a real daemon.** `test/multihost` (below) runs the
  adapter on Docker-in-Docker hosts over TLS; SSH is covered by unit tests only.
- **A redeploy refused because the app's host does not answer leaves the app `deploying`** (O-50).
  The refusal is as designed, and nothing of the app is placed elsewhere; but the deploy runner does
  not move the app out of `deploying` after a failed apply, so the reconciler does not start its
  stopped containers when the host returns. An app nobody tried to redeploy is started again.

## Tests this PR is done with

As written (unit tests, fakes, a real agent on loopback):

- `TestR023_AnAgentRefusesAConnectionWithoutPandosCertificate` — no certificate, another authority's,
  an agent's server certificate, the authority's with another name, plain TCP.
- `TestR023_AnAgentRefusesATargetOutsideItsAppNetworks`, `TestPermittedAddresses`.
- `TestR023_AnAgentForwardsPandosConnectionToTheNamedContainer`.
- `TestR053_ForgedHeadersAreReplacedThroughAHostAgent`,
  `TestR173_PandosOwnCookiesNeverReachAnAppThroughAHostAgent` — the proxy's forged-header and cookie
  tests, run with the upstream behind a real agent.
- `TestR023_AnAppOnAnotherHostIsReachedOnlyAfterTheDecision` — a denied request never dials the
  agent; allowed ones reuse one connection.
- `TestR023_AnAppOnAnotherHostIsReachedThroughItsHostsAgent`.
- `TestR256_ANewAppGoesToTheHostWithTheMostFreeMemoryThatFits`,
  `TestR256_NoHostThatFitsIsARefusalNamingTheLargestFreeSpace`.
- `TestR010_AnAppStaysOnItsHostAcrossRedeploys`.
- `TestR148_AnUnreachableHostMarksItsAppsUnobservableNotFailed`.
- `TestR243_CapacityReportsTheLargestPlaceAWorkloadFits`,
  `TestR243_OneHostsLargestFitIsWhatItsContainersLeave`.
- `TestR242_AnAppNoSinglePlaceHasRoomForIsRefusedAtPlanTime`,
  `TestR242_ARedeployThatFitsWhereTheAppRunsIsAllowed` (planner),
  `TestR242_TheRoomForAnAppIsWhereItRunsWithWhatItHoldsThere`.
- `TestR256_AFirstDeployIsPlacedWhileAnotherHostIsUnreachable`,
  `TestR010_ADeployedAppIsNeverPlacedElsewhereWhileAHostIsUnreachable`.
- `TestR224_TheAgentLeavesADeletedAppsNetworksOnAnAppHost`,
  `TestR224_DetachProxyLeavesOnlyTheDeletedBundlesNetworks`.

Against real daemons, in `test/multihost` (`make test-multihost`, build tag `multihost`): two
Docker-in-Docker app hosts reached over TLS with generated certificates, a third as the control host
running two replicas of Pando, and Postgres, a registry and BuildKit beside them. Each app host pulls
the agent's image and the apps' images from the registry.

- `TestR243_CapacityIsSummedOverTheHostsWithTheLargestFit` — totals summed over the open hosts, and
  an app that fits the sum but no one host refused at plan time.
- `TestR256_ANewAppGoesToTheHostWithTheMostFreeMemory`.
- `TestR023_AnAppOnAnotherHostIsReachedOnlyThroughTheProxy` — Sequence C to an app on host B:
  forged headers replaced, `pando_*` cookies removed, an assertion present, a redirect without a
  session; a connection routed into host B's app range, or to the app's port on host B, fails.
- `TestR026_AnAppHostPublishesOnlyTheAgentsPort`.
- `TestR023_AHostAgentRefusesAClientWithoutPandosCertificate` — real handshakes: no certificate,
  another authority's, plain text; with Pando's, the agent itself and a container on no app network
  are refused, and an app's container is reached.
- `TestR010_AnAppStaysOnItsHostAndAStoppedHostsAppsAreUnobservable` — a redeploy stays; with host B
  stopped its apps are unreachable and not failed, a new app goes to host A, a redeploy is refused and
  nothing is re-created on host A; when host B returns its app is started again.
- `TestR224_DeletingAnAppTearsItDownAndTheAgentLeavesItsNetworks`.
- `TestR120_ABuildIsDeliveredThroughTheRegistryAndPulledByDigest` — an uploaded source built by
  BuildKit, pushed to the registry and run by digest on its host.
- `TestR023_AReplacedAgentIsJoinedToItsHostsAppNetworksAgain`.
- `TestR023_EitherReplicaReachesAnAppOnAnotherHost` — both replicas reach host B, and their rejoin
  passes do not replace each other's agents.

## Decisions

All three were decided by the owner **[D]**; the table keeps the options that were weighed.


| ID | Question | Options | Decided |
|---|---|---|---|
| **O-45** | How does the proxy reach an app on another host? | (a) A proxy replica on every app host, with routing adapters sending each app's traffic there. (b) An overlay network per app with Pando attached. (c) A forwarding agent on each host, reachable only with Pando's client certificate, and `Upstream.Dial`. | **(c) [D].** Design 06 §4 states the mechanism. |
| **O-46** | Is moving an app between hosts an action Pando offers? | (a) No; delete and re-create with backups. (b) Yes, as one audited action (stop, snapshot, recreate, restore) behind an install verb. (c) Yes, for the app's owner. | **(a) [D]** for the first release. (b) is the likely next step: it moves data and causes downtime, so it would be an administrator's, with a new install-scoped verb — a change to R-080's table, not made here. |
| **O-47** | Can Pando's replicas run on more than one host in this topology? | (a) No: control host only. (b) Yes, with `/var/lib/pando` on NFS on each. (c) Yes, after O-39(b) removes the shared volume. | **(a) [D]** now; (c) when O-39(b) lands. |

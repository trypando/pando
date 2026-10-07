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

Each app host runs one Pando-owned container, `pando-agent` (Pando's binary, a hidden `pando host-agent`
command, as the egress gateway is). The adapter joins it to every app network on its host — the same
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

## Tests this PR is done with

Several Docker daemons for tests run as Docker-in-Docker containers (`testcontainers-go`), each standing
in for a host:

- `TestR023_AnAgentRefusesAConnectionWithoutPandosCertificate`.
- `TestR023_AnAppOnAnotherHostIsReachedOnlyThroughTheProxy` — the design 07 Sequence C run through the
  balancer, with the app on a different daemon from every replica, forged headers included.
- `TestR026_AnAppHostPublishesOnlyTheAgentsPort`.
- `TestR010_AnAppStaysOnItsHostAcrossRedeploys`.
- `TestR148_AnUnreachableHostMarksItsAppsUnobservableNotFailed`.
- `TestR243_CapacityReportsTheLargestPlaceAWorkloadFits`.

## Decisions

All three were decided by the owner **[D]**; the table keeps the options that were weighed.


| ID | Question | Options | Decided |
|---|---|---|---|
| **O-45** | How does the proxy reach an app on another host? | (a) A proxy replica on every app host, with routing adapters sending each app's traffic there. (b) An overlay network per app with Pando attached. (c) A forwarding agent on each host, reachable only with Pando's client certificate, and `Upstream.Dial`. | **(c) [D].** Design 06 §4 states the mechanism. |
| **O-46** | Is moving an app between hosts an action Pando offers? | (a) No; delete and re-create with backups. (b) Yes, as one audited action (stop, snapshot, recreate, restore) behind an install verb. (c) Yes, for the app's owner. | **(a) [D]** for the first release. (b) is the likely next step: it moves data and causes downtime, so it would be an administrator's, with a new install-scoped verb — a change to R-080's table, not made here. |
| **O-47** | Can Pando's replicas run on more than one host in this topology? | (a) No: control host only. (b) Yes, with `/var/lib/pando` on NFS on each. (c) Yes, after O-39(b) removes the shared volume. | **(a) [D]** now; (c) when O-39(b) lands. |

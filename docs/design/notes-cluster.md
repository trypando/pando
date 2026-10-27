# A Pando cluster (#347)

Issue #347 asks for an install that spans several hosts: start a new Pando, give it a join token minted
by an install, and it becomes part of that install. This note is the design. It builds on what issue
#72 already shipped — several replicas of Pando's process against one database
(`notes-multiple-replicas-issue-72.md`) and the `docker-hosts` runtime with its forwarding agent
(`notes-multi-host-docker-issue-72.md`) — and removes the limits both left: replicas on one host only
(O-47), a shared `/var/lib/pando` (O-39), hosts typed into an adapter's settings with a root credential
for each, and apps that go down with their host.

Decisions marked **[D]** were made by the owner while designing; **[P]** are defaults.

## What a cluster is

| Term | Meaning |
|---|---|
| **Cluster** | One install (R-015) running on more than one host. Every install is a cluster of one until a host joins. |
| **Host** | A machine running Pando's binary that has joined the cluster. Modeled in core: `cluster_hosts`, IDs `host_…` **[D]**. |
| **Control role** | Serves the API, console and proxy, runs the edge, competes for leader, builds. Is a replica in the #72 sense. |
| **App role** | Runs app workloads through its agent. |
| **Standby** | A stopped copy of an app on a second app host, kept current, started when the app's host is lost. |

A host holds the control role, the app role, or both **[D]**. The cluster and its hosts are core's
(R-256 amended): membership, roles, liveness and joining are install-wide facts the planner, the console
and the audit log all need, not one adapter's settings. Placement is still the runtime adapter's: core
hands the `docker-hosts` runtime the app hosts as configuration; it does not choose a host for a workload.

**Role defaults [P].** The first three hosts take both roles. A later token defaults to the app role
only, since three or five control hosts are enough and each costs a database pool and a leader
candidate. The token's minter can choose either.

## Joining

**At setup only [D].** A fresh Pando's setup screen offers *Create administrator* or *Join a cluster*;
`PANDO_JOIN_TOKEN` does the same at first start without the screen. A Pando that already has an
account or an app cannot join: that would be merging two installs, two audit logs among them, and is
not offered.

**The token [D], kubeadm-style:** `pando-join.<control address>.<CA hash>.<secret>`.

1. The joining host connects to the control address and checks the server's CA against the hash before
   sending anything. A token is never used against an install whose CA it was not minted for.
2. It sends the secret and a certificate request. Core checks the secret's keyed digest (as API tokens
   are stored, design 02 §2.1), the token's expiry, uses and role.
3. Core signs the host's certificate from the cluster authority, records the host pending until the
   host presents that certificate, and answers with what the role needs:
   - **App role:** nothing beyond the certificate. An app host never receives the database URL, the
     secrets key or the token key.
   - **Control role:** the database URL, the secrets key and the token key, sent only on this mutually
     authenticated connection, never in the token. These keys are what protect against a leaked
     database dump (R-190, R-063), which is why the token alone cannot fetch them.

**[P] Token defaults:** 24 hours, single use, revocable, listed with who minted it. Every mint, join,
refusal and revocation is audited.

**Who may mint [D].** `install.cluster.manage` (new, R-080). A control-role token hands over every key
the install has, so minting one needs every install verb, as configuring an identity provider does
(R-080a).

## Hosts talk on one port

**A joined host publishes one port: its agent's, mutual TLS only [D].** It never exposes its container
runtime's API. The agent (`pando host-agent serve`, extended) is given the host's own rootless runtime
socket and carries the runtime calls the `docker-hosts` adapter makes over that channel, beside the
proxy traffic it already forwards (O-45). The control host no longer holds an SSH key or a Docker TLS
client certificate for every app host, which was root on each.

**[P] Every host reaches every other host on that port.** Volume shipping goes host to host, and
fencing (below) needs hosts to see each other, not only the control plane. Hosts behind NAT that can
only dial out are not supported.

## Control-plane availability

Losing a control host leaves the install working. What #72 needed to be true on one host is now made
true across hosts:

| Was shared on one host | Now |
|---|---|
| Postgres on the control host | **An external HA Postgres the operator supplies [D].** Pando connects to one URL, as now. Pando running replicated Postgres itself is #341. |
| Secrets key, token key in `/var/lib/pando` | Handed to each control host at join, as above |
| Uploaded sources (one per app, at most 256 MiB) | Stored in Postgres |
| Build cache | Per host. A cold cache makes a build slower, not wrong |
| Local-disk backups, kept audit archives | Not allowed once a second host joins: the join asks for a remote backup destination first, and audit archives use `export` (design 02 §2.11) |
| Edge's dynamic config | Generated by each control host from the database |
| Image delivery | Through the install registry (`notes-image-registry-issue-72.md`), which a second host's join requires |

O-39 and O-47 are resolved by this: no shared directory, and replicas on any control host.

**[P] Ingress** to whichever control hosts are up is outside Pando: DNS with health checks, a virtual
address, or a Cloudflare tunnel with a connector per control host.

**[P] Builds** run on control hosts only (R-112 as now).

## Apps on several hosts

The first app host to join turns a `docker` install into a `docker-hosts` one **in place [D]**: the
existing host becomes the first app host, its apps are found by their labeled networks (sticky
placement already reads them) and nothing is re-created. This is not a runtime swap (R-257). Kubernetes
and Incus installs accept control-role joins only; their runtime already spans machines.

## Warm standby [D]

An app may have one **standby** on another app host: the same spec, created and stopped, with its data
kept current. When the app's host is lost, the standby is started and becomes the app. This is not app
replicas: one copy runs at a time (R-153 amended). It is the first time Pando acts on a host's failure
(R-010 amended).

**[P]** Standby is a host policy default (`standby`, on) with a per-app override in the spec, since
how an app runs is the spec's (R-020).

### Data

| What | How it is kept current | What a failover loses |
|---|---|---|
| No volumes | Nothing to ship | Nothing |
| Provisioned services | The engine's own replication through the services adapter (Postgres streaming, MySQL replicas, Redis replicas) | Seconds |
| Volumes | **A live incremental copy, shipped from host to host every interval [P: 5 minutes]** | The interval plus one copy's time |

**Volumes are copied live, with no pause and no filesystem requirement [D].** Pausing every few minutes
is not acceptable, and Pando does not change a host's filesystem. A copy of a volume that is written
during the copy may not be consistent — a database file in a volume can be torn (the same gap rolling
backups have, #344) — so:

- The app deploys and gets its standby. Its page carries a notice saying the standby's copy may be
  inconsistent. A notice, never a blocker.
- **The standby host keeps the last three copies [P]**, sharing unchanged files between them. A failover
  starts from the newest; if the app does not come up healthy, it tries the one before.
- **Every host's own storage is used as it is.** Nothing is migrated and no app is redeployed to gain a
  standby: the first copy is full, later ones send what changed.

### Capacity

Standbys reserve room so that **any one host can be lost and its apps fit on the others [D]**. The
check is per pair of hosts, which is how different sizes come out right: with A at 12 CPUs and B and C
at 32, losing B means C must hold most of B's load free, while losing A costs little. The largest host
decides how much everyone keeps free, and the console shows it.

- **[P] An app whose standby fits nowhere deploys without one**, with a notice naming the largest room
  any host has. Existing standbys' reservations are not given up for it.
- **Host policy `allow_failover_oversubscription` [D]**, off by default, lets standbys be placed past
  the reservation; at failover, apps start in priority order [P: a per-app priority].
- **Disk is never oversubscribed** (R-242): a standby's copies take disk on its host all the time and
  are counted.

### Deciding a host is gone, and fencing

**A host that is only cut off must never be taken for dead, and a host that is not cut off must never
stop its apps [D].** Both sides use one rule:

- Every host checks every other: an app host on its agent port, a control host on its cluster port.
- **A host stops its own apps** only when it has reached fewer than half the cluster's hosts for
  `T` **[P: 30 s]**.
- **The leader starts a host's standbys** only when more than half the hosts have not reached it for
  `3T`, so the lost host has stopped its apps before its standbys start.
- **With fewer than three hosts there is no automatic failover and no self-fencing.** Two hosts cannot
  tell a dead peer from a cut link. A person starts a standby, from the console or API, after saying the
  host is gone.

What this rules out:

| Situation | Outcome |
|---|---|
| Postgres or every control host is down | App hosts still reach each other: nothing stops, nothing fails over |
| One link between two hosts breaks | Both still reach a majority: nothing stops |
| A host's network is cut | It reaches nobody, stops its apps after `T`; its standbys start after `3T` |

An app host cut off from the control plane is serving no one anyway — every request reaches an app
through a control host's proxy (R-023) — so what fencing stops is its background writes, which are what
would make two copies diverge.

Durations are measured on each host's own monotonic clock; no host compares its clock to another's.

**Any answer from a host's machine is contact [D]**: a connection refused or reset, a TLS alert, a
refusal, a reply of any kind. Only silence is not. A host whose Pando process is down, restarting or
waiting for Postgres is not cut off, and its apps may be running. A host marked lost no longer counts
toward the cluster's size, on the leader and on every host alike.

### After a failover

The app's home is the standby's host. When the lost host returns, its stopped copy is removed and never
restarted (R-148 does not apply to a copy that was failed over), and it becomes the target for the
app's next standby. An app that was `failed` is not failed over (R-151).

## Upgrades and versions [P]

A host joins only at the control plane's version. The in-place upgrade (R-359) stays refused with more
than one replica; a cluster upgrades by rolling each control host, after which app hosts' agents are
re-created at the new version by the adapter, one host at a time, as now.

## What does not change

- A single-host install behaves exactly as before. Nothing in this design runs until a second host
  joins.
- The proxy is still the only route to an app (R-023), with the agent's certificate check as part of
  the route (O-45). Header and cookie stripping are unchanged.
- No bin-packing, no autoscaling, no running replicas of an app, no moving an app on a timer (R-010).

## Requirements

Amended: R-010, R-013, R-080, R-080a, R-153, R-256. Added: R-414 – R-422.

Open: O-64 (confirming a host is off through its management interface), O-65 (a storage adapter
category for operator-supplied replicated storage), O-66 (whether clustering is in every edition).

### As built: agent and hosts

- **Runtime calls through the agent (R-416).** After the TLS handshake the request line
  `PANDO-AGENT/1 docker` asks for the host's runtime; the agent answers `OK` and splices the stream to
  `PANDO_AGENT_RUNTIME_SOCKET`, or `NO <reason>`. Only for a certificate that `hostagent.IsControl`
  accepts: subject `pando-proxy` with organizational unit `control`, which only a holder of the cluster
  authority's key — a control host — can issue (`Authorities.IssueClient`). A host's own certificate
  (`Authorities.IssueHost`: subject and DNS name `pando-agent.<name>.invalid`, the roles as
  organizational units, server and client usage) is never accepted, nor is an agent's server-only one.
  An agent without a socket carries no runtime calls, so agents the runtime starts itself are unchanged.
- **`agent://host:port` endpoints.** The `docker-hosts` runtime reaches such a host's Docker API with
  one agent stream per client connection, verifying the agent by name and by the pinned
  `cert_sha256`; the proxy reaches that host's apps with the same TLS configuration. The runtime does not
  create or replace a joined host's agent: the host runs its own, as a container named `pando-agent`, so
  the per-host Docker adapter still joins it to the host's app networks. `tcp://` and `ssh://` hosts
  are unchanged.
- **Hosts from core.** `hosts.Runtime.Sync` writes every app-role row of `cluster_hosts` into the
  default runtime's host list as `agent://<address>` entries, keeps every entry the operator wrote, puts
  the cluster authority as the runtime's `agent_authority` credential and signals `cluster_signals`. It
  runs from `hosts.Service.OnChange`, after a host is removed or (for the join) after it is recorded.
  A join calls `hosts.Service.Runtime.CheckJoin(roles)` first.
- **Conversion (R-418).** `Sync` on a `docker` runtime with an app host rewrites the same adapter row
  (same ID, so apps stay pinned) to `docker-hosts`, with this machine as the control host under the
  name `control` [P], its `host`, totals, `network_pool`, `network_block_bits`, `oci_runtime`,
  `pids_limit` and `egress_gateway_image` (as `agent_image`) carried over. It is audited
  `cluster.runtime.convert` before the write, and a second run is a no-op. A volume handle recorded
  without a host is read as the control host's until the reconciler records it with one. The control
  host's agent address is the host of `server.advertise_url` and port 7443; without one, an app-role
  join is refused saying to set it. A runtime declared in the configuration file, and `network_pool`
  `off`, are refused the same way. Kubernetes and Incus refuse app-role joins (`hosts.CheckAppRoleJoin`).
- **Several control hosts** read this one host list, each as its own machine sees it: see "As built:
  several control hosts", below. Renewing a host's certificate updates `cert_sha256`, which the next
  `Sync` pins ("every host is watched", below).

### As built: failover capacity

R-422, in `internal/adapter/runtime/multidocker/reserve.go` (pure) and `standby_place.go`
(`chooseStandbyHost`), because placement is the adapter's (R-256).

- **Pairwise reservation.** For a lost host H and another host G, the standbys on G of H's apps must
  fit G's free CPU and memory, where free is G's total less its *running* containers' limits.
  Standbys are stopped, so they are not in free; G keeps the largest of its per-H sums, not their sum.
- **Choosing a host [P]:** any open host other than the app's own where the standby fits; among those
  the most room left for the app's host's loss (memory, then CPU), then fewer standbys, then host
  order. This spreads a large host's standbys rather than filling the roomiest host first.
- **Disk:** every standby's copies are counted on its host always, at what the standby reports or
  three times the app's disk limit [P], against what is left of the host's configured
  `total_disk_bytes` after its running apps' disk limits (`docker.CommittedDisk`, review fixes
  below); a host that reports no disk total is not checked, as the planner does not check it.
- **No room** is `standbyChoice.NoRoom`, an `errs.Error` (`CAPACITY_WOULD_OVERSUBSCRIBE`) whose message
  names the largest room any host has. It is never returned as an error and never blocks a deploy:
  the standby code records it, and `notice.Service.Standbys` raises it as notice `standby.no_room`.
  Existing standbys are never moved or dropped for a new one.
- **Policy** `allow_failover_oversubscription` (host policy document, no column) reaches the adapter
  as `BundlePlan.Failover.AllowOversubscription`, set by a deploy only; the reconciler sends false.
  It skips the CPU and memory check, never disk.
- **Priority** is the spec's `availability.priority` (integer, default 0, a benign change), carried as
  `BundlePlan.Failover.Priority`. `failoverOrder` starts a lost host's standbys on a host highest
  priority first, then by app, each if it fits what is left; one that does not fit is skipped and
  the next tried.
- **Capacity details** gain, per host row, `failover`: `kept_free_for` (lost host, CPU, memory and a
  sentence such as "b's failover needs 20.00 cores and 40.0 GB free on c."), `reserved_cpu_millis`,
  `reserved_memory_bytes` and `standby_disk_bytes` (R-243), from the standbys the hosts hold.
- **Not yet:** a new app's own placement (`choose` in `placement.go`) does not subtract what a host
  keeps free for failover, so a new running app can take reserved room (O-67). Doing so would refuse a
  deploy for room kept for standbys, which R-422 does not say; left for a decision.

### As built: control-plane availability

- **Uploads** are in `app_uploads` and `app_upload_chunks` (migration 000086): 4 MiB bytea chunks
  under one row holding the archive's SHA-256 and size, checked on every read. Not one bytea, which
  holds a 256 MiB archive whole in memory on both sides, and not a large object, which outlives its
  row unless something unlinks it and carries privileges `applyGrants` does not cover. A write is one
  transaction, so a deploy reading during an upload reads the previous archive whole.
  `source.Sources.Uploads` (`state.Uploads`) is the store; a file under `/var/lib/pando/uploads` from
  before is moved into the database the first time it is read, and at start (`MigrateUploads`). The
  DR bundle carries database uploads in its `pg_dump`, still carries files not yet moved under
  `uploads/`, and counts the first as `uploads_in_database` in its manifest.
- **Readiness for a second host** is `hosts.Readiness.ReadyToJoin`, for any role: the default backup
  destination must keep backups off the host, `audit_archive` must be `export` (or `off`), an
  `audit_archive_destination` must be off the host too, and the install must have an image registry.
  "Off the host" is a capability, `BackupCapabilities.OffHost` (R-254), not the kind's name; `local`
  reports false and `s3` true (O-69, below).
- **The in-place upgrade** is refused while the cluster has more than one host (`upgrade.Service.Hosts`,
  `hosts.Service.Count`), as it is with more than one replica.
- **The edge's routes** are written by `reconciler.RouteSync` from every live app's pinned spec, on
  one replica of each control host at a time (`state.Cluster.OnePerHost`), checked once a minute [P],
  while more than one host holds the control role (`hosts.Service.ControlCount`; a cluster of one
  control host and app hosts has one edge, the reconciler's). A route is written only when it differs
  from the one that host last wrote, or was last written `RouteSyncRefresh` [P: an hour] ago, in
  case its edge lost it (third review fixes, below). The reconciler writes
  a route only on the replica holding the app's lease, so without it a second control host's edge
  would never learn the route. Traefik's `Ensure` now leaves an unchanged file alone, since Traefik
  reloads on each write. A route left on a host for a deleted app still points at Pando's proxy,
  which no longer knows the hostname (R-023); it is removed by the GC only on the host that ran it.
- **What stays under `/var/lib/pando`, per host, and why that is fine:**

  | Path | Why it need not be shared |
  |---|---|
  | `secrets.key`, `token.key`, `cluster-ca.pem` | Handed to each control host at join |
  | `work/` | Clones, unpacked uploads and the upgrade's outcome file: scratch for work done on that host |
  | `buildcache/` | Per host by design: a cold cache makes a build slower, not wrong. A deleted app's cache is forgotten only on the host that runs the GC |
  | `audit-archives/` | Used only with `audit_archive: keep`, which a second host's join refuses |
  | `backups/` | The `local` destination, which a second host's join refuses as the default. An app whose spec names a `local` destination explicitly still backs up there |
  | The edge's `acme` volume | Per edge; how several edges share certificates is O-68 |

### As built: liveness and fencing

`internal/core/liveness` (R-421). The rule is decided in two pure types and run by three:

- **`Tracker`**, one host's own view. *Reached* is any contact with a peer in the last `T`, in either
  direction: the host's probe was answered, or the peer's probe arrived. Counting both keeps the two
  ends of a link in agreement, so a host the majority has not reached for `3T` has itself reached
  fewer than half for at least `2T`. A host fences when the cluster has three or more hosts, it
  reached fewer than half (itself included) in the last `T`, **and it has probed without a break for
  all of `T`**. A pause between its own rounds longer than three intervals (a suspended machine, a
  stalled process) restarts the window rather than counting as silence; such a host learns it was
  taken for lost from its peers' answers instead (see "review fixes, liveness"). **A peer known for
  less than `T` and not yet reached counts as reached** toward fencing, so hosts joining at once,
  still starting their agents, never make a host that reaches every host it knew fence; a fence
  already decided ends only on contact.
- **`Ledger`**, the leader's. A host is lost when the cluster has three or more hosts and more than
  half of them report having watched it without contact for `3T`, in a report the leader heard in
  the last three intervals. A host's report never claims longer than it has watched without a break,
  nor longer than it has known the peer.
- **`Monitor`** runs on every host: on an app host in its agent (`host-agent serve`, given
  `PANDO_HOST_ID`, `PANDO_CLUSTER_PEERS_FILE` and a host certificate), on a control host in the
  Pando process (see "every host is watched"). It needs no database: a peer list it cannot
  read keeps the last, and the list is read beside the round, never in it. Hosts marked lost are
  left out of it. Its fence stops every running container labeled for an app through the host's
  own runtime (`hostagent.DockerFencer`), all at once and beside the round, every round while cut
  off; it never starts one.
  App containers have no restart policy, so they stay stopped; starting them again is the control
  plane's (R-148), once the host is reached and was not taken for lost.
- **`Watch`** is the leader job `liveness`. It probes every host's agent, and each answers with its
  own view (`liveness.View`), so the leader learns every host's view from the probes it makes anyway:
  no reporting endpoint and no table of observations. **[P] The observations are kept in the
  leader's memory**, not in Postgres: a decision needs only the last `3T` of them, a new leader that
  starts empty only waits `3T` again, and the database stays off the path of deciding a host is
  gone. Migration 000088 keeps only outcomes: `cluster_hosts.lost_at`/`lost_by`, and
  `last_fence_id` so that each fence a host reports is audited once (`cluster.host.fence`).
- **`Service`** marks a host lost and calls `Failover.PromoteStandby` for each app homed on it with
  a standby, skipping a `failed` app (R-151); the leader's failovers run beside its round. The
  manual path, `POST /cluster/hosts/{hostID}/lost` (`install.cluster.manage`), works at any cluster
  size and **[P] is refused unless the host has gone unheard for `T`** (`liveness.Witness`), since a
  standby started beside running apps would be a second copy (R-153). Audited:
  `cluster.host.lost` before the host is marked, and `cluster.failover` per app before its standby
  starts. **Each event is a precondition** (R-421, R-228): a host whose `cluster.host.lost` cannot be
  written is not marked (by hand, the request fails with `INTERNAL`), and an app whose
  `cluster.failover` cannot be written is not started and is skipped as `audit_unavailable`; the
  leader tries that host's failover again every round until the event is written
  (`Service.refail`). Finishing a promotion cut short (`standby.Service.resume`) writes its own
  `cluster.failover` (`trigger: resume`) first, and waits for the next pass without it.
  `app.standby.promote` records the outcome after the runtime has acted, so it gates nothing.

Timings **[P]**: `T` 30 s, probes every `T/6` (5 s), each within half an interval.

The ping is `PANDO-AGENT/1 ping <host-id>` on the agent port (a control host's cluster port), answered `PONG <view>`. Hosts probe
with a certificate whose subject is `pando-host`, issued from the cluster authority to that host
alone: it carries the host's ID as its one URI, `pando-host:<host-id>`, and the leader's collector
has its own, marked by the organizational unit `collector` (review fixes, below). The ping is taken
to be from the host the certificate names, and one naming another host is refused. An agent answers
a ping from such a certificate and refuses anything else, so a host's certificate opens no
connection to another host's apps (R-023).

Left to other slices: `Homes` and `Failover` (the standby slice; until set, a lost host is marked
and nothing is started), clearing `lost_at` and removing the returned host's copies (R-419,
`Cluster.ClearHostLost`). The join now gives a host its ID, peer file and host certificate, the
install's first host is recorded, and every host, control hosts included, is watched (see "every
host is watched").

### As built: warm standby

- **Runtime API.** `RuntimeCapabilities.SupportsStandby` and `api.StandbyRuntime`, asserted only after
  the capability says yes, as `SelfUpgrader` is: `ApplyStandby`, `ShipVolumes`, `PromoteStandby`,
  `RemoveStandby`, `RemoveStaleCopy` (`internal/adapter/api/standby.go`). A host is an opaque string the
  runtime returns; core records and hands it back and never chooses one (R-256). `docker` does not
  report the capability; `docker-hosts` does with two hosts open to new apps.
- **The standby is the app's plan under its own bundle name**, `<bundle>-standby`, created and never
  started (`docker.ApplyStopped`), without its egress gateway. Placement reads where an app runs from
  its bundle label, so the standby's host is never mistaken for the app's. Promoting removes it and
  applies the app's own plan on that host from a restored copy: the "created and stopped" containers
  hold the image and prove the plan creates there; they are not renamed into the app.
- **Volume copies** live in one staging volume per app volume on the standby's host, labeled
  `io.pando.standby`, never `io.pando.bundle`. A ship lists the volume (`find` + `stat`: mode, size,
  mtime), compares with the newest copy's listing, and streams a tar of what differs through Pando into
  a hard-linked clone (`cp -al`) of the newest copy. A file whose mtime is in or after the second the
  last listing was taken is always sent. Owner changes alone are not detected. A file name holding a
  line break fails the ship with a message naming it. A ship with nothing changed makes no new copy.
  A file removed between being listed and being read is left out, not an error (review fixes below).
- **Promote** tries the newest copy, then older ones (up to three), each given
  `DefaultPromoteHealthTimeout` [P: 2 minutes] to be healthy on two looks in a row. When none is, the
  oldest tried is left running and reported `Healthy: false`. A standby with no copy of a volume is
  refused, not started empty. The promoted host gets a home mark (a labeled volume), written before the
  app's plan is first applied there, so a lost host that returns still holding the bundle loses to it
  in placement, in a restarted process too. Once the app is healthy the staging volumes on that host
  are removed (review fixes below).
- **Core.** `internal/core/standby` is the leader's job (every minute; ships when the newest copy is
  five minutes old [P]). It makes or refreshes the standby when the pinned spec changes, from
  `reconciler.Shape` plus `Complete` (one definition of what an app starts with), and records it in
  `app_standbys` (migration 000087). `Promote` and `RemoveStale` are for the liveness pass (R-421) and a
  manual start; neither decides a host is lost. A failed app's standby is neither refreshed nor started
  (R-151). After `RemoveStale`, the host that came back is where the next standby goes unless the app
  already has one elsewhere.
- **Provisioned services.** `ServicesCapabilities.ReplicaTypes` and `api.ReplicaServices` exist; no kind
  implements them yet. Postgres streaming replication needs a running replica on the standby's host
  reaching the primary across hosts, which the agent does not carry, so every provisioned service's
  data travels in the volume copy, and the app's notice names its types.
- **Notice** `standby.live_copy` on an app whose standby holds volume copies, fingerprinted on the host
  and the unreplicated types, never on when a copy was taken. Never a blocker.

### As built: joining

- **The cluster listener** is `server.cluster_addr` [P: `:7444`] on every control host, TLS 1.3 with a
  certificate issued from the cluster authority at each start and sent with the authority's own. It
  serves `GET /cluster/authority` (the authority's certificate, public) and `POST /cluster/join`, and
  `GET /cluster/peers` (finishing the join), pings on `pando-ping/1` (every host is watched), and nothing else; it is not under `/api/v1` and not in
  the API reference. A join token's address is this
  listener's, `https://<host>:<port>`: `https` only, and the default port is added when the minter
  gives none. Minted without one, it is the external URL's host (else the advertise URL's, else the
  machine's) with `cluster_addr`'s port.
- **Pinning.** The joining host connects trusting only a certificate authority in the server's chain
  whose public key hashes to the token's hash, and checks the server certificate chains to it and is
  the listener's: subject `pando-cluster-listener` and the one URI `urn:pando:cluster-listener`,
  which only the listener's own issuance writes (`hosts.PinnedTLS`, `isListener`). Chaining alone
  would not do: every host holds a certificate from the same authority, so an app host could pose as
  the listener and take a join token's secret, and for a control-role token the control keys. A
  host's certificate is named for the host, and the probe, collector and proxy certificates carry
  other subjects and client authentication only; only a holder of the authority's key, a control
  host, can issue the listener's. The joined host's peer list and renewal connect the same way. The
  server's name is not checked: the pinned authority and the listener's identity are the trust, as
  in kubeadm. The joining host is authenticated by the token's secret; it has no certificate until
  the join issues one.
- **Checks first, then one short transaction.** `hosts.Service.Join` checks the token without
  spending it (`state.Cluster.CheckJoinToken`), then asks `hosts.Service.Readiness` (a
  `hosts.JoinReadiness`; the server sets `hosts.Readiness`) and `Runtime.CheckJoin` and issues the
  certificates, holding no connection, within `JoinCheckTimeout` [P: 30 s]. Only then does
  `state.Cluster.Join` spend the use and record the host, pending, in one transaction (second review
  fixes below). A refusal before it spends nothing; a name taken gives the use back. The host gets
  the token's roles, never ones it asked for.
- **Refused before the token is touched:** a name the `docker-hosts` runtime cannot take (its own
  rule: lower-case letters, digits and hyphens, up to 40, starting and ending with a letter or
  digit), and `control`, the name the install's own machine falls back to (`hosts.CheckHostName`).
  A joined app host is written into the runtime's host list under its name, and one bad name stopped
  every later sync of it. Once the token checks out, `Runtime.CheckJoin` refuses an app host named as
  an entry of the runtime's host list Pando did not write (the operator's, or the install's
  machine's), and a version other than the server's (R-415 [P]) is refused (review fixes below);
  neither spends the use. The certificate is `hostagent.Authorities.IssueHost`'s, for the key in the
  joining host's request: `pando-agent.<name>.invalid`, the roles as organizational units.
- **What a control host is handed:** the database URL the server connects with (its owner URL), the
  API token key, the cluster authority's certificate and key, and the local secrets key with the path
  the secrets adapter's configuration names, which the joining host reads from the shared database
  after it restarts. An install using another secrets adapter hands no secrets key.
- **The joining side** writes, each file 0600 and replaced whole, into `server.cluster_dir`
  [P: `/var/lib/pando/cluster`]: `host.key`, `host.crt`, `ca.crt`, and for a control host
  `database-url`; the token key and authority replace the files at `server.token_key_path` and
  `server.cluster_authority_path`, made at its own first start. `joined.json` is written last and is
  what says the join happened. `config.Load` reads `database-url`, when present, ahead of
  `database.url` and `PANDO_DATABASE_URL`: the join is later than the Compose file.
- **After the join** Pando restarts in place. A control host comes back as a replica of the cluster on
  its database. A host with the app role alone starts no server and runs its agent (finishing the join).
- **"Has an account or an app"** counts any row in `users` or `apps`, deleted ones included. A Pando
  that has already joined cannot join again. `PANDO_JOIN_TOKEN` (with `PANDO_JOIN_NAME`) joins at
  first start and is ignored once `joined.json` exists; a failed join stops startup with its reason.
- **`POST /setup/join`** needs the setup token, as `POST /setup` does (finishing the join), and is
  refused cross-site like sign-in.
- **Audit:** the cluster writes `cluster.host.join` (principal: the join token) and
  `cluster.join.denied`, keeping of what a refused caller sent only a name and version that pass
  the checks, `(invalid)` otherwise (review fixes, below); the joining Pando writes `cluster.join` and `cluster.join.denied` in its own
  log, which a control host leaves behind for the cluster's when it restarts. Refusals past the
  rate limit are not audited each; one `cluster.join.limited` per address per window stands for
  them (review fixes, below).
- **Ports.** The cluster listener's default is 7444, the host agent's (`hostagent.DefaultPort`) 7443,
  so a host holding both roles binds both.
- The DR bundle carries the cluster authority as `cluster-ca.pem`, beside the token key (R-212).

  | The edge's `acme` volume | Used only while the cluster is one host. Once it is more, Pando orders the edge's certificates and every edge serves them (O-68, below) |

### As built: off-host backups and shared certificates

- **An off-host backup destination** is the `s3` backup kind (`internal/adapter/backup/s3`, O-69): any
  S3-compatible store, with endpoint (empty is Amazon S3), region, bucket, prefix and a path-style toggle
  as settings, and the access key ID and secret access key as adapter credentials (R-190); migration
  000087 refuses either key as a setting. It reports `OffHost: true`, which is what `ReadyToJoin` reads.
  A bundle is a multipart upload sent a part at a time [P: 32 MiB], so one part is the most held in
  memory, and nothing is in the bucket until `Close` completes the upload; a failed or cancelled write
  aborts it. `MaxBundleBytes` is 10,000 parts. Retention stays Pando's (`OwnsRetention: false`): the
  adapter cannot see a lifecycle rule. `HealthCheck` writes and deletes a probe object, so a key that may
  read and not write is unhealthy before a backup needs it.
- **Certificates once the cluster is more than one host** (O-68). The edge pass tells each routing
  adapter so (`EdgeRequest.Clustered`, from `hosts.Service.Count`). A Traefik Pando runs with
  certificates on then orders none itself: no resolver arguments, no `acme` volume, no DNS provider
  credentials in its environment. It asks Pando to issue instead (`EdgePlan.Issue`): the console's
  hostname and every app route's, which each route file now records on a `# pando-tls-hostname:` line, or
  the base domain's wildcard with DNS-01. The leader's `edges` job orders each certificate once
  (`edgecert.Issuer`). Every other control host runs its own edge pass (`edge.Service.RunFollower`,
  skipped while it leads or the cluster is one host, and for a runtime whose one edge serves every host),
  handing its edge what the leader holds and ordering nothing (`edgecert.Held`).
- **How the edge gets them.** The Docker runtime writes each certificate into the plan's
  `CertificateDir`, the routes directory Traefik already watches, as `<name>.crt` and `<name>.key` (the
  key 0600, read by Traefik as root in its container), only when changed, removing any it no longer
  holds. `pando-certificates.yml` there lists them for Traefik's certificate store, and app and console
  routers carry `tls: {}`, so Traefik serves the stored certificate for the name asked. Port 80 has a
  router for `/.well-known/acme-challenge/` to Pando's proxy ahead of a fallback that redirects to HTTPS,
  replacing the entrypoint redirect, which would redirect the CA's request too. Any control host's proxy
  answers the challenge from `edge_acme_challenges`.
- **What cannot be shared is refused at join.** A Traefik somebody else runs beside each host, with its
  own certificate resolver, reports `RoutingCapabilities.CertificatesPerEdge`, and `ReadyToJoin` refuses
  a control host's join while one is configured, naming the fix. An app host's join is not refused for
  it: an app host runs no edge.
- **Not verified on a real cluster yet**: Traefik's reading of a certificate file rewritten in its
  watched directory, and of the 0600 key under rootless Docker.

### As built: wiring standby, capacity and fencing

- **The reservation reads the standbys already placed (R-422).** `ApplyStandby` writes the standby's
  plan labels `pando.standby` (the app), `pando.standby.primary` (the app's host then),
  `pando.standby.priority` (`Failover.Priority`) and `pando.standby.disk` (what its copies are counted
  at); the Docker adapter puts every `pando.standby*` plan label on the standby's containers as
  `io.pando.standby*`, and a container whose standby labels differ from the plan's is made again.
  `docker.StandbyLoads` lists one host's standbys from a container list (stopped ones included),
  summing the `io.pando.limit.*` labels; `multidocker.listStandbys` joins every host's into the
  reservation's `standbyLoad`s. A host that does not answer is left out: it takes no new standby, and
  its standbys start nowhere while it is down. `Adapter.standbys` is now only a test override.
- **Keeping a standby where it is** (`standbyAsk.Prefer`): the current host is kept while it is open,
  is not the app's own, and has room for the standby past what it keeps for the app's host's loss,
  the app's own standby not counted (it is being placed again). Otherwise the reservation chooses, as
  for a new one. Keeping it is therefore always a placement the reservation allows, so it never breaks
  the loss of any one host. This replaced keeping it whenever it was listed and open.
- **Policy reaches placement.** The standby pass sets `Failover.AllowOversubscription` from host
  policy on the plan it hands `ApplyStandby`; `Shape`, like the reconciler, sends false.
- **No room is a notice (R-422).** Migration 000089 adds `app_standbys.last_error_code` and
  `last_error_remedy` beside `last_error`. `standby.Service.Standby` (the notice's `Rooms`) returns
  the message and remedy when the last placement failed with `CAPACITY_WOULD_OVERSUBSCRIBE` and the app
  has no standby; an app that keeps an older standby because a refresh found no room is not told it
  has none. Nothing is refused: the app runs as deployed.
- **An app's home is recorded [P].** `api.StandbyHandle.Home` is where the bundle runs;
  `app_standbys.home_host` keeps it, and a promotion sets it to the standby's host before the runtime
  starts anything (written ahead, review fixes below). It is how the
  leader knows which apps a lost host held, in the runtime's host names, which are `cluster_hosts`
  names (`hosts.Runtime.Sync`; the install's own machine included, review fixes below). Asking the runtime instead would fail exactly when it matters: a new
  leader cannot see a lost host's bundles. An app with no standby row is not listed for a lost host
  (it has nothing to fail over to); one whose placement never succeeded has no home recorded. The
  reconciler still holds such an app by the host the runtime found it on (`api.ObservedBundle.Host`,
  review fixes below). A promotion whose runtime did not name
  the former host takes the recorded home as the stale one.
- **Fencing calls standby (R-421).** `standby.Failover` implements `liveness.Homes`, `Failover` and
  `Returns` over `*standby.Service`, `state.Standbys` and `state.Cluster`; `cmd/pando` sets all three on
  the host-loss service. `AppsHomedOn` lists a host's apps highest `availability.priority` first, then
  by app ID, the order `failoverOrder` uses, with the priority read from each app's pinned spec in the
  same query (`state.Standbys.HomedOn`), and the liveness pass promotes them in queues side by side
  (third review fixes below).
  `multidocker.PromoteStandby` refuses (`CAPACITY_WOULD_OVERSUBSCRIBE`) a standby that does not fit
  its host's free CPU and memory now, counting what the bundle already runs there, before touching
  anything, so past the reservation the next app in order still starts. A failed app is skipped
  (R-151).
- **A returning host (R-419).** Each round the leader reads which hosts are lost (at any cluster size,
  so a host marked lost by hand in a cluster of two comes back too). A lost host that answered the
  probe, and that the majority no longer takes for lost, is back: `standby.Service.RemoveStale` runs
  for each app whose `stale_host` it is, deleted apps included (third review fixes below), then `Cluster.ClearHostLost`, then `cluster.host.returned` is
  audited with the apps whose copies went. A failure keeps the mark and is retried next round. Nothing
  is started on it; an app that was not failed over is still homed there and the reconciler restarts
  it (R-148). Without `Returns` wired a lost host stays marked.
- **Console.** Host policy `standby` is the *Warm standby* switch on the Deployments policy page, on
  when unset, beside *Failover oversubscription*.
- **Migration number.** 000089 was the next free number on `cluster-design` when this was built; a
  parallel slice adding its own 000089 renumbers one of them at merge.

### As built: finishing the join

- **The setup token (R-046).** `POST /setup/join` takes `setup_token` beside `token` and `name`, and
  `hosts.Joiner.SetupWithToken` checks it (`state.Users.CheckSetupToken`, the same digest and
  constant-time compare as `POST /setup`) before the join token is read, so a wrong one reaches no
  cluster. It is checked, not used up: once joined, Pando restarts into the cluster and leaves this
  database behind. `PANDO_JOIN_TOKEN` needs none, since setting it takes the host. The console's
  Join a cluster form and `pando join --setup-token` send it.
- **Ports.** `server.cluster_addr` defaults to `:7444` (`hosts.DefaultClusterPort`), the agent stays
  on 7443; a token minted with an address without a port gets 7444.
- **What an app host is given** besides its certificate: a `pando-host` client certificate
  carrying its host ID, and its key (`host-client.crt`, `host-client.key`), issued by the control side at join because only a
  holder of the authority's key can issue one — it opens nothing but pings (R-421) — and the
  runtime's `network_pool`, kept in `joined.json` as `network_pool`. Every join gets the client
  certificate; the pool is sent for the app role.
- **`runAppHost`** runs the agent in the Pando process, as `host-agent serve` does
  (`serveAgent`): `host.crt`/`host.key` (named `hostagent.ServerName(<name>)` by `IssueHost`),
  clients checked against `ca.crt`, the network pool from `PANDO_AGENT_NETWORK_POOL`, else
  `joined.json`, else 10.213.0.0/16, and the runtime socket from `PANDO_AGENT_RUNTIME_SOCKET`, else
  `DOCKER_HOST` when it is `unix://`, else `$XDG_RUNTIME_DIR/docker.sock` (rootless, R-402, R-416);
  none of those found stops it with the variable to set. It listens on `PANDO_AGENT_LISTEN`, else
  `:7443`. Its `liveness.Monitor` is its host ID from `joined.json`, probing with its probe
  certificate and fencing through the same socket. Both certificates are renewed while it runs
  (review fixes, below).
- **The peer list** is `GET /cluster/peers` on the cluster listener, which now asks for a client
  certificate and checks one when given (`VerifyClientCertIfGiven`: a joining host has none). The
  certificate must be one a host holds — its fingerprint is a row's `cert_sha256`, or
  `prev_cert_sha256` during a renewal — and never a revoked one, so a removed host hears nothing (`hosts.Service.Peers`, `AUTH_INVALID` otherwise). It answers
  `{"hosts":[{id,name,address}]}`. `hosts.PeerSync` fetches it from the join token's address every
  round (2 s timeout), writes `peers.json` in the cluster directory, and when the listener cannot be
  reached returns what that file last held. The joined host connects trusting the authority in
  `ca.crt`, by its key, and requiring the listener's own identity, not the listener's name, as at
  join (`hosts.PinnedTLS`).
- **The first host (R-414).** At each start a control host that has not joined calls
  `hosts.RecordInstallHost`: `state.Cluster.RecordFirstHost` inserts it under an advisory lock unless
  a row with no join token exists, so replicas of one install record one host between them and a
  restart records none. Name: the first label of the machine's hostname as a DNS label, cut to the
  40 characters the `docker-hosts` runtime takes, else `control`; the conversion names the runtime's
  control entry the same (review fixes below); roles control and app; address the advertise URL's host, else the external URL's, else
  the machine's, with port 7443; `cert_sha256` 64 zeros [P], since nothing pins it (below). A failure
  is logged and startup goes on. **A host with no join token is the install's own** (`hosts.InstallHost`);
  a join token cannot be deleted while a host joined with it (`ON DELETE RESTRICT`, migration
  000092), so `joined_with` stays set on every joined host. The first host cannot be removed
  (`state.ErrInstallHost`).
- **Counting.** The "+1" is gone: `hosts.Service.Count` is the rows, and never less than one, so a
  single-host install reports one host before and after its row is made; `DefaultRoles` reads
  `Count`.
- **What the first host is left out of, and why.** `RuntimeHosts` skips it, since it is the
  runtime's `control` entry already, so a conversion is unchanged. It was left out of
  `hosts.LivenessPeers` too, until "every host is watched", which made every control host answer
  pings; it is now watched like any other.
- **Not done here.** An app host's agent has to be on the host's app networks to reach its containers;
  the per-host Docker adapter connects the container named `pando-agent`, so on an app host Pando's
  own container must be named `pando-agent` (install files not changed here). `peers.json` is
  rewritten every round even when unchanged.

### As built: every host is watched

Every row of `cluster_hosts`, whatever its roles, is watched and watching (R-421). Before this only
joined app hosts answered pings and ran a `Monitor`: the first host was left out of the peer list,
and joined control hosts were in it but answered nowhere, so with three or more hosts a healthy
control host was reported unreached and could be taken for lost while its apps ran, and a control
host that was cut off never fenced.

- **A control host answers on its cluster listener [P]** (`server.cluster_addr`, 7444), not on an
  agent. A ping asks for the ALPN protocol `pando-ping/1` (`hostagent.PingProtocol`, sent by every
  `hostagent.Ping`); the listener's `GetConfigForClient` serves such a connection with the host's
  own certificate and **requires** a client certificate from the cluster authority, and
  `TLSNextProto` hands it to `hostagent.AnswerPing`, which answers one ping and refuses anything
  else. Every other connection is the listener's HTTP as before (HTTP/2 is no longer offered there;
  its clients use HTTP/1.1). An agent negotiates no protocol, so the ALPN offer changes nothing for
  app hosts. Chosen over giving the agent the runtime starts on a control host a `Liveness`: that
  agent is a separate container with no runtime socket, so its `Monitor` could not fence, and the
  agent port on the first host is already its. The listener runs in the Pando process, which has the
  host's Docker and the database, and is published on every control host already.
- **The control host's `Monitor`** is the one app hosts run (`newMonitor`), started in the Pando
  process on every control host (`startControlLiveness`, `cmd/pando/controlhost.go`), on every
  replica, not only the leader. Its peers come from the database (`hosts.ControlPeers`); a read that
  fails keeps the last list, so Postgres away stops nothing. It probes with a `pando-host`
  certificate carrying its host ID, issued in memory at start and again before it expires, since a
  control host holds the authority. It fences
  through `hostagent.DockerFencer` on the Docker client `DOCKER_HOST` names (else the default
  socket): only containers labeled for an app, never Pando, the edge or the runtime's agent. A
  runtime it cannot reach still leaves it answering pings; its fence then fails and says so every
  round it is cut off.
- **Who it is.** A joined control host is the row of its `joined.json` host ID; the install's own
  host is the row with no join token (`hosts.InstallHost`). Both are kept in `server.cluster_dir`
  (`joined.json`, `control-host.json`), with the peers last read (`peers.json`), so a control host
  answers pings and watches its peers before it reaches Postgres (see "review fixes, liveness").
  Only the first start ever waits for the database; until then it tries again every 5 s [P] and
  refuses pings by name.
- **Its certificate** is `host.crt`/`host.key` in `server.cluster_dir` (`hosts.ControlHostCert`):
  for a joined control host, the one its join wrote; for the first host, one issued from the
  authority at its first start and kept there, so its pin is the same across restarts. Either is
  issued again, and replaced, when missing, unreadable, for another name, or within 30 days [P] of
  its end, at start and while the host runs (`hosts.RenewingControlHostCert`), which also renews a
  joined control host's.
- **Its row.** Each round `hosts.ControlPeers` sets the host's own `ping_address` to the host it
  was recorded with and the cluster listener's port (`hosts.PingAddress`), and `ping_cert_sha256` to
  the certificate it answers with (`state.Cluster.SetHostPing`, migration 000091), when either
  differs, and a write that fails is made the next round. `address` and `cert_sha256` stay the
  agent's, which the runtime uses (review fixes, standby and runtime, below). A joined control host
  answers with the certificate its join wrote, which its agent serves too, so a renewed one also
  becomes its `cert_sha256`, the old one kept as `prev_cert_sha256` (`state.Cluster.RotateHostCert`);
  the first host keeps its placeholder agent pin, which nothing pins.
- **The peer list.** `hosts.LivenessPeers` is now every row, at its `ping_address` when it has one:
  the leader's `Watch`, `GET /cluster/peers` and a control host's `Monitor` all include the first
  host. `RuntimeHosts` still skips it (it is the runtime's control entry), and still skips
  control-only hosts.
- **Tests** (`cmd/pando/controlhost_test.go`): a first host, a joined control host and an app host,
  over TLS on loopback with real listeners, agents and `Monitor`s, only the network and clock
  simulated. All healthy for 10T: nobody unreached, nobody fences, nobody lost. Each cut off in
  turn (the leader's lease passing to the other control host when the first is cut): it alone fences
  after T, and it alone is taken for lost after 3T. Postgres away for 10T: nothing stops.
- **Not done here.** Replicas of
  the first host that do not share `server.cluster_dir` each keep their own certificate and rewrite
  its pin in turn; nothing pins it for liveness, so pings are unaffected. Under Kubernetes or Incus
  a control host has no app containers, and its fence logs that it cannot reach Docker.

### As built: review fixes, standby and runtime

Fixes from the review of #347, for what the standby, the reconciler and the runtime did wrong.

- **A fence is not undone (R-148, R-421).** A host's agent given a `Fence` (`hostagent.Agent.Fence`,
  its `liveness.Monitor`) serves each runtime stream call by call through an HTTP proxy and refuses
  `POST …/containers/create` and `…/containers/<id>/start|restart|unpause` while the host is fencing
  (`Monitor.Fencing`: from the decision to fence, through the stop, until the host reaches the
  cluster again; not only once every container has stopped), answering 409 with Docker's `{"message": …}`, on a connection opened before the fence too. Stopping,
  listing and everything else pass. An app host's agent takes its own Monitor; a host with both roles
  takes the control host's. The leader records what each host's answer says of its fence
  (`cluster_hosts.fenced_at`, migration 000091, written only when it changes, `cmd/pando`
  `fenceRecorder` around the `Watch`'s probe), because every replica reconciles and only the leader
  probes.
- **The reconciler leaves a held app alone (R-153).** `reconciler.Holds` (`state.Standbys.Held`)
  reads the app's standby row: a home host marked lost or fenced, or a promotion under way. Such an
  app is not observed or corrected; it is recorded unobservable with why, which counts toward
  nothing (R-151). A hold check that cannot read the database is not a hold, and an install of one
  host asks none (third review fixes, below). **An app with no standby is held too:** a runtime with several hosts names the
  host it found a bundle on (`api.ObservedBundle.Host`, set by `multidocker.Observe`), and an app
  found on a lost or fenced host (`Holds.HostHeld`) is recorded unobservable the same way, until the
  host is back or a person acts. A host that does not answer was already an observation error. A
  replica whose own control host is fencing (`Reconciler.LocalFenced`) reconciles nothing, and its
  standby pass does nothing. Deploys from a fenced control host are not stopped by
  this; only what Pando does by itself is.
- **One writer per app's standby (R-153).** Every write is compare-and-set on the row as it was read
  (`state.Standbys.SaveIf`, on the row's version since the third review fixes), and one process
  serializes the pass and promotions per app (`standby.Service` locks; the pass only tries). The pass
  skips an app whose home is lost (`StandbyApp.HomeLost`) or whose promotion is under way, so a spec
  change never moves the standby a promotion restores from, and a ship never drops a copy it may fall
  back to. `Delete` keeps a row whose promotion is under way. The runtime guards the same in process:
  `ApplyStandby`, `ShipVolumes` and `RemoveStandby` refuse a bundle being promoted.
- **Promotions are written ahead (R-153, R-419).** Before the runtime starts anything core saves
  `home_host` as the standby's host, `stale_host` as the host left, and `promoting_at`
  (migration 000091). A runtime error that names no host (`PromoteResult.Host` empty) is a refusal,
  and the row is put back; any error after it named one keeps the new home, with the error recorded.
  `HomedOn` lists an app under way for the host it is leaving, so marking that host lost again
  finishes it, and the pass finishes one under way for longer than a promotion takes
  (`(DefaultStandbyCopies+1) × health timeout` [P]), for a process that stopped mid-promotion. A
  returning host keeps its copy, and its lost mark, until the promotion is done.
- **The runtime's point of no return is the first start.** `docker-hosts` `PromoteStandby` removes the
  standby's own workloads, restores the newest copy and writes the home mark, and only then records
  the app on the host and applies its plan. A failure before that removes what it restored and names
  no host. A host that already holds the bundle's home mark had the app started there: nothing is
  restored over the volume it runs on, and the promotion is reported done. Once healthy, the host's
  staging volumes are removed. Placement never settles a bundle two hosts hold by their order: a
  promotion or the newest home mark does, else the host this process had it on, else neither, and
  `locate` reports the app unobservable (`placements.ambiguous`).
- **Disk is counted whole (R-242, R-422).** Each app container carries its disk limit
  (`io.pando.limit.disk`); `docker.CommittedDisk` sums it per running app, once. The reservation's
  free disk is the total less that (`roomOf`); placement refuses a host without the app's disk limit
  free beside its apps and the standby copies it keeps, naming the most any host has; and
  `api.Capacity.HeldDiskBytes`, the copies on every host, is counted as allocated by the planner.
  A container made before the label counts nothing until it is made again.
- **A busy volume ships (R-420).** The list and read helpers fail only for an error other than a
  file being gone (`vanishedOK`): a file removed between `find` and `stat`, or before `tar`, is left
  out, and the next listing shows it removed. Busybox tar has no `--ignore-failed-read`; its "No such
  file", "error exit delayed" and "empty archive" lines are filtered, and when every path went an
  empty archive is written in its place.
- **A host with both roles runs apps (R-416).** `cluster_hosts.address` and `cert_sha256` stay the
  agent's; the cluster listener a control host answers pings on is `ping_address` and
  `ping_cert_sha256` (above). `RuntimeHosts` lists the agent's, and a host joined with both roles
  runs its agent beside its server (`startJoinedAgent`), so the runtime reaches its apps. `GET
  /cluster/peers` accepts either pin.
- **The install's own machine has one name (R-421).** The conversion to `docker-hosts` names the
  control entry as the machine's `cluster_hosts` row (`hosts.Runtime`'s `localName`; `LocalName`, else
  `control`, only without a row), so the home the runtime reports is the name the liveness pass
  finds the machine's apps by when it is lost. `RuntimeHosts` no longer drops a joined host named
  as the control entry; the sync refuses the clash instead. A `docker-hosts` runtime an operator wrote
  by hand keeps the names it has: its control entry must be named as the row for the install's own
  machine's apps to fail over.
- **A single-host install does no standby work.** The pass builds no app's plan when no runtime
  adapter reports `SupportsStandby` (`Service.RuntimeRefs`), and still forgets rows left from before.
- **Tests:** `TestR153_TheReconcilerNeverRestartsAFencedOrFailedOverCopy`,
  `TestR151_AnAppWithoutAStandbyIsNotFailedWhileItsHostIsDown`,
  `TestR421_AnAgentStartsNothingWhileItsFenceIsUnderWay`, `TestR419_RemovingAHostLeavesNoStandbyNamingIt`,
  `TestR421_HostsJoiningAtOnceNeverMakeAHealthyHostFence`,
  `TestR421_AFailoverWhoseAuditCannotBeWrittenDoesNotHappen`,
  `TestR421_APromotionCutShortIsNotFinishedWithoutItsAuditEvent`,
  `TestR421_AFencedHostsAgentStartsNothingTheControlPlaneAsks`,
  `TestR153_AStandbyPassNeverPutsBackAPromotedAppsOldHome`,
  `TestR153_AStandbyRowIsWrittenOnlyOverWhatWasRead`,
  `TestR153_APromotionCutShortLeavesTheHomeOnTheNewHost`,
  `TestR153_APromotionNeverRestoresOverARunningVolume`,
  `TestR242_StandbyCopiesAndRunningDiskAreBothCounted`,
  `TestR422_StagingVolumesAreRemovedAfterPromotion`,
  `TestR420_ABusyVolumeWithVanishingFilesShips`,
  `TestR416_AHostWithBothRolesRunsItsAppsThroughItsAgent`,
  `TestR421_TheInstallsOwnMachinesAppsAreFailedOverWhenItIsLost`, among others.
- **Not done here.** A file that shrinks while busybox tar reads it still fails that ship ("short
  read"); the next ship tries again. Deploys are not refused on a fenced control host.
- **Merged with the certificate and liveness fixes.** The agent's pins are `cert_sha256` and
  `prev_cert_sha256` (`state.Host.Pins`, what `RuntimeHosts` writes); the ping columns are the
  listener's. `HoldsCert`, so `GET /cluster/peers` and renewal, accepts the ping pin too, and
  `RemoveHost` revokes it with the others. A host with both roles renews its agent's certificate as
  an app host does (`hosts.HostRenewal`), and its agent starts before the database, as its pings
  do. The manual-mark probe is the liveness slice's `liveness.Witness`, over the peer list.

### As built: review fixes, certificates and security

A security review of #347 found that no cluster certificate was ever renewed, that a ping's sender
was not authenticated, that the join listener let anyone fill the audit log, and that a removed host
kept working credentials. What changed:

- **Every cluster certificate is renewed (R-421).** Leaf certificates are valid 90 days
  (`hostagent.leafValidity`) and are issued again once 30 days or fewer are left
  (`hostagent.RenewBefore` [P]); left alone, every host's would have lapsed within days of the
  others' and the hosts would have fenced together. A running process takes the new one at its next
  handshake through `tls.Config.GetCertificate`/`GetClientCertificate` (`hostagent.RenewingCert`),
  never by restarting:
  - **In memory, from the authority** (`Authorities.InMemory`, renewed on the handshake that finds
    it due): Pando's proxy's client certificate (every `hostagent.ClientTLS` configuration made for
    one certificate shares one renewal, so the runtime adapters needed no change), the leader's
    collector's, a control host's probe certificate, and the cluster listener's own (365 days).
  - **A control host's own** (`hosts.RenewingControlHostCert`): reissued into `host.crt`/`host.key`;
    `hosts.ControlPeers.PinOf` writes the new fingerprint into its row the next round.
  - **A joined host's own and its probe certificate** (`hosts.HostRenewal`, checked hourly off the
    handshake): `POST /cluster/renew` on the cluster listener, presenting the certificate it holds,
    with a certificate request for a new key. `hosts.Service.Renew` finds the host by that
    certificate's fingerprint, issues a certificate for the new key with the same name and roles and
    a new probe certificate, and records the new fingerprint with `state.Cluster.RenewHostCert`, in
    one statement that keeps the presented one as `prev_cert_sha256` (migration 000092). Both are
    the host's until it presents the new one on `GET /cluster/peers`, which clears the old
    (`ConfirmHostCert`), so a renewal whose answer was lost leaves the host working with what it
    holds and able to renew again. A certificate is renewed once (second review fixes below): a
    renewal tried again is answered with the certificate already issued. The runtime's host list pins both meanwhile
    (`state.Host.Pins`, comma-separated, `hostagent.PinnedClientTLS`); renewing changes that list, so
    each renewal costs one runtime reload, about every 60 days per host. Audited as
    `cluster.host.renew`, refusals with a certificate as `cluster.host.renew.denied`; a renewal with
    no certificate is refused and not audited. A removed host's certificate, which still chains to
    the authority until it expires, is refused before anything else is done (`Service.CertRevoked`),
    with one `cluster.host.renew.denied` (`reason: revoked`) per certificate per window however often
    it is presented. Other refusals are limited per certificate [P: 5 in 15 minutes] and per client
    address [P: 20 in 15 minutes] (`handleClusterRenew`); each within the limit is audited, and past
    it the answer is `RATE_LIMITED` with one `cluster.host.renew.limited` per limit per window and no
    other row (R-347, R-348, R-404). The files are written key first; a crash between the
    four writes leaves a pair that does not match, which the next start reports.
  - Not changed: the multi-host Docker runtime's own agents, given their certificate in the
    environment, are re-created when it is due, as before (`labelAgentIssued`).
- **A ping's sender is its certificate's (R-421).** Each host's probe certificate carries its ID
  (`IssueHostClient(hostID)`), the collector's is marked `collector` (`IssueCollector`), and
  `hostagent.ProbeSender` reads the sender from the verified leaf. A ping naming anyone else, a host
  naming `-`, the collector naming a host, and any certificate naming no host (Pando's proxy's, or
  the one shared `pando-host` certificate every host used to hold) are refused. Before, one host
  could ping in every other host's name and keep a cut-off host from fencing.
- **Joins are bounded (R-347, R-404, R-415).** `hosts.CheckJoinRequest` checks everything that needs
  no token before the token: the name, the agent address as `host:port` with a numeric port and a
  DNS name or IP address (a bad one broke the runtime configuration for every host), and the
  platform and version as up to 64 printable characters. The audit keeps the name and version only
  when they pass, `(invalid)` otherwise, and no message echoes more than 64 characters of them. The
  cluster's version is said only once the token is good: it is checked after
  `state.Cluster.CheckJoinToken`, which spends no use. Refused joins are limited per
  client address as wrong passwords are, on `POST /cluster/join` and `POST /setup/join`. A refusal
  that tested a secret (a join token wrong, expired, used up or revoked, `AUTH_TOKEN_INVALID`, or a
  wrong setup token, `AUTH_INVALID`) counts under `state.ClusterJoinAddrKey` [P: 20 in 15 minutes].
  Any other refusal (a name, an address or a version Pando cannot use, an unreadable body, a cluster
  not ready) guesses at nothing and does not spend those; it counts under its own key instead
  [P: 200 in 15 minutes], so it still cannot fill the audit log (`settleJoin`). Over either limit the
  answer is `RATE_LIMITED` with `Retry-After`. Only the first request over a limit in a window is
  audited, as one `cluster.join.limited` per address; the requests after it add no row
  (`Server.auditOnce`, counted in the attempts store every replica shares). Auditing each one let an
  anonymous caller add rows nobody can remove for three months for as long as it kept asking
  (R-347, R-348).
- **Join-token role rules are the service's (R-261).** `hosts.Service.Mint` takes the principal and
  an every-install-verb checker (`authz.Authorizer`) and applies the default roles, their
  normalization and R-080a's rule for a control-role token itself; the handler decodes, calls,
  audits and answers.
  - **The listener stays on by default.** Considered and not done: starting it only once a join
    token exists or a second host has joined. Tokens are minted on any replica and a join is expected
    within minutes of minting, so every replica would have to watch for tokens; and once a second
    host exists the listener answers pings and serves the peer list and renewal anyway. With the
    bounds and the limits above, an unauthenticated caller can add at most the limit's refusals and
    one `cluster.join.limited` per address per window. A single-host install behaves as before.
- **A removed host's credentials are refused (R-415).** `state.Cluster.RemoveHost` records the
  fingerprints of the host's current, previous and ping certificates in `cluster_revoked_certs`, each
  row with the host's ID, in the same transaction as the delete, and in it clears the host from every `app_standbys` row (`forgetStandbyHost`): a standby
  there is placed again by the next pass, an app homed there has no home and is restored by the
  reconciler on a host the runtime has (R-148), a promotion onto it ends as a refused one does, a
  copy left on it is forgotten, and a promotion away from it goes on. Removing a host is not a
  failover: to start apps from their standbys, mark the host lost first; `Service.Peers` and `Service.Renew` refuse a revoked certificate, and the runtime's host
  list no longer has the host. Probe certificates are not recorded by fingerprint (one is issued at
  the join and at each renewal); they are revoked by the host's ID, which each carries as its one URI
  and which is never reused, and every removed host's ID is in the table, since every host has a
  current certificate. `state.Cluster.Revoked` lists the IDs and fingerprints, and every host that
  answers pings refuses a ping from a revoked ID or sent with a revoked certificate
  (`hosts.Members.Admits`, `hostagent.Member`, `AnswerPing`'s `member`), as well as one from a host
  not in the peer list it last heard. The revocations come with the peer list it reads every round
  (`GET /cluster/peers` answers `revoked` beside `hosts`, which `PeerSync` hands to `Members`; a
  control host reads them from the database with its list), so a removal reaches every answer within
  one probe interval of the next read; they are only ever added to, and kept in `revoked.json` beside
  `peers.json`, which a host reads before it hears any list. So a host that restarts while the
  control plane is away, or falls back to an older peer list, still refuses a removed host. Before
  any list is heard a host answers every ping from a host not revoked, so a host that has just
  started is not reported unreached. Before, a removed host's probe certificates were answered for
  their 90 days by any host that had not heard a list since, and by the app-host fallback below,
  which checked no membership at all. A removed
  control host held the authority's key and could issue itself anything: `DELETE
  /cluster/hosts/{id}` now answers 200 with the host and, for a control host, `rotate` and `notice`
  naming the secrets key, the API token key, the cluster certificate authority and the database
  password to replace. The CLI prints the notice, MCP returns it, and the console shows it.
- **The first host and this host cannot be removed (R-414).** `RemoveHost` refuses the row with no
  join token (`ErrInstallHost`), and `hosts.Service.Remove` refuses the host serving the request
  (`Service.IsSelf`); `GET /cluster/hosts` returns `this_host`, and the console offers no Remove on
  either. Removing the first host used to be accepted, and its next start recorded it again under a
  new ID, orphaning its apps' homes and its pin.
- **`pando join` needs no secret on the command line (R-194).** The join token is the optional
  argument, else `PANDO_JOIN_TOKEN`, else asked for without echo (or read from a pipe, one per
  line); the setup token is `--setup-token`, else `PANDO_SETUP_TOKEN`, else asked for. Either given
  as an argument is warned about.
- **Not done here.** `multidocker`'s `cert_sha256` field comment still says "the certificate the
  host joined with"; it now holds every certificate that is the host's (owned by the runtime slice).
  Rotating the keys a removed control host held is by hand; Pando has no rotation for them yet.
  Sign-in still audits every refusal over its limit (`session.denied`, `reason: rate_limited`); the
  once-per-window rule above was applied to the cluster listener and `POST /setup/join` only.
- **Tests.** `TestR347_ARateLimitedCallerAddsNoMoreAuditRows`,
  `TestR347_ARevokedHostCannotFloodTheLogThroughRenewal`,
  `TestR415_ARemovedHostsProbeCertificatesAreRefusedEverywhere` (integration),
  `TestR415_AControlHostRefusesARemovedHostItReadsFromTheDatabase`,
  `TestR415_ARevokedHostStaysRefused`, `TestR415_APeerListBringsWhatIsRevoked`,
  `TestR415_ThePeerListSaysWhoWasRemoved`, `TestR415_APingWithARevokedCertificateIsRefused`.

### As built: review fixes, routes and uploads

- **One route request.** `api.RouteFor(appID, spec.Routing, upstream)` builds the `RouteRequest`
  for the deploy, the reconciler's drift correction (`ensureRoute`) and `RouteSync`, TLS included:
  enabled for every mode but `port`, for the spec's hostname. The reconciler and route sync used to
  leave TLS out, so on a cluster every app's Traefik route lost its `tls:` block and its
  `# pando-tls-hostname:` marker (and a Kubernetes Ingress its TLS annotation) each minute, and the
  leader stopped ordering app certificates; on one host, a drift correction did the same (R-169,
  R-174).
- **An upload is spooled before it is stored.** `Sources.StoreUpload` copies the body to a hidden
  temporary file under `server.work_dir` (bounded by the handler's `maxUploadBytes`), then copies
  that into the database in one transaction, and removes the file. The transaction used to read the
  request body, holding a pooled connection for as long as the client took; a few dozen slow
  uploads held them all. A body that fails (over the limit) is returned as it failed, so the handler
  still answers it with its own message.
- **Moving an old upload file only fills an app with none.** `state.Uploads.AddUpload` inserts the
  `app_uploads` row with `ON CONFLICT DO NOTHING` and writes the chunks under that row's lock; the
  move from `/var/lib/pando/uploads` uses it at start and on first read, and reads the database's
  upload afterwards either way. It used to check, then replace, so an upload stored between the two
  was overwritten by the older file. A file whose app is deleted or no longer exists is removed
  rather than stored or tried again at every start (third review fixes, below).
- **`atomicfile.WriteIfChanged`** writes through `os.CreateTemp(dir, "."+base+"-*")`, synced,
  given its mode, then renamed, as `keyfile` does. With the fixed `path + ".tmp"`, route sync, the
  reconciler and a deploy writing one route at once truncated each other's file, or renamed one
  already moved.
- **Tests:** `TestR169_EveryRouteAsksForTLSUnlessItIsAPort`, `TestR169_RouteSyncKeepsTLSOnEveryAppsRoute`,
  `TestR169_ASingleHostDriftCorrectionKeepsTheAppsTLS` (integration),
  `TestR417_AStalledUploadHoldsNoDatabaseConnection`, `TestR417_AnOldUploadFileNeverReplacesANewerUpload`,
  `TestR417_AnOldUploadFileForADeletedAppIsDiscarded` (integration),
  `TestR417_AnUploadReachesTheStoreOnlyOnceItHasArrived`, `TestR417_ABodyThatFailsIsSaidAsItFailed`,
  `TestR417_AnOldUploadFileForADeletedAppIsRemoved`, `TestR417_ReadingAnOldFileKeepsANewerUpload`,
  `TestR417_ConcurrentWritersNeverLeaveAPartialFile`.

### As built: review fixes, liveness

Fixes to the liveness slice after review of #347, all held to one rule: a host that is not cut off is
never taken for lost and never stops its apps (R-421).

- **Contact is any answer from the machine.** `hostagent.Ping` returns a `*PingError` whose
  `Contact` says whether the machine answered: a dial refused or reset, and anything after the TCP
  connection is made (a failed handshake, a certificate for another name, a `NO …` refusal, an odd
  reply, even a read that times out on a connection the machine accepted). Only a dial that times
  out or finds no route is silence. The wiring wraps such an error in `liveness.ErrContactOnly`, and
  `Monitor`, `Watch` and `Witness` all count it as contact. A dead or restarting Pando on a running
  machine is no longer reported unreached by anyone.
- **The leader never takes for lost a host its own probe reached this round**, whatever the others
  report (`Watch.Round`).
- **A control host answers before its database.** `serve` loads the cluster authority, starts the
  control host's `Monitor` (`startControlLiveness`) and binds the cluster listener before
  `state.Connect`. The listener's HTTP is a `startingHandler` that refuses joins until the API is
  built; pings are answered from the start. Who the host is comes from the database when it
  answers, else `joined.json`, else `control-host.json` written at an earlier start; its peers come
  from the database once given (`controlPings.useStore`), and are kept in `peers.json` for the next
  start. **An app host whose agent cannot start in full** (its runtime socket unknown, a file
  unreadable) runs `appHostFallback`: an agent that carries no runtime calls, beside the host's own
  `Monitor`, and returns the startup error when stopped; only a host without its own certificate
  exits. It used to answer pings with a view naming no peer and run no `Monitor`, so cut off it never
  stopped its apps: the cluster took it for lost and started their standbys while its own copies
  ran (R-153). Now a host that answers pings is one that can fence. The `Monitor` needs only the
  host's own certificate and the authority's: it reads its peers as the full agent does, probes with
  the probe certificate or, when that cannot be read, with the host's own, which the others refuse,
  and a refusal is contact, all that fencing counts. It fences through the runtime at the socket
  `appHostRuntimeSocket` names. **[P] While that runtime does not answer, the host answers no
  ping** (`fenceGate`, rechecked at most every probe interval): it refuses with `NO this host
  cannot stop its own apps if it is cut off`. Chosen over the two alternatives because it is the
  only one that neither stops apps on a host that is not cut off nor lets a reachable host be taken
  for lost: a refusal is contact, so a host whose Pando cannot fence is never marked lost while it
  can be reached (no standby starts beside its apps), and it says nothing of the other hosts it
  could not act on. Stopping its apps at once was rejected: it stops apps on a host that is not cut
  off, and with no runtime reachable it cannot be done anyway. What remains: a host cut off while
  Pando can reach no runtime cannot stop apps running on one Pando does not know of; with no restart
  policy, such apps exist only if Pando was restarted under a different runtime socket while they
  ran. The fallback does not renew its certificates; it lasts until Pando is restarted.
  **A control host is held to the same rule** (`controlPing.answer`, the same `fenceGate`): while
  the Docker its fence stops apps through (`fenceClient`: a joined app host's agent socket, otherwise `client.FromEnv`) does not answer, its cluster listener
  refuses every ping with the same reason, rechecked at most every probe interval, and answers again
  once Docker does. It used to answer and only log the fences that failed: cut off, it was taken for
  lost and its apps' standbys started beside its own copies. **The exception is a control host the
  install's runtime places no apps on** (`placesApps`): on Kubernetes or Incus apps run on that
  runtime's own machines, never through this host's Docker, and such installs take no app-role join
  (`hosts.CheckAppRoleJoin`); a host with the control role alone runs none under Docker either; and
  with no runtime nothing runs anywhere. Such a host has nothing of its own to fence, so it answers
  whatever its Docker does; refusing would only hide its view of the other hosts. Docker on one host
  or many places apps on the install's own host and on any host holding the app role, so those
  refuse. The kind is the install's default runtime, known once the adapters are loaded
  (`controlPings.useRuntime`) and kept in `control-runtime.json` in `server.cluster_dir` for the
  next start. Before it is ever known (the first start, before the database), a host with the app
  role is taken as one apps run on: refusing then is contact and costs nothing but its view, while
  answering could be saying it is well with apps it cannot stop.
- **The manual guard is sustained silence** (`liveness.Witness`): this control host's `Monitor` has
  watched the host without contact for `T` (`Monitor.UnreachedFor`), a probe now gets no answer,
  and no other host that answers reports contact with it within `T` (each host's `View`, read on
  the spot, so any replica can decide, not only the leader). A host already marked lost is no
  longer watched, and re-marking it (a retry) skips the first check. The remedy says to turn the
  machine off or disconnect it, never to stop Pando while apps run.
- **The peer list never waits on the database.** `Monitor` reads it in a goroutine, one read at a
  time, waiting `PeerWait` [P: the smaller of the probe timeout and 2 s] before it probes the last
  list; a read that comes back late is taken the next round. A cut-off control host whose Postgres
  read hangs still fences after `T`.
- **Fencing is bounded.** `DockerFencer` stops up to 32 [P] containers at once, each with a 3 s [P]
  grace before Docker kills it (was 10 s, one after another), and the `Monitor` runs the fence
  beside the round, under a bound of 20 s [P] (at most `T`), one at a time. Twenty slow containers
  stop in about one grace.
- **A suspended host learns it was lost.** Its monotonic clock stops while it sleeps, so its own
  window restarts on waking and counts no silence. Each `View` now carries `silent_for_ms`: how long
  the answering host had watched the sender without contact until its ping arrived. When more than
  half the cluster (three hosts or more) answers with at least `3T`, the host fences at once, and
  keeps fencing until its apps are stopped. **[P] `CLOCK_BOOTTIME` was not adopted**: a clock that
  counts the sleep would only make the window restart visible, and the peers' answers decide this
  without comparing clocks.
- **Lost hosts leave the count.** `cluster_hosts.lost_at` now reaches `state.Host.Lost`, the peer
  list (`hosts.Peer.Lost`, `lost` in `GET /cluster/peers`) and `liveness.Peer.Lost`. A `Monitor`
  neither probes nor counts a lost host; `Watch` counts only hosts not marked lost, and weighs a
  returning host among them. Below three counted hosts nothing fails over or fences by itself.
  Hosts learn of a loss at their next peer read, and until then count one host more: the direction
  that only delays a failover.
- **Failovers run beside the leader's round.** `Service.declare` audits and marks the host, then
  promotes in a goroutine; rounds go on probing, auditing fences and welcoming hosts back.
  `Service.back` does not clear `lost_at` while a failover from that host runs in this process.
  **Not done:** a manual `MarkLost` on one replica and `back()` on the leader replica are not
  coordinated; the window is the length of one promotion.
- **An app host's peer list survives its join address going away.** `hosts.Peer.Control` marks
  control hosts; `PeerSync` asks the join token's address, then every control host in the list it
  last kept, and keeps the first answer.
- **Startup on upgrade.** `server.cluster_authority_path` empty (the default now) is `cluster-ca.pem`
  beside `server.token_key_path`, so an install that moved its token key needs no new directory.
  A cluster listener that cannot bind stops a host of a cluster but not a single host that never
  joined: it logs, runs without it, and cannot be joined until it is restarted with a free port
  (R-414).
- **Tests.** `TestR421_AMachineThatRefusesTheProbeIsNotLost`,
  `TestR421_ADeadPandoOnARunningMachineIsNotTakenForLost` (real listeners, each host's Pando stopped
  in turn), `TestR421_AnyAnswerFromTheMachineIsContact`,
  `TestR421_ACutOffHostWhosePeerListHangsStillFences`, `TestR421_ASlowFenceDoesNotStallProbing`,
  `TestR421_ManySlowContainersFenceWithinTheBound`,
  `TestR421_ASuspendedHostFencesWhenItWakesIfTheMajorityTookItForLost`,
  `TestR421_AnAnswerSaysHowLongTheSenderWentUnreached`, `TestR421_LostHostsLeaveTheClusterSize`,
  `TestR421_AFailoverDoesNotStallTheLeader`, `TestR421_MarkingAHostLostByHandNeedsSustainedSilence`,
  `TestR421_AJoinedHostAsksTheOtherControlHostsWhenItsOwnIsGone`,
  `TestR421_AControlHostAnswersPingsBeforeItReachesItsDatabase`,
  `TestR421_AnAppHostThatCannotStartItsAgentIsStillContact`,
  `TestR153_AnAppHostWhoseAgentFailsToStartStillFencesWhenCutOff`,
  `TestR153_AControlHostThatCannotReachItsRuntimeIsNeverTakenForLost`,
  `TestR414_ASingleHostStartsWithoutItsClusterListener`. The last checks the listener decision and
  the configuration default as units; no test starts the whole server with the port taken.

### As built: second review fixes

The owner's second review of #347 found four races and one console gap. What changed:

- **A promotion re-reads the app under its lock (R-151).** `standby.Service.Promote` read whether the
  app had failed before taking the app's lock and never again, so an app that failed in that gap had
  its standby started. The state is now read under the lock (`startable`, which `resume` goes
  through too), and the write-ahead is conditional on the app being live and not failed
  (`state.Standbys.SaveIfLive`), so one that fails between that read and the write is refused.
- **A certificate is renewed once (R-421).** Two renewals presenting one certificate both
  succeeded, and the second replaced the first's fingerprint, so a host that installed the first
  answer held a certificate nothing pinned. `state.Cluster.RenewHostCert` is now a compare-and-set
  on the presented fingerprint being the host's current one, and records the issued certificate's
  PEM (`cluster_hosts.next_cert_pem`, migration 000093) until the host presents it. A renewal that
  finds the presented certificate renewed already is answered with that certificate when it asks
  for the same key, and refused (`STATE_INVALID`) otherwise. The joined host keeps the key it asks
  for in `host-next.key` until the answer is written (`HostRenewal.nextKey`), so a renewal tried
  again after its answer was lost asks for the same key and gets the same certificate. Whatever the
  host ends up holding — the one it presented or the one it was answered with — is pinned.
- **No slow work under the token's lock (R-415).** A join ran readiness, the runtime's checks and
  certificate issuance inside the transaction that had locked the token's row, so one hung check
  held a connection and kept every other join on that token waiting. They now run before it,
  bounded by `hosts.Service.JoinCheckTimeout` [P: 30 s, `DefaultJoinCheckTimeout`]; the transaction
  only spends the use and inserts the row. The token's uses are checked when spent, not before.
- **A join is pending until the joining host confirms it (R-415).** The cluster recorded the host
  before the joining side wrote its files, so a failed write left a listed host, a spent token and a
  name that could not rejoin. The row is now inserted with `confirmed_at` empty (migration 000093):
  `Hosts` and `Host` leave it out, so it takes part in nothing — runtime host list, liveness, size,
  console. The joining host confirms by presenting its new certificate on `GET /cluster/peers`
  right after writing its files (`Joiner.confirm`, three tries); `Service.Peers` then sets
  `confirmed_at` and syncs the runtime. If it cannot confirm, it removes `joined.json`, so it can
  join again. The next join of the same name with the same key (third review fixes, below), and any
  pending row older than `state.PendingJoinLifetime` [P: 10 minutes], are deleted in that join's
  transaction (and before the token list is read), their certificates revoked and their tokens'
  uses given back. Chosen over
  a background sweep because a pending row is invisible, so it only matters to the next join and to
  the uses shown. A pending host cannot renew.
- **The setup join form no longer needs a host name (R-261).** Join is enabled without one, as the API takes
  the machine's hostname when the name is empty; the field's placeholder says so.

### As built: third review fixes

The owner's third review of #347 found four. What changed:

- **Two hosts joining under one name do not evict each other (R-415).** Cloned machines that both
  default to one hostname, such as `ubuntu`, each deleted the other's pending join, back and forth.
  A pending row is now replaced only by a join with the same key: `cluster_hosts.key_sha256`
  (migration 000095), the SHA-256 of the public key in the join's certificate request. Another
  machine's join of a name whose join is pending and within `PendingJoinLifetime` is refused
  (`STATE_INVALID`, naming the clash and `--name`, `PANDO_JOIN_NAME` or the setup screen's host
  name), and spends no use. The joining host keeps its key in `join.key` in the cluster directory,
  written before the join is sent and removed once it is confirmed, so a join tried again asks with
  the same key and replaces its own pending row.
- **A deleted app keeps no upload (R-417, R-224).** Apps are kept as rows once deleted, so
  `app_uploads`' foreign key never refused one: moving an old upload file in stored up to 256 MiB
  for a deleted app, which nothing removed and every backup carried. `state.Uploads` now locks the
  app's row (`FOR SHARE`) and refuses a deleted app (`NOT_FOUND`) before storing, so the move removes
  the file and stores nothing, and an app cannot be deleted while its upload is written. Deleting an
  app removes its upload whatever deletes it (trigger `apps_forget_upload_on_delete`, migration
  000095), which also removed what earlier versions kept; the GC's teardown still discards it too.
- **A hold check that fails is not a hold (R-148, R-151).** One that could not read the database
  skipped every app as held: drift never corrected, metrics healthy, nothing alerting, on
  single-host installs too. An install of one host now asks no holds (`Reconciler.Hosts`,
  `hosts.Service.Count`, counted once a pass; a count that fails is logged at error and holds are
  asked). A check that fails is logged at error, counted as a failed reconcile in
  `pando.reconcile.duration` (R-399), recorded on the app as a database error rather than a hold,
  and the app is left for the next pass, counting toward nothing.
- **Route sync writes only what changed (R-417).** It rewrote every route every minute on every
  replica of any cluster of two hosts, one control host included. It now runs only with more than
  one control host, on one replica per host (`state.Cluster.OnePerHost`: a transaction's
  `pg_try_advisory_xact_lock` on the host's join ID, or one name for the install's first host), and
  writes a route only when its request differs from the one that host last wrote, kept in memory per
  app, or after `RouteSyncRefresh` [P: an hour]. A replica that has not written a route yet writes it
  once.
- **Tests:** `TestR415_TwoHostsJoiningUnderOneNameDoNotEvictEachOther` (integration),
  `TestR415_AJoinThatFailsOnTheJoiningHostCanBeTriedAgain`,
  `TestR417_ADeletedAppsOldUploadIsDiscardedNotStored`, `TestR417_DeletingAnAppRemovesItsUpload`
  (integration), `TestR148_AHoldCheckErrorIsSaidAndNotTakenForAHold`,
  `TestR148_ASingleHostInstallNeverConsultsHolds` (integration),
  `TestAHostCountThatCannotBeReadStillConsultsHolds`,
  `TestR417_RouteSyncWritesNothingWhenRoutesAreCurrent`,
  `TestR417_RouteSyncRunsOnlyWithMoreThanOneControlHost`, `TestR417_ControlHostsAreCountedByRole`,
  `TestR417_OneReplicaOfAHostRunsItsPassAtATime` (integration),
  `TestR417_ReplicasOfAHostShareOneRouteSyncPass`.

The owner's third review of #347 found three faults in the standby. What changed:

- **A deleted app on a lost host is cleaned up (R-148, R-204, R-419).** The standby pass sent a
  deleted app whose home was lost to the failover's branch, so its standby, and the copy a failover
  had left on another lost host, stayed for good, holding room. A deleted app is now looked at before
  that branch (`standby.Service.clearDeleted`): a promotion under way is left until it ends or is
  taken for cut short; the standby is removed from every host that answers (`RemoveStandby` reports a
  host that does not after removing the rest) and tried again each pass until every host has; a row
  naming a `stale_host` is kept, with no standby, until that host returns. `state.Standbys.LeftOn` now
  lists deleted apps, and `RemoveStale` removes a deleted app's copy with
  `api.StaleCopyOptions{AppDeleted, KeepVolumes}`: no host is a deleted app's home, so none is refused
  as one, and the copy keeps its volumes unless the delete discarded the app's storage
  (`apps.discard_storage`, read by `state.Standbys.Fate`); then the row is forgotten. Nothing is
  started on the returning host. A deleted app's own copy on its home, lost or not, stays the
  reconciler's teardown (`GC.tearDownDeletedBundles`), which retries until the host answers.
- **A standby write covers every column (R-153).** `SaveIf` compared the standby host, home and the
  two promotion times, so a write from a reading taken before a returning host's `stale_host` was
  cleared put it back, and the copy was removed twice. `app_standbys.version` (migration 000094) is
  changed by a `BEFORE UPDATE` trigger on every write, whoever makes it (the pass, a promotion,
  `RemoveStale`, `forgetStandbyHost` on a host's removal), and `SaveIf` and `SaveIfLive` are an
  `UPDATE … WHERE version = $prev`. A row forgotten since it was read is no longer written back. A
  promotion's write-ahead carries the version it wrote (one on) to the write that ends it.
- **A big host's failover starts at once (R-421, R-422).** `AppsHomedOn` built a plan for every app on
  the lost host, one at a time, to read its priority before any standby started. The priority is now
  read from the pinned spec in `HomedOn`'s query. `liveness.HomedApp.Queue` says which apps start one
  after another: the liveness pass runs queues side by side, up to `Service.FailoverConcurrency`
  [P: 8, `DefaultFailoverConcurrency`], and the apps of one queue in order, reporting what started in
  the list's order. Under the reservation every standby fits, so each app is its own queue; with
  `allow_failover_oversubscription` (or host policy that cannot be read) the standbys on one host
  share a queue, so `failoverOrder`'s semantics hold: highest priority first, each into what the one
  before left, one that does not fit skipped for the next. Started side by side on one host,
  `docker-hosts` `PromoteStandby` still checks that host's room one promotion at a time, from the
  check through the first start (`standbyState.holdRoom`; the health wait is outside it), so two
  never both take room only one fits, as when the policy is turned off over standbys placed past
  the reservation.

### As built: several control hosts

The cluster's end-to-end test found a host joined with the control and app roles deleting its own
Pando container. Every control host reads one stored host list, and the install's machine's entry
names its Docker by its own socket (or the environment): on the joined host that is the joined host's
Docker. Its runtime took it for the install's machine, kept "that machine's agent" there by the agent's
container name, found its own Pando under that name (a joined host's container is named `pando-agent`)
and force-removed it. A control-only joined host instead tried to start the install machine's agent in
its own Docker and failed on port 7443.

- **Each process is told which machine it is (R-414, R-416).** `multidocker.Machine` is a constructor
  argument (`multidocker.NewOn`), not something the adapter reads from core (R-027):
  `cmd/pando` `runtimeMachine` passes nothing on the install's first host, which is the entry marked
  `control`, and on a joined host the name it joined with and its own runtime (`ownRuntimeEndpoint`:
  the agent's runtime socket for a host with the app role, the environment's otherwise). The stored
  list is unchanged and means the right thing everywhere (`seenFrom`): a process reaches its own entry
  directly — an `agent://` entry at its own runtime — and every other machine through that machine's
  agent; an entry whose endpoint names a local Docker (empty, `unix://`, `npipe://`) is, on every
  other machine, `agent://<agent_address>`. Entries an operator wrote over `tcp://` or `ssh://` are as
  before.
- **`control` now means the install's own machine**, whose agent the runtime keeps: still exactly
  one, and still where a volume handle recorded without a host is (R-418). The edge, trial runs and
  self-upgrade run on each control host's own machine (`Adapter.local`), and `HealthCheck` asks that
  machine's Docker, not the install's: a control host does not stop deploying because another one is
  away. A control host with no app role has no entry; its own Docker is still `local`, and its edge's
  storage is named for it (`Adapter.unlisted`).
- **The install machine's agent carries runtime calls (R-416).** Only the install's machine's own
  process keeps its agent, and gives it the machine's runtime socket: the bind mount Pando's own
  container reaches Docker through (`docker.Adapter.OwnSocket`: the mount at the `unix://` path the
  client uses), mounted into the agent alone with the socket's group (`GroupAdd`), and
  `PANDO_AGENT_RUNTIME_SOCKET` set. The Docker API is never published: the agent publishes its one
  mutual-TLS port and carries runtime calls only for a control host's certificate
  (`hostagent.IsControl`), as a joined host's does. An agent kept without the socket is made again
  with it (the socket is part of its digest); one with no socket to give — Pando not in a container,
  or reaching Docker over TCP — is made as before, so operators' `tcp://` and `ssh://` hosts' agents
  are not re-created. **[P]** The install machine's entry has no `cert_sha256`: the agent's certificate
  is issued again whenever the agent is re-created, so it is verified by the cluster authority and its
  name, `pando-agent.<name>.invalid`, which no joined host can hold (names are unique in
  `cluster_hosts`).
- **The agent keeper never removes a container it did not make.** `agentKeeper.keepCurrent` replaces,
  removes or starts the container named `pando-agent` only when it carries the keeper's labels
  (`io.pando.managed=true`, `io.pando.role=host-agent`), and otherwise refuses with what to do.
  Pando's own container never carries them. No keeper is made for an `agent://` entry, nor for a
  joined host's own entry seen from itself, whose agent is that very process.
- **A second control host converts a single-host Docker runtime (R-418 amended).** It reaches the
  install's machine through its agent, which only `docker-hosts` does, so `hosts.Runtime.Sync`
  converts when a host other than the install's machine holds the control role, not only on the first
  app host; a control host with no app role adds no entry. `CheckJoin` makes the refusals the
  conversion would make (a declared runtime, `network_pool` `off`, no advertise address) for a
  control-role join too. A conversion made on a joined control host takes the install machine's
  agent address from that machine's `cluster_hosts` row (`Runtime.Joined`), not from its own
  advertise address, so every control host writes the same list.
- **Tests.** `multidocker`: `TestR416_ASecondControlHostNeverTouchesItsOwnPandoContainer` (the bug,
  reproduced with the old reading: Pando's own container force-removed),
  `TestR414_EachControlHostTreatsOnlyItsOwnEntryAsLocal`,
  `TestR414_AControlHostWithNoAppRoleRunsItsEdgeOnItsOwnMachine`,
  `TestR416_TheAgentKeeperNeverRemovesAContainerItDidNotMake`,
  `TestR416_TheInstallMachinesAgentCarriesRuntimeCalls`; `hosts`:
  `TestR416_ASecondControlHostTurnsDockerIntoDockerHosts`,
  `TestR416_AConversionOnAJoinedControlHostNamesTheInstallsMachine`; `cmd/pando`:
  `TestR416_EachControlHostTellsTheRuntimeWhichMachineItIs`.
- **Not done here.** The install machine's agent is not fenced: a runtime call from another control
  host to start a container there is not refused while the install's machine is fencing, as a joined
  host's agent refuses it (R-421). The machine's own replicas reconcile nothing while it fences, and
  the reconciler leaves apps on a fenced host alone (`Holds.HostHeld`), which is what stands in.

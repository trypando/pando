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
3. Core signs the host's certificate from the cluster authority, records the host, and answers with
   what the role needs:
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

- Every host checks every other on the agent port.
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
- **Not done here.** The `docker-hosts` runtime still has exactly one control host: a second control
  host's replica would read the first one's `unix://` endpoint as its own machine's. Renewing a host's
  certificate must update `cert_sha256`, which the next `Sync` then pins.

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
  three times the app's disk limit [P], against the host's configured `total_disk_bytes`. The adapter
  does not know what running apps hold on a host's disk, so copies are counted against the whole
  total; a host that reports no disk total is not checked, as the planner does not check it.
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
- **The edge's routes** are written on every replica by `reconciler.RouteSync` once a minute [P],
  from every live app's pinned spec, while the cluster has more than one host. The reconciler writes
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
  stalled process) restarts the window rather than counting as silence.
- **`Ledger`**, the leader's. A host is lost when the cluster has three or more hosts and more than
  half of them report having watched it without contact for `3T`, in a report the leader heard in
  the last three intervals. A host's report never claims longer than it has watched without a break,
  nor longer than it has known the peer.
- **`Monitor`** runs on every joined host, in its agent (`host-agent serve`, given `PANDO_HOST_ID`,
  `PANDO_CLUSTER_PEERS_FILE` and a host certificate). It needs no database: a peer list it cannot
  read keeps the last. Its fence stops every running container labeled for an app through the
  host's own runtime (`hostagent.DockerFencer`), every round while cut off; it never starts one.
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
  a standby, skipping a `failed` app (R-151). The manual path, `POST /cluster/hosts/{hostID}/lost`
  (`install.cluster.manage`), works at any cluster size and **[P] is refused while the host answers
  on its agent port**, since a standby started beside running apps would be a second copy (R-153).
  Audited: `cluster.host.lost`, and `cluster.failover` per app before its standby starts.

Timings **[P]**: `T` 30 s, probes every `T/6` (5 s), each within half an interval.

The ping is `PANDO-AGENT/1 ping <host-id>` on the agent port, answered `PONG <view>`. Hosts probe
with a certificate whose subject is `pando-host`, issued from the cluster authority; an agent
answers a ping from it and refuses anything else, so a host's certificate opens no connection to
another host's apps (R-023).

Left to other slices: `Homes` and `Failover` (the standby slice; until set, a lost host is marked
and nothing is started), clearing `lost_at` and removing the returned host's copies (R-419,
`Cluster.ClearHostLost`). The join now gives a host its ID, peer file and host certificate, and
the install's first host is recorded; see "finishing the join" for why it is not yet watched.

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
- **Promote** tries the newest copy, then older ones (up to three), each given
  `DefaultPromoteHealthTimeout` [P: 2 minutes] to be healthy on two looks in a row. When none is, the
  oldest tried is left running and reported `Healthy: false`. A standby with no copy of a volume is
  refused, not started empty. The promoted host gets a home mark (a labeled volume), so a lost host that
  returns still holding the bundle loses to it in placement, in a restarted process too.
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
  `GET /cluster/peers` (finishing the join), and nothing else; it is not under `/api/v1` and not in
  the API reference. A join token's address is this
  listener's, `https://<host>:<port>`: `https` only, and the default port is added when the minter
  gives none. Minted without one, it is the external URL's host (else the advertise URL's, else the
  machine's) with `cluster_addr`'s port.
- **Pinning.** The joining host connects trusting only a certificate authority in the server's chain
  whose public key hashes to the token's hash, and checks the server certificate chains to it. The
  server's name is not checked: the pinned authority is the trust, as in kubeadm. The joining host is
  authenticated by the token's secret; it has no certificate until the join issues one.
- **One transaction.** `state.Cluster.Join` checks and spends the token's use, asks
  `hosts.Service.Readiness` (a `hosts.JoinReadiness`; the server sets `hosts.Readiness`) and then
  `Runtime.CheckJoin`, issues the certificate and records the host together, and `OnChange` runs after.
  A refusal after the token checked out (a name taken, a cluster not ready) gives the use back. The
  host gets the token's roles, never ones it asked for.
- **Refused before the token is touched:** a version other than the server's (R-415 [P]), and a name
  that is not a DNS label. The certificate is `hostagent.Authorities.IssueHost`'s, for the key in the
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
  `cluster.join.denied`; the joining Pando writes `cluster.join` and `cluster.join.denied` in its own
  log, which a control host leaves behind for the cluster's when it restarts.
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
  `app_standbys.home_host` keeps it, and a promotion sets it to the standby's host. It is how the
  leader knows which apps a lost host held, in the runtime's host names, which are `cluster_hosts`
  names (`hosts.Runtime.Sync`). Asking the runtime instead would fail exactly when it matters: a new
  leader cannot see a lost host's bundles. An app with no standby row is not listed for a lost host;
  one whose placement never succeeded has no home recorded. A promotion whose runtime did not name
  the former host takes the recorded home as the stale one.
- **Fencing calls standby (R-421).** `standby.Failover` implements `liveness.Homes`, `Failover` and
  `Returns` over `*standby.Service`, `state.Standbys` and `state.Cluster`; `cmd/pando` sets all three on
  the host-loss service. `AppsHomedOn` lists a host's apps highest `availability.priority` first, then
  by app ID, the order `failoverOrder` uses, and the liveness pass promotes them one after another.
  `multidocker.PromoteStandby` refuses (`CAPACITY_WOULD_OVERSUBSCRIBE`) a standby that does not fit
  its host's free CPU and memory now, counting what the bundle already runs there, before touching
  anything, so past the reservation the next app in order still starts. A failed app is skipped
  (R-151).
- **A returning host (R-419).** Each round the leader reads which hosts are lost (at any cluster size,
  so a host marked lost by hand in a cluster of two comes back too). A lost host that answered the
  probe, and that the majority no longer takes for lost, is back: `standby.Service.RemoveStale` runs
  for each app whose `stale_host` it is, then `Cluster.ClearHostLost`, then `cluster.host.returned` is
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
- **What an app host is given** besides its certificate: a `pando-host` client certificate and its
  key (`host-client.crt`, `host-client.key`), issued by the control side at join because only a
  holder of the authority's key can issue one — it opens nothing but pings (R-421) — and the
  runtime's `network_pool`, kept in `joined.json` as `network_pool`. Every join gets the client
  certificate; the pool is sent for the app role.
- **`runAppHost`** runs the agent in the Pando process, as `host-agent serve` does
  (`serveAgent`): `host.crt`/`host.key` (named `hostagent.ServerName(<name>)` by `IssueHost`),
  clients checked against `ca.crt`, the network pool from `PANDO_AGENT_NETWORK_POOL`, else
  `joined.json`, else 10.213.0.0/16, and the runtime socket from `PANDO_AGENT_RUNTIME_SOCKET`, else
  `DOCKER_HOST` when it is `unix://`, else `$XDG_RUNTIME_DIR/docker.sock` (rootless, R-402, R-416);
  none of those found stops it with the variable to set. It listens on `PANDO_AGENT_LISTEN`, else
  `:7443`. Its `liveness.Monitor` is its host ID from `joined.json`, probing with the `pando-host`
  certificate and fencing through the same socket.
- **The peer list** is `GET /cluster/peers` on the cluster listener, which now asks for a client
  certificate and checks one when given (`VerifyClientCertIfGiven`: a joining host has none). The
  certificate must be one a host joined with — its fingerprint is a row's `cert_sha256` — so a
  removed host hears nothing (`hosts.Service.Peers`, `AUTH_INVALID` otherwise). It answers
  `{"hosts":[{id,name,address}]}`. `hosts.PeerSync` fetches it from the join token's address every
  round (2 s timeout), writes `peers.json` in the cluster directory, and when the listener cannot be
  reached returns what that file last held. The joined host connects trusting the authority in
  `ca.crt`, by its key, not the listener's name, as at join.
- **The first host (R-414).** At each start a control host that has not joined calls
  `hosts.RecordInstallHost`: `state.Cluster.RecordFirstHost` inserts it under an advisory lock unless
  a row with no join token exists, so replicas of one install record one host between them and a
  restart records none. Name: the first label of the machine's hostname as a DNS label, else
  `control`; roles control and app; address the advertise URL's host, else the external URL's, else
  the machine's, with port 7443; `cert_sha256` 64 zeros [P], since nothing pins it (below). A failure
  is logged and startup goes on. **A host with no join token is the install's own** (`hosts.InstallHost`);
  join tokens are never deleted, so `joined_with` stays set on every joined host.
- **Counting.** The "+1" is gone: `hosts.Service.Count` is the rows, and never less than one, so a
  single-host install reports one host before and after its row is made; `DefaultRoles` reads
  `Count`.
- **What the first host is left out of, and why.** `RuntimeHosts` skips it, since it is the
  runtime's `control` entry already, so a conversion is unchanged. `hosts.LivenessPeers` (the
  leader's `Watch` and the peer list) skips it too: its agent is the one the runtime starts, which
  takes no part in liveness and refuses pings, and a host that never answers would be taken for lost
  by the joined hosts once the cluster has three. So the first host is counted in the cluster's
  size and not watched, as before.
- **Not done here.** Joined *control* hosts run no agent and no `Monitor` either, yet are watched:
  with app-only hosts more than half the cluster, they would report a control host unreached. A
  control host's agent taking part in liveness (and the first host's with it) is left to the
  liveness slice. An app host's agent has to be on the host's app networks to reach its containers;
  the per-host Docker adapter connects the container named `pando-agent`, so on an app host Pando's
  own container must be named `pando-agent` (install files not changed here). `peers.json` is
  rewritten every round even when unchanged.

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

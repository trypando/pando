# Incus runtime

The first runtime adapter whose isolation class is `vm` (R-115). Each app runs in its own Incus virtual
machine, so the boundary between two apps is a hypervisor rather than a shared kernel. R-256 names
Incus as the multi-machine model: Incus places the VMs across its cluster, and Pando does no placement
of its own (R-010).

This note designs the adapter, records what it deliberately does not do, and proposes how an install
that runs its apps on Incus is laid out (design 00 §1.1's open question). It depends on
`notes-multi-host-docker-issue-72.md`, whose forwarding agent it reuses, and on
`notes-image-registry-issue-72.md`, for getting built images to the VMs.

## Shape

```
      ┌──── Pando's host (the Compose install) ───────┐
      │ pando, postgres, registry, buildkit, the edge │
      └──────┬───────────────────────┬────────────────┘
   Incus API │ (unix socket, or      │ mTLS to the agent in the app's VM (port 7443)
             │  HTTPS + client cert) │
      ┌──────┴───────────────────────┴───────────────┐
      │ Incus (one server, or a cluster)              │
      │  ┌── VM pando-app-01hq8… ──┐  ┌── VM … ──┐    │
      │  │ podman                  │  │          │    │
      │  │ pando-agent  (7443)     │  │          │    │
      │  │ app workloads, services │  │          │    │
      │  └─────────────────────────┘  └──────────┘    │
      └───────────────────────────────────────────────┘
```

- **One VM per app [P].** R-116 asks for "an isolated environment per app", and an app's bundle is
  already the unit the runtime interface deals in (`BundlePlan`). Every workload of the app, and every
  service provisioned for it (R-134), runs in that VM. Two apps never share a kernel; two workloads of
  one app do, as they would on any runtime.
- **Inside the VM, Podman runs the app's OCI images**, and the adapter drives it with the existing
  Docker runtime adapter's code through Podman's Docker-compatible API. Incus can run OCI images itself,
  but only as containers, not inside a VM. Podman is what Debian ships and the Docker adapter is
  already tested against it (the `podman` CI job), so every per-workload behavior — networks, the
  egress gateway, health checks, carried files, log caps, exec, logs, usage, the trial's port and write
  observation — is code that exists and is tested, rather than a second implementation of each.
- **The Docker API is reached through the Incus API**, not over the network: each connection is an
  exec, through the Incus agent every VM runs, of `socat` bridging stdio to Podman's socket. The Incus
  API's own authentication is the only credential, and nothing in the VM listens for it. HTTP
  keep-alive means one exec per burst of calls, not one per call.
- **The adapter owns the VM; the Docker adapter owns what is in it.** Incus-shaped vocabulary
  (instances, profiles, devices, ACLs, storage pools) stays in this package (R-251).

## As built

What a real two-member Incus 6.21 cluster changed from the design below.

| What | As built | Why |
|---|---|---|
| Where the ACL goes | On each VM's NIC when the VMs are in the network's project; **on the network** when they are not, and then the network must be Pando's alone | Incus 6.21 resolves a bridge NIC's ACLs from the instance's project when it applies them (`device/nic_bridged.go`), though it validates them from the network's, so a VM in Pando's own project with a bridge in `default` fails to start with "Network ACL not found". The network-level ACL is resolved correctly. `HealthCheck` refuses a network anything else uses, since the ACL would close it to that too |
| Certificate scope | Pando's project **and** the network's (`default` for a bridge) | A bridge network's ACLs are kept in the network's project. A certificate restricted to Pando's project alone is refused at the ACL, with a message saying so |
| Docker API latency | About 45 ms a call over the exec bridge, one exec per burst | The websocket, the VM agent and socat, measured. Proxied requests do not take this path: they go to the agent over the network |
| VM to VM on a bridge | Every NIC on a bridge has `security.port_isolation` | Incus applies a bridge network's ACLs only between the bridge and the host, never between two instances on it (Incus docs, "Bridge limitations"). The first integration run proved it: one app's VM reached another's agent port. Port isolation closes that at the bridge; traffic routed through the host crosses the ACL |
| Guest image root disk, volumes | Sized: 8 GiB for the image's builder, `volume_size_bytes` (10 GiB) for each volume | A project that limits disk refuses a root device or a custom volume without a size |

## Reaching an app (R-023, R-025, R-026)

The proxy reaches a workload exactly as on a multi-host Docker install (O-45, design 06 §4): each VM
runs Pando's forwarding agent, `pando host-agent serve`, joined to the app's Podman networks and
publishing one port. The proxy, having decided the request, dials the agent with Pando's client
certificate and names the workload and port. `Upstream` returns that `Dial`. Nothing about the proxy,
the routing adapters, or the agent changes.

Three things keep every other path closed:

1. **The VM gets no profile's NIC.** Incus's default profile commonly puts an instance on the LAN
   (a macvlan or bridged NIC — on the cluster this was designed against, it does). The adapter creates
   VMs with no profiles unless configured, and adds exactly one NIC, on the network named in its
   settings.
2. **An Incus network ACL on that NIC** admits only the agent's port, only from the addresses Pando's
   proxy connects from (`proxy_sources`), and drops every other inbound connection. Another app's VM,
   the LAN, and the host are all refused at the NIC (R-025, R-026). The ACL is the adapter's, named
   `pando-ingress` (`pando-ingress-<project>` outside `default`), and rewritten when the settings
   change. Where Incus cannot apply it to a NIC, it goes on the network instead (As built).
3. **The agent forwards only into the app's own networks.** It refuses any address outside the app
   network range and any of its own interfaces (`hostagent.Permitted`), so a holder of the client
   certificate still cannot use it to reach the VM's host side.

### Which network [P]

The operator names an Incus network that Pando's host can route to. On one Incus server, the managed
bridge (`incusbr0`) is enough when Pando runs on that server or on a VM attached to the same bridge. On
a cluster, a bridge exists separately on each member, so a VM on another member is unreachable. The
adapter therefore refuses, at `HealthCheck`, a bridge network on a cluster of more than one member
unless `target` pins its VMs to one member or one cluster group, and its remedy names the two ways out:
an OVN network, which spans members, or pinning. Routing between per-member bridges is an operator's
network design, not something Pando configures.

Tunneling the proxy's connections through the Incus API (an exec per connection) would reach any member
with no network at all. It is not the default because it puts the Incus API on the path of every
request to every app; it can be added behind the same `Upstream.Dial` later without any other change.

## The VM

| Concern | What the adapter does |
|---|---|
| Name | `pando-<bundle ID>`, lower-cased. The `user.pando.bundle` config key is what the adapter matches on, never the name. |
| Image | A guest image the adapter prepares once: `images:debian/13/cloud` plus Podman and socat, published as `pando/guest/<recipe digest>` in the adapter's project. Preparing it is a VM launched, provisioned by cloud-init, stopped and published; it happens on the first `Apply` and is reported in the deploy log. An operator may name their own image instead (`guest_image`). |
| CPU and memory | The sum of the workloads' limits (`ResourcePlan`), rounded up to whole CPUs, plus `guest_overhead_bytes` (512 MiB) for the guest kernel, Podman and the agent. An app with no limits gets `default_cpus` and `default_memory_bytes`. |
| Disk | The root disk on `storage_pool`, sized `root_size_bytes` (20 GiB) [P]: images and the workloads' writable layers live there. |
| Placement | Incus's. `target` is passed through when set. |
| Secure boot | Off: the guest image is Debian's cloud image, and Pando adds no signed components. |
| Agent and egress gateway | Pando's own image, `agent_image`, pulled by the VM. Required: the VM has no other way to obtain Pando's binary. |

**Images reach the VM from a registry only [P]**, as on the multi-host Docker and Kubernetes runtimes.
`ImageDelivery` is `registry`; `ImportImage` refuses with a remedy naming the image registry adapter.
A published image needs nothing, and a built one needs the install to have an image registry. Import
would need the tarball stored somewhere every cluster member can read, and garbage-collected by an
adapter that does not know which deployments still need it.

**Changing an app's size** sets the new `limits.cpu` and `limits.memory` on the VM. Incus applies a CPU
change and a memory increase to a running VM; a memory decrease is applied by restarting the VM, which
the adapter does only inside `Apply`, which the reconciler calls only for a plan that changed.

## Volumes (R-200 – R-206)

**Each Pando volume is its own Incus custom filesystem volume** on `storage_pool`, named
`pando-<volume ID>` and labeled with its bundle. `Apply` attaches each planned volume to the VM as a
disk device at `/srv/pando/volumes/<volume ID>`, and the Docker adapter inside binds the workload's
mount to that directory instead of creating a Podman volume (its `volume_root` setting, added for this).

- **Volumes outlive the VM (R-204).** Destroying an app deletes its VM; the custom volumes stay unless
  `KeepVolumes` is false, and attach to the next VM for the same bundle.
- **Snapshot and restore (R-212)** read and write the custom volume over Incus's SFTP endpoint for
  custom volumes, as a tar archive with relative paths — the same format the Docker adapter produces,
  so a backup taken on one runtime restores on the other. The VM need not be running.
- **Pando does not read the contents (R-206)**; the archive is streamed.
- **Moving an app to Incus** is O-8's destructive spec change: keep the volumes, redeploy, reattach.
  Volumes do not move between runtimes; a backup restored onto the new runtime is how data travels.

## Egress (R-180 – R-187)

Enforced inside the VM, by the Docker adapter's existing mechanism: a restricted app's workloads are on
an internal Podman network with no route out, and the only thing that can leave is the app's egress
gateway, which applies the rules (R-187). The adapter reports `SupportsEgressRestriction` exactly when
the Docker adapter in a VM would — it has `agent_image` to run the gateway from — so the honesty of R-186
is the Docker adapter's, unchanged. An unrestricted app gets nothing in its path.

The VM itself can reach out (it pulls images), and that does not widen what an app can reach: a
restricted app's workloads have no route to the VM's network.

## Restarts and "failed stays failed" (R-148 – R-151)

Unchanged from Docker: Podman restarts a crashed workload per its restart policy, and `Observe` reports
restarts and exit codes as the Docker adapter reads them. The VM is started with `boot.autostart` on,
so a host reboot brings it back, and the workloads inside restart with it. The adapter never starts a
VM outside `Apply`: `Observe` on a stopped VM reports the app's workloads present and not running, read
from the VM's `user.pando.workloads` key, and does not start it (R-148).

## Capacity (R-240 – R-245)

Reported by the adapter from Incus's own resources API, never from the host (R-243):

- **Totals** are the sum of every cluster member's CPU threads and memory (or the target member's or
  group's, when `target` is set). `total_cpu_millis` and `total_memory_bytes` cap them, for an operator
  who gives Pando part of a machine that runs other things.
- **Committed** is every running instance's `limits.cpu` and `limits.memory` on those members, Pando's
  or not: a VM somebody else runs on the same cluster takes room an app cannot have.
- **`LargestFit`** is the member with the most free memory, by committed limits. A bundle that fits on
  no single member is refused at plan time (R-242), even when the cluster's total has room.
- **Usage** (R-245) is the Docker adapter's reading inside each VM; the VM's own overhead is not an
  app's usage.

## The runtime interface, method by method

| Method / capability | Incus |
|---|---|
| `IsolationClass` | `vm` |
| `Apply` | Ensure the ACL, the VM (create, resize, attach volumes, start), the agent, then the Docker adapter's `Apply` inside |
| `Observe` | VM missing → `Exists: false`. VM stopped → workloads present, not running. Running → the Docker adapter's `Observe` |
| `Stop` | Stop the VM: a stopped app holds no CPU or memory (R-242) |
| `Destroy` | Delete the VM; delete its custom volumes only without `KeepVolumes` |
| `CreateVolume` / `DestroyVolume` | Incus custom volumes; destroying one attached to a VM detaches it first |
| `SnapshotVolume` / `RestoreVolume` | SFTP, tar |
| `ImportImage` | Refused; `ImageDelivery` is registry only |
| `Logs`, `Exec`, `Usage` | The Docker adapter inside the VM |
| `Upstream` | The Docker adapter's address, dialed through the VM's agent; the VM's address is cached from `Apply` and `Observe` |
| `Trial` | A throwaway VM, `pando-trial-<trial ID>`, deleted afterwards, running the Docker adapter's `Trial` — the trial is in the app's isolation class, and port and write observation work as on Docker |
| `SupportsEdge`, `SupportsSelfUpgrade` | False: the edge and Pando run on the install's Docker host (below) |
| `SupportsBundleEvents` | False [P]: the reconciler visits each app on every pass, as before O-52. Incus's event stream reports the VM, not the workloads inside it; joining the two is a later change |
| `SupportsResourceLimits` | True: the VM's limits are applied by the hypervisor, and the workloads' by Podman inside |
| `Platform` | The cluster's architecture, refused at `HealthCheck` when members differ |

## Building inside the VM (R-116) — not in this change [P]

R-116 prefers building inside the app's own environment where a runtime provides one, and design 03 §3
asks whoever writes the first VM-class runtime to consider it. Considered; deferred, as O-59:

- **Core needs a third delivery.** An image built in the VM is already where it runs, which is neither
  `import` nor `registry`. `ImageDelivery` gains `in_place`, `planner.ChooseDelivery` and
  `deploy.build` a branch for it, and `BuildRequest` a bundle ID. That part is small.
- **The first build has no VM.** A build runs before the first `Apply`. Either the builder creates the
  app's VM — the runtime's job, leaking into the builder — or builds use a separate VM per app, which is
  the same isolation class but not "inside that environment".
- **Later builds run beside the live app** and compete with it for the CPU and memory R-242 has
  already committed to it.
- **Only Dockerfile builds are straightforward** (`podman build` in the guest). Railpack, static and
  buildpack builds produce BuildKit plans and would still go to BuildKit.
- **A builder cannot share the runtime's connection.** One adapter has one category, so the builder
  needs its own Incus address and credential.

Until then BuildKit builds as it does today, its image is pushed to the install's registry, and the VM
pulls it. Each build is isolated by BuildKit at `container` class, which `BuilderCapabilities` reports
honestly, so a build isolation floor of `vm` is refused at plan time rather than silently unmet (R-114).

## Services (R-130 – R-134)

No Incus services adapter. The existing services adapter (kind `docker`) returns ordinary OCI
workloads and volumes in the app's bundle; on this runtime they run inside the app's VM, on its private
Podman network, with their data on Incus custom volumes like any other. A second adapter claiming the
same slot types would stop Pando from starting (R-271), and would add nothing. The kind's name is
misleading on an Incus install and is left alone here.

## An install that runs apps on Incus (design 00 §1.1) [P]

Pando itself runs as the Compose install, unchanged, on any Docker host — including a VM on the Incus
cluster, which is the recommended layout: one VM for Pando, its Postgres, the registry, BuildKit and the
edge, attached to the same network the apps' VMs use, so the proxy routes to them directly. The Incus
runtime is then added as a second runtime adapter and chosen per app, or made the default for new
apps. The Docker runtime stays configured: it runs the edge (`SupportsEdge`) and Pando's own upgrades.

So an Incus install supplies Postgres the way every install does — the Compose file's `postgres`
service, or an external database — and design 00 §1.1's open question is answered by the same path.
Running Pando and Postgres as Incus instances without Docker at all would need a runtime that can run
the edge and Pando's upgrade helper, which this adapter does not; it is not proposed.

## Settings

| Setting | Meaning |
|---|---|
| `address` | `unix:///var/lib/incus/unix.socket` (default), or `https://host:8443` |
| `server_certificate` | The Incus server's certificate (PEM), for `https://`. Not secret |
| `project` | Incus project for everything the adapter creates. Default `default`; a dedicated one is recommended |
| `network` | The Incus network VMs attach to. Required |
| `proxy_sources` | Addresses (CIDRs) the proxy connects from; the only sources the ACL admits. Required |
| `storage_pool` | Pool for root disks and volumes. Default: the project's default profile's root pool |
| `target` | A cluster member, or `@group`, to pin VMs to |
| `profiles` | Profiles to apply to each VM. Default none |
| `agent_image`, `agent_port` | Pando's image, and the agent's port (7443) |
| `guest_image` | An image to use instead of preparing one |
| `total_cpu_millis`, `total_memory_bytes` | Caps on reported capacity |
| credential `tls_client` | Client certificate and key (PEM) for `https://` (R-190) |
| credential `agent_authority` | The authority the agents' certificates are issued from, generated if empty |

## Requirements this touches

R-010, R-023, R-025, R-026, R-097, R-114, R-115, R-116, R-134, R-148, R-151, R-186, R-187, R-190,
R-204, R-206, R-212, R-242, R-243, R-245, R-251, R-254, R-256, R-271.

## Tests this change is done with

Unit, against a fake Incus server and a fake Docker adapter:

- `TestR115_TheIncusRuntimeReportsVMIsolation`
- `TestR026_AVMHasOnlyTheAdaptersNICAndItsACL`
- `TestR026_TheACLAdmitsOnlyTheProxyOnTheAgentPort`
- `TestR254_ABridgeOnAClusterWithoutATargetIsRefused`
- `TestR254_ConfigurationThatCannotWorkIsRefused`
- `TestR190_TheClientCertificateIsACredential`
- `TestR204_DestroyKeepsTheAppsVolumes`
- `TestR242_LargestFitIsTheRoomiestMember`
- `TestR243_CapacityComesFromIncus`
- `TestR148_ObserveNeverStartsAStoppedVM`
- `TestR023_UpstreamDialsTheAgentInTheAppsVM`

Against a real Incus server (`make test-incus`, tag `incus`):

- `TestR023_AnImageAppRunsInItsOwnVM` — deploy, reach it through the proxy, inspect the VM
- `TestR025_OneAppsVMCannotReachAnothers`
- `TestR134_APostgresSlotRunsInsideTheAppsVM`
- `TestR212_AVolumeRestoresAfterItsVMIsGone`
- `TestR097_ATrialRunsInAThrowawayVM`

## Decisions

| ID | Question | Proposed |
|---|---|---|
| O-59 | Building inside the app's VM (R-116) | Deferred, above. BuildKit and the registry until the first-build and capacity questions are answered |

One VM per app rather than per workload is `[P]`, not open: it is the isolation R-116 names, and a VM
per workload would put a hypervisor between an app and its own database.

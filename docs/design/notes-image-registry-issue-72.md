# Built images go to a registry (issue #72, PR 5)

The fifth PR of the stack in `notes-multiple-replicas-issue-72.md`. Neither multi-machine runtime
(PR 6, Kubernetes; PR 7, Docker on several hosts) can take a built image the way single-host Docker
does: `ImportImage` loads a tarball into the one daemon that will run it, and a cluster has no such
daemon. The issue's author decided **[D]** that built images go to a registry, that Pando runs one by
default, and that setup may point at the organization's own instead. This note is how.

PR 5 implemented it; [What was built](#what-was-built-pr-5) says how, and what was left for later.
The rest of the note is the design as it was decided.

## What was built (PR 5)

No migration: nothing new is stored. The registry is startup configuration, and what a deployment
ran is already recorded in `deployments.image_ref` and `workload_images`.

| Item | What | Where | Test |
|---|---|---|---|
| The refused-build gap (R-146) | BuildKit tags each streamed build `pando/<app>:<deployment-id>` (`BuildRequest.Tag`). Docker's `ImportImage` returns the loaded image's ID (`sha256:…`), which the deployment records and the reconciler restores. Docker prunes an app's build tags past the newest eleven on each import, never `:latest` (an older deployment may name it) and never an image a container uses | `buildkit.imageName`, `docker.ImportImage`, `docker.pruneBuildTags` | `TestR146_ARefusedBuildIsNotWhatTheReconcilerRestores` (Postgres, fake runtime), `TestR146_AnImportedBuildIsNamedByItsImageID`, `TestR146_EachBuildIsTaggedByItsDeployment`, `TestR224_OldBuildTagsArePrunedAndLatestIsKept` |
| Capabilities (R-254) | `RuntimeCapabilities.ImageDelivery` replaces `SupportsImageImport`; `BuilderCapabilities.SupportsPush`. `planner.ChooseDelivery` decides at plan step 5a and again before the build | `adapter/api`, `core/planner/delivery.go`, `deploy.build` | `TestR254_HowABuildReachesTheRuntimeIsDecidedFromData`, `TestR254_NoWayToDeliverABuildIsAPlanTimeRefusal`, `TestR254_ARuntimeThatPullsWithNoRegistryIsRefusedBeforeBuilding` |
| Configuration | `PANDO_REGISTRY_URL`, `_USERNAME`, `_PASSWORD` or `_PASSWORD_FILE`, `_KIND`, `_LAYOUT`, `_INSECURE`, and `_ALWAYS` (below). An `http://` URL without `_INSECURE` is refused at startup | `core/imageregistry`, `config.Registry` | `TestR194_PlainHTTPOnlyWhenTheOperatorSaysSo`, `TestR194_TheRegistryPasswordIsReadAndNeverReported` |
| Push and pin (R-120) | BuildKit's `image` exporter with `push=true`; the credential reaches buildkitd through the session's auth provider, only for the target registry's host. The result is `repository@digest`; a push with no digest fails | `buildkit/push.go`, `deploy.build` | `TestR120_APushIsPinnedByTheDigestTheRegistryReported`, `TestR120_ABuiltImageIsPinnedByDigest` |
| Pull by digest | The deploy sets `WorkloadPlan.PullAuth` for a workload running a pushed build; the port check's trial, the scanner (`ScanRequest.PullAuth`, which the Trivy adapter uses to fetch the image) and the reconciler's corrective apply get the same | `deploy.withBuiltImageAuth`, `Reconciler.BuiltImageAuth` | `TestR194_TheRegistryCredentialNeverReachesALogOrAWorkload`, `TestR194_ThePushCredentialGoesOnlyToItsRegistry` |
| Deleted apps' manifests (R-224) | Teardown lists the app's repositories (the app-wide one and each separately built workload any revision named) and deletes every manifest by digest. A registry that cannot be reached leaves the app for the next pass | `imageregistry.DeleteApp`, `GC.RegistryImages` | `TestR224_ADeletedAppsImagesAreRemovedFromTheRegistry`, `TestR224_TeardownDeletesADeletedAppsRegistryImages` |
| Topology | `docker-compose.registry.yml`, an overlay with a `registry:3` service (`storage.delete.enabled`, `htpasswd`, the operator's certificate), the build service's trust of its CA, and a `registry-gc` service under the `maintenance` profile for the weekly read-only blob collection, with the schedule in the file's header (O-38). Not in the default `docker-compose.yml` (O-34) | repository root | — |
| Uploads in the DR bundle (O-37) | Every `uploads/<app>.tar.gz`, restored 0600 | `core/backup` | `TestR212_UploadsAreInTheDRBundle` |

**`PANDO_REGISTRY_ALWAYS` [P].** Not in the decisions above: it sends every build through the
registry even on a runtime that imports. It is how the push path can run on one host before PRs 6
and 7 add a runtime that only pulls, and it gives an install that wants one delivery path one.

**Left for later**, each a [P] in the design below rather than a decision:

- Collecting a live app's old manifests (revisions R-152 no longer keeps). Only deleted apps'
  manifests are deleted; a live app's pushed builds accumulate until it is deleted. Single-host
  Docker's per-build tags are pruned, so that tier does not have this gap.
- Rollback reusing a recorded image instead of rebuilding.
- After a restore, turning an image that cannot be pulled into a redeploy of the pinned revision.
- `GET /capacity` counting the images Pando holds in the registry.
- The replicas-topology run with the registry service, and the BuildKit push against a real registry:
  neither was run for this PR (it needs the Docker daemon). The overlay's Distribution environment
  overrides (`REGISTRY_STORAGE_MAINTENANCE_READONLY`, the TLS paths) are as documented by Distribution
  and unverified here.

## What happened before PR 5

- BuildKit exports the image as a Docker tarball (`ExporterDocker`) into `BuildRequest.ImageSink`.
  Core pipes it into `RuntimeAdapter.ImportImage`, which `docker load`s it and returns the
  reference the daemon recorded (`internal/core/deploy/deploy.go`, `Runner.build`).
- That reference is `pando/<namespace>:latest` (`buildkit.imageName`), and it is what the deployment
  records as `image_ref` and the reconciler re-applies (`reconciler.PlanShape(s, app.ImageRef, …)`).
- A runtime without `SupportsImageImport` is refused at deploy time with
  `PLAN_CAPABILITY_UNSUPPORTED`: *"%q cannot take an image built here."*
- Image apps (issue #41) are already pinned by digest: the runtime is given `image@digest`, and
  `WorkloadPlan.PullAuth` carries a resolved credential for the one pull.

**A gap found while reading this, independent of the registry.** A built image is pinned by a tag that
the next build moves. A build that succeeds and is then refused — by the security scan (R-312, step 10)
or the port check (step 10a) — has already been loaded as `pando/<app>:latest`. The running container
keeps its old image, so nothing changes immediately. But if the reconciler later recreates that
workload (a container removed by hand, R-148), it creates it from `pando/<app>:latest`, which is now
the refused image. R-146 says a failed build replaces nothing, and the scan's refusal is meant to have
the same contract. Pinning built images by digest closes this on every runtime, including single-host
Docker (below). It wants a test named for R-146 before the fix, so the fix is shown to be needed.
It is recorded as a known issue in `notes-multiple-replicas-issue-72.md`, fixed by this PR.

## Decisions

### Which registry [P]

**CNCF Distribution v3 (`registry:3`).** It is the reference implementation of the OCI distribution
API, one static binary, and has the storage drivers a cluster needs (filesystem, S3, GCS, Azure). Harbor
and Zot were considered: Harbor is a product with its own database and UI, which is a second control
plane beside Pando's; Zot is a reasonable alternative with online garbage collection, and would be
the choice if the offline GC below proves to be a problem. Neither is needed for what Pando asks of a
registry: push, pull by digest, list tags, delete a manifest.

### Who starts it [P]: the install topology, as it starts Postgres

Two ways were weighed.

**Pando starts it through a runtime adapter**, the way it starts the edge (R-174, design 03 §4.4).
For it: R-002 — one less service in the topology for the operator to know about — and R-174's
precedent that a component Pando needs is Pando's to run. Against it, and decisive:

- **Bootstrap inversion, the same one design 00 §1.1 rejected for Postgres.** On Kubernetes and on
  several Docker hosts, the registry is where every node's container runtime pulls from. Starting it
  through the runtime adapter means the runtime adapter's health decides whether the image store
  exists, and a runtime outage takes out the thing recovery from that outage pulls from.
- **Node trust is not something a runtime adapter can configure.** A kubelet or a remote Docker daemon
  pulls over TLS with a certificate it trusts, or from a registry its own configuration marks as
  insecure (below). Both are host or node configuration, set once at setup. A registry Pando starts
  on demand still needs that configuration done beforehand by someone with access to every node, so
  the setup cost is paid either way.
- **A registry is state.** It holds the only copy of every built image, which rollback and the
  reconciler read. Postgres is supplied by the topology for the same reason: Pando should connect to
  its state, not create it.

R-174 does not decide this: it is about a component *in front of* Pando that terminates `:80` and
`:443`, and its stated exception is the state store. The registry is closer to the state store than to
the edge. No requirement change is needed; if the owner reads R-174 more broadly, the note proposes
adding the registry to its exception beside the state store.

So the multi-machine topologies ship a `registry` service beside `pando` and `postgres`: a Compose
service on the control host for multi-host Docker (PR 7), a Deployment with a PersistentVolumeClaim (or
object storage) in Pando's namespace for Kubernetes (PR 6). Pando connects to it as it connects to
the database. Configuration names it:

| Setting | Meaning |
|---|---|
| `PANDO_REGISTRY_URL` | `https://registry.internal:5000`, or the organization's registry and a path prefix (`123456789012.dkr.ecr.us-east-1.amazonaws.com/pando`) |
| `PANDO_REGISTRY_USERNAME`, `PANDO_REGISTRY_PASSWORD` (or `_FILE`) | The one credential Pando pushes and pulls with |
| `PANDO_REGISTRY_KIND` | `basic` (default) or `ecr`, the same kinds `core/oci` already mints for image apps |
| `PANDO_REGISTRY_LAYOUT` | `per_app` (default) or `single` (below) |
| `PANDO_REGISTRY_INSECURE` | `true` permits plain HTTP. Off by default |

**Startup configuration only [P], like the database URL.** Settable in the console would mean storing
the credential, which then belongs in encrypted storage (R-190) and needs its own restart semantics.
Nothing needs that yet. The credential is a `secret.Value` from the moment it is read (R-194).

**Single-host Docker does not need a registry and does not get one [D] (O-34).** `ImportImage`
stays, for the reason `SupportsImageImport`'s comment gives: a registry on one host needs daemon
configuration and charges the setup cost R-002 says is paid once. Two changes apply there anyway, so the
gap above closes on the single-VM tier too:

- The builder tags each build `pando/<namespace>:<deployment-id>` rather than `:latest`.
- `ImportImage` returns the loaded image's ID (`sha256:…`, content-addressed), and core records and
  re-applies that, so a later build cannot change what a recorded deployment means.

The runtime's existing teardown (`ImageLabelBundle`) already removes a deleted app's images, so the
extra tags do not accumulate past the app. Old tags of a live app are pruned with the same retention as
the registry (below).

### Repository layout [P]

`<prefix>/apps/<app_id>/<workload>`, tagged with the deployment ID, pulled by digest. One repository
per workload so a compose app's services are separable, and per app so a deleted app's images are one
listing to delete. The tag is for people reading the registry; nothing Pando runs refers to it.

`single` layout puts everything in `<prefix>` with tags `<app_id>-<workload>-<deployment-id>`, for
registries where a repository must exist before a push (ECR, unless the account creates repositories
on push). It loses nothing Pando uses — Pando never reads a repository's permissions — and costs a
longer tag listing at GC.

### How a build is pushed and pinned (R-120)

1. Core decides how the image reaches the runtime, from data (R-254): the runtime's
   `ImageDelivery` (below) and whether an install registry is configured. Neither available is a
   plan-time refusal naming the setting, not a failure after the build.
2. For registry delivery, `BuildRequest.Push` names the repository and tag and carries the push
   credential. BuildKit uses its `image` exporter with `push=true`. The credential reaches buildkitd
   through the client session's auth provider for the duration of the build, so it is never written to
   buildkitd's configuration or disk.
3. `BuildResult.Digest` is the manifest digest from the exporter's response
   (`containerimage.digest`). The field exists in `api.BuildResult` today and nothing fills it.
4. Core records `<repository>@<digest>` as the deployment's `image_ref` (and in `workload_images` for
   compose apps). Everything downstream — the scan, the port check, `Apply`, the reconciler — uses that
   string. A later push to the same tag changes nothing that runs.
5. `WorkloadPlan.PullAuth` is set to the install registry's pull credential for workloads running a
   built image, exactly as it is set to an app's registry credential for an image app today
   (design 03 §2.1). The Docker runtimes send it as `X-Registry-Auth` and keep nothing. Kubernetes
   has no per-pull credential and needs a Secret (PR 6 note, O-41).
6. `ObservedWorkload.ImageDigest` is the manifest digest the runtime resolved, compared against the
   recorded one for drift (design 05 §2.1: "wrong image digest → recreate").

**Rollback reuses the image.** Today every source deploy builds, including a rollback (R-152) to a
revision that already ran. With images kept by digest for the revisions R-152 retains, a rollback to a
revision whose deployment succeeded can run the recorded image without building. This is a [P] for
the deploy path, not a requirement change: R-152 says revisions are kept for rollback and says nothing
about rebuilding. A rollback whose image has been removed rebuilds, as now.

**The scanner reads the registry.** The image-scanner adapter scans images on the local daemon. With
registry delivery there is no local copy, so the scanner is given the digest reference and the pull
credential, as the runtime is. Trivy and Grype both scan a remote reference.

### Authentication: only Pando, the builder and runtimes [P]

Apps never hold a registry credential and never reach the registry:

- On Kubernetes, the registry's NetworkPolicy admits Pando's pods and BuildKit, and nothing from an app
  namespace. Kubelet pulls from the node's network, which NetworkPolicy does not govern.
- On several Docker hosts the registry is on the control host and is not on any app network. Its port is
  reachable from the hosts' own addresses; apps reach it only as they reach any host address, and
  without a credential.

**One credential, `htpasswd`, for the Pando-run registry [D] (O-36).** Distribution's `htpasswd` auth grants every
authenticated user full access; there is no read-only account. The credential is held by Pando (and
handed per build to BuildKit and per pull to a runtime) and, on Kubernetes, by a pull Secret in each app
namespace that nothing in the namespace can read (PR 6). Pull by digest is what makes a leaked
credential less dangerous than it sounds: a push cannot change an image a deployment pinned. A scoped
alternative — Pando as the registry's token issuer, minting pull-only tokens per repository — is for
when per-app pull isolation is wanted; it matters most on Kubernetes, where the credential sits in
every app namespace.

### TLS and insecure registries

Docker and containerd pull over TLS from any registry not on loopback, unless the daemon's own
configuration lists it as insecure (`insecure-registries` in `daemon.json`; a `hosts.toml` for
containerd). Both are node configuration Pando cannot set (R-087 covers the access it would need, but
the adapter does not hold it). So:

- **TLS with a certificate the operator supplies [D] (O-35)**, mounted into the registry
  container, and a hostname every node resolves. A certificate from a private CA works if every node
  trusts that CA.
- **`PANDO_REGISTRY_INSECURE=true`** is accepted for a registry on a private network, and plain HTTP
  is used only when it is set [D]. Pando cannot
  check that each node is configured to allow it, so the first pull's failure is turned into a message
  that names the setting on the node: *"The host app-3 refused to pull from registry.internal:5000
  over HTTP. Add it to insecure-registries in /etc/docker/daemon.json on that host, or give the
  registry a certificate."*
- **Managed Kubernetes** (EKS, GKE, AKS) makes node configuration awkward; there the organization's
  registry (ECR, Artifact Registry, ACR) is the recommended setting, since nodes already trust it.

ACME DNS-01 reusing the edge's DNS credential remains a later option if installs ask for it.

### Pointing at the organization's registry

Set `PANDO_REGISTRY_URL` and the credential; Pando does not start or run anything. ECR uses
`PANDO_REGISTRY_KIND=ecr` with access keys, minting a password before each push and each plan exactly as
`core/oci` does for image apps (whether Pando's own instance role may be used is O-28, already open).
Harbor needs the project to exist. Artifact Registry and GHCR create repositories on push. GCR is shut
down and is not a target.

Registry authentication remains outside the adapter categories (design 03 §2.1's reasoning holds: the
planner asks it nothing). The install registry is configuration, like the database.

### Garbage collection (R-224)

Two halves, because Distribution splits them:

- **Manifests, by Pando [P].** The GC leader deletes manifests through the registry API
  (`DELETE /v2/<name>/manifests/<digest>`, which needs `storage.delete.enabled: true` in the shipped
  registry configuration):
  - **Deleted apps:** every manifest in the app's repositories, in the same teardown step that calls
    `BuilderAdapter.Forget` today. An app deleted with a kept final backup (R-204) keeps nothing in the
    registry; its backup is of volumes, and its image is rebuilt from the spec if it is restored.
  - **Live apps:** manifests referenced by none of the revisions R-152 keeps and by no deployment in
    flight. At most about eleven images per workload: ten revisions and one being deployed.
- **Blobs, by the topology [P].** `registry garbage-collect --delete-untagged` frees layers no manifest
  references, and Distribution requires the registry to be read-only or stopped while it runs. Pando
  does not run it: that would need Pando to control the registry container, which the decision above
  does not give it. The shipped topology runs it on a weekly schedule with the registry switched to
  read-only (`maintenance.readonly`), and a deploy during that window fails its push with a message that
  says so. **[D] (O-38)** The weekly window is accepted; if deploys during it become a real complaint,
  the registry changes to Zot, which collects online.

The registry's disk is bounded by the image count, not measured. Pando does not read the registry's
storage (R-243's rule, applied here: Pando reports what something tells it). `GET /capacity` shows how
many images Pando holds in the registry, from its own records.

The organization's registry is collected the same way for manifests; blob collection is the
organization's (ECR lifecycle policies, Harbor's scheduled GC).

### Backups and DR (R-212)

**The registry's contents are not in the DR bundle [D] (O-37).** At the cluster tier that is tens of
gigabytes per thousand apps, in a file the operator must store offsite and decrypt interactively
(R-214). What that means, by source type:

| Source | After a restore onto a registry that has lost the image |
|---|---|
| `git` | Rebuilt from the pinned commit (R-120). Not bit-identical: base images, unpinned dependencies and the network have moved, and a force-pushed or deleted repository cannot be rebuilt at all. |
| `image` | Pulled again by the pinned digest, if the upstream registry still has it. |
| `upload` | Rebuilt from the stored upload — **if the upload survived.** Uploads are kept under `/var/lib/pando/uploads` and are not in the DR bundle today. Without the image and without the upload there is nothing to rebuild from. |

So the upload gap exists now, with or without a registry, and the registry does not close it. It is
recorded as a known issue in `notes-multiple-replicas-issue-72.md`. **[D] (O-37) Uploads go into the
DR bundle**: they are the source of record for those apps (R-020's logic, as
`core/source/upload.go` says), sized by what people upload rather than by what builds produce.

After a restore, the reconciler finds a workload whose recorded image cannot be pulled. Today that is a
failed `Apply` and backoff toward `failed`. **[P]** It becomes a redeploy of the pinned revision
instead, with the deploy log saying the image was rebuilt and may differ from the one that ran before.
That is a deploy, so it goes through approval where approval applies (R-154). An optional registry
export in the bundle, off by default, is reasonable later for installs whose builds cannot be
reproduced.

Restoring with the registry intact (same topology, Postgres lost) needs none of this: the recorded
digests still resolve.

## Interface changes

All data, per R-254.

```go
// RuntimeCapabilities: how a built image can reach this runtime, in order of
// preference. Replaces the single SupportsImageImport bool.
ImageDelivery []ImageDelivery // "import" | "registry"

// single-host Docker: [import, registry]   (registry only if the install has one)
// multi-host Docker, Kubernetes: [registry]

// BuilderCapabilities
SupportsPush bool // can push to a registry with credentials it is handed per build

// BuildRequest: exactly one of ImageSink and Push is set.
type PushTarget struct {
    Repository string        // registry host and path, no tag
    Tag        string        // the deployment ID
    Auth       *RegistryAuth // per build; the builder keeps nothing
    Insecure   bool
}
Push *PushTarget

// BuildResult.Digest: filled on push. ImageRef is repository@digest.
```

`SupportsImageImport` can stay as a derived accessor during the change so callers move one at a time.
`ImportImage` keeps its signature; its single-host return value becomes the image ID. Nothing about an
image app changes.

The planner checks, in order: a runtime offering `import` and an import-capable builder → stream;
otherwise `registry` offered, an install registry configured, and a builder with `SupportsPush` →
push; otherwise `PLAN_CAPABILITY_UNSUPPORTED` with a message naming what is missing, for example:
*"This app runs on the Kubernetes runtime, which pulls every image from a registry. This install has no
registry configured. Set PANDO_REGISTRY_URL to the registry Pando should push built images to."*

## Tests this PR is done with

- `TestR146_ARefusedBuildIsNotWhatTheReconcilerRestores` — the gap above, on single-host Docker.
- `TestR120_ABuiltImageIsPinnedByDigest` — the recorded `image_ref` is `repo@sha256:…`, and a later push
  to the same tag leaves the running workload alone.
- `TestR224_ADeletedAppsImagesAreRemovedFromTheRegistry`.
- `TestR254_NoWayToDeliverABuildIsAPlanTimeRefusal`.
- `TestR194_TheRegistryCredentialNeverReachesALogOrAWorkload` — the push credential is not in
  buildkitd's configuration, and no workload's environment carries it.
- `TestR212_UploadsAreInTheDRBundle`.
- A replicas-topology run (`make test-replicas`) with the registry service and registry delivery forced
  on the single host, so the push path is exercised before PRs 6 and 7 depend on it.

## Decisions

All five were decided by the owner as recommended **[D]**; the table keeps the options that were weighed.


| ID | Question | Options | Decided |
|---|---|---|---|
| **O-34** | Does the single-VM install run a registry too? | (a) No: keep `ImportImage`, pin by image ID. (b) Yes, always: one delivery path everywhere. | **[D]** (a). A registry on one host adds a service and TLS or daemon configuration for no capability the host lacks. The pinning fix applies either way. |
| **O-35** | How does the Pando-run registry get a certificate every node trusts? | (a) Operator supplies a certificate and hostname. (b) Pando issues one by ACME DNS-01, reusing the edge's DNS credential. (c) Pando generates a CA the operator installs on each node. (d) Plain HTTP, with each node configured as insecure. | **[D]** (a) by default, (d) accepted with an explicit setting; (b) later if installs ask for it. (c) moves a trust root onto every node, which is the larger setup cost. |
| **O-36** | Is one full-access registry credential acceptable? | (a) `htpasswd`, one account, pull by digest limiting what a leak can change. (b) Pando acts as the registry's token issuer and mints pull-only tokens per app repository. | **[D]** (a) now; (b) when per-app pull isolation is wanted (it matters most on Kubernetes, where the credential sits in every app namespace). |
| **O-37** | Should the DR bundle include the registry? | (a) Never; rebuild after restore. (b) Optional, off by default. (c) Always. | **[D]** (a), with uploads added to the bundle. (b) is reasonable later for installs whose builds cannot be reproduced. |
| **O-38** | Is a weekly read-only window for blob GC acceptable? | (a) Yes, Distribution with a scheduled read-only GC. (b) Use Zot, which collects online. | **[D]** (a). Revisit with (b) if deploys during the window are a real complaint. |

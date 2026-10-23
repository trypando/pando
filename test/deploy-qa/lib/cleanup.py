"""Complete per-app cleanup, with a residue check, plus the disk gate.

Pando's own delete removes the app's containers, images, anonymous volumes,
build cache and uploaded source. It keeps some things on purpose: named
volumes unless the delete was forced (R-204), the app's network until Pando
restarts, and pre-delete backups until someone discards them. What it left is
recorded first (left_by_pando), so the sweep cannot hide a leak; then this
removes everything for one test app and checks that nothing carrying that
app's ID remains.

It only ever touches resources whose name carries a test app's ID, unnamed
volumes no container uses, images that were not on the machine when the QA
instance came up, and the QA instance's own data and build cache.
"""
import shutil
import subprocess
import threading
import time

from . import state

DATA = "/var/lib/pando"
_prune_lock = threading.Lock()


def sh(*args, timeout=180):
    try:
        r = subprocess.run(list(args), capture_output=True, text=True, timeout=timeout)
        return r.returncode, r.stdout.strip()
    except subprocess.TimeoutExpired:
        return 124, ""


def lines(out):
    return [l for l in out.splitlines() if l.strip()]


def all_images():
    _, out = sh("docker", "images", "--format", "{{.Repository}}:{{.Tag}}")
    return set(lines(out))


def _names(kind, ulid):
    fmt = {"container": ("ps", "-a", "--format", "{{.Names}}"),
           "volume": ("volume", "ls", "--format", "{{.Name}}"),
           "network": ("network", "ls", "--format", "{{.Name}}"),
           "image": ("images", "--format", "{{.Repository}}:{{.Tag}}")}[kind]
    _, out = sh("docker", *fmt)
    return [n for n in lines(out) if ulid.lower() in n.lower()]


def _data_files(ulid):
    _, out = sh("docker", "exec", state.PANDO, "sh", "-c", f"find {DATA} /tmp -iname '*{ulid}*' 2>/dev/null")
    return lines(out)


def residue(app_id):
    ulid = app_id.split("_", 1)[1]
    found = {k: _names(k, ulid) for k in ("container", "volume", "network", "image")}
    found["data"] = _data_files(ulid)
    return {k: v for k, v in found.items() if v}


def _remove(found, ulid):
    for c in found.get("container", []):
        sh("docker", "rm", "-f", "-v", c)
    for n in found.get("network", []):
        sh("docker", "network", "disconnect", "-f", n, state.PANDO)
        sh("docker", "network", "rm", n)
    for v in found.get("volume", []):
        sh("docker", "volume", "rm", "-f", v)
    if found.get("image"):
        sh("docker", "rmi", "-f", *found["image"])
    sh("docker", "exec", state.PANDO, "sh", "-c",
       f"find {DATA} /tmp -iname '*{ulid}*' -prune -exec rm -rf {{}} + 2>/dev/null")


def left_by_pando(app_id, wait=45):
    """What Pando's own delete left, before this module removes anything.

    Checked first so the sweep below cannot hide a leak: the sweep is what made
    every case report clean while Pando left build caches, uploads, images and
    volumes behind (issue #55). An app's network is not counted — Pando removes
    it at its next restart, by design (docs/design/notes-deploy-qa-issue-55.md).
    """
    ulid = app_id.split("_", 1)[1]
    t0 = time.time()
    while True:
        found = {k: _names(k, ulid) for k in ("container", "volume", "image")}
        found["data"] = _data_files(ulid)
        found = {k: v for k, v in found.items() if v}
        if not found or time.time() - t0 > wait:
            return found
        time.sleep(5)


def clean_app(p, app_id, wait=60):
    """Delete through the API, then remove everything left. Returns (residue, left by Pando)."""
    ulid = app_id.split("_", 1)[1]
    protected = set(state.load().get("protected_images", []))
    used = set()
    for c in _names("container", ulid):
        _, img = sh("docker", "inspect", c, "--format", "{{.Config.Image}}")
        if img:
            used.add(img if ":" in img.split("/")[-1] else img + ":latest")
    try:
        p.req("DELETE", f"/apps/{app_id}?force=true", timeout=180)
    except Exception:
        pass
    t0 = time.time()  # give Pando's own teardown a chance first
    while time.time() - t0 < wait and _names("container", ulid):
        time.sleep(5)
    pando_left = left_by_pando(app_id)
    _remove({k: _names(k, ulid) for k in ("container", "network", "volume", "image")}, ulid)
    for img in used - protected:
        sh("docker", "rmi", img)  # no -f: an image another test app still runs stays
    sweep_shared()
    left = residue(app_id)
    if left:  # one retry: a container can be mid-recreate when we look
        time.sleep(10)
        _remove(left, ulid)
        left = residue(app_id)
    return left, pando_left


def sweep_shared(max_age_min=45):
    """Leftovers that carry no app ID.

    - Unnamed volumes no container uses (images that declare VOLUME, such as mysql).
      `volume prune` without -a never removes a named volume.
    - Uploaded-source checkouts (source/upload.go fetchUpload has no cleanup) idle
      longer than any build takes, and pre-delete backups.
    """
    sh("docker", "volume", "prune", "-f")
    sh("docker", "exec", state.PANDO, "sh", "-c",
       f"find /tmp -maxdepth 1 -name 'pando-upload-*' -mmin +{max_age_min} -exec rm -rf {{}} + 2>/dev/null; "
       f"find {DATA}/backups -mindepth 1 -maxdepth 1 -mmin +{min(max_age_min, 2)} -exec rm -rf {{}} + 2>/dev/null")


def final_sweep():
    """End of a run: nothing in flight, so everything temporary can go."""
    sweep_shared(max_age_min=0)
    sh("docker", "exec", state.PANDO, "sh", "-c", "rm -rf /tmp/pando-upload-* /tmp/pando-src-* 2>/dev/null")
    protected = set(state.load().get("protected_images", []))
    for img in all_images() - protected:
        sh("docker", "rmi", img)


def prune_build_cache(keep_mb=20000):  # noqa: ARG001 - callers still pass the cap they want
    """Trim what builds leave. Serialized; never concurrent with itself.

    Each app's BuildKit holds its cache only until Pando removes it, after its idle window or to
    make room for another app's (R-117), and its API takes Pando's certificate alone, so there is
    no shared BuildKit cache here to prune: dangling images are what is left to trim.
    """
    if not _prune_lock.acquire(blocking=False):
        return
    try:
        sh("docker", "image", "prune", "-f")
    finally:
        _prune_lock.release()


def free_gb(path="/"):
    return shutil.disk_usage(path).free / 1e9


def wait_for_disk(min_free_gb, log):
    """Hold before starting a case until the machine has enough free disk."""
    warned = False
    while free_gb() < min_free_gb:
        if not warned:
            log(f"disk gate: {free_gb():.1f} GB free < {min_free_gb} GB, holding new cases and trimming build cache")
            warned = True
        prune_build_cache(keep_mb=5000)
        time.sleep(30)
    if warned:
        log(f"disk gate: {free_gb():.1f} GB free, resuming")


def snapshot():
    """Everything a run could leave behind, as a sorted list (for before/after comparison)."""
    out = []
    for kind, fmt in (("container", ("ps", "-a", "--format", "{{.Names}}")),
                      ("volume", ("volume", "ls", "--format", "{{.Name}}")),
                      ("network", ("network", "ls", "--format", "{{.Name}}")),
                      ("image", ("images", "--format", "{{.Repository}}:{{.Tag}}"))):
        _, o = sh("docker", *fmt)
        out += [f"{kind} {n}" for n in lines(o) if n != "pando-trivy-cache"]
    _, o = sh("docker", "exec", state.PANDO, "sh", "-c",
              f"find {DATA} -mindepth 2 -maxdepth 2 2>/dev/null | grep -v /secrets; ls /tmp")
    out += [f"data {n}" for n in lines(o)]
    return sorted(out)

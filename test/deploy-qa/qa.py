#!/usr/bin/env python3
"""Deploy QA: deploy ~170 real and generated apps through a throwaway Pando and report what works.

    python3 test/deploy-qa/qa.py preflight          check this machine can run it safely
    python3 test/deploy-qa/qa.py up                 build and start the QA instance (compose project pando-qa)
    python3 test/deploy-qa/qa.py run --pass noai    run every case (also: --pass ai, --pass both)
    python3 test/deploy-qa/qa.py dashboard          live progress (in a second terminal)
    python3 test/deploy-qa/qa.py report             write out/report.html and out/summary.md
    python3 test/deploy-qa/qa.py down               remove everything the QA instance created
    python3 test/deploy-qa/qa.py baseline           save this run as the reference future reports compare with

See test/deploy-qa/README.md.
"""
import argparse
import json
import os
import secrets
import shutil
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from lib import cleanup, state  # noqa: E402

# Every test source: the generated apps and the lists of public repositories and
# images. Pinned so a run is reproducible; move the pin to pick up changes there.
FIXTURES_REPO = "https://github.com/trypando/pando-qa-fixtures"
FIXTURES_REF = "d4597f7bc0ee5063b173ee660fccbec768a92d9e"
SENTINEL_LABEL = "io.pando.qa.sentinel"
MIN_FREE_GB = 40
PREFLIGHT_MIN_DISK_GB = 60


def sh(*args, timeout=120, check=False):
    r = subprocess.run(list(args), capture_output=True, text=True, timeout=timeout)
    if check and r.returncode != 0:
        raise SystemExit(f"`{' '.join(args)}` failed:\n{r.stderr.strip()}")
    return r.returncode, r.stdout.strip(), r.stderr.strip()


def docker_info():
    code, out, err = sh("docker", "info", "--format", "{{json .}}", timeout=20)
    if code != 0:
        raise SystemExit("Docker is not responding. Start Docker Desktop (or the Docker daemon) and try again.\n" + err)
    return json.loads(out)


def other_pando_servers():
    """Running Pando servers that are not the QA instance."""
    _, out, _ = sh("docker", "ps", "--filter", "label=com.docker.compose.service=pando",
                   "--format", '{{.Names}}\t{{.Label "com.docker.compose.project"}}')
    return [l.split("\t")[0] for l in out.splitlines() if l.strip() and l.split("\t")[1] != state.PROJECT]


def managed_networks():
    """Every Pando-managed network on this Docker host, whoever created it."""
    _, out, _ = sh("docker", "network", "ls", "--filter", "label=io.pando.managed=true", "--format", "{{.Name}}")
    return [n for n in out.splitlines() if n]


def plan(info):
    mem_gb = info["MemTotal"] / 1e9
    cpus = info["NCPU"]
    pools = info.get("DefaultAddressPools") or []
    concurrency = int(max(2, min(25, (mem_gb - 4) / 0.5, cpus * 2)))
    max_builds = int(max(2, min(12, (mem_gb - 4) / 1.2)))
    if not pools:
        concurrency = min(concurrency, 8)
    return {"mem_gb": round(mem_gb, 1), "cpus": cpus, "pools": pools,
            "concurrency": concurrency, "max_builds": max_builds}


def preflight(verbose=True):
    problems, notes = [], []
    info = docker_info()
    p = plan(info)
    free = cleanup.free_gb()
    if free < PREFLIGHT_MIN_DISK_GB:
        problems.append(f"Only {free:.0f} GB of disk is free. A run needs about {PREFLIGHT_MIN_DISK_GB} GB of headroom "
                        f"(new cases pause below {MIN_FREE_GB} GB). Free some space, or run `docker system df` to see what Docker holds.")
    if p["mem_gb"] < 8:
        problems.append(f"Docker has {p['mem_gb']} GB of memory. Give it at least 8 GB (Docker Desktop → Settings → Resources); 16 GB is comfortable.")
    elif p["mem_gb"] < 12:
        notes.append(f"Docker has {p['mem_gb']} GB of memory, so builds are capped at {p['max_builds']} at a time.")
    if not p["pools"]:
        notes.append("Docker uses its default network pool (about 30 networks), so the run is capped at 8 apps at a time. "
                     "To allow more, add to Docker Desktop → Settings → Docker Engine:\n"
                     '      "default-address-pools": [{ "base": "10.200.0.0/16", "size": 24 }]\n'
                     "    and restart Docker.")
    others = other_pando_servers()
    if others:
        problems.append("Another Pando is running on this Docker host: " + ", ".join(others) + ".\n"
                        "    Two installs on one host adopt each other's networks (issue #55). Stop it for the run:\n"
                        f"      docker stop {' '.join(others)}\n"
                        f"    and start it again afterwards with `docker start {' '.join(others)}`.")
    busy = []
    for port in [8199] + list(range(9100, 9150)):
        code, out, _ = sh("lsof", "-nP", f"-iTCP:{port}", "-sTCP:LISTEN")
        if out:
            busy.append(port)
    if busy and not os.path.exists(state.STATE):
        problems.append(f"Ports the QA instance needs are in use: {busy[:6]}{'…' if len(busy) > 6 else ''}. "
                        "It uses 8199 for Pando and 9100–9149 for apps.")
    if sys.version_info < (3, 10):
        problems.append("Python 3.10 or newer is required.")
    if verbose:
        print(f"Docker: {p['cpus']} CPUs, {p['mem_gb']} GB memory, network pool {'custom' if p['pools'] else 'default'}")
        print(f"Disk: {free:.0f} GB free")
        print(f"Plan: {p['concurrency']} apps at a time, at most {p['max_builds']} builds and 4 clones at once, 0.25 CPU per app")
        for n in notes:
            print("  note:", n)
        for pr in problems:
            print("  PROBLEM:", pr)
        print("Preflight passed." if not problems else "Preflight failed; nothing was changed.")
    return not problems, p


def fetch_fixtures():
    """Clone the test sources at the pinned commit (or move an existing clone to it)."""
    d = state.FIXTURES
    if not os.path.isdir(os.path.join(d, ".git")):
        code, _, err = sh("git", "clone", "-q", FIXTURES_REPO, d, timeout=600)
        if code != 0:
            sys.exit(f"Could not clone {FIXTURES_REPO}:\n{err}")
    else:
        sh("git", "-C", d, "fetch", "-q", "origin", timeout=600)
    code, _, err = sh("git", "-C", d, "checkout", "-q", "--detach", FIXTURES_REF)
    if code != 0:
        sys.exit(f"Could not check out fixtures commit {FIXTURES_REF}:\n{err}")
    print(f"Test sources: {FIXTURES_REPO} @ {FIXTURES_REF[:10]}")


def build_prebuilt_binary():
    src = os.path.join(state.FIXTURES, "src", "prebuilt-binary")
    dst = os.path.join(state.FIXTURES, "apps", "edge-prebuilt-binary", "server")
    arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(docker_info()["Architecture"], "amd64")
    env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=arch)
    if shutil.which("go"):
        subprocess.run(["go", "build", "-o", dst, "."], cwd=src, env=env, check=True)
    else:
        subprocess.run(["docker", "run", "--rm", "-v", f"{src}:/src", "-v", f"{os.path.dirname(dst)}:/out", "-w", "/src",
                        "-e", "CGO_ENABLED=0", "-e", f"GOARCH={arch}", "golang:1.22-alpine",
                        "go", "build", "-o", "/out/server", "."], check=True)
    os.chmod(dst, 0o755)


def protect_foreign_networks(nets):
    """Keep another install's networks non-empty, so the QA instance's startup
    reclaim (which removes empty Pando networks) cannot delete them."""
    if not nets:
        return
    sh("docker", "pull", "-q", "alpine:3.20", timeout=300)
    for i, n in enumerate(nets):
        name = f"pando-qa-sentinel-{i}"
        sh("docker", "rm", "-f", name)
        sh("docker", "run", "-d", "--name", name, "--label", f"{SENTINEL_LABEL}=true", "--network", n,
           "--restart", "no", "alpine:3.20", "sleep", "infinity")


def remove_sentinels():
    _, out, _ = sh("docker", "ps", "-aq", "--filter", f"label={SENTINEL_LABEL}=true")
    if out:
        sh("docker", "rm", "-f", *out.split())


def compose(*args, timeout=1800):
    env_file = state.path("qa.env")
    cmd = ["docker", "compose", "-p", state.PROJECT, "-f", os.path.join(state.REPO, "docker-compose.yml"),
           "--env-file", env_file, *args]
    return subprocess.run(cmd, cwd=state.REPO, timeout=timeout)


def cmd_up(a):
    if os.path.exists(state.STATE):
        print("A QA instance is already set up (out/state.json). Run `qa.py down` first, or `qa.py run` to use it.")
        return
    ok, p = preflight()
    if not ok:
        sys.exit(1)
    os.makedirs(state.OUT, exist_ok=True)
    images_before = sorted(cleanup.all_images())
    nets = managed_networks()  # the QA instance is not up yet, so every one of these belongs to someone else
    st = {"port": 8199, "app_port_start": 9100, "app_port_end": 9149,
          "admin_password": secrets.token_urlsafe(18), "images_before_up": images_before,
          "foreign_networks": nets, "plan": p, "min_free_gb": MIN_FREE_GB,
          "git_ref": sh("git", "-C", state.REPO, "describe", "--always", "--dirty")[1],
          "run_started": time.strftime("%Y-%m-%d %H:%M")}
    state.save(st)
    with open(state.path("qa.env"), "w") as f:
        f.write(f"PANDO_PORT={st['port']}\nPANDO_APP_PORT_START={st['app_port_start']}\nPANDO_APP_PORT_END={st['app_port_end']}\n"
                f"PANDO_ADMIN_PASSWORD={st['admin_password']}\nPOSTGRES_PASSWORD=pandoqa\n"
                "PANDO_RECONCILER_BACKOFF=0s,5s,10s\nPANDO_RECONCILER_FAILURE_WINDOW=3m\nPANDO_RECONCILER_GC_INTERVAL=1m\n")
    fetch_fixtures()
    print("Building the prebuilt-binary fixture…")
    build_prebuilt_binary()
    protect_foreign_networks(nets)
    print(f"Building and starting Pando from {st['git_ref']} as compose project {state.PROJECT} (a few minutes the first time)…")
    if compose("up", "-d", "--build").returncode != 0:
        sys.exit("The QA instance did not start. Run `qa.py down` to clean up.")
    from lib.pando import Pando
    for _ in range(90):
        try:
            Pando()
            break
        except Exception:
            time.sleep(2)
    else:
        sys.exit("Pando did not come up within 3 minutes. Check `docker logs pando-qa-pando-1`, then `qa.py down`.")
    st = state.load()
    st["protected_images"] = sorted(cleanup.all_images())
    state.save(st)
    with open(state.path("snapshot-after-up.txt"), "w") as f:
        f.write("\n".join(cleanup.snapshot()) + "\n")
    print(f"QA instance is up at http://localhost:{st['port']} (user admin, password in test/deploy-qa/out/qa.env).")


def set_ai(p, on):
    """Screening on or off through host policy (design 10 §7); registers the adapter the first time."""
    from lib.pando import Pando
    st = state.load()
    if on and not st.get("ai_registered"):
        key = os.environ.get("ANTHROPIC_API_KEY", "")
        if not key:
            sys.exit("The AI pass needs ANTHROPIC_API_KEY in the environment. It is sent to the QA instance as a sealed "
                     "credential and is never written to a file or printed.")
        code, out = p.req("POST", "/adapters", {"id": "ai_anthropic", "category": "ai", "kind": "anthropic",
                                                 "name": "anthropic", "config": {"screen_plans": True},
                                                 "credentials": {"api_key": key}, "is_default": True, "enabled": True})
        del key
        if code >= 300 and "exists" not in json.dumps(out).lower():
            sys.exit(f"Could not register the AI adapter: {code} {json.dumps(out)[:300]}")
        sh("docker", "restart", state.PANDO)
        for _ in range(60):
            try:
                p = Pando()
                break
            except Exception:
                time.sleep(2)
        st["ai_registered"] = True
        state.save(st)
    pol = p.get("/policy")
    doc = pol.get("policy") or pol
    doc["disable_ai_screening"] = not on
    code, out = p.req("PUT", "/policy", doc)
    if code >= 300:
        sys.exit(f"Could not set the AI screening policy: {code} {json.dumps(out)[:300]}")
    return p


def rerun_environment_failures(label, p):
    """Re-run, once, cases that failed for reasons outside Pando's control or ours to judge."""
    from lib import classify, harness
    f = state.results_file(label)
    rows = [json.loads(l) for l in open(f)]
    env = [r["id"] for r in rows if classify.cause(r) == "harness"]
    if not env:
        return
    print(f"Re-running {len(env)} case(s) that failed for environment reasons: {', '.join(env)}")
    with open(f, "w") as fh:
        for r in rows:
            if r["id"] not in env:
                fh.write(json.dumps(r) + "\n")
    harness.run_pass(label, concurrency=3, only=env, max_builds=3, max_clones=2, http_wait=180)


def cmd_run(a):
    from lib import harness
    from lib.pando import Pando
    st = state.load()
    ok, p_plan = preflight(verbose=False)
    if not ok:
        preflight()
        sys.exit(1)
    passes = ["noai", "ai"] if a.pass_ == "both" else [a.pass_]
    if "ai" in passes and not os.environ.get("ANTHROPIC_API_KEY") and not st.get("ai_registered"):
        sys.exit("The AI pass needs ANTHROPIC_API_KEY in the environment.")
    concurrency = a.concurrency or st["plan"]["concurrency"]
    for label in passes:
        if a.fresh and os.path.exists(state.results_file(label)):
            os.rename(state.results_file(label), state.results_file(label) + f".{int(time.time())}.old")
        p = set_ai(Pando(), on=(label == "ai"))
        print(f"Pass {label}: {concurrency} apps at a time. Watch it with `python3 test/deploy-qa/qa.py dashboard"
              f"{' --pass ai' if label == 'ai' else ''}`.")
        only = a.only.split(",") if a.only else ()
        for _ in range(3):  # resume after watchdog restarts
            left, stopped = harness.run_pass(label, concurrency=concurrency, only=only,
                                             max_builds=st["plan"]["max_builds"], min_free_gb=st["min_free_gb"])
            if stopped:
                sys.exit("Stopped: an app left something behind after cleanup. See the RESIDUE line in "
                         f"{state.log_file(label)}; nothing else was started.")
            if not left:
                break
        if not only:
            rerun_environment_failures(label, p)
    diff = check_environment()
    from lib import report
    report.main()
    if diff:
        print("WARNING: the QA instance holds things it did not have after `up`:\n  " + "\n  ".join(diff[:20]))


def check_environment():
    try:
        before = open(state.path("snapshot-after-up.txt")).read().split("\n")
    except FileNotFoundError:
        return []
    now = cleanup.snapshot()
    return sorted(set(now) - set(filter(None, before)))


def cmd_dashboard(a):
    from lib import dashboard
    dashboard.main(a.pass_)


def cmd_report(a):
    if not (os.path.exists(state.results_file("noai")) or os.path.exists(state.results_file("ai"))):
        sys.exit("No results yet. Run `qa.py run` first.")
    from lib import report
    report.main()


def cmd_baseline(a):
    """Save this run's per-app outcomes as the reference later reports compare against."""
    from lib import classify
    st = state.load() if os.path.exists(state.STATE) else {}
    passes = {}
    for label in ("noai", "ai"):
        f = state.results_file(label)
        if os.path.exists(f):
            passes[label] = {json.loads(l)["id"]: json.loads(l) for l in open(f)}
    if "noai" not in passes:
        sys.exit("A baseline needs a complete no-AI pass.")
    date = time.strftime("%Y-%m-%d")
    base = {"run": date, "commit": st.get("git_ref", ""), "issue": a.issue, "apps": {}}
    for i, r in sorted(passes["noai"].items()):
        entry = {"noai": r["outcome"], "cause_noai": classify.cause(r)}
        if "ai" in passes and i in passes["ai"]:
            entry.update(ai=passes["ai"][i]["outcome"], cause_ai=classify.cause(passes["ai"][i]))
        base["apps"][i] = entry
    out = os.path.join(state.HERE, "baseline", f"{date}.json")
    with open(out, "w") as f:
        json.dump(base, f, indent=1)
    print(f"wrote {out}")


def cmd_status(a):
    st = state.load()
    print(f"QA instance: http://localhost:{st['port']}  ({st.get('git_ref')}, started {st.get('run_started')})")
    for label in ("noai", "ai"):
        f = state.results_file(label)
        if os.path.exists(f):
            rows = [json.loads(l) for l in open(f)]
            ok = sum(r["outcome"].startswith("PASS") for r in rows)
            print(f"  {label}: {len(rows)} done, {ok} passed")


def qa_app_ulids():
    """IDs of every app the QA instance ever created, read from its own database."""
    code, out, _ = sh("docker", "exec", state.POSTGRES, "psql", "-U", "pando", "-d", "pando", "-tAc",
                      "select replace(id, 'app_', '') from apps")
    return [u.strip().lower() for u in out.splitlines() if u.strip()] if code == 0 else []


def cmd_down(a):
    st = state.load()
    ulids = qa_app_ulids()
    print(f"Removing the QA instance, its volumes and build cache, and anything left of its {len(ulids)} apps…")
    if os.path.exists(state.path("qa.env")):
        compose("down", "-v", "--remove-orphans", timeout=600)
    # The BuildKit the QA instance's Pando started, outside Compose, by the names it gives it: the
    # QA project's, never another installation's.
    sh("docker", "rm", "-f", "-v", state.BUILDKIT)
    sh("docker", "network", "rm", f"{state.PROJECT}-build")
    sh("docker", "volume", "rm", "-f", f"{state.PROJECT}-buildkit-cache")
    # Only resources whose name carries one of the QA instance's app IDs. Never by label alone:
    # another install's containers carry the same Pando labels.
    if ulids:
        for kind, fmt in (("container", ("ps", "-a", "--format", "{{.Names}}")),
                          ("network", ("network", "ls", "--format", "{{.Name}}")),
                          ("volume", ("volume", "ls", "--format", "{{.Name}}"))):
            _, out, _ = sh("docker", *fmt)
            for name in out.splitlines():
                if any(u in name.lower() for u in ulids):
                    sh("docker", {"container": "rm", "network": "network", "volume": "volume"}[kind],
                       *(["-f", "-v", name] if kind == "container" else ["rm", "-f", name] if kind == "volume" else ["rm", name]))
    remove_sentinels()
    before = set(st.get("images_before_up", []))
    for img in cleanup.all_images() - before:
        sh("docker", "rmi", img)
    sh("docker", "image", "prune", "-f")
    sh("docker", "volume", "prune", "-f")
    os.remove(state.STATE)
    print("Done. Results and the report stay in test/deploy-qa/out/.")
    if st.get("foreign_networks"):
        print(f"{len(st['foreign_networks'])} network(s) belonging to another Pando were protected during the run and are untouched.")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("preflight").set_defaults(fn=lambda a: sys.exit(0 if preflight()[0] else 1))
    sub.add_parser("up").set_defaults(fn=cmd_up)
    r = sub.add_parser("run")
    r.add_argument("--pass", dest="pass_", choices=["noai", "ai", "both"], default="noai")
    r.add_argument("--concurrency", type=int, default=0)
    r.add_argument("--only", default="", help="comma-separated case IDs")
    r.add_argument("--fresh", action="store_true", help="start the pass over instead of resuming")
    r.set_defaults(fn=cmd_run)
    d = sub.add_parser("dashboard")
    d.add_argument("--pass", dest="pass_", choices=["noai", "ai"], default="noai")
    d.set_defaults(fn=cmd_dashboard)
    sub.add_parser("report").set_defaults(fn=cmd_report)
    sub.add_parser("status").set_defaults(fn=cmd_status)
    b = sub.add_parser("baseline", help="save this run as the reference for future reports")
    b.add_argument("--issue", type=int, default=0, help="issue that tracks the fixes")
    b.set_defaults(fn=cmd_baseline)
    sub.add_parser("down").set_defaults(fn=cmd_down)
    a = ap.parse_args()
    a.fn(a)


if __name__ == "__main__":
    main()

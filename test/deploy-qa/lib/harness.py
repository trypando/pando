"""One pass over the cases: create -> detect -> answer -> accept -> fill slots -> deploy -> HTTP check.

Resumable: a case already in the pass's results file is skipped. Every app is
fully removed after its case (see cleanup.py), and the run stops if anything is
left behind.
"""
import gzip
import io
import json
import os
import re
import secrets
import tarfile
import threading
import time
import traceback
from concurrent.futures import ThreadPoolExecutor

import queue
import subprocess

from . import cleanup, state
from .pando import Pando

BUILD_SEM = threading.Semaphore(int(os.environ.get("QA_MAX_BUILDS", "12")))
IMAGE_PORTS = queue.Queue()  # the top three app ports are kept for hand-written image specs
STOP = threading.Event()
CLONE_SEM = threading.Semaphore(int(os.environ.get("QA_MAX_CLONES", "4")))
RESTARTS = {"n": 0}
LAST_DONE = {"t": time.time()}
INFLIGHT = {"n": 0}


def watchdog(log):
    """Pando's git fetch can hang until its 10-minute detection deadline (see report).
    If nothing finishes for 6 minutes while cases are in flight, restart Pando."""
    while not STOP.is_set():
        time.sleep(30)
        if INFLIGHT["n"] and time.time() - LAST_DONE["t"] > 360:
            log(f"watchdog: no case finished in 6 min, restarting {state.PANDO}; in-flight cases will be re-run")
            RESTARTS["n"] += 1
            subprocess.run(["docker", "restart", state.PANDO], capture_output=True)
            LAST_DONE["t"] = time.time()


CPU_MILLIS = int(os.environ.get("QA_CPU_MILLIS", "250"))

SKIP = {".git", "node_modules", ".venv", "venv", "__pycache__", ".terraform", "vendor",
        ".DS_Store", ".idea", ".vscode"}
DB_SLOT_TYPES = {"postgres", "mysql", "redis", "mariadb", "mongodb", "mongo"}
lock = threading.Lock()


LOGFILE = {"path": None}


def log(msg):
    line = time.strftime("%H:%M:%S") + " " + msg
    with lock:
        print(line, flush=True)
        if LOGFILE["path"]:
            with open(LOGFILE["path"], "a") as fh:
                fh.write(line + "\n")


def pack(path):
    """Same packing rules as `pando deploy <path>` (internal/cli/upload.go)."""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        for root, dirs, files in os.walk(path):
            dirs[:] = [d for d in dirs if d not in SKIP]
            for f in files:
                if f in SKIP:
                    continue
                full = os.path.join(root, f)
                if os.path.islink(full) or not os.path.isfile(full):
                    continue
                tf.add(full, arcname=os.path.relpath(full, path), recursive=False)
    return buf.getvalue()


def answer_for(q, facts, packaging):
    """The answer a person who knows their app would give, or None."""
    key = q.get("key", "")
    opts = q.get("options") or []
    cand = None
    if key in ("port", "primary_port") or key.endswith("_port"):
        cand = facts.get("port")
    elif key == "start_command":
        cand = facts.get("start_command")
    elif key == "primary_service":
        cand = facts.get("primary_service")
    elif key == "static_source":
        cand = facts.get("static_source")
    elif key == "dockerfile_path":
        cand = facts.get("dockerfile_path")
    elif key == "deployable_project":
        cand = facts.get("deployable_project")
    elif key.startswith("build_arg."):
        cand = (facts.get("build_args") or {}).get(key.split(".", 1)[1])
    elif key in ("build_method", "build_strategy"):
        cand = facts.get("build_method") or {
            "dockerfile": "dockerfile", "compose": "compose", "static": "static",
            "buildpack": "buildpack", "image": "image"}.get(packaging)
    if cand is None and key in (facts.get("answers") or {}):
        cand = facts["answers"][key]
    if cand is None:
        return None
    cand = str(cand)
    if opts:
        for o in opts:
            if str(o) == cand:
                return o
        for o in opts:
            if cand.lower() in str(o).lower() or str(o).lower() in cand.lower():
                return o
        return None
    return cand


def poll_detection(p, aid, timeout=1200):
    t0 = time.time()
    while True:
        d = p.get(f"/apps/{aid}/detection")
        st = d.get("status")
        if st not in ("running", "pending", None, "") or time.time() - t0 > timeout:
            return d, time.time() - t0
        time.sleep(4)


def summarize_detection(d):
    det = d.get("detection") or {}
    spec = det.get("draft_spec") or {}
    build = spec.get("build") or {}
    return {
        "status": d.get("status"),
        "strategy": build.get("strategy"),
        "winner": det.get("winning_bid"),
        "dockerfile": build.get("dockerfile"),
        "static_dir": build.get("static_dir"),
        "workloads": [{"name": w.get("name"), "primary": w.get("primary"),
                       "ports": [x.get("number") for x in (w.get("ports") or [])],
                       "command": w.get("command")}
                      for w in (spec.get("workloads") or [])],
        "questions": [{"key": q.get("key"), "question": q.get("prompt") or q.get("question"), "kind": q.get("kind"),
                       "options": q.get("options"), "deferred": q.get("deferred"),
                       "valid": q.get("valid_answer")} for q in (det.get("questions") or [])],
        "warnings": [w.get("code") + ": " + (w.get("message") or "")[:200]
                     for w in (spec.get("warnings") or [])],
        "screening": det.get("screening"),
        "error": det.get("error") or d.get("error"),
        "slots": [{"key": s.get("key"), "type": s.get("type"), "required": s.get("required")}
                  for s in (spec.get("slots") or [])],
    }



def run_case(p, c, label):
    r = {"id": c["id"], "pass": label, "source": c.get("source"), "language": c.get("language"),
         "packaging": c.get("packaging"), "category": c.get("category"), "expect": c.get("expect", "web"),
         "shape": c.get("shape"), "steps": {}, "answered": {}, "slots_filled": {}}
    facts = c.get("facts") or {}
    t0 = time.time()
    name = ("qa-" + label + "-" + c["id"])[:60]

    # 1. create
    src = {}
    if c["kind"] == "git":
        src = {"type": "git", "url": c["url"], "ref": c.get("ref") or "", "subdir": c.get("subdir") or ""}
    elif c["kind"] == "image":
        src = {"type": "image", "image": c["image"]}
    else:
        src = {"type": "upload"}
    # Pando stalls when many git clones start at once; gate create->detection for git sources.
    gate = CLONE_SEM if c["kind"] == "git" else None
    if gate:
        gate.acquire()
        r["_holds_clone"] = True
    code, app = p.req("POST", "/apps", {"name": name, "source": src})
    r["steps"]["create"] = code
    if code >= 300:
        if r.pop("_holds_clone", None):
            CLONE_SEM.release()
        r.update(outcome="FAIL_CREATE", detail=json.dumps(app)[:1500])
        return r
    aid = app["id"]
    r["app_id"] = aid
    r["slug"] = app.get("slug")

    try:
        if c["kind"] == "upload":
            code, out = p.req("POST", f"/apps/{aid}/source", raw=pack(c["path"]), ctype="application/gzip", timeout=300)
            r["steps"]["upload"] = code
            if code >= 300:
                r.update(outcome="FAIL_UPLOAD", detail=json.dumps(out)[:1500])
                return r
            code, out = p.req("POST", f"/apps/{aid}/detection/rerun", {})
            r["steps"]["rerun"] = code

        # 2. detect
        d, dt = poll_detection(p, aid)
        r["detect_seconds"] = round(dt)
        r["detection"] = summarize_detection(d)
        if r.pop("_holds_clone", None):
            CLONE_SEM.release()
        r["detection_initial_questions"] = len([q for q in r["detection"]["questions"] if not q["deferred"]])

        # 3. answer questions a person would know the answer to
        rounds = 0
        while d.get("status") == "needs_answers" and rounds < 3 and c["kind"] != "image":
            rounds += 1
            qs = [q for q in (d.get("detection") or {}).get("questions") or [] if not q.get("deferred")]
            ans = {}
            for q in qs:
                a = answer_for(q, facts, c.get("packaging"))
                if a is not None and q.get("key") not in (d.get("answers") or {}):
                    ans[q["key"]] = str(a)
            if not ans:
                break
            r["answered"].update(ans)
            code, out = p.req("POST", f"/apps/{aid}/detection/answers", {"answers": ans})
            r["steps"][f"answers{rounds}"] = code
            if code >= 300:
                r["answer_error"] = json.dumps(out)[:1000]
                break
            d, _ = poll_detection(p, aid)
            r["detection_after_answers"] = summarize_detection(d)
            # Answers are applied at accept (Sequence A: answers -> accept); the
            # detection status itself stays needs_answers.
            got = d.get("answers") or {}
            open_q = [q for q in (d.get("detection") or {}).get("questions") or []
                      if not q.get("deferred") and q.get("key") not in got]
            if not open_q:
                d = dict(d, status="ready")
                break

        status = d.get("status")
        r["detection_final_status"] = status
        if status != "ready" and c["kind"] == "image" and r["expect"] == "web":
            # Console path for images: a person sets the port and Pando runs the image as is.
            r["image_manual_spec"] = True
            port = facts.get("port") or 80
            spec = {"schema_version": 1, "source": {"type": "image", "image": c["image"]},
                    "build": {"strategy": "prebuilt"},
                    "workloads": [{"name": "web", "primary": True, "exposed": True,
                                   "ports": [{"number": port, "protocol": "http", "source": "user"}]}],
                    "resources": {"cpu_millis": CPU_MILLIS, "overridden": True},
                    "routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": r.setdefault("image_port", IMAGE_PORTS.get())},
                    "runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
                    "deploy": {"strategy": "recreate"}}
            code, out = p.req("POST", f"/apps/{aid}/specs", spec)
            r["steps"]["write_spec"] = code
            if code >= 300:
                r.update(outcome="FAIL_IMAGE_SPEC", detail=json.dumps(out)[:1500])
                return r
            rev = out.get("revision")
            code, out = p.req("POST", f"/apps/{aid}/specs/{rev}/pin", {})
            r["steps"]["pin"] = code
            if code >= 300:
                r.update(outcome="FAIL_IMAGE_SPEC", detail=json.dumps(out)[:1500])
                return r
            status = "ready"
            r["answered"]["manual_spec"] = f"image with port {port}"
        if status != "ready":
            if r["expect"] == "refuse":
                r.update(outcome="PASS_REFUSED", detail=f"detection status {status}")
            else:
                r.update(outcome="FAIL_DETECT", detail=f"detection status {status}; unanswered questions: " +
                         json.dumps([q for q in (d.get('detection') or {}).get('questions') or []
                                     if not q.get('deferred')])[:2500] + " error: " + json.dumps(d.get("error") or (d.get("detection") or {}).get("error"))[:800])
            return r

        # 4. accept
        code, out = (200, {}) if r.get("image_manual_spec") else p.req("POST", f"/apps/{aid}/detection/accept", {})
        r["steps"]["accept"] = code
        if code >= 300:
            r.update(outcome="PASS_REFUSED" if r["expect"] == "refuse" else "FAIL_ACCEPT", detail=json.dumps(out)[:1500])
            return r

        # 5. fill required slots the way a person would
        slots = p.get(f"/apps/{aid}/slots").get("slots") or []
        for s in slots:
            if s.get("resolution") or not s.get("required"):
                continue
            key, typ = s.get("key"), (s.get("type") or "").lower()
            given = (facts.get("slots") or {}).get(key)
            if given is None and typ in DB_SLOT_TYPES:
                body = {"mode": "provisioned"}
            else:
                body = {"mode": "literal", "value": given or ("qa-" + secrets.token_hex(12))}
            code, out = p.req("PUT", f"/apps/{aid}/slots/{key}", body)
            r["slots_filled"][key] = {"type": typ, "mode": body["mode"], "code": code,
                                      "err": None if code < 300 else json.dumps(out)[:400]}

        specs = p.get(f"/apps/{aid}/specs")
        revs = [s.get("revision") for s in (specs.get("specs") or specs.get("revisions") or []) if s.get("revision")]
        rev = max(revs) if revs else 0
        # Test setting, not Pando's default: 0.25 CPU per app instead of 1, so 25 apps fit a
        # 12-CPU host. Done the way a person would, as an edited spec revision.
        if rev:
            _, one = p.req("GET", f"/apps/{aid}/specs/{rev}")
            body = one.get("body") or {}
            body = json.loads(body) if isinstance(body, str) else body
            if body.get("resources"):
                body["resources"]["cpu_millis"] = CPU_MILLIS
                body["resources"]["overridden"] = True
                code, out = p.req("POST", f"/apps/{aid}/specs", body)
                r["steps"]["cpu_spec"] = code
                if code < 300 and out.get("revision"):
                    rev = out["revision"]

        # 6. deploy (builds are capped; cases waiting here hold no build)
        BUILD_SEM.acquire()
        r["_holds_build"] = True
        for _try in range(40):
            code, dep = p.req("POST", f"/apps/{aid}/deployments", {"spec_revision": rev} if rev else {})
            if "CAPACITY_WOULD_OVERSUBSCRIBE" not in json.dumps(dep):
                break
            r["capacity_waits"] = _try + 1
            time.sleep(30)
        r["steps"]["deploy"] = code
        if code >= 300:
            r.update(outcome="PASS_REFUSED" if r["expect"] == "refuse" else "FAIL_PLAN", detail=json.dumps(dep)[:2000])
            return r
        t1 = time.time()
        while True:
            dd = p.get(f"/apps/{aid}/deployments/{dep['id']}")
            if dd.get("status") in ("succeeded", "failed", "superseded") or time.time() - t1 > 2400:
                break
            time.sleep(5)
        r["deploy_seconds"] = round(time.time() - t1)
        BUILD_SEM.release()
        r.pop("_holds_build", None)
        r["deploy_status"] = dd.get("status")
        if dd.get("status") != "succeeded":
            logs = p.sse(f"/apps/{aid}/deployments/{dep['id']}/logs", timeout=20)
            r["deploy_error"] = json.dumps({k: dd.get(k) for k in ("status", "error", "failure", "message", "phase") if dd.get(k)})[:1500]
            r["deploy_log_tail"] = logs[-4000:]
            r.update(outcome="FAIL_DEPLOY" if r["expect"] == "web" else "FAIL_FALSE_POSITIVE",
                     detail=r["deploy_error"])
            return r

        # 7. reachability through Pando's proxy
        path = c.get("http_path") or "/"
        want = c.get("expect_text")
        t2 = time.time()
        hc, body = 0, ""
        r["address"] = (p.get(f"/apps/{aid}") or {}).get("address") or f"/{r['slug']}/"
        while time.time() - t2 < int(os.environ.get("QA_HTTP_WAIT", "90")):
            hc, body = p.fetch_app(r["address"], path)
            if hc and hc < 500 and not (want and want not in body and hc == 200 and time.time() - t2 < 60):
                break
            time.sleep(5)
        r["http_status"] = hc
        r["http_body"] = body[:600]
        r["marker_ok"] = (want in body) if want else None
        st = p.get(f"/apps/{aid}/status")
        r["app_status"] = json.dumps(st)[:800]
        if r["expect"] == "refuse":
            r.update(outcome="FAIL_FALSE_POSITIVE", detail=f"deployed something for a repo that is not a web app (HTTP {hc})")
        elif hc == 0 or hc >= 500:
            r["app_logs"] = json.dumps(p.get(f"/apps/{aid}/logs?tail=80"))[-3000:]
            r.update(outcome="FAIL_UNREACHABLE", detail=f"deployed but proxy returned {hc}: {body[:300]}")
        elif want and want not in body:
            r.update(outcome="FAIL_WRONG_CONTENT", detail=f"HTTP {hc} but marker {want!r} missing: {body[:300]}")
        else:
            r.update(outcome="PASS_ZERO_TOUCH" if not r["answered"] and not r["slots_filled"] else "PASS_WITH_INPUT",
                     detail=f"HTTP {hc}")
        return r
    except Exception as e:
        r.update(outcome="HARNESS_ERROR", detail=traceback.format_exc()[-1500:])
        return r
    finally:
        if r.pop("_holds_build", None):
            BUILD_SEM.release()
        if r.pop("_holds_clone", None):
            CLONE_SEM.release()
        if "image_port" in r:
            IMAGE_PORTS.put(r["image_port"])
        r["total_seconds"] = round(time.time() - t0)
        if not os.environ.get("QA_KEEP"):
            t3 = time.time()
            left, pando_left = cleanup.clean_app(p, aid)
            r["cleanup_seconds"] = round(time.time() - t3)
            r["residue"] = left
            # What Pando's own delete left behind, before the sweep removed it.
            # Not a stop: the sweep still cleaned it up. It is a leak in Pando,
            # counted in the report so it cannot hide behind clean=OK again.
            r["pando_left"] = pando_left
            if pando_left:
                log(f"PANDO LEFT for {c['id']} ({aid}): {json.dumps(pando_left)[:400]}")
            if left:
                STOP.set()
                log(f"CLEANUP RESIDUE for {c['id']} ({aid}): {json.dumps(left)} -- stopping the run")


def run_pass(label, concurrency=10, only=(), max_builds=12, max_clones=4, min_free_gb=40, http_wait=90):
    """Run every case not yet in results-<label>.jsonl.

    Returns (cases left unfinished, whether the run stopped on cleanup residue).
    """
    global BUILD_SEM, CLONE_SEM
    BUILD_SEM = threading.Semaphore(max_builds)
    CLONE_SEM = threading.Semaphore(max_clones)
    os.environ["QA_HTTP_WAIT"] = str(http_wait)
    st = state.load()
    while not IMAGE_PORTS.empty():
        IMAGE_PORTS.get()
    for port in range(st["app_port_end"] - 2, st["app_port_end"] + 1):
        IMAGE_PORTS.put(port)
    out = state.results_file(label)
    LOGFILE["path"] = state.log_file(label)
    STOP.clear()

    cases = state.load_cases()
    done = set()
    if os.path.exists(out):
        for line in open(out):
            try:
                done.add(json.loads(line)["id"])
            except Exception:
                pass
    only = set(only or ())
    todo = [c for c in cases if c["id"] not in done and (not only or c["id"] in only)]
    log(f"{len(todo)} cases to run ({len(done)} already done)")
    counter = {"n": 0}

    def worker(c):
        if STOP.is_set():
            return
        cleanup.wait_for_disk(min_free_gb, log)
        if STOP.is_set():
            return
        p = Pando()
        log(f"start {c['id']}  (disk free {cleanup.free_gb():.0f} GB)")
        epoch = RESTARTS["n"]
        with lock:
            INFLIGHT["n"] += 1
        try:
            r = run_case(p, c, label)
        finally:
            with lock:
                INFLIGHT["n"] -= 1
                LAST_DONE["t"] = time.time()
        if RESTARTS["n"] != epoch:
            log(f"requeue {c['id']} (Pando was restarted while it ran)")
            return
        with lock:
            with open(out, "a") as fh:
                fh.write(json.dumps(r) + "\n")
        log(f"done  {c['id']:34} {r.get('outcome'):20} {r.get('total_seconds')}s "
            f"clean={'OK' if not r.get('residue') else 'RESIDUE'}  {str(r.get('detail'))[:120]}")
        with lock:
            counter["n"] += 1
            do_prune = counter["n"] % 10 == 0
        if do_prune:
            threading.Thread(target=cleanup.prune_build_cache, daemon=True).start()

    threading.Thread(target=watchdog, args=(log,), daemon=True).start()
    with ThreadPoolExecutor(concurrency) as ex:
        list(ex.map(worker, todo))
    cleanup.final_sweep()
    log("stopped: cleanup residue" if STOP.is_set() else "all done")
    finished = set()
    if os.path.exists(out):
        finished = {json.loads(l)["id"] for l in open(out)}
    return len([c for c in todo if c["id"] not in finished]), STOP.is_set()

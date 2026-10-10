#!/usr/bin/env python3
"""Start Pando from a compose file and check that it works as an operator
would use it: it comes up, reports its version, serves the console, takes its
first administrator, and deploys an app that is then reachable through it.

The release workflow runs this against the compose file it ships and the image
it just pushed (issue #52), so a release whose image cannot run an app fails
before anyone downloads it. CI runs it against an image built from the pull
request.

Everything runs under its own compose project and ports, and everything it
creates is removed at the end: the compose project with its volumes, and the
container and network Pando made for the test app.

usage: scripts/smoke-image.py COMPOSE_FILE [--version 0.3.0] [--pull] [--keep]
"""
import argparse
import http.cookiejar
import json
import os
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.request

PROJECT = "pando-smoke"
PORT = int(os.environ.get("SMOKE_PORT", "18080"))
APP_PORTS = (19000, 19004)
APP_IMAGE = "traefik/whoami:v1.10.3"  # small, multi-arch, answers any path

BASE = f"http://localhost:{PORT}"
jar = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))


def compose(args, *cmd, check=True, capture=False):
    env = dict(os.environ,
               PANDO_PORT=str(PORT),
               PANDO_APP_PORT_START=str(APP_PORTS[0]),
               PANDO_APP_PORT_END=str(APP_PORTS[1]),
               POSTGRES_PASSWORD=args.db_password)
    return subprocess.run(["docker", "compose", "-p", PROJECT, "-f", args.compose, *cmd],
                          env=env, check=check, text=True, capture_output=capture)


def request(method, path, body=None, timeout=30):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"} if data else {})
    try:
        with opener.open(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", "replace")
            return resp.status, raw
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except OSError as e:
        return 0, str(e)


def api(method, path, body=None):
    code, raw = request(method, "/api/v1" + path, body)
    try:
        return code, json.loads(raw) if raw else {}
    except ValueError:
        return code, {"raw": raw[:500]}


# Once a second: every check is a request to a server on this machine, and a
# longer interval was only ever time spent after the thing had happened.
def wait(what, fn, timeout, every=1):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        ok, last = fn()
        if ok:
            return last
        time.sleep(every)
    raise SystemExit(f"FAIL: {what} within {timeout}s (last: {str(last)[:600]})")


def step(msg):
    print(f"--- {msg}", flush=True)


def run(args):
    if args.pull:
        step("pulling the images the compose file names")
        compose(args, "pull")
    step("starting the stack")
    compose(args, "up", "-d")

    wait("Pando did not become ready", lambda: (request("GET", "/readyz")[0] == 200, request("GET", "/readyz")), 240)
    code, body = request("GET", "/healthz")
    assert code == 200, f"/healthz answered {code}: {body}"
    step("ready")

    out = compose(args, "exec", "-T", "pando", "pando", "version", capture=True).stdout
    print(out.strip())
    if args.version:
        want = f"pando {args.version.lstrip('v')}"
        if want not in out:
            raise SystemExit(f"FAIL: `pando version` says {out.strip()!r}, expected {want!r}")

    # What the hardened runtime base does not carry and the image copies in
    # (Dockerfile, the packages stage): a library missing from that copy shows
    # up here rather than at the first backup. And the server must not be root.
    checks = ("set -e; pg_dump --version; pg_restore --version; test -f /usr/share/zoneinfo/UTC; "
              "echo \"server runs as $(stat -c %U /proc/1)\"")
    out = compose(args, "exec", "-T", "pando", "sh", "-c", checks, capture=True, check=False)
    print(out.stdout.strip())
    if out.returncode != 0 or "(PostgreSQL) 17" not in out.stdout:
        raise SystemExit(f"FAIL: the image's backup tools or time zones are missing: {out.stdout} {out.stderr}")
    if "server runs as root" in out.stdout:
        raise SystemExit("FAIL: the server runs as root; the entrypoint did not drop privileges")
    step("backup tools present, server not root")

    code, body = request("GET", "/")
    if code != 200 or "<html" not in body.lower():
        raise SystemExit(f"FAIL: the console did not load: {code} {body[:300]}")
    step("console served")

    # The setup token, the way an operator who lost the log line gets one
    # (R-046, issue #130). It goes to stdout alone; the logger writes stderr.
    token = compose(args, "exec", "-T", "pando", "pando", "admin", "setup-token", capture=True).stdout.strip()
    if not token:
        raise SystemExit("FAIL: pando admin setup-token printed no token")
    code, out = api("POST", "/setup", {"setup_token": "not-the-token", "username": "smoke", "password": "x" * 20})
    if code != 401:
        raise SystemExit(f"FAIL: first-run setup without the setup token answered {code}, not 401: {out}")
    password = secrets.token_urlsafe(18)
    code, out = api("POST", "/setup", {"setup_token": token, "username": "smoke", "display_name": "Smoke test", "password": password})
    if code >= 300:
        raise SystemExit(f"FAIL: first-run setup answered {code}: {out}")
    step("first administrator set up")

    code, app = api("POST", "/apps", {"name": "smoke-whoami", "source": {"type": "image", "image": APP_IMAGE}})
    if code >= 300:
        raise SystemExit(f"FAIL: creating an app answered {code}: {app}")
    app_id = app["id"]
    args.app_id = app_id

    def detected():
        _, d = api("GET", f"/apps/{app_id}/detection")
        return d.get("status") in ("ready", "needs_answers", "blocked", "failed"), d
    d = wait("detection did not finish", detected, 300)
    if d.get("status") != "ready":
        raise SystemExit(f"FAIL: detection ended {d.get('status')}: {json.dumps(d)[:800]}")
    code, out = api("POST", f"/apps/{app_id}/detection/accept", {})
    if code >= 300:
        raise SystemExit(f"FAIL: accepting the proposal answered {code}: {out}")
    step("detected and accepted")

    code, dep = api("POST", f"/apps/{app_id}/deployments", {})
    if code >= 300:
        raise SystemExit(f"FAIL: starting a deploy answered {code}: {dep}")

    def deployed():
        _, dd = api("GET", f"/apps/{app_id}/deployments/{dep['id']}")
        return dd.get("status") in ("succeeded", "failed", "canceled"), dd
    dd = wait("the deploy did not finish", deployed, 420)
    if dd.get("status") != "succeeded":
        raise SystemExit(f"FAIL: the deploy ended {dd.get('status')}: {json.dumps(dd)[:800]}")
    step("deployed")

    slug = app.get("slug") or api("GET", f"/apps/{app_id}")[1].get("slug")
    body = wait("the app was not reachable through Pando",
                lambda: (lambda r: (r[0] == 200 and "Hostname" in r[1], r))(request("GET", f"/{slug}/")), 120)
    step(f"app reachable through Pando's proxy at /{slug}/")
    print(body[1].splitlines()[0] if body[1] else "")

    code, out = api("DELETE", f"/apps/{app_id}?force=true")
    if code >= 300:
        raise SystemExit(f"FAIL: deleting the app answered {code}: {out}")
    step("PASS")


def cleanup(args):
    if args.keep:
        print(f"--keep: leaving compose project {PROJECT} running on {BASE}")
        return
    if args.app_id:
        # Wait for Pando's own teardown of the test app before stopping it.
        ulid = args.app_id.split("_", 1)[1].lower()
        for _ in range(20):
            names = subprocess.run(["docker", "ps", "-a", "--format", "{{.Names}}"],
                                   capture_output=True, text=True).stdout
            if ulid not in names.lower():
                break
            time.sleep(3)
    compose(args, "down", "-v", "--remove-orphans", check=False)
    if args.app_id:
        # The app's network outlives Pando's container by design (removed at the
        # next start), and there is no next start here. Only what carries this
        # test app's ID is touched.
        ulid = args.app_id.split("_", 1)[1].lower()
        for kind, ls, rm in (("container", ["ps", "-a"], ["rm", "-f", "-v"]),
                             ("network", ["network", "ls"], ["network", "rm"]),
                             ("volume", ["volume", "ls"], ["volume", "rm"])):
            names = subprocess.run(["docker", *ls, "--format", "{{.Name}}" if kind != "container" else "{{.Names}}"],
                                   capture_output=True, text=True).stdout.split()
            for n in names:
                if ulid in n.lower():
                    subprocess.run(["docker", *rm, n], capture_output=True)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("compose")
    ap.add_argument("--version", help="the version `pando version` must report")
    ap.add_argument("--pull", action="store_true", help="pull the images first (a published release)")
    ap.add_argument("--keep", action="store_true", help="leave the stack running afterwards")
    args = ap.parse_args()
    args.db_password = secrets.token_urlsafe(18)
    args.app_id = None
    try:
        run(args)
    except BaseException:
        print("--- pando logs (last 120 lines)", flush=True)
        compose(args, "logs", "--tail", "120", "pando", check=False)
        raise
    finally:
        cleanup(args)


if __name__ == "__main__":
    sys.exit(main())

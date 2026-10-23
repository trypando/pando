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

With --build it also deploys an app Pando builds itself, from an uploaded
directory, which runs Pando's BuildKit container on the same runtime. With
--restart-user-services it restarts this user's systemd instance once the
first app is up, which stops every rootless container as a reboot does, and
requires Pando and the app to come back.

SMOKE_PROJECT names the compose project (default pando-smoke), and
POSTGRES_PASSWORD, if set, is the database password, so the smoke can drive an
install whose .env names the same.

usage: scripts/smoke-image.py COMPOSE_FILE [--version 0.3.0] [--pull] [--build] [--restart-user-services] [--keep]
"""
import argparse
import http.cookiejar
import io
import json
import os
import secrets
import subprocess
import sys
import tarfile
import time
import urllib.error
import urllib.request

PROJECT = os.environ.get("SMOKE_PROJECT", "pando-smoke")
PORT = int(os.environ.get("SMOKE_PORT", "18080"))
APP_PORTS = (19000, 19004)
APP_IMAGE = "traefik/whoami:v1.10.3"  # small, multi-arch, answers any path

BASE = f"http://localhost:{PORT}"
READYZ = "/readyz"
NAMES = "{{.Names}}"  # docker ps --format, a container per line
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


def request(method, path, body=None, timeout=30, raw=None, content_type=None):
    data = json.dumps(body).encode() if body is not None else raw
    headers = {"Content-Type": content_type or "application/json"} if data else {}
    url = path if path.startswith("http") else BASE + path
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with opener.open(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", "replace")
            return resp.status, raw
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except OSError as e:
        return 0, str(e)


def app_url(address):
    """Where an app is reached, from the address the API gives for it. An app
    on a port or a hostname is served there only, not at /<slug> on Pando's
    own address (R-166)."""
    address = address or ""
    if address.startswith("//"):  # port mode: on the host Pando was reached on
        return "http:" + address
    if address.startswith("/"):  # path mode
        return BASE + address
    return address


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

    wait_ready("Pando did not become ready", 240)
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

    deploy(args, "smoke-whoami", {"type": "image", "image": APP_IMAGE}, "Hostname", restart=args.restart_user_services)
    if args.build:
        deploy(args, "smoke-static", {"type": "upload"}, BUILT_MARKER, files={"index.html": BUILT_PAGE})
    step("PASS")


# What the app Pando builds serves, to be read back through its proxy.
BUILT_MARKER = "pando-smoke-built"
BUILT_PAGE = f"<!doctype html><title>{BUILT_MARKER}</title><p>{BUILT_MARKER}</p>\n"


def wait_ready(what, timeout):
    def ready():
        r = request("GET", READYZ)
        return r[0] == 200, r
    wait(what, ready, timeout)


def must(code, what, out):
    if code >= 300:
        raise SystemExit(f"FAIL: {what} answered {code}: {str(out)[:300]}")


def gzipped_tar(files):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tar:
        for path, text in files.items():
            data = text.encode()
            info = tarfile.TarInfo(path)
            info.size = len(data)
            tar.addfile(info, io.BytesIO(data))
    return buf.getvalue()


def detect(app_id, name):
    def detected():
        _, d = api("GET", f"/apps/{app_id}/detection")
        return d.get("status") in ("ready", "needs_answers", "blocked", "failed"), d
    d = wait(f"detection of {name} did not finish", detected, 300)
    if d.get("status") != "ready":
        raise SystemExit(f"FAIL: detection of {name} ended {d.get('status')}: {json.dumps(d)[:800]}")
    code, out = api("POST", f"/apps/{app_id}/detection/accept", {})
    must(code, f"accepting {name}'s proposal", out)
    step(f"{name} detected and accepted")


def run_deploy(app_id, name):
    code, dep = api("POST", f"/apps/{app_id}/deployments", {})
    must(code, f"starting {name}'s deploy", dep)

    def deployed():
        _, dd = api("GET", f"/apps/{app_id}/deployments/{dep['id']}")
        return dd.get("status") in ("succeeded", "failed", "canceled"), dd
    dd = wait(f"{name}'s deploy did not finish", deployed, 600)
    if dd.get("status") != "succeeded":
        raise SystemExit(f"FAIL: {name}'s deploy ended {dd.get('status')}: {json.dumps(dd)[:800]}")
    step(f"{name} deployed")


def restart_user_services(name, reachable):
    unit = f"user@{os.getuid()}.service"
    step(f"stopping {unit}, then starting it")
    # Stopped in full first, as a reboot does. `restart` started it again
    # while the old Podman service was still going, and podman.socket hit
    # systemd's trigger limit and refused to start, which no boot does.
    subprocess.run(["sudo", "systemctl", "stop", unit], check=True)
    subprocess.run(["sudo", "systemctl", "start", unit], check=True)
    wait_ready("Pando did not come back after the restart", 300)
    step("Pando is back")
    subprocess.run(["docker", "ps", "-a", "--format", "{{.Names}}\t{{.State}}\t{{.Status}}"], check=False)
    wait(f"{name} was not reachable through Pando after the restart", reachable, 300)
    step(f"{name} is back")


def deploy(args, name, source, expect, files=None, restart=False):
    """Creates an app, deploys it, reads `expect` from it through Pando's
    proxy, and deletes it. `files` are sent as its source for an upload;
    with `restart`, the user's services are restarted while it is up, and it
    must be up again after."""
    code, app = api("POST", "/apps", {"name": name, "source": source})
    must(code, f"creating {name}", app)
    app_id = app["id"]
    args.app_ids.append(app_id)

    if files:
        code, out = request("POST", f"/api/v1/apps/{app_id}/source", raw=gzipped_tar(files), content_type="application/gzip")
        must(code, f"sending {name}'s files", out)
        code, out = api("POST", f"/apps/{app_id}/detection/rerun", {})
        must(code, f"detecting {name} again", out)

    detect(app_id, name)
    run_deploy(app_id, name)

    url = app_url(api("GET", f"/apps/{app_id}")[1].get("address"))

    def reachable():
        r = request("GET", url)
        return r[0] == 200 and expect in r[1], r
    body = wait(f"{name} was not reachable through Pando", reachable, 120)
    step(f"{name} reachable through Pando's proxy at {url}")
    print(body[1].splitlines()[0] if body[1] else "")

    if restart:
        restart_user_services(name, reachable)

    code, out = api("DELETE", f"/apps/{app_id}?force=true")
    must(code, f"deleting {name}", out)


def cleanup(args):
    if args.keep:
        print(f"--keep: leaving compose project {PROJECT} running on {BASE}")
        return
    ulids = [a.split("_", 1)[1].lower() for a in args.app_ids]
    # Wait for Pando's own teardown of the test apps before stopping it.
    for _ in range(20 if ulids else 0):
        names = subprocess.run(["docker", "ps", "-a", "--format", NAMES],
                               capture_output=True, text=True).stdout.lower()
        if not any(u in names for u in ulids):
            break
        time.sleep(3)
    compose(args, "down", "-v", "--remove-orphans", check=False)
    if ulids:
        # The app's network outlives Pando's container by design (removed at the
        # next start), and there is no next start here. Only what carries this
        # test app's ID is touched.
        for kind, ls, rm in (("container", ["ps", "-a"], ["rm", "-f", "-v"]),
                             ("network", ["network", "ls"], ["network", "rm"]),
                             ("volume", ["volume", "ls"], ["volume", "rm"])):
            names = subprocess.run(["docker", *ls, "--format", "{{.Name}}" if kind != "container" else NAMES],
                                   capture_output=True, text=True).stdout.split()
            for n in names:
                if any(u in n.lower() for u in ulids):
                    subprocess.run(["docker", *rm, n], capture_output=True)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("compose")
    ap.add_argument("--version", help="the version `pando version` must report")
    ap.add_argument("--pull", action="store_true", help="pull the images first (a published release)")
    ap.add_argument("--build", action="store_true", help="also deploy an app Pando builds from uploaded files")
    ap.add_argument("--restart-user-services", action="store_true",
                    help="restart this user's systemd instance once the first app is up, standing in for a reboot")
    ap.add_argument("--keep", action="store_true", help="leave the stack running afterwards")
    args = ap.parse_args()
    args.db_password = os.environ.get("POSTGRES_PASSWORD") or secrets.token_urlsafe(18)
    args.app_ids = []
    try:
        run(args)
    except BaseException:
        if args.restart_user_services:
            print("--- user services", flush=True)
            subprocess.run(["systemctl", "--user", "status", "--no-pager", "podman.socket", "pando.service"], check=False)
            journal = subprocess.run(["journalctl", "--user", "--no-pager", "-n", "20000"],
                                     capture_output=True, text=True).stdout.splitlines()
            print("\n".join(l for l in journal if "/start" in l or "/restart" in l), flush=True)
        print("--- containers", flush=True)
        subprocess.run(["docker", "ps", "-a", "--format", "{{.Names}}\t{{.State}}\t{{.Status}}\t{{.Image}}"], check=False)
        # Why the reconciler's last correction failed, which only the database holds.
        compose(args, "exec", "-T", "postgres", "psql", "-U", "pando", "-d", "pando", "-c",
                "SELECT id, state, consecutive_failures, last_reconcile_error FROM apps", check=False)
        for name in [n for n in subprocess.run(["docker", "ps", "-a", "--format", NAMES], capture_output=True,
                                                text=True).stdout.split() if n.endswith("-buildkit")]:
            print(f"--- {name} logs", flush=True)
            subprocess.run(["docker", "logs", "--tail", "40", name], check=False)
        # What the engine says when asked to start a stopped app, or a build
        # service that was never started, by hand.
        stopped = subprocess.run(["docker", "ps", "-a", "--filter", "status=exited", "--filter", "status=created",
                                  "--format", NAMES], capture_output=True, text=True).stdout.split()
        for name in [n for n in stopped if n.startswith("pando-app_") or n.endswith("-buildkit")]:
            print(f"--- {name}", flush=True)
            subprocess.run(["docker", "inspect", "-f", "{{json .State}}", name], check=False)
            subprocess.run(["docker", "logs", "--tail", "20", name], check=False)
            print(f"--- docker start {name}", flush=True)
            subprocess.run(["docker", "start", name], check=False)
        # Without the line every request writes, which a wait that polls an
        # app fills the tail with.
        print("--- pando logs, requests left out (last 150 lines)", flush=True)
        out = compose(args, "logs", "--no-color", "pando", check=False, capture=True).stdout.splitlines()
        noise = ('"msg":"request"', '"msg":"upstream failed"', '"msg":"proxy refused"')
        print("\n".join([l for l in out if not any(n in l for n in noise)][-150:]), flush=True)
        raise
    finally:
        cleanup(args)


if __name__ == "__main__":
    sys.exit(main())

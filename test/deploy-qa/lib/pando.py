"""Minimal client for the QA instance's API."""
import http.cookiejar
import json
import time
import urllib.error
import urllib.request

from . import state


class Pando:
    def __init__(self):
        st = state.load()
        self.base = f"http://localhost:{st['port']}"
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        code, out = self.req("POST", "/sessions", {"username": "admin", "password": st["admin_password"]})
        if code >= 300:
            raise RuntimeError(f"could not sign in to the QA instance: {code} {out}")
        if out.get("must_change_password"):
            # R-046: a password set in the environment is changed at first
            # sign-in before the session can do anything else. To itself here.
            code, out = self.req("POST", "/me/password", {
                "current_password": st["admin_password"], "new_password": st["admin_password"]})
            if code >= 300:
                raise RuntimeError(f"could not keep the QA instance's password: {code} {out}")

    def req(self, method, path, body=None, raw=None, ctype="application/json", timeout=180):
        url = self.base + "/api/v1" + path
        data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
        r = urllib.request.Request(url, data=data, method=method)
        if data is not None:
            r.add_header("Content-Type", ctype)
        # Only reads are retried: repeating a POST could create a second app or deployment.
        tries = 4 if method == "GET" else 1
        for attempt in range(tries):
            try:
                with self.op.open(r, timeout=timeout) as resp:
                    txt, code = resp.read().decode("utf-8", "replace"), resp.status
                break
            except urllib.error.HTTPError as e:
                txt, code = e.read().decode("utf-8", "replace"), e.code
                break
            except (TimeoutError, ConnectionError, urllib.error.URLError):
                if attempt == tries - 1:
                    raise
                time.sleep(5 * (attempt + 1))
        try:
            out = json.loads(txt) if txt.strip() else {}
        except json.JSONDecodeError:
            out = {"_raw": txt}
        return code, out

    def get(self, path):
        return self.req("GET", path)[1]

    def sse(self, path, timeout=20):
        """Read a server-sent-event stream until it ends or goes quiet."""
        r = urllib.request.Request(self.base + "/api/v1" + path)
        lines = []
        try:
            with self.op.open(r, timeout=timeout) as resp:
                for raw in resp:
                    s = raw.decode("utf-8", "replace").rstrip("\n")
                    if s.startswith("data: "):
                        lines.append(s[6:])
        except Exception as e:
            lines.append(f"[log stream ended: {e.__class__.__name__}]")
        return "\n".join(lines)

    def fetch_app(self, slug, path="/", timeout=15):
        """GET an app through Pando's proxy, signed in."""
        try:
            with self.op.open(urllib.request.Request(f"{self.base}/{slug}{path}"), timeout=timeout) as resp:
                return resp.status, resp.read(4096).decode("utf-8", "replace")
        except urllib.error.HTTPError as e:
            return e.code, e.read(4096).decode("utf-8", "replace")
        except Exception as e:
            return 0, f"{e.__class__.__name__}: {e}"

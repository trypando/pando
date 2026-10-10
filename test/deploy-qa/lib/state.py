"""Where a run keeps its settings and output.

Everything a run writes lives under test/deploy-qa/out/ (git-ignored). The
instance's settings are in out/state.json so every subcommand, and the
dashboard in another terminal, talks to the same instance.
"""
import json
import os

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # test/deploy-qa
REPO = os.path.dirname(os.path.dirname(HERE))
OUT = os.path.join(HERE, "out")
STATE = os.path.join(OUT, "state.json")

PROJECT = "pando-qa"
PANDO = f"{PROJECT}-pando-1"
# Started by the QA instance's Pando, not Compose, and named after its Compose
# project (internal/adapter/builder/buildkit/managed.go).
BUILDKIT = f"{PROJECT}-buildkit"
POSTGRES = f"{PROJECT}-postgres-1"
# Every test source (the generated apps and the lists of public repositories
# and images) lives in trypando/pando-qa-fixtures, so Pando's repository does
# not carry their Dockerfiles, compose files and app configs. `qa.py up` clones
# it here at the commit pinned in qa.py.
FIXTURES = os.path.join(OUT, "fixtures")
CASE_FILES = [os.path.join(FIXTURES, "cases", f) for f in ("generated.json", "public-repos.json", "images.json")]


def load():
    try:
        with open(STATE) as f:
            return json.load(f)
    except FileNotFoundError:
        raise SystemExit("No QA instance is set up. Run `python3 test/deploy-qa/qa.py up` first.")


def save(st):
    os.makedirs(OUT, exist_ok=True)
    tmp = STATE + ".tmp"
    with open(tmp, "w") as f:
        json.dump(st, f, indent=1)
    os.replace(tmp, STATE)


def path(*parts):
    return os.path.join(OUT, *parts)


def results_file(label):
    return path(f"results-{label}.jsonl")


def log_file(label):
    return path(f"run-{label}.log")


def load_cases():
    if not os.path.exists(CASE_FILES[0]):
        raise SystemExit("The fixtures are not fetched. Run `python3 test/deploy-qa/qa.py up` first.")
    cases = []
    for f in CASE_FILES:
        with open(f) as fh:
            for c in json.load(fh):
                if c.get("kind") == "upload":
                    c["path"] = os.path.join(FIXTURES, c["path"])
                    c.setdefault("source", "generated")
                c.setdefault("packaging", c.get("category"))
                c.setdefault("language", c.get("category"))
                cases.append(c)
    return cases

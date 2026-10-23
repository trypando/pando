#!/usr/bin/env python3
"""Fail if an image still trusts a root certificate that has been distrusted.

Pando's server is built with a Go that refuses these roots' compromised chains
itself, but everything else in an image — git, Railpack, mise, psql, and the
programs in the images Pando pins — trusts the CA bundle the image carries. A
base image pinned by digest keeps the bundle it was built with, so a root that
browsers and distributions have since removed stays trusted until someone bumps
the digest. This makes that a failed check rather than something to remember.

Every certificate under /etc/ssl is read, the bundle and any per-certificate
files beside it, and matched by SHA-256 fingerprint, since a bundle carries no
names. A per-certificate file linked from elsewhere is not followed; its
certificate is in the bundle too. An image with no certificates under /etc/ssl passes: it trusts nothing,
and whatever is built on it installs its own (debian:trixie-slim).

Images are named on the command line, or read with --from from a Compose file
or Dockerfile, where every `image:` and `FROM` reference pinned by digest is
checked.

Usage:
    scripts/check-trust-store.py trypando/pando:0.0.0-ci
    scripts/check-trust-store.py --from docker-compose.yml --from internal/adapter/builder/buildkit/bases/Dockerfile
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import io
import re
import subprocess
import sys
import tarfile
import uuid
from pathlib import Path

# Roots removed from Mozilla's store (NSS 3.131.1) and the distributions' after
# eMudhra's intermediate keys were found to be recoverable, October 2026
# (CVE-2026-111943). AAA Certificate Services cross-signed those intermediates;
# Mozilla had already removed it, and Debian and Ubuntu removed it now.
DISTRUSTED = {
    "86a1ecba089c4a8d3bbe2734c612ba341d813e043cf9e8a862cd5c57a36bbe6b": "emSign ECC Root CA - G3",
    "bc4d809b15189d78db3e1d8cf4f9726a795da1643ca5f1358e1ddb0edc0d7eb3": "emSign ECC Root CA - C3",
    "125609aa301da0a249b97a8239cb6a34216f44dcac9f3954b14292f2e8c8608f": "emSign Root CA - C1",
    "d7a7a0fb5d7e2731d771e9484ebcdef71d5f0c3e0a2948782bc83ee0ea699ef4": "AAA Certificate Services",
}

PEM = re.compile(rb"-----BEGIN CERTIFICATE-----(.*?)-----END CERTIFICATE-----", re.S)
REFERENCE = re.compile(r"^\s*(?:image:|FROM)\s+(?:--platform=\S+\s+)?(\S+@sha256:[0-9a-f]{64})", re.I | re.M)


def fingerprints(data: bytes) -> set[str]:
    """SHA-256 fingerprints of every PEM certificate in data."""
    return {hashlib.sha256(base64.b64decode(b"".join(body.split()))).hexdigest() for body in PEM.findall(data)}


def trusted(archive: bytes) -> set[str]:
    """Fingerprints of every certificate in a tar of /etc/ssl, as docker cp writes it."""
    found: set[str] = set()
    with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
        for member in tar:
            if member.isfile():
                found |= fingerprints(tar.extractfile(member).read())
    return found


def references(path: Path) -> list[str]:
    """Image references pinned by digest in a Compose file or Dockerfile."""
    return REFERENCE.findall(path.read_text())


def etc_ssl(image: str) -> bytes:
    """A tar of the image's /etc/ssl, empty only if the image has none.

    Any other failure to read it raises, so an image that could not be read is
    never reported as one that trusts nothing.
    """
    name = f"pando-trust-{uuid.uuid4().hex[:12]}"
    # Long enough to pull an image the runner does not have yet.
    created = subprocess.run(["docker", "create", "--name", name, image], capture_output=True, timeout=600)
    if created.returncode != 0:
        raise RuntimeError(f"could not create a container from {image}: {created.stderr.decode().strip()}")
    try:
        cp = subprocess.run(["docker", "cp", "-L", f"{name}:/etc/ssl", "-"], capture_output=True, timeout=120)
        if cp.returncode == 0:
            return cp.stdout
        if b"Could not find the file /etc/ssl" in cp.stderr:
            return b""
        raise RuntimeError(f"could not read /etc/ssl from {image}: {cp.stderr.decode().strip()}")
    finally:
        subprocess.run(["docker", "rm", "-f", name], capture_output=True, timeout=60)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("images", nargs="*")
    parser.add_argument("--from", dest="files", action="append", default=[], type=Path)
    args = parser.parse_args()

    images = list(dict.fromkeys(args.images + [ref for f in args.files for ref in references(f)]))
    if not images:
        parser.error("name an image, or a file with --from")

    failed = False
    for image in images:
        try:
            archive = etc_ssl(image)
        except (RuntimeError, subprocess.TimeoutExpired) as err:
            failed = True
            print(f"FAIL {image} could not be checked: {err}")
            continue
        bad = sorted(DISTRUSTED[fp] for fp in trusted(archive) & DISTRUSTED.keys()) if archive else []
        if bad:
            failed = True
            print(f"FAIL {image} trusts {', '.join(bad)}. Rebuild it on, or bump it to, a base with a current CA bundle.")
        else:
            print(f"ok   {image}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())

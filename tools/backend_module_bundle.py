#!/usr/bin/env python3
"""Pack every Go module the backend needs into one file, for building it
on a machine that cannot download them.

The backend's SQLite driver (modernc.org/sqlite and its libraries) is
served from gitlab.com, and every module normally comes through
proxy.golang.org. A machine that can reach neither, such as a
restricted network or an outside reviewer's sandbox, cannot build the
backend from a checkout. This file carries the modules there.

On a machine with internet access, from a clean checkout:

    python3 tools/backend_module_bundle.py

It writes mesedi-backend-modules-<commit>.tar.gz and prints its SHA-256.
On the other machine, from a checkout of the same commit:

    mkdir -p /tmp/mesedi-modules
    tar -xzf mesedi-backend-modules-<commit>.tar.gz -C /tmp/mesedi-modules
    cd backend
    GOPROXY=file:///tmp/mesedi-modules/download GOSUMDB=off go build ./...

Why this is safe to accept from someone else: Go checks every module it
reads from the file against backend/go.sum, which comes from the
checkout, not from the file. A file with an altered module is refused
with "checksum mismatch". GOSUMDB=off only skips the online checksum
database, which go.sum already covers for every module the backend uses.

The file holds modules only, not a Go toolchain: the other machine needs
the Go version named in backend/go.mod, or newer, already installed.

Exit codes: 0 written, 1 a go command failed, 2 the checkout is not clean
or a path is wrong.
"""

from __future__ import annotations

import argparse
import hashlib
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
from typing import Callable, List, Optional

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)

# The host's own Go download, fetched when the local Go is older than
# backend/go.mod asks for. It is for this machine only, so it is left
# out of the file.
TOOLCHAIN_DIR = os.path.join("golang.org", "toolchain")

Runner = Callable[[List[str], str, dict], int]


def _say(text: str, err: bool = False) -> None:
    (sys.stderr if err else sys.stdout).write(text + "\n")


def _run(cmd: List[str], cwd: str, env: dict) -> int:
    return subprocess.run(cmd, cwd=cwd, env=env).returncode


def bundle_name(commit: str) -> str:
    return f"mesedi-backend-modules-{commit}.tar.gz"


def git_output(repo: str, *args: str) -> str:
    return subprocess.run(
        ["git", "-C", repo, *args], check=True, capture_output=True, text=True
    ).stdout.strip()


def dirty_backend_paths(repo: str) -> List[str]:
    """Changed or untracked files under backend/, which would make the
    file differ from the commit it is named after."""
    out = git_output(repo, "status", "--porcelain", "--", "backend")
    return [line[3:] for line in out.splitlines() if line.strip()]


def module_env(cache_dir: str) -> dict:
    """The environment for downloading into a fresh, private module cache."""
    env = dict(os.environ)
    env["GOMODCACHE"] = cache_dir
    env["GOFLAGS"] = "-modcacherw"
    return env


def drop_host_toolchain(download_dir: str) -> None:
    shutil.rmtree(os.path.join(download_dir, TOOLCHAIN_DIR), ignore_errors=True)


def write_bundle(download_dir: str, out_path: str) -> None:
    """Write the module proxy tree as download/... inside a .tar.gz."""
    with tarfile.open(out_path, "w:gz") as tar:
        tar.add(download_dir, arcname="download")


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def instructions(name: str) -> str:
    return "\n".join([
        "On the machine without internet access, from a checkout of the same commit:",
        "",
        "    mkdir -p /tmp/mesedi-modules",
        f"    tar -xzf {name} -C /tmp/mesedi-modules",
        "    cd backend",
        "    GOPROXY=file:///tmp/mesedi-modules/download GOSUMDB=off go build ./...",
        "",
        "Go checks every module against backend/go.sum from the checkout, so an",
        "altered module is refused. The Go version in backend/go.mod, or newer,",
        "must already be installed there.",
    ])


def main(argv: Optional[List[str]] = None, repo: str = REPO, run: Runner = _run) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--out", help="where to write the file (default: the current folder)")
    args = parser.parse_args(argv)

    backend = os.path.join(repo, "backend")
    if not os.path.isfile(os.path.join(backend, "go.mod")):
        _say(f"backend_module_bundle: no backend/go.mod under {repo}", err=True)
        return 2
    dirty = dirty_backend_paths(repo)
    if dirty:
        _say("backend_module_bundle: backend/ has uncommitted changes, so the file", err=True)
        _say("would not match any commit. Commit or stash them first:", err=True)
        for p in dirty:
            _say(f"  {p}", err=True)
        return 2
    commit = git_output(repo, "rev-parse", "--short=7", "HEAD")
    name = bundle_name(commit)
    out_path = os.path.abspath(args.out or name)

    with tempfile.TemporaryDirectory(prefix="mesedi-modules-") as tmp:
        cache = os.path.join(tmp, "mod")
        env = module_env(cache)
        # Without arguments, go mod download fetches every module needed
        # to build and test the backend's packages.
        if run(["go", "mod", "download"], backend, env) != 0:
            _say("backend_module_bundle: go mod download failed", err=True)
            return 1
        download = os.path.join(cache, "cache", "download")
        drop_host_toolchain(download)
        write_bundle(download, out_path)

    _say(f"Wrote {out_path}")
    _say(f"SHA-256 {sha256_file(out_path)}")
    _say("")
    _say(instructions(os.path.basename(out_path)))
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Tests for tools/backend_module_bundle.py.

The backend's SQLite driver is served from gitlab.com, so a machine that
cannot reach it (or proxy.golang.org) could not build the backend; the
outside reviewer hit exactly that in rounds 2 to 4. These tests hold the
bundle's rules without touching the network: the file is refused for a
checkout with changes, it is named after the commit, it carries the
modules as download/... and leaves out the host's own Go download, and
it prints how to use it.

Run:  python3 tools/test_backend_module_bundle.py   (standard library only)
"""

from __future__ import annotations

import importlib.util
import io
import os
import subprocess
import tarfile
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout

HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "backend_module_bundle", os.path.join(HERE, "backend_module_bundle.py")
)
mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(mod)


def _git(repo: str, *args: str) -> None:
    subprocess.run(["git", "-C", repo, *args], check=True, capture_output=True)


def _repo_with_backend(tmp: str) -> str:
    repo = os.path.join(tmp, "repo")
    os.makedirs(os.path.join(repo, "backend"))
    with open(os.path.join(repo, "backend", "go.mod"), "w") as fh:
        fh.write("module example.com/backend\n\ngo 1.27\n")
    _git(repo, "init", "-q")
    _git(repo, "add", "backend/go.mod")
    _git(repo, "-c", "user.email=t@example.com", "-c", "user.name=t",
         "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
    return repo


def _fake_download(files: dict):
    """A runner standing in for go mod download: it writes the given
    files into the private module cache it was pointed at."""
    calls = []

    def run(cmd, cwd, env):
        calls.append((cmd, cwd, env))
        root = os.path.join(env["GOMODCACHE"], "cache", "download")
        for rel, body in files.items():
            path = os.path.join(root, rel)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path, "w") as fh:
                fh.write(body)
        return 0

    return run, calls


class BundleRules(unittest.TestCase):
    def test_the_file_is_named_after_the_commit(self) -> None:
        self.assertEqual(mod.bundle_name("15f9be4"), "mesedi-backend-modules-15f9be4.tar.gz")

    def test_downloads_go_to_a_private_cache(self) -> None:
        env = mod.module_env("/tmp/private-cache")
        self.assertEqual(env["GOMODCACHE"], "/tmp/private-cache")
        self.assertEqual(env["GOFLAGS"], "-modcacherw")

    def test_the_host_toolchain_is_removed_and_written_bundles_keep_the_rest(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            download = os.path.join(tmp, "download")
            for rel in ("golang.org/toolchain/@v/x.zip", "example.com/m/@v/v1.0.0.zip"):
                os.makedirs(os.path.dirname(os.path.join(download, rel)), exist_ok=True)
                open(os.path.join(download, rel), "w").close()
            mod.drop_host_toolchain(download)
            out = os.path.join(tmp, "b.tar.gz")
            mod.write_bundle(download, out)
            with tarfile.open(out) as tar:
                names = tar.getnames()
            self.assertIn("download/example.com/m/@v/v1.0.0.zip", names)
            self.assertNotIn("download/golang.org/toolchain/@v/x.zip", names)

    def test_the_instructions_name_the_file_and_the_offline_build(self) -> None:
        text = mod.instructions("mesedi-backend-modules-abc1234.tar.gz")
        self.assertIn("tar -xzf mesedi-backend-modules-abc1234.tar.gz -C /tmp/mesedi-modules", text)
        self.assertIn("GOSUMDB=off go build ./...", text)
        self.assertIn("go.sum", text)

    def test_the_commit_is_read_from_the_checkout(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            repo = _repo_with_backend(tmp)
            head = subprocess.run(["git", "-C", repo, "rev-parse", "HEAD"], check=True,
                                  capture_output=True, text=True).stdout.strip()
            self.assertEqual(mod.git_output(repo, "rev-parse", "HEAD"), head)
            self.assertEqual(mod.dirty_backend_paths(repo), [])

    def test_a_checkout_with_backend_changes_is_refused(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            repo = _repo_with_backend(tmp)
            with open(os.path.join(repo, "backend", "extra.go"), "w") as fh:
                fh.write("package x\n")
            run, calls = _fake_download({})
            err = io.StringIO()
            with redirect_stderr(err):
                code = mod.main(["--out", os.path.join(tmp, "b.tar.gz")], repo=repo, run=run)
            self.assertEqual(code, 2)
            self.assertEqual(calls, [], "nothing may be downloaded for a checkout with changes")
            self.assertIn("backend/extra.go", err.getvalue())

    def test_a_folder_without_the_backend_is_refused(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            run, calls = _fake_download({})
            with redirect_stderr(io.StringIO()):
                code = mod.main([], repo=tmp, run=run)
            self.assertEqual(code, 2)
            self.assertEqual(calls, [])

    def test_modules_are_packed_and_the_host_toolchain_is_left_out(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            repo = _repo_with_backend(tmp)
            run, calls = _fake_download({
                "modernc.org/sqlite/@v/v1.58.0.zip": "module zip",
                "modernc.org/sqlite/@v/v1.58.0.mod": "module modernc.org/sqlite\n",
                "golang.org/toolchain/@v/v0.0.1-go1.27.4.darwin-arm64.zip": "host go",
            })
            out = os.path.join(tmp, "b.tar.gz")
            stdout = io.StringIO()
            with redirect_stdout(stdout):
                code = mod.main(["--out", out], repo=repo, run=run)
            self.assertEqual(code, 0)
            cmd, cwd, env = calls[0]
            self.assertEqual(cmd, ["go", "mod", "download"])
            self.assertEqual(cwd, os.path.join(repo, "backend"))
            self.assertEqual(env["GOFLAGS"], "-modcacherw")
            with tarfile.open(out) as tar:
                names = tar.getnames()
            self.assertIn("download/modernc.org/sqlite/@v/v1.58.0.zip", names)
            self.assertFalse(any("toolchain" in n for n in names), names)
            text = stdout.getvalue()
            self.assertIn("SHA-256 " + mod.sha256_file(out), text)
            self.assertIn("GOPROXY=file:///tmp/mesedi-modules/download GOSUMDB=off go build ./...", text)

    def test_a_failed_download_writes_nothing(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            repo = _repo_with_backend(tmp)
            out = os.path.join(tmp, "b.tar.gz")
            with redirect_stderr(io.StringIO()):
                code = mod.main(["--out", out], repo=repo, run=lambda cmd, cwd, env: 1)
            self.assertEqual(code, 1)
            self.assertFalse(os.path.exists(out))


if __name__ == "__main__":
    unittest.main(verbosity=2)

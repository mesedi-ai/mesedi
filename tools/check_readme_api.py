#!/usr/bin/env python3
"""Fail CI when a shipped README documents an API that does not exist.

WHY THIS EXISTS
On 2026-09-01 both SDK READMEs, the ones that render as the package
description on PyPI and npm, told readers to write code that cannot run:

    pip install mesedi[langgraph]          # extra does not exist
    pip install mesedi[openai-agents]      # extra does not exist
    from mesedi.integrations.langgraph import instrument_graph
                                           # real name: instrument_langgraph
    from mesedi.integrations.openai_agents import instrument_agent
                                           # does not exist; it is MesediRunHooks

    import { instrumentGraph } from "mesedi/integrations/langgraph";
                                           # real name: instrumentLangGraph
    import { instrumentAgent } from "mesedi/integrations/openai_agents";
                                           # does not exist; it is MesediRunHooks

Four of those had been live since the adapters were renamed. A README is
the first thing a developer reads and the last thing anyone re-reads, so
drift there is invisible until a stranger tries the quickstart and fails.

WHAT IT CHECKS
  1. Every `pip install mesedi[extra]` in the Python README names an extra
     that pyproject.toml actually defines.
  2. Every `from mesedi... import NAME` in the Python README resolves to a
     top-level name in that module.
  3. Every `import { NAME } from "mesedi/..."` in the TypeScript README
     resolves to an `export` in the corresponding source file.

EXIT CODES
  0  clean
  1  at least one documented API does not exist
  2  could not run (a README or source tree is missing, or zero imports
     were extracted, which would mean the regexes silently stopped matching)
"""
from __future__ import annotations

import ast
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PY_DIR = os.path.join(ROOT, "sdk-python")
TS_DIR = os.path.join(ROOT, "sdk-typescript")


def fail(msg: str) -> int:
    print(f"could not run: {msg}", file=sys.stderr)
    return 2


def python_extras(readme: str, pyproject: str) -> list[str]:
    m = re.search(r"\[project\.optional-dependencies\](.*?)(\n\[|\Z)",
                  pyproject, re.S)
    defined = set(re.findall(r"^\s*([A-Za-z0-9_-]+)\s*=\s*\[",
                             m.group(1) if m else "", re.M))
    problems = []
    for group in re.findall(r"pip install\s+[\"']?mesedi\[([A-Za-z0-9_,-]+)\]",
                            readme):
        for extra in (x.strip() for x in group.split(",")):
            if extra not in defined:
                problems.append(
                    f"README says `pip install mesedi[{extra}]` but "
                    f"pyproject.toml defines only {sorted(defined)}")
    return problems


def module_top_level_names(path: str) -> set[str]:
    names: set[str] = set()
    try:
        tree = ast.parse(open(path, encoding="utf-8").read())
    except (OSError, SyntaxError):
        return names
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            names.add(node.name)
        elif isinstance(node, ast.Assign):
            for target in node.targets:
                if isinstance(target, ast.Name):
                    names.add(target.id)
        elif isinstance(node, (ast.Import, ast.ImportFrom)):
            for alias in node.names:
                names.add(alias.asname or alias.name.split(".")[0])
    return names


def python_imports(readme: str) -> tuple[list[str], int]:
    problems, checked = [], 0
    for module, raw in re.findall(r"from\s+(mesedi[\w.]*)\s+import\s+([^\n]+)",
                                  readme):
        rel = module.replace(".", os.sep)
        candidates = [os.path.join(PY_DIR, rel + ".py"),
                      os.path.join(PY_DIR, rel, "__init__.py")]
        path = next((c for c in candidates if os.path.exists(c)), None)
        if path is None:
            problems.append(f"README imports from `{module}` but no such module")
            continue
        available = module_top_level_names(path)
        for name in (n.split(" as ")[0].strip() for n in raw.split(",")):
            if not name or name.startswith("("):
                continue
            checked += 1
            if name not in available:
                problems.append(
                    f"README says `from {module} import {name}` but that "
                    f"module does not define {name}")
    return problems, checked


def ts_imports(readme: str) -> tuple[list[str], int]:
    problems, checked = [], 0
    pattern = r'import\s+\{([^}]+)\}\s+from\s+["\'](mesedi[^"\']*)["\']'
    for raw, module in re.findall(pattern, readme):
        if module == "mesedi":
            sub = os.path.join("src", "index")
        else:
            sub = os.path.join("src", module[len("mesedi/"):])
        candidates = [os.path.join(TS_DIR, sub + ".ts"),
                      os.path.join(TS_DIR, sub, "index.ts")]
        path = next((c for c in candidates if os.path.exists(c)), None)
        if path is None:
            problems.append(f"README imports from `{module}` but no such source")
            continue
        src = open(path, encoding="utf-8").read()
        for name in (n.split(" as ")[0].strip() for n in raw.split(",")):
            if not name:
                continue
            checked += 1
            declared = re.search(
                r"export\s+(?:async\s+)?(?:class|function|const|let|type|interface)"
                r"\s+" + re.escape(name) + r"\b", src)
            re_exported = re.search(r"export\s*\{[^}]*\b" + re.escape(name) + r"\b",
                                    src)
            if not (declared or re_exported):
                problems.append(
                    f"README says `import {{ {name} }} from \"{module}\"` but "
                    f"that module does not export {name}")
    return problems, checked


def main() -> int:
    py_readme_path = os.path.join(PY_DIR, "README.md")
    ts_readme_path = os.path.join(TS_DIR, "README.md")
    pyproject_path = os.path.join(PY_DIR, "pyproject.toml")
    for p in (py_readme_path, ts_readme_path, pyproject_path):
        if not os.path.exists(p):
            return fail(f"missing {p}")

    py_readme = open(py_readme_path, encoding="utf-8").read()
    ts_readme = open(ts_readme_path, encoding="utf-8").read()
    pyproject = open(pyproject_path, encoding="utf-8").read()

    problems = python_extras(py_readme, pyproject)
    p2, py_checked = python_imports(py_readme)
    p3, ts_checked = ts_imports(ts_readme)
    problems += p2 + p3

    if py_checked == 0 or ts_checked == 0:
        return fail(f"extracted {py_checked} Python and {ts_checked} TypeScript "
                    f"imports; the regexes are probably broken")

    print(f"=== README API check: {py_checked} Python and {ts_checked} "
          f"TypeScript imports ===")
    if not problems:
        print("clean: every documented import and extra exists.")
        return 0
    for p in problems:
        print(f"VIOLATION  {p}")
    print()
    print(f"{len(problems)} documented API reference(s) do not exist. "
          f"These READMEs ship as the PyPI and npm package descriptions.")
    return 1


if __name__ == "__main__":
    sys.exit(main())

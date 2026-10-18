#!/usr/bin/env python3
# Copyright Bootstrapping.org 2026
# SPDX-License-Identifier: Apache-2.0
#
# The smoke check on a set of ready-built binaries, run by CI and by the
# release workflow on each kind of machine before anything is published:
#
#   python3 scripts/smoke.py BIN_DIR
#
# BIN_DIR holds bootstrapping, bst and bootstrapping-demo (with .exe on
# Windows). The check:
#   - every binary reports the version in bootstrapping.go
#   - the demo passes its own checks, including the apply it kills halfway
#   - Python drives the command through the demo's story with its own
#     sqlite3 module making the admin's edits, and checks every exit code,
#     the JSON plans and the table contents
#   - eight processes apply the same seed to one file at the same moment,
#     and exactly one of them writes
import json
import os
import re
import sqlite3
import subprocess
import sys
import tempfile

ROOT = os.path.realpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
EXAMPLE = os.path.join(ROOT, "examples", "permissions")
EXT = ".exe" if os.name == "nt" else ""
checks = 0


def fail(msg):
    print("SMOKE FAILED: " + msg, file=sys.stderr)
    sys.exit(1)


def check(cond, msg):
    global checks
    checks += 1
    if not cond:
        fail(msg)


def version():
    with open(os.path.join(ROOT, "bootstrapping.go"), encoding="utf-8") as f:
        m = re.search(r'^const Version = "(.*)"$', f.read(), re.M)
    return m.group(1)


def main():
    if len(sys.argv) != 2:
        fail("usage: smoke.py BIN_DIR")
    bindir = os.path.abspath(sys.argv[1])
    exe = {n: os.path.join(bindir, n + EXT) for n in ("bootstrapping", "bst", "bootstrapping-demo")}
    for path in exe.values():
        check(os.path.isfile(path), path + " is missing")

    want = version()
    for name in ("bootstrapping", "bst"):
        out = subprocess.run([exe[name], "version"], capture_output=True, text=True)
        check(out.returncode == 0 and out.stdout.strip() == name + " " + want, f"{name} version: {out.stdout!r}")

    demo = subprocess.run([exe["bootstrapping-demo"]], capture_output=True, text=True, timeout=120)
    check(demo.returncode == 0 and "All checks passed" in demo.stdout, "demo:\n" + demo.stdout + demo.stderr)
    check("Killed after 1 statement" in demo.stdout, "the demo didn't kill its apply halfway")

    with tempfile.TemporaryDirectory() as tmp:
        story(exe["bst"], tmp)
        together(exe["bootstrapping"], tmp)
    print(f"smoke: {checks} checks passed")


def cli(binary, db, seeds, *args, expect=0):
    out = subprocess.run([binary, args[0], "-db", db, "-seeds", seeds, *args[1:]], capture_output=True, text=True, timeout=60)
    check(out.returncode == expect, f"{' '.join(args)}: exit {out.returncode}, want {expect}\n{out.stdout}{out.stderr}")
    return out.stdout


def story(bst, tmp):
    db = os.path.join(tmp, "app.db")
    with open(os.path.join(EXAMPLE, "schema.sql"), encoding="utf-8") as f:
        conn = sqlite3.connect(db)
        conn.executescript(f.read())
        conn.close()
    seeds = {v: os.path.join(EXAMPLE, "seeds", v) for v in ("v1", "v2", "v3")}

    cli(bst, db, seeds["v1"], "status", expect=2)
    cli(bst, db, seeds["v1"], "apply")
    cli(bst, db, seeds["v1"], "status", expect=0)

    conn = sqlite3.connect(db)
    conn.execute("UPDATE roles SET name = 'Content Editor' WHERE code = 'editor'")
    conn.execute("""DELETE FROM role_permissions
        WHERE role_id = (SELECT id FROM roles WHERE code = 'editor')
        AND permission_id = (SELECT id FROM permissions WHERE code = 'posts.delete')""")
    conn.commit()
    conn.close()

    plan = json.loads(cli(bst, db, seeds["v2"], "plan", "-json", expect=2))
    acts = sorted((c["table"], c["key_text"], c["action"], c.get("column", "")) for c in plan["changes"])
    check(acts == sorted([
        ("permissions", "billing.view", "insert", ""),
        ("roles", "editor", "update", "description"),
        ("roles", "editor", "keep-edit", "name"),
        ("roles", "viewer", "update", "name"),
        ("role_permissions", "admin, billing.view", "insert", ""),
        ("role_permissions", "editor, posts.delete", "keep-delete", ""),
    ]), f"v2 plan: {acts}")
    cli(bst, db, seeds["v2"], "apply")

    blocked = json.loads(cli(bst, db, seeds["v3"], "apply", "-json", expect=3))
    check(blocked["blocked"] and not blocked["applied"] and blocked["counts"]["conflicts"] == 1, f"v3: {blocked['summary']}")
    conn = sqlite3.connect(db)
    check(conn.execute("SELECT retired_at FROM permissions WHERE code = 'users.manage'").fetchone()[0] is None, "a blocked apply wrote")
    conn.close()

    done = json.loads(cli(bst, db, seeds["v3"], "apply", "-json", "-take", "roles/editor/name=local"))
    check(done["applied"] and done["run"] == 3, f"v3 with -take: {done['summary']}")
    cli(bst, db, seeds["v3"], "status", expect=0)

    conn = sqlite3.connect(db)
    rows = dict(conn.execute("SELECT code, name FROM roles").fetchall())
    check(rows == {"admin": "Administrator", "editor": "Content Editor", "viewer": "Reader"}, f"roles: {rows}")
    links = conn.execute("SELECT count(*) FROM role_permissions").fetchone()[0]
    check(links == 9, f"links: {links}")
    retired = conn.execute("SELECT retired_at FROM permissions WHERE code = 'users.manage'").fetchone()[0]
    check(retired is not None, "users.manage wasn't retired")
    conn.close()

    report = json.loads(cli(bst, db, seeds["v3"], "explain", "-json", "roles", "editor"))
    verdicts = {f["column"]: f["verdict"] for f in report["fields"]}
    check(verdicts.get("name") == "kept local edit", f"explain: {verdicts}")
    history = json.loads(cli(bst, db, seeds["v3"], "history", "-json"))
    check([r["id"] for r in history] == [3, 2, 1], f"history: {history}")


def together(binary, tmp):
    db = os.path.join(tmp, "shared.db")
    with open(os.path.join(EXAMPLE, "schema.sql"), encoding="utf-8") as f:
        conn = sqlite3.connect(db)
        conn.executescript(f.read())
        conn.close()
    seeds = os.path.join(EXAMPLE, "seeds", "v2")
    procs = [subprocess.Popen([binary, "apply", "-json", "-db", db, "-seeds", seeds], stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE, text=True) for _ in range(8)]
    results = []
    for p in procs:
        out, err = p.communicate(timeout=60)
        check(p.returncode == 0, "a parallel apply failed: " + err)
        results.append(json.loads(out))
    wrote = sum(1 for r in results if r["applied"])
    check(wrote == 1, f"{wrote} of 8 parallel applies wrote, want exactly 1")
    conn = sqlite3.connect(db)
    n = conn.execute("SELECT count(*) FROM _bootstrapping_runs").fetchone()[0]
    check(n == 1, f"{n} runs recorded")
    check(conn.execute("PRAGMA integrity_check").fetchone()[0] == "ok", "integrity check")
    conn.close()


if __name__ == "__main__":
    main()

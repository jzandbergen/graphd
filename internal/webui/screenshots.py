"""Regenerate the README screenshots (docs/img/*.png).

Like browser_test.py this is a dev-only Playwright script: there is no npm here,
it is not part of `go build`, and it is deliberately not wired into
`go test ./...`. Run it by hand when the UI changes and the images go stale.

    python3 -m venv /tmp/pwenv
    /tmp/pwenv/bin/pip install playwright pillow
    sudo apt-get install -y --no-install-recommends chromium fonts-liberation

    go build -o /tmp/graphd .
    /tmp/graphd serve --db /tmp/shots.db --seed-fixture --listen 127.0.0.1:7491 &
    /tmp/pwenv/bin/python internal/webui/screenshots.py http://127.0.0.1:7491 docs/img

The PNG bytes are not byte-for-byte reproducible: the appbar status readout and
the sub-pixel result of `fit` depend on timing, so a rerun differs by a handful of
pixels. It is structurally identical — re-running against a fresh database gives a
mean block-intensity difference of 0.04/255 — so do not chase a stable hash.

Two projects are used:

  - `fixture` (--seed-fixture), patched so the shots show real content: FIX-3 gets
    notes and an output and is put in `doing`, so the canvas shows all four node
    states (ready ring, amber doing, red blocked, grey cancelled) rather than an
    all-todo grid.
  - `rate`, built to match the worked example in the README exactly: the same four
    tasks and four edges as the `scaffold_plan` block, with RATE-1 finished and
    carrying the output from the second `get_next_task` block. So the README's
    screenshot and its JSON payloads show the same project in the same state.
"""
import json
import os
import sys
import urllib.request

from playwright.sync_api import sync_playwright

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:7491"
OUT = sys.argv[2] if len(sys.argv) > 2 else "docs/img"
CHROME = "/usr/bin/chromium"

# 1600x1000 at 2x, downscaled to 1400x875: crisp on a HiDPI README reader,
# still a sane file size.
VIEW = {"width": 1600, "height": 1000}
SCALE = 2
FINAL = (1400, 875)

os.makedirs(OUT, exist_ok=True)


def api(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req) as r:
        raw = r.read()
    return json.loads(raw) if raw else None


NOTES = """## Problem

The limiter has to work per tenant, not globally, or one noisy tenant
starves everyone else.

## Action Items

- [ ] middleware wrapper on the write path
- [ ] counter key includes the tenant
- [ ] integration test with two tenants

## Interfaces

```go
func Limit(next http.Handler) http.Handler
```

## Validation contract

`go test ./limiter -run TestPerTenant`
"""

OUTPUT = """## What was built

Token-bucket middleware, 120 rps burst 240, keyed per tenant.

## Measurements

| case | p99 |
|---|---|
| single tenant | 1.9 ms |
| two tenants, hot | 3.4 ms |

Counter key is `rl:{tenant}:{window}`; ttl is 2x the window so a cold
window never reads a stale count.

## Follow-ups

None — the Redis store lands in RATE-4.
"""


def fixture_id():
    """The id of the --seed-fixture project, looked up rather than assumed."""
    for p in api("GET", "/api/projects")["projects"]:
        if p["name"] == "fixture":
            return p["id"]
    raise SystemExit("no `fixture` project — start the server with --seed-fixture")


def seed_demo():
    """Give the fixture a task with notes + output, in flight, and one blocked."""
    g = api("GET", "/api/projects/%d/graph" % fixture_id())
    by_key = {t["key"]: t for t in g["tasks"]}
    api("PATCH", "/api/tasks/%d" % by_key["FIX-3"]["id"],
        {"notes": NOTES, "output": OUTPUT, "status": "doing"})
    # FIX-4 is blocked by FIX-3, so it renders with the red blocked outline and
    # FIX-3's output becomes its input.
    api("PATCH", "/api/tasks/%d" % by_key["FIX-4"]["id"],
        {"notes": "Counter store keyed per tenant.\n\nBlocked until the middleware lands."})
    return by_key


# The README's worked example, verbatim: the same tasks, the same edges, the same
# output string. If the prose changes, this must change with it — the screenshot
# is evidence for the JSON, not decoration.
RATE_TASKS = [
    {"ref": "a", "label": "Design the limiter", "priority": 1},
    {"ref": "b", "label": "Implement middleware", "priority": 1},
    {"ref": "c", "label": "Redis counter store", "priority": 2},
    {"ref": "d", "label": "Integration tests", "priority": 2},
]
RATE_EDGES = [
    {"blocker": "a", "blocked": "b"},
    {"blocker": "b", "blocked": "c"},
    {"blocker": "b", "blocked": "d"},
    {"blocker": "c", "blocked": "d"},
]
RATE_OUTPUT = "Bucket key is rl:{tenant}:{window}; ttl = 2x window."


def seed_rate():
    """Build the README's example project, with RATE-1 finished and handing off.

    Built through the REST API rather than MCP: `scaffold_plan` is an MCP-only
    tool (there is no /scaffold route), and this script is a plain HTTP client.
    The resulting graph is identical — same four tasks, same four edges.
    """
    projects = {p["name"]: p for p in api("GET", "/api/projects")["projects"]}
    p = projects.get("rate")
    if p is not None:
        # Delete rather than reuse: a project's key counter only moves forward, so
        # a second run would create RATE-5..8 and the screenshot would disagree
        # with the README's RATE-1..4. Recreating resets the counter. This is the
        # one hard delete in the system (README decision 3) and it cascades.
        api("DELETE", "/api/projects/%d?confirm=%s" % (p["id"], p["name"]))
    p = api("POST", "/api/projects", {"name": "rate", "key_prefix": "RATE"})
    pid = p["id"]

    id_of = {}
    for t in RATE_TASKS:
        created = api("POST", "/api/projects/%d/tasks" % pid,
                      {"label": t["label"], "priority": t["priority"]})
        id_of[t["ref"]] = created["id"]
    for e in RATE_EDGES:
        api("POST", "/api/projects/%d/edges" % pid,
            {"blocker_id": id_of[e["blocker"]], "blocked_id": id_of[e["blocked"]]})

    # Finish the first task with the output the README shows, so the second is
    # the frontier and carries it as an input. Referenced by the id we just
    # created, not by key: the project's key counter only ever moves forward, so
    # a second run would produce RATE-5..8 rather than RATE-1..4.
    api("PATCH", "/api/tasks/%d" % id_of["a"],
        {"status": "done", "output": RATE_OUTPUT})
    return pid


def click_node(pg, key):
    """Tap a cytoscape node by its task key and return the key that was hit."""
    return pg.evaluate("""(key) => {
      const host = document.getElementById('cy');
      const cy = host && host._cyreg && host._cyreg.cy;
      if (!cy) return null;
      const n = cy.nodes().filter(n => n.data('key') === key)[0];
      if (!n) return null;
      cy.center(n);
      n.emit('tap');
      return n.data('key');
    }""", key)


def layout_and_fit(pg):
    pg.click("#btn-layout")
    pg.wait_for_timeout(900)
    pg.click("#btn-fit")
    pg.wait_for_timeout(600)


def main():
    fix_id = fixture_id()
    seed_demo()
    rate_id = seed_rate()
    with sync_playwright() as p:
        b = p.chromium.launch(executable_path=CHROME, args=["--no-sandbox"])
        pg = b.new_page(viewport=VIEW, device_scale_factor=SCALE)
        errors = []
        pg.on("pageerror", lambda e: errors.append(str(e)))

        # Each shot navigates, settles, and is captured immediately. Capturing
        # all of them at the end would photograph whichever page happened to be
        # loaded last.
        raw = {}

        def capture(name, path, key=None, layout=False, project=None, settle=1200):
            url = BASE + path
            if project is not None:
                url += "?p=%d" % project
            pg.goto(url, wait_until="networkidle")
            pg.wait_for_timeout(settle)
            if layout:
                # The fixture has no stored positions, so without this the nodes
                # sit in the client-side grid rather than a dagre layout.
                layout_and_fit(pg)
            if key:
                hit = click_node(pg, key)
                if hit != key:
                    raise SystemExit("could not open %s (got %r)" % (key, hit))
                pg.wait_for_timeout(900)
            tmp = os.path.join(OUT, "." + name)
            pg.screenshot(path=tmp)
            raw[name] = tmp

        # The README's worked example, so the screenshot matches the JSON above it.
        capture("example.png", "/canvas", project=rate_id, layout=True)
        # The fixture project: the richest one, so it carries the general shots.
        # The project is always passed explicitly — localStorage remembers the
        # last one viewed, so leaving it out would shoot whatever was last open.
        capture("canvas.png", "/canvas", project=fix_id, layout=True)
        capture("board.png", "/board", project=fix_id)
        capture("panel.png", "/canvas", project=fix_id, key="FIX-3", layout=True)

        print("page errors:", errors)
        b.close()

    # Downscale with pillow if it is available; otherwise keep the 2x files.
    try:
        from PIL import Image
    except ImportError:
        for name, tmp in raw.items():
            os.replace(tmp, os.path.join(OUT, name))
        print("pillow not installed — kept %dx%d screenshots" % (VIEW["width"] * SCALE, VIEW["height"] * SCALE))
        return

    for name, tmp in raw.items():
        im = Image.open(tmp).convert("RGB").resize(FINAL, Image.LANCZOS)
        dest = os.path.join(OUT, name)
        im.save(dest, optimize=True)
        os.remove(tmp)
        print("%s  %dx%d  %d KB" % (dest, FINAL[0], FINAL[1], os.path.getsize(dest) // 1024))


if __name__ == "__main__":
    main()

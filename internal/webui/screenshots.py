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

The fixture is patched first so the shots show real content: FIX-3 gets notes and
an output and is put in `doing`, so the canvas shows all four node states (ready
ring, amber doing, red blocked, grey cancelled) rather than an all-todo grid.
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


def seed_demo():
    """Give the fixture a task with notes + output, in flight, and one blocked."""
    g = api("GET", "/api/projects/1/graph")
    by_key = {t["key"]: t for t in g["tasks"]}
    api("PATCH", "/api/tasks/%d" % by_key["FIX-3"]["id"],
        {"notes": NOTES, "output": OUTPUT, "status": "doing"})
    # FIX-4 is blocked by FIX-3, so it renders with the red blocked outline and
    # FIX-3's output becomes its input.
    api("PATCH", "/api/tasks/%d" % by_key["FIX-4"]["id"],
        {"notes": "Counter store keyed per tenant.\n\nBlocked until the middleware lands."})
    return by_key


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
    seed_demo()
    with sync_playwright() as p:
        b = p.chromium.launch(executable_path=CHROME, args=["--no-sandbox"])
        pg = b.new_page(viewport=VIEW, device_scale_factor=SCALE)
        errors = []
        pg.on("pageerror", lambda e: errors.append(str(e)))

        # Each shot navigates, settles, and is captured immediately. Capturing
        # all of them at the end would photograph whichever page happened to be
        # loaded last.
        raw = {}

        def capture(name, path, key=None, layout=False, settle=1200):
            pg.goto(BASE + path, wait_until="networkidle")
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

        capture("canvas.png", "/canvas", layout=True)
        capture("board.png", "/board")
        capture("panel.png", "/canvas", key="FIX-3", layout=True)

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

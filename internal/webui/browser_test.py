#!/usr/bin/env python3
"""graphd — browser smoke tests for the web UI.

Runs a real headless Chromium against a running `graphd serve`, so it checks
things no static analysis can: that the panel actually hides, that a control is
inside the viewport at a given window size, and that nothing throws on boot.

Usage:

    graphd serve --db /tmp/ui.db --seed-fixture --listen 127.0.0.1:7431 &
    python3 internal/webui/browser_test.py http://127.0.0.1:7431

Exit status is 0 when every check passes, 1 otherwise. It is deliberately a
plain script with one dependency (playwright) rather than a test framework: the
repo has no npm and no node_modules, and this must never become part of
`go build`. See internal/webui/README-browser.md for how to run it.

Requires chromium; pass --chromium PATH to override /usr/bin/chromium.
"""

import argparse
import json
import sys
import urllib.error
import urllib.request

try:
    from playwright.sync_api import sync_playwright
except ImportError:
    print("playwright is not installed. See internal/webui/README-browser.md.")
    sys.exit(2)

FAILURES = []
CHECKS = 0


def check(name, cond, detail=""):
    global CHECKS
    CHECKS += 1
    if cond:
        print("  ok   " + name)
    else:
        FAILURES.append(name)
        print("  FAIL " + name + (("\n         " + detail) if detail else ""))


def api(base, method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        base + path, data=data, method=method,
        headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        return json.loads(e.read())


def seed_hub(base):
    """Create a task with many blockers and dependents.

    The panel bugs this file guards against only appear when an edge list is
    long enough to overflow the pane, so a fixture with one or two edges would
    silently pass. Nine each way reproduces the real shape.
    """
    proj = api(base, "GET", "/api/projects")["projects"][0]
    pid = proj["id"]
    hub = api(base, "POST", f"/api/projects/{pid}/tasks",
              {"label": "Confirm decisions with stakeholders", "priority": 1})["id"]
    for i in range(9):
        b = api(base, "POST", f"/api/projects/{pid}/tasks",
                {"label": f"Upstream prerequisite {i} with a long label"})["id"]
        api(base, "POST", f"/api/projects/{pid}/edges",
            {"blocker_id": b, "blocked_id": hub})
    for i in range(9):
        d = api(base, "POST", f"/api/projects/{pid}/tasks",
                {"label": f"Downstream dependent {i} with a long label"})["id"]
        api(base, "POST", f"/api/projects/{pid}/edges",
            {"blocker_id": hub, "blocked_id": d})
    return pid, hub


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("base", help="e.g. http://127.0.0.1:7431")
    ap.add_argument("--chromium", default="/usr/bin/chromium")
    ap.add_argument("--shots", default="", help="directory to write screenshots into")
    args = ap.parse_args()
    base = args.base.rstrip("/")

    pid, hub = seed_hub(base)
    print(f"seeded hub task {hub} in project {pid}")

    with sync_playwright() as p:
        browser = p.chromium.launch(
            executable_path=args.chromium,
            args=["--no-sandbox", "--disable-gpu"])

        # ---- 1. the panel is genuinely hidden at boot ----
        #
        # The property and the *rendered result* are different things, which is
        # exactly the bug this guards: `.detail { display: flex }` beat
        # `[hidden] { display: none }`, so `hidden = true` left the panel on
        # screen. Assert on computed style and geometry, never on `.hidden`.
        print("panel visibility")
        pg = browser.new_page(viewport={"width": 960, "height": 700})
        pg.goto(f"{base}/canvas", wait_until="networkidle")
        pg.wait_for_timeout(900)
        st = pg.evaluate("""() => { const e = document.getElementById('detail');
            return {hidden: e.hidden, display: getComputedStyle(e).display,
                    w: e.getBoundingClientRect().width}; }""")
        check("panel is hidden at boot", st["hidden"] and st["display"] == "none"
              and st["w"] == 0, f"got {st}")

        # ---- 2. closing the panel actually hides it, in both views ----
        for view in ("canvas", "board"):
            pg.goto(f"{base}/{view}", wait_until="networkidle")
            pg.wait_for_timeout(900)
            if view == "board":
                pg.click(f"article.card[data-id='{hub}']")
            else:
                pg.evaluate("""() => {
                    const host = document.getElementById('cy');
                    const cy = host._cyreg.cy;
                    const n = cy.nodes().filter(x => x.data('task')
                        && x.data('task').id === %d)[0];
                    const r = n.renderedPosition();
                    const h = host.getBoundingClientRect();
                    window.__clickAt = [h.left + r.x, h.top + r.y];
                }""" % hub)
                c = pg.evaluate("() => window.__clickAt")
                pg.mouse.click(c[0], c[1])
            pg.wait_for_timeout(500)
            opened = not pg.eval_on_selector("#detail", "e => e.hidden")
            check(f"[{view}] clicking a task opens the panel", opened)
            pg.click("#detail-close")
            pg.wait_for_timeout(400)
            st = pg.evaluate("""() => { const e = document.getElementById('detail');
                return {hidden: e.hidden, display: getComputedStyle(e).display,
                        w: e.getBoundingClientRect().width}; }""")
            check(f"[{view}] close hides the panel",
                  st["hidden"] and st["display"] == "none" and st["w"] == 0,
                  f"got {st}")
        pg.close()

        # ---- 3. the links add rows stay reachable on a short window ----
        #
        # The regression: a long edge list pushed the add row below the fold, so
        # you had to scroll to discover a control you had not used yet. Above the
        # floor the lists shrink instead; below it the pane scrolls, so the row
        # is always reachable. Assert *reachability*, not a fixed offset.
        print("links pane reachability")
        for vh in (1000, 800, 700, 600, 540, 500, 460, 420, 360, 320):
            pg = browser.new_page(viewport={"width": 960, "height": vh})
            pg.goto(f"{base}/board", wait_until="networkidle")
            pg.wait_for_timeout(700)
            pg.click(f"article.card[data-id='{hub}']")
            pg.wait_for_timeout(300)
            pg.click(".tab[data-pane='links']")
            pg.wait_for_timeout(300)
            m = pg.evaluate("""() => {
                const pane = document.querySelector('.pane[data-pane=links]');
                const pr = pane.getBoundingClientRect();
                const a = document.querySelector('#add-blocked-btn').getBoundingClientRect();
                const b = document.querySelector('#add-blocker-btn').getBoundingClientRect();
                const inside = (r) => r.top >= pr.top - 1 && r.bottom <= pr.bottom + 1;
                return {bothInside: inside(a) && inside(b),
                        scrollable: pane.scrollHeight > pane.clientHeight,
                        listsShrink: document.querySelector('#d-blocks').getBoundingClientRect().height};
            }""")
            if m["bothInside"]:
                check(f"[h={vh}] both add rows inside the pane without scrolling", True)
            elif m["scrollable"]:
                # Acceptable on a very short window, but then scrolling must
                # actually bring it into view and it must be clickable.
                pg.evaluate("""() => { const p = document.querySelector('.pane[data-pane=links]');
                    p.scrollTop = p.scrollHeight; }""")
                pg.wait_for_timeout(200)
                ok = pg.evaluate("""() => {
                    const pane = document.querySelector('.pane[data-pane=links]');
                    const pr = pane.getBoundingClientRect();
                    const a = document.querySelector('#add-blocked-btn').getBoundingClientRect();
                    return a.top >= pr.top - 1 && a.bottom <= pr.bottom + 1;
                }""")
                check(f"[h={vh}] add row reachable by scrolling the pane", ok)
            else:
                check(f"[h={vh}] add row reachable", False,
                      f"not inside the pane and the pane does not scroll: {m}")
            if args.shots and vh == 600:
                pg.screenshot(path=f"{args.shots}/links-{vh}.png")
            pg.close()

        # ---- 4. the appbar never pushes the page sideways ----
        #
        # The regression: the header controls total ~1240px before gaps, so a
        # single non-wrapping row overflowed the document at any width below
        # ~1375px. That scrolled the *whole page* horizontally, which pushed the
        # canvas and the panel out of view and clipped the left edge of the
        # header — exactly the symptom in the screenshot that prompted this
        # file. The bar now wraps, so every control stays reachable.
        print("appbar layout")
        for vw in (1600, 1400, 1280, 1152, 1024, 960, 800):
            pg = browser.new_page(viewport={"width": vw, "height": 800})
            pg.goto(f"{base}/canvas", wait_until="networkidle")
            pg.wait_for_timeout(700)
            m = pg.evaluate("""() => {
                const bar = document.querySelector('.appbar');
                const ctrls = [...bar.querySelectorAll('button,select,input,a')];
                const offscreen = ctrls.filter(e => {
                    const r = e.getBoundingClientRect();
                    return r.width > 0 && r.right > window.innerWidth + 1;
                }).map(e => e.id || e.className);
                return {docScrollW: document.documentElement.scrollWidth,
                        vw: window.innerWidth,
                        offscreen: offscreen};
            }""")
            check(f"[w={vw}] no horizontal document scroll",
                  m["docScrollW"] <= m["vw"] + 1,
                  f"document is {m['docScrollW']}px wide in a {m['vw']}px viewport")
            check(f"[w={vw}] every appbar control is on screen",
                  not m["offscreen"], "off-screen: " + ", ".join(m["offscreen"][:4]))
            pg.close()

        # ---- 5. nothing throws on boot, in either view ----
        print("console health")
        for view in ("canvas", "board"):
            pg = browser.new_page(viewport={"width": 1280, "height": 800})
            errs = []
            pg.on("pageerror", lambda e: errs.append(str(e)))
            pg.on("console", lambda m: errs.append(m.text) if m.type == "error" else None)
            pg.goto(f"{base}/{view}", wait_until="networkidle")
            pg.wait_for_timeout(1200)
            # The fixture deliberately ships no favicon; ignore that one 404.
            real = [e for e in errs if "favicon" not in e.lower()
                    and "404" not in e and "Failed to load resource" not in e]
            check(f"[{view}] no page errors on boot", not real, "; ".join(real[:3]))
            pg.close()

        browser.close()

    print()
    if FAILURES:
        print(f"{len(FAILURES)} of {CHECKS} checks failed:")
        for f in FAILURES:
            print("  - " + f)
        return 1
    print(f"all {CHECKS} browser checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())

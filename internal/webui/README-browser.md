# Browser tests

`browser_test.py` drives a real headless Chromium against a running
`graphd serve`. It exists because some bugs are invisible to every other kind of
check in this repo — the two it was written for were both found by hand:

1. **The detail panel never actually hid.** `.detail { display: flex }` beat the
   user-agent's `[hidden] { display: none }` — equal specificity, later origin
   wins — so `panel.hidden = true` set the attribute and left the panel fully
   rendered. The close button looked like it did nothing, and the panel sat
   there visible-but-empty at boot. `panel_test.js` now guards the *stylesheet*
   cause statically; this file guards the *behaviour*.

2. **The "add" button fell below the fold.** With a long edge list, the links
   pane grew to fit its content and pushed the add row past the bottom of the
   window, so you had to scroll to discover a control you had not used yet. Only
   a real layout engine can tell you that, which is why this is not a jsdom test.

## Running it

There is no npm here and no `node_modules`, and this must never become part of
`go build`. Playwright is a dev-only Python dependency, kept out of the repo:

```sh
python3 -m venv /tmp/pwenv
/tmp/pwenv/bin/pip install playwright

# chromium from the distro; playwright's bundled download is not needed
sudo apt-get install -y --no-install-recommends chromium fonts-liberation

go build -o /tmp/graphd .
/tmp/graphd serve --db /tmp/ui.db --seed-fixture --listen 127.0.0.1:7431 &
/tmp/pwenv/bin/python internal/webui/browser_test.py http://127.0.0.1:7431
```

Exit status is 0 when every check passes. `--shots DIR` writes screenshots;
`--chromium PATH` overrides `/usr/bin/chromium`.

It is **not** wired into `go test ./...`: the Go test suite must stay runnable
with no browser installed, and skipping silently when chromium is missing would
make a green run mean less than it appears to. `panel_test.js` is the part that
runs under `go test`; this is the part you run when you touch the UI.

## Screenshots

`screenshots.py` regenerates the images the README embeds (`docs/img/*.png`). It
needs the same venv, plus `pillow` for the downscale, and it patches the fixture
first so the shots show real content — `FIX-3` gets notes and an output and is put
in `doing`, so the canvas shows all four node states rather than an all-todo grid:

```sh
/tmp/pwenv/bin/pip install pillow
/tmp/graphd serve --db /tmp/shots.db --seed-fixture --listen 127.0.0.1:7491 &
/tmp/pwenv/bin/python internal/webui/screenshots.py http://127.0.0.1:7491 docs/img
```

Run it by hand when the UI changes and the images go stale. Without `pillow` it
keeps the 2x originals instead of downscaling.

## Notes for this environment

- Chromium needs `--no-sandbox` here: the container is an unprivileged LXC with
  `CapEff: 0`, so the setuid `chrome-sandbox` helper cannot work.
- `/dev/shm` is 32G, so `--disable-dev-shm-usage` is not needed.
- Headless needs no X server and no `DISPLAY` — it renders offscreen. There is
  no graphical session in this container and none is required.
- `fonts-liberation` matters: without it fontconfig has zero faces and every
  screenshot is tofu boxes.

## What it checks

| check | why it needs a browser |
|---|---|
| panel hidden at boot | `[hidden]` losing to `display: flex` is a rendered fact |
| close hides the panel, canvas **and** board | the canvas path and the board path open it differently |
| both add rows inside the pane, 420px–1000px | needs real layout geometry |
| add row reachable by scrolling below ~420px | asserts graceful degradation, not a fixed offset |
| no page errors on boot | the only place a real JS engine runs the whole boot path |

The seeding step builds a task with nine blockers and nine dependents on purpose:
with one or two edges the lists never overflow and the panel bugs pass silently.

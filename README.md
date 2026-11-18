# pteron

`pteron` is a local web publishing CLI and background daemon for sharing standalone HTML pages across a Tailscale network and collecting structured user feedback. Each published page receives a direct URL on the host Tailscale IPv4 address, reloads automatically when updated, and synchronizes form inputs back to disk without custom backend code.

## Architecture

The `pteron` binary is installed at `~/.local/bin/pteron` and works alongside a systemd user daemon (`pteron.service`). The daemon binds to `127.0.0.1:8848` and the machine's Tailscale IPv4 address (`100.64.0.2:8848`), serving both the management interface at `/` and published pages at `/s/<id>` using the numeric Tailscale IPv4 address directly.

All site state lives on disk under `~/.local/share/pteron/sites/<id>/`, where each site directory holds three files:

- `index.html` stores the standalone HTML document supplied during `publish` or `update`.
- `meta.json` stores page metadata including the site ID and title.
- `feedback.json` stores synchronized form fields, custom `extra` state, submission status, and the latest update timestamp.

When serving `/s/<id>`, the daemon injects `/_pteron/runtime.js` into the page. This runtime handles automatic input synchronization with `/api/sites/<id>/feedback` and maintains an SSE connection so open browser tabs reload automatically after a page update.

## CLI Commands

### `publish`

Publish a standalone HTML page from a file path or standard input. `pteron` prints the assigned site ID, title, and Tailscale URL (`http://100.64.0.2:8848/s/<id>`).

```bash
pteron publish page.html --title "Architecture Review"

# Publish from stdin
cat page.html | pteron publish --title "Architecture Review"

# Specify a custom slug or emit JSON output
pteron publish page.html --title "Architecture Review" --id arch-review --json
```

### `update`

Replace the HTML document of an existing site ID while keeping previously saved feedback intact. Connected browsers reload automatically over SSE.

```bash
pteron update <id> page.html --title "Updated Architecture Review"
```

### `feedback`

Read the collected feedback JSON for a site (`fields`, `extra`, `submitted`, and `updated_at`), or clear stored feedback before starting a fresh round.

```bash
pteron feedback <id>
pteron feedback <id> --clear
```

### `list` and `rm`

List all currently published pages or remove a page from disk.

```bash
pteron list
pteron rm <id>
```

## Automatic Form and Input Synchronization (`runtime.js`)

Every page served under `/s/<id>` loads `/_pteron/runtime.js` automatically, enabling persistent form state across devices with plain HTML markup.

### Field discovery and persistence

`runtime.js` discovers every `<input>`, `<textarea>`, and `<select>` anywhere in the document, including controls scattered across cards, tables, diagrams, or tabs without a `<form>` wrapper. Field keys resolve in order from `name`, then `data-field`, and finally `id`. Elements marked with `contenteditable="true"` and a `data-field` attribute are also tracked.

Whenever a user edits text or changes a selection, `runtime.js` debounces updates for 300ms and sends the field values to `/api/sites/<id>/feedback`. Pending updates flush via `sendBeacon` when the tab hides or closes. Reopening the page on another Tailscale device or reloading after `pteron update <id>` restores saved values into their matching elements.

### Submit triggers

Because edits save automatically, a submit button is optional and `runtime.js` leaves the page layout untouched by default. To provide an explicit completion action, include `<button type="submit">` inside a `<form>` or place `<button data-pteron-submit>Submit Feedback</button>` anywhere in the HTML. Clicking the button sets `"submitted": true` in `feedback.json` and displays a brief confirmation toast.

### Custom widget state

Pages with interactive widgets outside standard form controls, such as canvases or custom selection boards, can persist state through `window.pteron`:

- `window.pteron.save(data)` merges `data` into the `extra` object in `feedback.json`.
- `window.pteron.submit(data)` merges `data` into `extra` and sets `"submitted": true`.

## Management UI

Open `http://100.64.0.2:8848/` from any device on the tailnet (or `http://127.0.0.1:8848/` locally) to access the management interface. From this page you can inspect all published sites, preview collected `feedback.json` payloads, and delete obsolete pages.

## Systemd User Service Operations

The `pteron` daemon runs as a systemd user service (`pteron.service`). Use `systemctl --user` and `journalctl --user` to manage the daemon and inspect logs:

```bash
# Check service status
systemctl --user status pteron.service

# Start, stop, or restart the daemon
systemctl --user start pteron.service
systemctl --user stop pteron.service
systemctl --user restart pteron.service

# Enable or disable automatic startup on login
systemctl --user enable pteron.service
systemctl --user disable pteron.service

# Follow daemon logs
journalctl --user -u pteron.service -f
```

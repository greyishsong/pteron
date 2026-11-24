# pteron

`pteron` is a local web publishing CLI and background daemon for sharing standalone HTML pages and collecting structured user feedback. Published pages receive a direct URL, reload automatically when updated, and synchronize form inputs back to disk without custom backend code.

## Architecture

`pteron` ships as a single Go binary that provides both the CLI commands and the background HTTP daemon (`pteron serve`), paired with an optional systemd user unit (`pteron.service`).

At startup, `pteron serve` listens on `127.0.0.1:<port>` (default `8848`, configurable with `--port` or `PTERON_PORT`) so local CLI commands can communicate with the daemon. When Tailscale is active on the host, the daemon detects the host's Tailscale IPv4 address dynamically (via the `tailscale0` interface or `tailscale ip -4`) and binds to that address as well, making the management UI (`/`) and published pages (`/s/<id>`) reachable from other devices on the tailnet. On machines without Tailscale, it serves on `127.0.0.1:<port>`.

All site data is stored under `~/.local/share/pteron/sites/<id>/` (configurable with `--data-dir` or `PTERON_DATA_DIR`), with three files per site:

- `index.html`: the standalone HTML document supplied during `publish` or `update`.
- `meta.json`: page metadata, including the site ID, title, and timestamps.
- `feedback.json`: synchronized form fields, custom `extra` state, submission status, and the latest update timestamp.

When serving `/s/<id>`, the daemon injects `/_pteron/runtime.js` into the HTML response. This script handles automatic input synchronization with `/api/sites/<id>/feedback` and maintains an SSE connection so open browser tabs reload automatically after an update.

## Build, Package, and Install

Use `./install.sh` to compile `pteron`, bundle a portable archive for other machines, and install the binary and systemd user service.

### Build, package, and install locally

Running `./install.sh` from a source checkout builds the binary, packages a tarball under `dist/`, and installs `pteron` for the current user:

```bash
./install.sh
```

You can also run each stage individually:

```bash
# Compile the binary to dist/pteron
./install.sh build

# Build and create dist/pteron-<os>-<arch>.tar.gz
./install.sh package

# Build (if needed) and install to ~/.local/bin and ~/.config/systemd/user
./install.sh install
```

### Package for another machine

Cross-compile and bundle a self-contained tarball by setting `--os` / `--arch` (or `GOOS` / `GOARCH`):

```bash
./install.sh package --os linux --arch arm64
```

Copy `dist/pteron-linux-arm64.tar.gz` to the target machine, extract it, and run `./install.sh`. Because the archive bundles the prebuilt binary, `pteron.service`, `README.md`, `skills/pteron/SKILL.md`, and `install.sh`, the target machine does not need a Go toolchain:

```bash
tar -xzf pteron-linux-arm64.tar.gz
cd pteron-linux-arm64
./install.sh
```

### Installer options

| Option | Description |
| --- | --- |
| `--bin-dir DIR` | Install the binary into `DIR` instead of `~/.local/bin` (updates `ExecStart` in the installed service unit accordingly). |
| `--service-dir DIR` | Install `pteron.service` into `DIR` instead of `~/.config/systemd/user`. |
| `--no-service` | Skip installing and enabling `pteron.service`. |
| `--no-restart` | Install `pteron.service` without running `systemctl --user` to enable or restart it. |
| `--with-skill` | Also install `skills/pteron/SKILL.md` into `~/.gemini/config/skills/pteron/`. |
| `--skill-dir DIR` | Install `skills/pteron/SKILL.md` into `DIR/pteron/` (implies `--with-skill`). |
| `--os GOOS` / `--arch GOARCH` | Override the target OS and architecture for `build` and `package`. |

## CLI Commands

All commands accept `--port` (default: `PTERON_PORT` or `8848`) and `--data-dir` (default: `PTERON_DATA_DIR` or `~/.local/share/pteron`).

### `publish`

Publish a standalone HTML page from a file path or standard input. `pteron` prints the assigned site ID, title, and page URL (`http://<host-ip>:8848/s/<id>`).

```bash
pteron publish page.html --title "Architecture Review"

# Publish from stdin
cat page.html | pteron publish --title "Architecture Review"

# Specify a custom slug or emit JSON output
pteron publish page.html --title "Architecture Review" --id arch-review --json
```

### `update`

Replace the HTML document of an existing site ID while preserving previously saved feedback. Connected browsers reload automatically over SSE.

```bash
pteron update <id> page.html --title "Updated Architecture Review"
```

### `feedback`

Read the collected feedback JSON (`fields`, `extra`, `submitted`, and `updated_at`), or clear stored feedback before starting a fresh round.

```bash
pteron feedback <id>
pteron feedback <id> --clear
```

### `list`, `rm`, and `serve`

```bash
# List all published sites
pteron list

# Delete one or more sites from disk
pteron rm <id>

# Run the HTTP daemon in the foreground
pteron serve --port 8848
```

## Automatic Form and Input Synchronization (`runtime.js`)

Every page served under `/s/<id>` loads `/_pteron/runtime.js` automatically, enabling persistent form state across reloads and devices with plain HTML markup.

### Field discovery and persistence

`runtime.js` tracks every `<input>`, `<textarea>`, and `<select>` anywhere in the document, including controls placed inside cards, tables, diagrams, or tabs without a `<form>` wrapper. Field keys resolve in order from `data-field`, then `name`, and finally `id`. Elements marked with `contenteditable="true"` and a `data-field` attribute are also tracked. Add `data-pteron-ignore` to an element or ancestor to exclude controls from tracking.

Edits are debounced for 300ms and sent to `/api/sites/<id>/feedback`, with pending changes flushed via `sendBeacon` when the tab hides or closes. Reopening the URL or reloading after `pteron update <id>` restores saved values into their matching elements.

### Submit triggers

Because edits save automatically, a submit button is optional and `runtime.js` leaves the page layout untouched by default. To provide an explicit confirmation step, include `<button type="submit">` inside a `<form>` or place `<button data-pteron-submit>Submit Feedback</button>` anywhere in the HTML. Clicking the button sets `"submitted": true` in `feedback.json` and displays a brief confirmation toast.

### Custom widget state

Pages with interactive widgets outside standard form controls can persist and inspect state through `window.pteron`:

- `window.pteron.save(data)` merges `data` into the `extra` object in `feedback.json`.
- `window.pteron.submit(data)` merges `data` into `extra` and sets `"submitted": true`.
- `window.pteron.getFields()` returns the current field map collected from the DOM.
- `window.pteron.getFeedback()` fetches the latest stored `feedback.json` object.

## Management UI

Open `http://<tailscale-ip>:8848/` from any device on your tailnet (or `http://127.0.0.1:8848/` locally) to access the management interface. From this page you can inspect all published sites, copy share links, preview or clear collected `feedback.json` payloads, and delete obsolete pages.

## Systemd User Service

When installed with `pteron.service`, manage the background daemon with `systemctl --user` and `journalctl --user`:

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

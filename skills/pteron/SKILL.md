---
name: pteron
description: Publish single-file HTML artifacts and interactive feedback pages to the local pteron web server (accessible over Tailscale), update pages in-place with live reload, and retrieve user form/feedback data asynchronously.
---

# pteron

`pteron` publishes standalone HTML pages to a local background daemon (`pteron.service`) and collects user responses asynchronously. The daemon serves pages on `127.0.0.1:8848` and the host Tailscale IPv4 address (`100.64.0.2:8848`), generating a direct URL (`http://<tailscale-ip>:8848/s/<id>`) that users can open from any device on the tailnet.

## Publishing Workflow

Follow these steps when sharing interactive reports, design reviews, forms, or prototypes with the user:

1. Write a standalone HTML file containing inline CSS and JavaScript (or public CDN scripts).
2. Publish the file with `pteron publish` and share the returned Tailscale URL (`http://100.64.0.2:8848/s/<id>`).
3. Update the page directly with `pteron update <id>` whenever revisions are needed; open browser tabs reload automatically while preserving earlier user input.
4. Retrieve structured responses with `pteron feedback <id>` once the user finishes reviewing.

## CLI Reference

### Publish a page

Publish from a file path or standard input. `pteron` prints the assigned site ID, title, and Tailscale URL.

```bash
pteron publish page.html --title "Architecture Review"

# Read HTML from stdin
cat page.html | pteron publish --title "Architecture Review"

# Set a custom URL slug or output JSON metadata
pteron publish page.html --title "Architecture Review" --id arch-review --json
```

### Update an existing page

Replace the HTML content for an existing site ID. Previously collected feedback remains intact, and connected browsers reload automatically over SSE.

```bash
pteron update <id> page.html --title "Updated Architecture Review"
```

### Read or clear feedback

Fetch the current feedback state as JSON, or clear saved entries before starting a new feedback round.

```bash
pteron feedback <id>
pteron feedback <id> --clear
```

### List and delete pages

Inspect all published sites or remove pages that are no longer needed.

```bash
pteron list
pteron rm <id>
```

## Authoring Interactive HTML Pages

Build each page as a standalone HTML document. Inline your styles and scripts or load established libraries from a CDN, such as Tailwind CSS, Chart.js, Mermaid, or Lucide icons.

### Automatic form and input tracking

When serving `/s/<id>`, `pteron` injects `/_pteron/runtime.js` into the HTML response. This runtime automatically tracks every `<input>`, `<textarea>`, and `<select>` element anywhere in the document, including inputs placed inside cards, tables, diagrams, or tabs without a `<form>` wrapper.

`runtime.js` identifies each field by checking `name`, then `data-field`, and finally `id`. You can also make any element editable and tracked by adding `contenteditable="true"` alongside a `data-field` attribute:

```html
<div contenteditable="true" data-field="reviewer_notes">
  Click to add review notes...
</div>
```

Whenever the user types or toggles a control, `runtime.js` debounces changes for 300ms and sends the full field map to `/api/sites/<id>/feedback`. Pending changes flush through `sendBeacon` when the browser tab hides or closes. Reopening the URL on another Tailscale device or reloading after `pteron update <id>` automatically restores saved values into the matching inputs.

### Explicit submit buttons

Because `runtime.js` saves every edit automatically, a submit button is optional and no floating button is added by default. When a page calls for an explicit confirmation step, include either `<button type="submit">` inside a `<form>` or `<button data-pteron-submit>` anywhere in the document:

```html
<button data-pteron-submit>Submit Feedback</button>
```

Clicking a submit trigger sets `"submitted": true` in `feedback.json` and displays a brief confirmation toast.

### Custom interactive state

For widgets that store state outside standard form elements, such as interactive canvases, selection boards, or draggable layouts, use the global `window.pteron` helper. Data passed to these functions is stored under the `extra` key in `feedback.json`:

```javascript
// Save incremental custom state
window.pteron.save({ selectedNodes: ["auth", "api"], layout: "grid" });

// Save custom state and mark feedback as submitted
window.pteron.submit({ approved: true, score: 5 });
```

## Reading Feedback JSON

Running `pteron feedback <id>` returns a JSON object containing tracked form fields, custom `extra` state, submission status, and the last update timestamp:

```json
{
  "fields": {
    "reviewer_notes": "Split the worker queue into its own package.",
    "decision": "approve_with_changes"
  },
  "extra": {
    "selectedNodes": ["auth", "api"]
  },
  "submitted": true,
  "updated_at": "2026-11-18T11:05:12Z"
}
```

Check `submitted` to see whether the user clicked an explicit submit button, and inspect `fields` and `extra` for any incremental edits already saved. Run `pteron feedback <id>` to read the latest stored responses before acting on them.

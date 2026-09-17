# Dashboard status data

The overview, node table and resource views use `ClusterSummary`. Older
controllers can still return `ClusterStatus`; the browser immediately projects
that input to summaries without retaining peer, route, next-hop or BPF arrays.
The global **Cluster Status JSON** dialog also exports only this projection.

Selecting a node does not request diagnostics. **Load data** reuses an unexpired
browser snapshot or asks the controller for its cache/current request.
**Refresh** requests a fresh collection. Neither summary updates, reconnection,
opening a dialog nor expiry initiates collection.

The left Node Info pane, health badges and last-update ages come from routine
summaries and remain visible before loading and after details expire. Current
summary metadata takes precedence over an older diagnostic snapshot. Unloaded
tabs remain selectable but show no tables, pagination or diagnostic validation
results. The node table shows the last received status age alongside its source.

The node dialog distinguishes not-loaded, loading, loaded, expired and error.
Peer/route/BPF tables and full node JSON are available only with valid details.
A failed Refresh can display the previous still-valid snapshot, labeled with
the request error, collection age, receipt time and expiry. At expiry the cache
drops its payload and the heavy table component unmounts. Reads check expiry
again; reads and summary updates never extend it. Browser caching uses the
controller's TTL without additional entry-count limits. A burst of explicit
loads can therefore retain many snapshots until their individual expirations.

## Detail API

Requests use the existing same-origin viewer authentication:

- `POST /status/node/{name}/details`, body `{"forceRefresh":false}` (Load data)
  or `{"forceRefresh":true}` (Refresh).
- `GET /status/node/{name}/details?requestId={id}` while pending.
- Results contain `state`, `nodeName`, optional `requestId`, `deadline`, `error`,
  and `details`. States are `pending` (202), `complete` (200), `expired` (410),
  `unavailable` (404), or `retryable` (503).
- `details` contains `nodeName`, `requestId`, `collectedAt`, `receivedAt`,
  `expiresAt`, and `status` (the full node payload).

Pending requests poll at most once per second until the original controller
deadline. Selection changes cancel obsolete browser waiters; unmount cancels
all requests and timers. Errors require an explicit retry, not auto-loading.
Old controllers without this API still provide an overview, but explicit
detail loads report an error rather than falling back to persistent subscriptions.

## Validation

```sh
npm --prefix frontend run build
frontend/node_modules/.bin/tsc -p frontend/tsconfig.json
node --test frontend/tests/*.test.ts
```

The tests require Node's built-in TypeScript stripping; no frontend test
framework is required. Tests cover
summary compatibility, observed count semantics, API mapping, explicit loads,
cache reuse, refresh failures, cancellation, fixed deadlines and expiry.

The optional Chromium regression test mounts the real detail dialog and hook
under React StrictMode, using native browser timers and same-origin HTTP
requests. It covers button events, correlated polling, cache reuse, failed
refresh, expiry, close/reopen and unmount cancellation. It also checks summary
metadata and health badges, unloaded tabs, and the persistent info pane across
loading, failure and expiry. Run it with an existing
Playwright installation and its Chromium browser (no production dependency):

```sh
PLAYWRIGHT_MODULE=file:///absolute/path/to/playwright/index.mjs \
PLAYWRIGHT_BROWSERS_PATH=/absolute/path/to/playwright-browsers \
node --test frontend/tests/nodeDetails.browser.test.mjs
```

Both paths can point to project-local tooling. If Playwright is already
resolvable from `frontend`, omit `PLAYWRIGHT_MODULE`. Keep native browser timers
in this test: fake timers do not detect the `Illegal invocation` caused by
calling Window timer functions with a custom clock object as their receiver.

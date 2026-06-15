# Unbounded Dashboard

## Summary

Unbounded needs one management dashboard for the whole project. The dashboard
should show status, resources, and actions from many components without making
each component build its own frontend.

The first version should be simple:

- A Go HTTP server in a new `cmd/dashboard` binary.
- Server-side rendered HTML.
- Bootstrap 5 for a good default visual style.
- htmx for small interactive updates.
- JSON endpoints next to the HTML pages.
- A static module registry for core Unbounded components.

The dashboard owns the layout, navigation, style, and shared UI behavior.
Components provide structured data and actions. The dashboard renders that data
in a consistent way.

## Problem

Unbounded is not one small controller. It is a set of related systems:

- Networking and CNI.
- Node and host operations.
- Machine lifecycle management.
- Storage.
- Image distribution.
- Inventory.
- Future operational tools.

Each system has its own health, resources, warnings, errors, and actions. Users
need a single place to see and manage these things.

A single hand-built frontend that knows every detail of every component will not
scale well. It would become hard to maintain and hard to extend. Separate UIs
for each component would also be a poor result because users would lose a common
Unbounded experience.

The dashboard should solve this by giving every component a clear way to expose
dashboard data while keeping one shared visual language.

## Goals

- Provide one Unbounded dashboard with shared navigation and visual identity.
- Make the dashboard useful for backend-focused developers to extend.
- Prefer server-rendered pages over a long-lived browser application.
- Use JSON APIs for the same data shown in the HTML pages.
- Let components expose data and actions without owning frontend code.
- Keep Kubernetes RBAC as the source of truth for permissions.
- Start with a small design that can be merged and improved over time.

## Non-Goals

- Build a React, Vue, Svelte, or similar single-page app as the main dashboard.
- Let components ship arbitrary JavaScript plugins.
- Build a marketplace-style runtime plugin system in the first version.
- Replace Prometheus, Grafana, logs, or tracing tools.
- Build every component page in the first milestone.
- Reach full feature parity with every existing UI on day one.

## Proposed Approach

Add a new dashboard component:

```text
cmd/dashboard
```

This binary runs a Go HTTP server. It renders HTML pages and serves JSON APIs.
It is separate from any one Unbounded controller.

The dashboard has two main jobs:

1. Provide the shared user experience.
2. Adapt component data into that user experience.

Component data should be structured and boring. For example, a component should
provide health, metrics, tables, details, alerts, and actions. The component
should not provide custom page markup or custom frontend code.

The dashboard should have a small set of shared UI building blocks:

- Page.
- Navigation.
- Card.
- Metric card.
- Status badge.
- Alert.
- Table.
- Detail section.
- Action form.
- Empty state.
- Raw JSON view.

These building blocks should use Bootstrap internally. Modules should not need
to know Bootstrap details.

## User Experience

The dashboard should be simple and operational.

The first pages should be:

- A global overview page.
- A modules page.
- One page per module.
- Resource list pages.
- Resource detail pages.
- Raw JSON views for debugging and external consumers.

The dashboard should work well on a laptop screen and should remain usable on a
tablet-sized screen. It does not need to be a rich graphical console in the first
version.

The dashboard should avoid visual choices that require design skill from every
contributor. Bootstrap gives us reasonable defaults for spacing, forms, tables,
cards, badges, and responsive layout. A small custom stylesheet can add
Unbounded colors and names.

## Dashboard Modules

A module represents one Unbounded component or feature area. Example modules:

- `net` for networking and CNI.
- `machines` for Machina machines and machine configurations.
- `operations` for node and host operations.
- `storage` for storage health and capacity.
- `gantry` for image distribution.
- `inventory` for hardware inventory.

A module has a manifest. The manifest describes what the module is and what it
can show.

Example:

```json
{
  "id": "net",
  "title": "Networking",
  "description": "Sites, gateway pools, routes, tunnels, and CNI health",
  "capabilities": ["summary", "resources", "details", "actions"],
  "requiredPermissions": [
    {
      "apiGroup": "status.net.unbounded-cloud.io",
      "resource": "status",
      "verb": "get"
    }
  ]
}
```

The first implementation should use a static in-repo module registry. This keeps
the system easy to understand. Runtime discovery can come later.

## Data Contract

Modules should expose domain data, not HTML.

The dashboard should be responsible for turning that data into pages. This keeps
the UI consistent and keeps modules from becoming mini frontend projects.

The first data contract should cover these shapes:

### Manifest

The manifest describes the module.

Fields:

- `id`: stable module ID.
- `title`: display name.
- `description`: short description.
- `capabilities`: supported features.
- `requiredPermissions`: Kubernetes permissions needed to use the module.

### Summary

The summary is the small overview shown on the home page and module page.

Fields:

- `status`: one of `healthy`, `warning`, `critical`, or `unknown`.
- `metrics`: list of important numbers.
- `alerts`: list of warnings or errors.
- `updatedAt`: time the data was produced.

Example:

```json
{
  "status": "warning",
  "metrics": [
    { "label": "Nodes", "value": "42" },
    { "label": "Healthy", "value": "39" }
  ],
  "alerts": [
    { "severity": "warning", "message": "3 nodes have stale status" }
  ],
  "updatedAt": "2026-06-15T19:00:00Z"
}
```

### Resources

Resources are lists of objects owned by the module.

Fields:

- `kind`: resource kind, such as `node`, `site`, or `machine`.
- `columns`: table columns.
- `rows`: table rows.
- `links`: links to detail pages when useful.

### Details

Details describe one resource.

Fields:

- `title`: page title.
- `status`: resource status.
- `sections`: groups of key/value data.
- `relatedResources`: optional links to related objects.
- `actions`: optional actions the user can take.

### Actions

Actions are operations a user can request from the dashboard.

Actions must be explicit. They should use server-side `POST` requests and should
have clear permission requirements.

Examples:

- Reboot a host.
- Drain a node.
- Start a machine operation.
- Enable or disable a feature.

Dangerous actions should have confirmation steps.

## HTML and JSON Endpoints

Every useful HTML page should have a matching JSON endpoint. This lets people
build their own tools without scraping HTML.

Initial HTML endpoints:

```text
GET /
GET /modules
GET /modules/{moduleID}
GET /modules/{moduleID}/resources/{kind}
GET /modules/{moduleID}/resources/{kind}/{name}
```

Initial JSON endpoints:

```text
GET /api/dashboard/v1/modules
GET /api/dashboard/v1/modules/{moduleID}/manifest
GET /api/dashboard/v1/modules/{moduleID}/summary
GET /api/dashboard/v1/modules/{moduleID}/resources/{kind}
GET /api/dashboard/v1/modules/{moduleID}/resources/{kind}/{name}
```

Fragment endpoints can be added for htmx:

```text
GET /fragments/modules/{moduleID}/summary
GET /fragments/modules/{moduleID}/resources/{kind}
```

Fragments return partial HTML, not full pages. They should only be used by the
dashboard itself.

## Server-Side Rendering

The dashboard should render pages on the server using Go templates or another
small Go-friendly template system.

Server-side rendering gives us:

- Simple request and response behavior.
- Easy testing.
- Pages that work without a large JavaScript bundle.
- Fewer frontend build steps.
- A clearer ownership model for the UI.

The browser should not be responsible for assembling the whole application.

## Styling and Interaction

The first version should use Bootstrap 5. Bootstrap is a good fit because it
gives backend developers a reasonable default design without asking each person
to invent spacing, colors, and form styles.

The dashboard should use Bootstrap through shared Unbounded templates. For
example, a module should ask for a status badge, not choose Bootstrap classes
itself.

The first version should also use htmx for simple interactions:

- Refresh a card.
- Submit an action form.
- Replace a table after filtering.
- Load a detail panel.

All Bootstrap, htmx, and custom CSS assets should be served by the dashboard
itself. The dashboard should not depend on public CDNs because clusters may be
private or offline.

## Static and Live Data

Most dashboard data should use snapshots. A normal page load or htmx refresh is
enough for many operations pages.

Some modules may need live data later. For example, a module may want to update a
small region without polling. The module contract should reserve a live update
capability, but the first prototype does not need to build a full streaming
system.

The preferred order is:

1. Snapshot pages.
2. htmx polling for small regions.
3. Server-Sent Events or WebSockets only when polling is not enough.

## Authentication and Authorization

The dashboard should use Kubernetes authorization as the source of truth.

The normal flow should be:

1. The browser authenticates to the dashboard.
2. The dashboard identifies the user.
3. The dashboard checks Kubernetes RBAC with SubjectAccessReview.
4. The dashboard renders only pages and actions the user is allowed to use.
5. Component actions are sent as explicit server-side requests.

The design should reuse existing Unbounded auth patterns where possible, but the
shared dashboard code should live in a neutral package. It should not require the
dashboard to import code from another `cmd/` package.

The design document for implementation must define:

- How a user gets a dashboard session or token.
- How SubjectAccessReview checks are cached.
- How action permissions are checked.
- How component calls are authenticated.
- How user identity is propagated, if needed.
- How CSRF protection works for browser form posts.
- What audit information is recorded for actions.

## Component Communication

`cmd/dashboard` is a separate process. It needs to get data from other
components.

The first prototype should keep this simple. A module adapter may call a
configured HTTP JSON endpoint or Kubernetes API endpoint and convert the result
into the dashboard data model.

The design should not require every component to implement a brand new protocol
before the dashboard can exist. Instead, the first modules can adapt existing
status data into the shared dashboard shapes.

The trust boundary is important:

- The browser talks to the dashboard.
- The dashboard talks to component APIs or Kubernetes APIs.
- The dashboard must not become an unsafe privileged proxy.
- The dashboard must check the user's permissions before showing data or taking
  action.
- Components may still enforce their own authorization when directly reachable.

The detailed design must decide whether component calls use:

- The dashboard service account after local SubjectAccessReview checks.
- Forwarded user identity.
- Kubernetes API aggregation.
- A mix of these choices.

## Static Registry First

The first version should use a static module registry compiled into the
dashboard or loaded from dashboard configuration.

The static registry should define:

- Module ID.
- Module title and description.
- Adapter type.
- Config needed to reach the component.
- Resource kinds the module exposes.

Cluster-specific service addresses should come from configuration or manifests,
not from hard-coded source code.

This keeps v1 understandable and testable.

## Future Runtime Discovery

Runtime discovery can be added after the static contract is proven.

A future version may use a Kubernetes resource like this:

```yaml
apiVersion: dashboard.unbounded-cloud.io/v1alpha1
kind: DashboardModule
metadata:
  name: net
spec:
  title: Networking
  serviceRef:
    namespace: unbounded-system
    name: unbounded-net-controller
  basePath: /dashboard/v1
```

Runtime discovery should not be part of the first prototype. It adds API design,
security, and lifecycle questions that are easier to answer after one or two
static modules exist.

## Initial Module

The first module should be `net` because network status data already exists and
is useful. The first dashboard version does not need to reproduce every advanced
network view immediately.

The initial `net` module should focus on:

- Overview metrics.
- Sites.
- Nodes.
- Node details.
- Raw JSON.

More advanced topology and connectivity views can be migrated after the shared
dashboard structure works.

## First Prototype

The first mergeable prototype should include:

- `designs/dashboard.md`.
- A new `cmd/dashboard` binary.
- A small internal dashboard package.
- Embedded templates.
- Self-hosted static assets.
- A health endpoint.
- Basic HTML pages.
- Matching JSON endpoints.
- A static module registry.
- One sample or real `net` adapter.
- Tests for routing and rendering.

The first prototype does not need production deployment manifests unless the
auth and trust model are ready. It is better to merge a small correct artifact
than a large incomplete system.

## Risks

### The dashboard could become too powerful

If the dashboard can call every component with broad service account privileges,
it may become a dangerous proxy. The implementation must check user permissions
before showing data or running actions.

### The module contract could become too UI-specific

If modules return page layouts instead of data, each module becomes its own UI.
The contract should stay focused on domain data, actions, and links.

### Styling could drift over time

If every module writes its own Bootstrap markup, the dashboard will become
inconsistent. Shared templates should hide most Bootstrap details.

### Live updates could make v1 too complex

Streaming is useful, but it should not block the first prototype. Polling and
server-rendered snapshots are enough for v1.

### Auth extraction may take longer than expected

Some existing auth code is tied to a specific component. The dashboard needs a
neutral shared package for common auth behavior.

## Open Questions

- What is the first supported browser authentication path?
- Should the dashboard be reachable through the Kubernetes API aggregation layer?
- Should component calls use forwarded user identity or the dashboard service
  account after local RBAC checks?
- What is the exact JSON schema for summaries, tables, details, and actions?
- Which package should own shared dashboard auth helpers?
- What is the first production deployment shape?
- Which static assets should be vendored for Bootstrap and htmx?

## Milestones

### Milestone 1: Design and skeleton

- Merge this design document.
- Add a minimal `cmd/dashboard` server.
- Add shared templates and static assets.
- Add health, HTML, and JSON routes.
- Add a static module registry.

### Milestone 2: First useful module

- Add a `net` module adapter.
- Render overview, sites, nodes, and node details.
- Add raw JSON views.
- Add tests for the adapter and pages.

### Milestone 3: Auth and deployment

- Extract shared auth helpers into a neutral package.
- Add Kubernetes RBAC checks.
- Add action permission checks.
- Add deployment manifests when the trust model is settled.

### Milestone 4: More modules

- Add machine and operation pages.
- Add storage pages.
- Add image distribution pages.
- Add inventory pages.

### Milestone 5: Runtime discovery

- Define a dashboard module discovery API.
- Add a CRD or service annotation model.
- Keep the dashboard compatible with static modules.

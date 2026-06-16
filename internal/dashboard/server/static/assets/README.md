# Vendored frontend assets

These third-party assets are vendored and embedded into the dashboard binary so
it runs without any external CDN (air-gapped friendly). Served at `/static/assets/`.

| File                    | Upstream                | Version | License |
| ----------------------- | ----------------------- | ------- | ------- |
| `bootstrap.min.css`     | twbs/bootstrap          | 5.3.3   | MIT     |
| `htmx.min.js`           | bigskysoftware/htmx     | 2.0.3   | BSD-2-Clause |
| `htmx-ext-sse.min.js`   | bigskysoftware/htmx-extensions (sse) | 2.2.2 | BSD-2-Clause |

Source URLs (jsDelivr mirror of npm):

- https://cdn.jsdelivr.net/npm/bootstrap@5.3.3/dist/css/bootstrap.min.css
- https://cdn.jsdelivr.net/npm/htmx.org@2.0.3/dist/htmx.min.js
- https://cdn.jsdelivr.net/npm/htmx-ext-sse@2.2.2/sse.min.js

To update, re-download the pinned versions above and adjust the table.

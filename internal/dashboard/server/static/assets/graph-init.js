// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// graph-init.js is the dashboard's framework-agnostic topology primitive. It
// hydrates any element with class "ub-graph" by fetching the URL in its
// data-graph-url attribute (contract.Graph JSON) and rendering it with
// Cytoscape. Modules supply structured graph data only; rendering lives here.
//
// It runs on initial load and again after htmx swaps (so live panel refetches
// re-render the graph), and is idempotent per element.
(function () {
  "use strict";

  function healthColor(health) {
    switch (health) {
      case "ok":
        return "#198754";
      case "warning":
        return "#ffc107";
      case "error":
        return "#dc3545";
      default:
        return "#6c757d";
    }
  }

  function render(el) {
    if (!window.cytoscape) {
      return;
    }
    var url = el.getAttribute("data-graph-url");
    if (!url) {
      return;
    }
    // Guard against double-init of the same element.
    if (el.__ubGraphLoading) {
      return;
    }
    el.__ubGraphLoading = true;

    fetch(url, { headers: { Accept: "application/json" } })
      .then(function (resp) {
        if (!resp.ok) {
          throw new Error("graph fetch failed: " + resp.status);
        }
        return resp.json();
      })
      .then(function (graph) {
        var elements = [];
        (graph.nodes || []).forEach(function (n) {
          elements.push({
            data: { id: n.id, label: n.label || n.id, group: n.group || "", color: healthColor(n.health) },
          });
        });
        (graph.edges || []).forEach(function (e) {
          elements.push({
            data: {
              id: e.source + "->" + e.target,
              source: e.source,
              target: e.target,
              color: healthColor(e.health),
            },
          });
        });

        var cy = window.cytoscape({
          container: el,
          elements: elements,
          style: [
            {
              selector: "node",
              style: {
                "background-color": "data(color)",
                label: "data(label)",
                "font-size": "10px",
                color: "#adb5bd",
                "text-valign": "bottom",
                "text-halign": "center",
                width: 18,
                height: 18,
              },
            },
            {
              selector: "edge",
              style: {
                "line-color": "data(color)",
                width: 2,
                "curve-style": "bezier",
                opacity: 0.7,
              },
            },
          ],
          layout: { name: "cose", animate: false, padding: 20 },
        });
        el.__ubGraphCy = cy;
        el.__ubGraphLoading = false;
      })
      .catch(function (err) {
        el.__ubGraphLoading = false;
        el.innerHTML =
          '<div class="text-danger small">Failed to load topology: ' + err.message + "</div>";
      });
  }

  function renderAll(root) {
    var scope = root && root.querySelectorAll ? root : document;
    var els = scope.querySelectorAll(".ub-graph");
    els.forEach(render);
  }

  document.addEventListener("DOMContentLoaded", function () {
    renderAll(document);
  });

  // Re-hydrate graphs inside content swapped in by htmx (live panel refetch).
  document.body.addEventListener("htmx:afterSwap", function (evt) {
    renderAll(evt.target);
  });
})();

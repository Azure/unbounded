// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { buildRequestError, bytes, distributionState, escape as esc, mergeNodeUpdates, percent, phaseLabel, sortWorkers, storagePercent, storageState, summary } from "./model.js";
import { clusterActivityHTML, drawHistoryChart } from "./activity.js";
import { historyWindow, readinessMilestones } from "./history.js";
import { duration, startupTimeline } from "./startup.js";

const $ = id => document.getElementById(id);
const state = {
  config: null, images: [], deployments: [], deployment: null, snapshot: null,
  selected: null, detail: null, events: [], eventsError: "", detailError: "",
  stream: null, zoom: 1, busy: false, connected: false,
  stale: false, lastUpdate: 0, eventsRequest: null, cells: new Map(),
  build: null, buildBusy: false, buildTimer: null, buildRequest: null,
  renderFrame: 0, renderedNode: null, detailLoading: false, detailObservation: null,
  eventsPodUID: null, clusterPanelHTML: "", timeRange: null,
  reclaimBusy: false, reclaimTimer: null, reclaimController: null, reclaimResetUID: null, reclaimStartController: null,
};
const kindBadge = image => `<span class="badge ${image.streaming ? "streaming" : "oci"}">${image.streaming ? "ACR STREAMING" : "OCI"}</span>`;
const shortName = name => name.match(/flex\d+$/)?.[0] || name;
const number = value => new Intl.NumberFormat().format(value);
const time = value => value ? new Date(value).toLocaleTimeString() : "Not observed";
const progressText = value => value === null ? "Unknown" : `${value.toFixed(0)}%`;
const bar = (value, className = "") => `<div class="progress-track ${className}"><span data-progress="${value ?? 0}"></span></div>`;

function paintBars(root) {
  for (const element of root.querySelectorAll("[data-progress]")) {
    element.style.width = `${Math.max(0, Math.min(100, Number(element.dataset.progress)))}%`;
  }
}

function error(message, source = "operation") {
  $("error").textContent = message;
  $("error").hidden = !message;
  $("error").dataset.source = source;
}

async function api(path, options = {}) {
  const response = await fetch(path, options);
  const data = await response.json();
  if (!response.ok) {
    const failure = new Error(data.error || `HTTP ${response.status}`);
    failure.retryAfter = response.status === 409 ? Number(response.headers.get("Retry-After")) : 0;
    throw failure;
  }
  return data;
}

function connection(label, live = false) {
  $("connection").textContent = label;
  $("connection").classList.toggle("live", live);
}

function renderCatalog() {
  const search = $("search").value.toLowerCase();
  const images = state.images.filter(image => `${image.repository}:${image.tag}`.toLowerCase().includes(search));
  $("images").innerHTML = images.map(image => {
    const compatible = image.architecture === state.config.architecture && image.os === "linux";
    const existing = state.deployments.some(deployment => deployment.id === image.id);
    return `<button class="image-card ${state.deployment?.id === image.id ? "selected" : ""}" data-image="${esc(image.id)}" ${!compatible || state.busy || state.reclaimBusy ? "disabled" : ""}>
      <span class="image-card-top"><span class="image-glyph">◇</span><strong>${esc(image.repository)}</strong>${existing ? '<span class="deployed-dot" title="Existing deployment"></span>' : ""}</span>
      <span class="image-tag">:${esc(image.tag)}</span>${kindBadge(image)}
      <span class="image-card-meta">${bytes(image.size)} <span>${esc(image.architecture)}${compatible ? "" : " · incompatible"}</span></span>
    </button>`;
  }).join("") || '<p class="small muted">No matching images.</p>';
  $("catalog-status").textContent = state.busy ? "Creating or resuming DaemonSet..." : `${number(images.length)} images · ${state.config.architecture} workers`;
}

async function loadImages(refresh = false) {
  $("refresh").disabled = true;
  $("catalog-status").textContent = "Reading ACR manifests...";
  try {
    const catalog = await api(`/api/images${refresh ? "?refresh=1" : ""}`);
    state.images = catalog.images;
    $("catalog-warning").hidden = !catalog.errors.length;
    $("catalog-warning").textContent = catalog.errors.length ? `Catalog incomplete: ${number(catalog.errors.length)} registry requests failed. ${catalog.errors[0]}. Use refresh to retry.` : "";
    renderCatalog();
  } finally { $("refresh").disabled = false; }
}

async function selectImage(id) {
  if (state.busy || state.reclaimBusy) return;
  $("image-picker").open = false;
  const existing = state.deployments.find(deployment => deployment.id === id);
  if (existing) { activate(existing, true); return; }
  state.busy = true;
  renderCatalog();
  if ($("error").dataset.source === "stream") error("");
  try {
    const deployment = await api("/api/deployments", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ imageId: id }),
    });
    state.deployments.push(deployment);
    activate(deployment, false);
  } catch (failure) { error(failure.message); }
  finally { state.busy = false; renderCatalog(); }
}

function activate(deployment, resumed) {
  if (state.reclaimBusy) return;
  state.stream?.close();
  state.eventsRequest?.abort();
  Object.assign(state, { deployment, snapshot: null, selected: null, detail: null, events: [], cells: new Map(), lastUpdate: 0, connected: false, timeRange: null, renderedNode: null, detailLoading: false, detailObservation: null, eventsPodUID: null });
  for (const grid of document.querySelectorAll(".node-grid")) grid.replaceChildren();
  $("history-from").value = "0";
  $("history-to").value = "1000";
  $("stats").replaceChildren();
  $("grid-summary").textContent = "Collecting workers for this deployment...";
  $("welcome").hidden = true;
  $("workspace").hidden = false;
  $("image-picker-label").textContent = "Change image";
  $("image-title").textContent = deployment.image.repository;
  $("image-kind").className = `badge ${deployment.image.streaming ? "streaming" : "oci"}`;
  $("image-kind").textContent = deployment.image.streaming ? "ACR ARTIFACT STREAMING" : "OCI";
  $("image-meta").textContent = `:${deployment.image.tag} · ${deployment.image.architecture} · ${bytes(deployment.image.size)} · ${deployment.image.layers.length} layers · ${deployment.image.digest}`;
  $("deployment-name").textContent = deployment.name;
  $("resume-note").textContent = resumed ? "Existing deployment resumed" : "DaemonSet created · waiting for observations";
  $("updated").textContent = "Collecting current worker state...";
  $("tooltip").hidden = true;
  localStorage.setItem(storageKey(), deployment.id);
  const url = new URL(location.href);
  url.searchParams.set("deployment", deployment.id);
  history.replaceState(null, "", url);
  renderCatalog();
  renderDetail();
  renderCharts();
  connection("Connecting");
  const stream = new EventSource(`/api/deployments/${deployment.id}/stream`);
  state.stream = stream;
  stream.addEventListener("snapshot", event => {
    if (state.stream !== stream) return;
    try {
      state.snapshot = JSON.parse(event.data);
      receivedObservations();
    } catch (failure) { error(`Cannot display live state: ${failure.message}`); }
  });
  stream.addEventListener("node-updates", event => {
    if (state.stream !== stream || !state.snapshot) return;
    try {
      state.snapshot = mergeNodeUpdates(state.snapshot, JSON.parse(event.data));
      receivedObservations();
    } catch (failure) { error(`Cannot display worker updates: ${failure.message}`); }
  });
  stream.addEventListener("status-error", event => {
    state.stale = true;
    $("grid-panel").classList.add("stale");
    connection("State unavailable");
    error(JSON.parse(event.data).error, "stream");
  });
  stream.onerror = () => {
    if (state.stream !== stream) return;
    state.connected = false;
    state.stale = true;
    connection("Reconnecting");
    $("grid-panel").classList.add("stale");
  };
}

function receivedObservations() {
  state.lastUpdate = Date.parse(state.snapshot.observedAt);
  if (!Number.isFinite(state.lastUpdate)) throw new Error("Invalid observation timestamp");
  state.connected = true;
  state.stale = false;
  renderUpdated();
  $("grid-panel").classList.remove("stale");
  connection("Live", true);
  error("");
  if (state.renderFrame) return;
  state.renderFrame = requestAnimationFrame(() => {
    state.renderFrame = 0;
    renderSnapshot();
    const node = selectedNode();
    if (node && observationKey(node) !== state.detailObservation) void loadNode(true);
  });
}

function renderSnapshot() {
  const snapshot = state.snapshot;
  if (!snapshot) return;
  const image = snapshot.deployment.image;
  renderClusterOverview();
  const active = new Set(snapshot.nodes.map(node => node.name));
  if (state.selected && !active.has(state.selected)) {
    state.eventsRequest?.abort();
    state.selected = null;
    state.detail = null;
  }
  for (const [key, cell] of state.cells) {
    if (!active.has(cell.dataset.node)) { cell.remove(); state.cells.delete(key); }
  }
  for (const grid of document.querySelectorAll(".node-grid")) renderGrid(grid, image);
  const ready = snapshot.nodes.filter(node => node.phase === "ready").length;
  $("readiness-summary").textContent = `${number(ready)} / ${number(snapshot.nodes.length)} workers Ready now`;
  $("readiness-milestones").innerHTML = readinessMilestones(snapshot.nodes, snapshot.history?.start, snapshot.observedAt)
    .map(milestone => `<div><span>${milestone.percent}% Ready</span><strong>${duration(milestone.seconds)}</strong><small>${number(milestone.target)} workers · ${milestone.at === null ? "Not observed" : esc(time(milestone.at))}</small></div>`).join("");
  renderCharts();
  if (state.renderedNode !== selectedNode()) renderDetail();
}

function renderGrid(grid, image) {
  const view = grid.dataset.view;
  const snapshot = state.snapshot;
  const counts = summary(snapshot.nodes, image, view);
  const summaryID = view === "distribution" ? "grid-summary" : `${view}-summary`;
  $(summaryID).textContent = view === "distribution"
    ? `${number(snapshot.nodes.length)} workers · ${number(counts.complete || 0)} bytes complete · ${number(counts.pulling || 0)} downloading · ${number(counts.failed || 0)} pull errors`
    : `${number(snapshot.nodes.length)} workers · ${number(counts.ready || 0)} Ready · ${number((counts.preparing || 0) + (counts.queued || 0) + (counts.scheduling || 0))} preparing · ${number(counts.failed || 0)} errors`;
  if (view === "storage") {
    const reporting = snapshot.nodes.filter(node => storagePercent(node.storage) !== null);
    const totals = reporting.reduce((sum, node) => ({ used: sum.used + node.storage.used, available: sum.available + node.storage.available }), { used: 0, available: 0 });
    $(summaryID).textContent = `${number(reporting.length)} / ${number(snapshot.nodes.length)} workers observed · ${reporting.length ? bytes(totals.used) : "--"} used · ${reporting.length ? bytes(totals.available) : "--"} left`;
  }
  let position = 0;
  grid.dataset.empty = "No Linux workers match this pool and architecture.";
  for (const node of sortWorkers(snapshot.nodes, image, view)) {
    const key = `${view}:${node.name}`;
    let cell = state.cells.get(key);
    if (!cell) {
      cell = document.createElement("button");
      cell.type = "button";
      cell.dataset.node = node.name;
      cell.className = "node-cell";
      cell.innerHTML = '<span class="cell-fill"></span><span class="cell-label"></span>';
      state.cells.set(key, cell);
    }
    const phase = view === "distribution" ? distributionState(node, image) : view === "storage" ? storageState(node) : node.phase;
    const value = view === "storage" ? storagePercent(node.storage) : percent(node.available, image.size);
    const filter = view === "distribution" ? $("filter").value : "all";
    const before = grid.children[position++];
    if (before !== cell) grid.insertBefore(cell, before || null);
    const storageLabel = view === "storage" ? `, ${bytes(node.storage?.used)} used, ${bytes(node.storage?.available)} left` : "";
    const visualKey = JSON.stringify([phase, value, storageLabel, view, state.selected === node.name, node.nodeReady, filter, position === 1]);
    if (cell.dataset.visualKey === visualKey) continue;
    cell.dataset.visualKey = visualKey;
    cell.className = `node-cell ${phase} ${state.selected === node.name ? "selected" : ""} ${node.nodeReady ? "" : "node-unready"}`;
    cell.setAttribute("aria-label", `${node.name}: ${phaseLabel(phase)}${view === "distribution" ? `, ${progressText(value)} bytes available` : storageLabel}`);
    cell.setAttribute("aria-pressed", String(state.selected === node.name));
    cell.tabIndex = state.selected === node.name || (!state.selected && position === 1) ? 0 : -1;
    cell.querySelector(".cell-fill").style.width = `${view === "lifecycle" ? 100 : value ?? 0}%`;
    cell.querySelector(".cell-label").textContent = view === "distribution" ? `${shortName(node.name)} ${progressText(value)}` : view === "storage" ? `${shortName(node.name)} ${bytes(node.storage?.available)} left` : shortName(node.name);
    cell.classList.toggle("dimmed", filter !== "all" && (filter === "active" ? !["pulling", "preparing", "scheduling", "running"].includes(phase) : phase !== filter));
  }
  grid.classList.toggle("zoomed", state.zoom > 1);
  grid.style.gridTemplateColumns = `repeat(${Math.max(8, Math.floor(25 / state.zoom))}, minmax(0, 1fr))`;
  grid.style.gridAutoRows = `${14 * state.zoom}px`;
  const legend = view === "distribution" ? ["complete", "pulling", "queued", "unknown", "failed"] : view === "storage" ? ["storage-ok", "storage-high", "storage-full", "unknown"] : ["scheduling", "preparing", "running", "ready", "completed", "failed"];
  $(view === "distribution" ? "legend" : `${view}-legend`).innerHTML = legend.map(phase => `<span class="${phase}"><i></i>${phaseLabel(phase)}</span>`).join("");
}

function renderClusterOverview() {
  const activity = state.snapshot?.activity;
  const rendered = clusterActivityHTML(activity, state.snapshot?.history);
  if ($("stats").innerHTML !== rendered.cards) $("stats").innerHTML = rendered.cards;
  $("activity-coverage").textContent = rendered.coverage;
  if (state.clusterPanelHTML !== rendered.panel) {
    state.clusterPanelHTML = rendered.panel;
    $("cluster-activity").innerHTML = rendered.panel;
    paintBars($("cluster-activity"));
  }
}

function renderCharts() {
  const history = state.snapshot?.history;
  const observed = state.snapshot?.observedAt;
  drawHistoryChart($("cluster-bandwidth"), history, observed, state.timeRange);
  drawHistoryChart($("readiness-chart"), history, observed, state.timeRange, true);
  const window = historyWindow(history, observed, state.timeRange);
  $("history-range").textContent = window.available ? `${time(window.start)} - ${time(window.end)}${state.timeRange ? " · zoomed" : " · full run"}` : "Waiting for server history";
  for (const id of ["history-from", "history-to", "history-reset"]) $(id).disabled = !window.available;
}

function selectedNode() { return state.snapshot?.nodes.find(node => node.name === state.selected); }
function observationKey(node) { return JSON.stringify([node?.podUID, node?.updatedAt, node?.telemetryError, node?.storageUpdatedAt]); }

function clearSelection() {
  state.eventsRequest?.abort();
  Object.assign(state, { selected: null, detail: null, events: [], eventsPodUID: null, detailLoading: false, detailObservation: null });
  renderSnapshot();
  renderDetail();
}

function chooseNode(name) {
  if (state.selected === name) { clearSelection(); return; }
  state.selected = name;
  state.detail = null;
  state.events = [];
  state.eventsPodUID = null;
  state.eventsError = "";
  state.detailError = "";
  $("tooltip").hidden = true;
  renderSnapshot();
  renderDetail();
  void loadNode();
}

async function loadNode(live = false) {
  const name = state.selected;
  const deployment = state.deployment?.id;
  if (!name || !deployment) return;
  if (live && state.detailLoading) return;
  state.eventsRequest?.abort();
  const controller = new AbortController();
  state.eventsRequest = controller;
  state.detailLoading = true;
  state.detailObservation = observationKey(selectedNode());
  const base = `/api/deployments/${deployment}`;
  const podUID = selectedNode()?.podUID;
  const current = () => !controller.signal.aborted && state.selected === name && state.deployment?.id === deployment && selectedNode()?.podUID === podUID;
  const requests = [api(`${base}/nodes/${encodeURIComponent(name)}`, { signal: controller.signal }).then(detail => {
    if (!current()) return;
    state.detail = detail;
    state.detailError = "";
    renderDetail();
  }, failure => {
    if (!current()) return;
    state.detailError = failure.message;
    renderDetail();
  })];
  if (!live || state.eventsPodUID !== podUID) requests.push(api(`${base}/events?node=${encodeURIComponent(name)}`, { signal: controller.signal }).then(events => {
    if (!current()) return;
    state.events = events;
    state.eventsPodUID = podUID;
    state.eventsError = "";
    renderDetail();
  }, failure => {
    if (!current()) return;
    state.eventsError = failure.message;
    renderDetail();
  }));
  await Promise.all(requests);
  if (state.eventsRequest === controller) state.detailLoading = false;
}

function renderDetail() {
  const node = selectedNode();
  state.renderedNode = node;
  $("node-detail").hidden = !node;
  if (!node) {
    renderClusterOverview();
    return;
  }
  const image = state.deployment.image;
  const value = percent(node.available, image.size);
  const phase = node.phase;
  const restoreClearFocus = document.activeElement?.id === "clear-selection";
  let content = `<div class="detail-heading"><h2>${esc(shortName(node.name))}</h2><span class="phase-badge ${esc(phase)}">${esc(phaseLabel(phase))}</span><button id="clear-selection" class="clear-selection">Clear selection &#215;</button><div class="detail-progress"><span>Image bytes available <strong>${progressText(value)}</strong></span>${bar(value)}</div></div><p class="small muted node-name">${esc(node.name)}</p>`;
  if (!node.nodeReady) content += '<p class="notice warning-notice">Kubernetes reports this worker as not Ready.</p>';
  {
    content += "<h3>Image layers</h3>";
    const detail = state.detail;
    if (state.detailError) content += `<p class="notice error-notice">${esc(state.detailError)}</p>`;
    if (!detail && !state.detailError) content += '<p class="muted">Reading layer observations...</p>';
    if (detail) {
      content += '<div class="table-scroll"><table><thead><tr><th>Layer</th><th>Available / total</th><th>Progress</th><th>Local content</th></tr></thead><tbody>';
      content += (detail.layers || []).map((layer, index) => {
        const pct = percent(layer.available, layer.size);
        return `<tr><td><strong>L${index + 1}</strong><span class="digest" title="${esc(layer.digest)}">${esc(layer.digest.slice(7, 19))}</span></td><td>${bytes(layer.available)} / ${bytes(layer.size)}</td><td class="layer-progress">${bar(pct)}<span>${progressText(pct)}</span></td><td><span class="badge">${layer.available === null ? "Unknown" : layer.committed ? "Committed" : "Ranges / ingest"}</span></td></tr>`;
      }).join("");
      content += `</tbody></table></div><p class="small muted detail-note">${detail.updatedAt ? `Last byte observation: ${esc(time(detail.updatedAt))}. ` : ""}Unique locally available bytes, not cumulative traffic. Per-layer peer/origin attribution is not currently recorded.</p>`;
      if (image.streaming) content += '<p class="small streaming-note">Artifact Streaming: a Pod can be Ready before all layer bytes are available. Cache coverage may decrease after an agent restart.</p>';
    }
  }
  {
    content += "<h3>Disk usage</h3>";
    const storage = node.storage;
    if (storagePercent(storage) === null) content += '<p class="muted">Waiting for the first filesystem observation.</p>';
    else {
      content += `<div class="storage-cards">${[
        ["Used", storage.used], ["Space left", storage.available], ["Total capacity", storage.total], ["Filesystem reserved", storage.total - storage.used - storage.available],
      ].map(([label, amount]) => `<div><span>${label}</span><strong>${bytes(amount)}</strong></div>`).join("")}</div><p class="small muted">Filesystem backing <span class="digest">${esc(storage.path)}</span>. Includes all files on this filesystem, not just Gantry or the selected image. Usage percentage excludes filesystem-reserved space.</p><p class="small muted">Last storage observation: ${esc(time(node.storageUpdatedAt))}.</p>`;
    }
  }
  content += "<h3>Startup waterfall</h3>" + startupHTML(node);
  {
    content += "<h3>Pod lifecycle</h3>";
    content += `<p class="lifecycle-reason">${esc(node.reason)}</p><div class="timeline">${[
      ["Pod created", node.created], ["Scheduled", node.scheduled], ["Container started", node.started], ["Pod Ready", node.readyAt],
    ].map(([label, at]) => `<div class="${at ? "observed" : ""}"><i></i><strong>${label}</strong><span>${time(at)}</span></div>`).join("")}</div>`;
    content += '<p class="small muted">Download, unpack, and mount work can overlap. "Preparing" is an honest combined phase; Kubernetes does not expose separate unpack percentages.</p><h3>Observed Kubernetes events</h3>';
    if (state.eventsError) content += `<p class="notice error-notice">${esc(state.eventsError)}</p>`;
    else content += `<div class="event-list">${state.events.length ? state.events.map(event => `<article class="${event.type === "Warning" ? "warning-event" : ""}"><time>${time(event.at)}</time><div><strong>${esc(event.reason)}</strong><p>${esc(event.message)}</p>${event.count > 1 ? `<span class="small muted">${event.count} occurrences</span>` : ""}</div></article>`).join("") : '<p class="muted">No retained events for this Pod. Events may expire; missing history is not reconstructed.</p>'}</div>`;
  }
  const detail = state.detail;
  content += `<h3>Worker transfer activity</h3><p class="small muted">All layer traffic on ${esc(shortName(node.name))}, not attribution to this image.</p><div class="traffic-cards"><div class="peer"><span>From peers</span><strong>${bytes(detail?.peerRate)}/s</strong></div><div class="origin"><span>From origin</span><strong>${bytes(detail?.originRate)}/s</strong></div></div><p class="small muted">${detail?.peerRate == null ? "Rates require two successful observations." : `Observed ${time(detail.updatedAt)}. Retries count as network traffic, not extra progress.`}</p>`;
  $("node-detail").innerHTML = content;
  paintBars($("node-detail"));
  for (const element of $("node-detail").querySelectorAll("[data-startup-offset]")) {
    element.style.left = `${Number(element.dataset.startupOffset)}%`;
    element.style.width = `${Number(element.dataset.startupWidth)}%`;
  }
  if (restoreClearFocus) $("clear-selection").focus({ preventScroll: true });
}

function startupHTML(node) {
  const timing = startupTimeline(node, state.events, state.snapshot.observedAt);
  const horizon = Math.max(1, timing.elapsed || 0, ...timing.stages.map(stage => (stage.offset || 0) + (stage.seconds || 0)));
  const rows = timing.stages.map(stage => `<div class="startup-row ${stage.seconds === null ? "unavailable" : ""} ${stage.ongoing ? "ongoing" : ""}"><span>${esc(stage.name)}</span><div class="startup-track">${stage.seconds === null ? "" : `<i class="startup-bar" data-startup-offset="${stage.offset / horizon * 100}" data-startup-width="${stage.seconds / horizon * 100}"></i>`}</div><span title="${esc(stage.source)}">${duration(stage.seconds)}${stage.ongoing ? "+" : ""}</span></div>`).join("");
  return `<div class="startup-heading"><div><span>${node.restarts ? "Created to current Ready" : "Created to Ready"}</span><strong>${duration(timing.total)}</strong></div><div class="small muted">${timing.total === null ? `Elapsed at last observation: ${duration(timing.elapsed)}` : "Observed Pod timestamps"}</div></div>
    <div class="startup-axis"><span>Startup stage</span><div>${[0, .25, .5, .75, 1].map(fraction => `<span>${Math.round(horizon * fraction)}s</span>`).join("")}</div><span>Duration</span></div>${rows}
    <div class="startup-source"><span class="badge">${timing.attempts === null ? "Pull attempt count unavailable" : `${timing.attempts} reported pull attempt${timing.attempts === 1 ? "" : "s"}`}</span>${timing.runtime !== null ? `<span class="badge oci">Runtime preparation: ${duration(timing.runtime)}</span>` : ""}${timing.cached ? '<span class="badge active">Runtime reported cache hit</span>' : ""}<span class="badge">${node.restarts || 0} container restarts</span></div>
    <p class="startup-note">Image preparation includes runtime pull and unpack work, not just network transfer. Event-derived durations are approximate. Hover a duration for its source.</p>
    ${state.deployment.image.streaming ? '<p class="small streaming-note">Artifact Streaming: Pod Ready and full-image byte completion are separate. Remaining bytes may arrive on demand.</p>' : ""}
    ${timing.warnings.map(warning => `<p class="small muted">${esc(warning)}</p>`).join("")}`;
}

function storageKey() { return `gantry-ui:${state.config.registry}:${state.config.nodePool}:${state.config.namespace}`; }

function generationSummary() {
  const layers = Number($("generate-layers").value);
  const layerMB = Number($("generate-layer-mb").value);
  const failure = buildRequestError(layers, layerMB, state.config.generation);
  $("generate-summary").textContent = `${layers} payload layers × ${layerMB} MB = ${bytes(layers * layerMB * 1000000)} random payload, plus the small runtime base. ${$("generate-streaming").checked ? "ACR streaming conversion enabled." : "Regular OCI image."}`;
  $("generate-error").textContent = failure;
  $("generate-error").hidden = !failure;
  $("generate-submit").disabled = !!failure || state.buildBusy || state.reclaimBusy;
}

function renderBuild(build, failure = "") {
  const progress = build.progress;
  const label = ({
    queued: "Build queued", building: "Building image", base: "Preparing runtime base",
    generating: "Generating random payload", pushing: "Pushing layer to ACR",
    publishing: "Publishing image", converting: "Converting image for ACR streaming", deploying: "Starting cluster pull", complete: "Image published. Cluster pull started.", failed: "Image generation failed",
  })[progress.stage] || "Building image";
  $("build-status").hidden = false;
  $("build-status").classList.toggle("failed", progress.stage === "failed");
  $("build-status").innerHTML = `<h2>${esc(label)}</h2><p class="small muted">${build.request.layers} payload layers × ${build.request.layerMB} MB · ${build.request.streaming ? "ACR streaming" : "OCI"} · ${esc(build.name)}</p><p class="small">${esc(progress.message)}${progress.layer ? ` · Layer ${progress.layer} / ${progress.layers}` : ""}${progress.layerBytes ? ` · ${bytes(progress.bytes)} / ${bytes(progress.layerBytes)}` : ""}</p>${progress.layerBytes ? bar(percent(progress.bytes, progress.layerBytes)) : ""}${failure ? `<p class="generation-error">${esc(failure)}. Retrying status...</p>` : ""}`;
  paintBars($("build-status"));
  $("generate-image").disabled = state.buildBusy || state.reclaimBusy;
  $("reclaim-space").disabled = state.buildBusy || state.reclaimBusy;
  $("generate-image").textContent = state.buildBusy ? "Build in progress..." : "+ Generate & pull";
}

function watchBuild(name) {
  clearTimeout(state.buildTimer);
  state.buildRequest?.abort();
  state.buildBusy = true;
  localStorage.setItem(`${storageKey()}:build`, name);
  const controller = new AbortController();
  state.buildRequest = controller;
  const poll = async () => {
    try {
      const build = await api(`/api/builds/${encodeURIComponent(name)}`, { signal: controller.signal });
      if (controller.signal.aborted) return;
      state.build = build;
      const finished = build.progress.stage === "failed" || !!build.deployment;
      state.buildBusy = !finished;
      renderBuild(build);
      if (finished) {
        localStorage.removeItem(`${storageKey()}:build`);
        if (build.deployment) {
          if (!state.deployments.some(item => item.id === build.deployment.id)) state.deployments.push(build.deployment);
          if (!state.images.some(image => image.id === build.deployment.id)) state.images.unshift(build.deployment.image);
          activate(build.deployment, false);
          void loadImages(true).catch(failure => error(`Image created, but catalog refresh failed: ${failure.message}`));
        }
        return;
      }
    } catch (failure) {
      if (controller.signal.aborted) return;
      if (state.build) renderBuild(state.build, failure.message);
      else error(`Cannot read image build status: ${failure.message}`);
    }
    state.buildTimer = setTimeout(poll, 2000);
  };
  void poll();
}

async function resumeBuild() {
  const builds = await api("/api/builds");
  const saved = localStorage.getItem(`${storageKey()}:build`);
  const build = builds.find(item => item.name === saved && !["failed", "complete"].includes(item.progress.stage)) || builds.find(item => !["failed", "complete"].includes(item.progress.stage));
  if (build) {
    state.build = build;
    renderBuild(build);
    watchBuild(build.name);
  } else if (saved) localStorage.removeItem(`${storageKey()}:build`);
}

$("generate-image").addEventListener("click", () => {
  generationSummary();
  $("generate-dialog").showModal();
  $("generate-layers").focus();
});

function resetRemovedDemos() {
  state.stream?.close();
  state.eventsRequest?.abort();
  state.stream = null;
  Object.assign(state, { deployment: null, snapshot: null, selected: null, detail: null, deployments: [], cells: new Map(), connected: false, lastUpdate: 0 });
  localStorage.removeItem(storageKey());
  const url = new URL(location.href);
  url.searchParams.delete("deployment");
  history.replaceState(null, "", url);
  $("workspace").hidden = true;
  $("welcome").hidden = false;
  for (const grid of document.querySelectorAll(".node-grid")) grid.replaceChildren();
  $("build-status").hidden = true;
  $("tooltip").hidden = true;
  $("image-picker-label").textContent = "Choose image";
  connection("Reclaiming disk space");
  renderCatalog();
}

function renderReclaim(reclaim) {
  const progress = reclaim.progress;
  state.reclaimBusy = !["complete", "failed"].includes(progress.stage);
  $("reclaim-space").disabled = state.reclaimBusy || state.buildBusy;
  $("generate-image").disabled = state.reclaimBusy || state.buildBusy;
  if (state.reclaimBusy && state.reclaimResetUID !== reclaim.uid) {
    state.reclaimResetUID = reclaim.uid;
    resetRemovedDemos();
  }
  const dismissed = localStorage.getItem(`${storageKey()}:reclaim-dismissed`) === reclaim.uid;
  $("reclaim-status").dataset.uid = reclaim.uid;
  $("reclaim-status").hidden = dismissed;
  $("reclaim-show").hidden = !dismissed;
  $("reclaim-status").classList.toggle("failed", progress.stage === "failed");
  const percentDone = progress.total ? progress.completed / progress.total * 100 : null;
  $("reclaim-status").innerHTML = `<div class="reclaim-heading"><h2>${progress.stage === "complete" ? "Disk reclamation finished" : progress.stage === "failed" ? "Disk reclamation needs attention" : "Reclaiming disk space"}</h2><button id="reclaim-dismiss" type="button" aria-label="Dismiss disk reclamation status" title="Hide status without stopping cleanup">Dismiss</button></div><p>${esc(progress.message)}</p><p class="small muted">${number(progress.deleted || 0)} demos deleted · ${number(progress.completed || 0)} / ${number(progress.total || 0)} workers · ${number(progress.failed || 0)} failures · ${bytes(progress.reclaimed || 0)} observed space recovered${progress.protectedLeases ? ` · ${number(progress.protectedLeases)} protected runtime leases` : ""}</p>${bar(percentDone)}<div class="reclaim-workers">${(reclaim.workers || []).map(worker => `<span class="${worker.stage === "complete" ? "complete" : worker.stage === "failed" ? "failed" : "pulling"}" title="${esc(worker.node)}: ${esc(worker.stage)}">${esc(shortName(worker.node))}</span>`).join("")}</div>`;
  paintBars($("reclaim-status"));
  renderCatalog();
}

async function pollReclaim() {
  clearTimeout(state.reclaimTimer);
  state.reclaimController?.abort();
  const controller = new AbortController();
  state.reclaimController = controller;
  try {
    const reclaim = await api("/api/reclaim", { signal: controller.signal });
    if (controller.signal.aborted) return;
    if (!reclaim) return;
    renderReclaim(reclaim);
    if (!state.reclaimBusy) {
      state.deployments = await api("/api/deployments");
      renderCatalog();
      if (!state.deployment) connection("Connected", true);
      return;
    }
  } catch (failure) {
    if (controller.signal.aborted) return;
    error(`Cannot read disk reclamation status: ${failure.message}`);
  }
  state.reclaimTimer = setTimeout(() => { void pollReclaim(); }, 2000);
}

$("reclaim-status").addEventListener("click", event => {
  if (!event.target.closest("#reclaim-dismiss")) return;
  localStorage.setItem(`${storageKey()}:reclaim-dismissed`, $("reclaim-status").dataset.uid);
  $("reclaim-status").hidden = true;
  $("reclaim-show").hidden = false;
  $("reclaim-show").focus();
});
$("reclaim-show").addEventListener("click", () => {
  localStorage.removeItem(`${storageKey()}:reclaim-dismissed`);
  $("reclaim-status").hidden = false;
  $("reclaim-show").hidden = true;
  $("reclaim-dismiss").focus();
});
$("reclaim-space").addEventListener("click", () => {
  $("reclaim-confirm").value = "";
  $("reclaim-submit").disabled = true;
  $("reclaim-error").hidden = true;
  $("reclaim-namespace").textContent = state.config.namespace;
  $("reclaim-dialog").showModal();
  $("reclaim-confirm").focus();
});
$("reclaim-close").addEventListener("click", () => $("reclaim-dialog").close());
$("reclaim-dialog").addEventListener("close", () => state.reclaimStartController?.abort());
$("reclaim-dialog").addEventListener("cancel", () => state.reclaimStartController?.abort());
$("reclaim-confirm").addEventListener("input", () => {
  $("reclaim-submit").disabled = $("reclaim-confirm").value !== "DELETE ALL DEMOS" || state.reclaimBusy || state.buildBusy || !!state.reclaimStartController;
});
$("reclaim-form").addEventListener("submit", async event => {
  event.preventDefault();
  if ($("reclaim-submit").disabled) return;
  $("reclaim-submit").disabled = true;
  const controller = new AbortController();
  state.reclaimStartController = controller;
  const deadline = Date.now() + 120000;
  try {
    const body = JSON.stringify({ confirm: $("reclaim-confirm").value });
    let reclaim;
    while (!controller.signal.aborted) {
      try {
        reclaim = await api("/api/reclaim", {
          method: "POST", headers: { "Content-Type": "application/json" },
          body, signal: controller.signal,
        });
        break;
      } catch (failure) {
        if (controller.signal.aborted) return;
        if (!(failure.retryAfter > 0) || Date.now() >= deadline) throw failure;
        $("reclaim-error").textContent = "Removing the previous cleanup's Jobs. Retrying automatically; close this dialog to cancel.";
        $("reclaim-error").hidden = false;
        await new Promise(resolve => {
          const done = () => { clearTimeout(timer); controller.signal.removeEventListener("abort", done); resolve(); };
          const timer = setTimeout(done, Math.min(failure.retryAfter, 5) * 1000);
          controller.signal.addEventListener("abort", done, { once: true });
        });
      }
    }
    if (controller.signal.aborted) return;
    state.reclaimStartController = null;
    $("reclaim-dialog").close();
    renderReclaim(reclaim);
    void pollReclaim();
  } catch (failure) {
    if (controller.signal.aborted) return;
    $("reclaim-error").textContent = failure.message;
    $("reclaim-error").hidden = false;
    $("reclaim-submit").disabled = false;
  } finally {
    if (state.reclaimStartController === controller) state.reclaimStartController = null;
  }
});
$("generate-close").addEventListener("click", () => $("generate-dialog").close());
$("generate-layers").addEventListener("input", generationSummary);
$("generate-layer-mb").addEventListener("input", generationSummary);
$("generate-streaming").addEventListener("change", generationSummary);
$("generate-form").addEventListener("submit", async event => {
  event.preventDefault();
  generationSummary();
  if ($("generate-submit").disabled) return;
  state.buildBusy = true;
  $("generate-submit").disabled = true;
  try {
    const build = await api("/api/builds", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ layers: Number($("generate-layers").value), layerMB: Number($("generate-layer-mb").value), streaming: $("generate-streaming").checked }),
    });
    state.build = build;
    $("generate-dialog").close();
    renderBuild(build);
    watchBuild(build.name);
  } catch (failure) {
    state.buildBusy = false;
    $("generate-error").textContent = failure.message;
    $("generate-error").hidden = false;
    $("generate-submit").disabled = false;
  }
});

$("images").addEventListener("click", event => {
  const card = event.target.closest("[data-image]");
  if (card && !card.disabled) void selectImage(card.dataset.image);
});
$("search").addEventListener("input", renderCatalog);
$("refresh").addEventListener("click", () => { void loadImages(true).catch(failure => error(failure.message)); });
$("node-detail").addEventListener("click", event => {
  if (event.target.closest("#clear-selection")) clearSelection();
});
document.addEventListener("click", event => {
  if (!$("image-picker").contains(event.target)) $("image-picker").open = false;
});
document.addEventListener("keydown", event => {
  if (event.key !== "Escape") return;
  if ($("image-picker").open) { $("image-picker").open = false; $("image-picker").querySelector("summary").focus(); }
  else if (state.selected) clearSelection();
});
for (const id of ["history-from", "history-to"]) $(id).addEventListener("input", () => {
  const window = historyWindow(state.snapshot?.history, state.snapshot?.observedAt);
  if (!window.available) return;
  const from = Math.min(Number($("history-from").value), Number($("history-to").value) - 1);
  const to = Math.max(from + 1, Number($("history-to").value));
  $("history-from").value = String(Math.max(0, from));
  $("history-to").value = String(Math.max(1, to));
  const span = window.fullEnd - window.fullStart;
  state.timeRange = [window.fullStart + span * Math.max(0, from) / 1000, window.fullStart + span * Math.max(1, to) / 1000];
  renderCharts();
});
$("history-reset").addEventListener("click", () => {
  state.timeRange = null;
  $("history-from").value = "0";
  $("history-to").value = "1000";
  renderCharts();
});
$("filter").addEventListener("change", renderSnapshot);
$("fit").addEventListener("click", () => { state.zoom = 1; renderSnapshot(); });
$("zoom-in").addEventListener("click", () => { state.zoom = Math.min(4, state.zoom * 1.4); renderSnapshot(); });
$("zoom-out").addEventListener("click", () => { state.zoom = Math.max(1, state.zoom / 1.4); renderSnapshot(); });
for (const grid of document.querySelectorAll(".node-grid")) {
grid.addEventListener("click", event => {
  const cell = event.target.closest("[data-node]");
  if (cell) chooseNode(cell.dataset.node);
});
grid.addEventListener("keydown", event => {
  const cells = [...grid.children];
  const index = cells.indexOf(event.target);
  if (index < 0) return;
  const columns = Math.max(8, Math.floor(25 / state.zoom));
  const offsets = { ArrowRight: 1, ArrowLeft: -1, ArrowDown: columns, ArrowUp: -columns };
  let next = index;
  if (event.key in offsets) next += offsets[event.key];
  else if (event.key === "Home") next = 0;
  else if (event.key === "End") next = cells.length - 1;
  else return;
  event.preventDefault();
  cells[Math.max(0, Math.min(cells.length - 1, next))]?.focus();
});
grid.addEventListener("pointerover", event => {
  const cell = event.target.closest("[data-node]");
  const node = state.snapshot?.nodes.find(node => node.name === cell?.dataset.node);
  if (!node) return;
  const pct = percent(node.available, state.deployment.image.size);
  const tooltip = $("tooltip");
  const view = grid.dataset.view;
  tooltip.innerHTML = view === "storage"
    ? `<strong>${esc(shortName(node.name))}</strong><span>${bytes(node.storage?.used)} used · ${bytes(node.storage?.available)} left</span><span>${progressText(storagePercent(node.storage))} used · ${bytes(node.storage?.total)} capacity</span><span class="small muted">${node.storageUpdatedAt ? `Observed ${esc(time(node.storageUpdatedAt))}` : "Waiting for a filesystem observation"}</span>`
    : `<strong>${esc(shortName(node.name))}</strong><span>${view === "distribution" ? `Image bytes available: ${progressText(pct)}` : esc(phaseLabel(node.phase))}</span><span class="small muted">${view === "distribution" && node.updatedAt ? `Last byte observation: ${esc(time(node.updatedAt))}` : "Click for details"}</span>`;
  const rect = cell.getBoundingClientRect();
  tooltip.style.left = `${Math.max(8, Math.min(innerWidth - 290, rect.left + 12))}px`;
  tooltip.style.top = `${Math.max(8, Math.min(innerHeight - 130, rect.bottom + 10))}px`;
  tooltip.hidden = false;
});
grid.addEventListener("pointerleave", () => { $("tooltip").hidden = true; });
}
window.addEventListener("beforeunload", () => { state.stream?.close(); state.eventsRequest?.abort(); state.buildRequest?.abort(); state.reclaimController?.abort(); state.reclaimStartController?.abort(); clearTimeout(state.buildTimer); clearTimeout(state.reclaimTimer); });
setInterval(() => {
  if (state.selected && !state.detailLoading) void loadNode();
}, 5000);
function renderUpdated() {
  if (!state.lastUpdate) return;
  const age = Math.max(0, Math.floor((Date.now() - state.lastUpdate) / 1000));
  if (age > 45) {
    state.stale = true;
    $("grid-panel").classList.add("stale");
    connection("Stale observations");
  }
  $("updated").textContent = `Observed ${time(state.lastUpdate)} · ${age < 2 ? "just now" : `${age}s ago`}${state.stale ? " · stale" : ""}`;
}
setInterval(renderUpdated, 1000);

async function start() {
  try {
    state.config = await api("/api/config");
    if (state.config.reclaim?.enabled) $("reclaim-space").hidden = false;
    if (state.config.generation?.enabled) {
      $("generate-image").hidden = false;
      $("generate-layers").max = state.config.generation.maxLayers;
      $("generate-layer-mb").max = state.config.generation.maxTotalMB;
      $("generate-limits").textContent = `Up to ${state.config.generation.maxLayers} payload layers and ${bytes(state.config.generation.maxTotalMB * 1000000)} total payload.`;
    }
    $("registry").textContent = state.config.registry;
    $("cluster-label").textContent = `${state.config.nodePool} · ${state.config.namespace}`;
    const results = await Promise.allSettled([loadImages(), api("/api/deployments")]);
    if (results[1].status === "fulfilled") {
      state.deployments = results[1].value;
      renderCatalog();
      const saved = new URL(location.href).searchParams.get("deployment") || localStorage.getItem(storageKey());
      const deployment = state.deployments.find(item => item.id === saved);
      if (deployment) activate(deployment, true);
      else if (saved) error("The previously selected deployment no longer exists. Select an image to create a new one.");
      else connection("Connected", true);
    } else error(`Cannot discover deployments: ${results[1].reason.message}`);
    if (results[0].status === "rejected") error(`Cannot load ACR catalog: ${results[0].reason.message}`);
    if (state.config.generation?.enabled) void resumeBuild().catch(failure => error(`Cannot resume image builds: ${failure.message}`));
    if (state.config.reclaim?.enabled) void pollReclaim();
  } catch (failure) { error(failure.message); connection("Unavailable"); }
}
void start();

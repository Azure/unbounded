// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { buildRequestError, bytes, distributionState, escape as esc, mergeNodeUpdates, percent, phaseLabel, storagePercent, storageState, summary } from "./model.js";
import { clusterActivityHTML, drawBandwidth } from "./activity.js";
import { duration, startupTimeline } from "./startup.js";

const $ = id => document.getElementById(id);
const state = {
  config: null, images: [], deployments: [], deployment: null, snapshot: null,
  selected: null, detail: null, events: [], eventsError: "", detailError: "",
  view: "distribution", stream: null, zoom: 1, busy: false, connected: false,
  stale: false, lastUpdate: 0, eventsRequest: null, cells: new Map(), traffic: [],
  build: null, buildBusy: false, buildTimer: null, buildRequest: null,
  renderFrame: 0, renderedNode: null, detailLoading: false, detailObservation: null,
  clusterTraffic: [], eventsPodUID: null, timingsHTML: "", clusterPanelHTML: "",
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
  Object.assign(state, { deployment, snapshot: null, selected: null, detail: null, events: [], cells: new Map(), traffic: [], lastUpdate: 0, connected: false });
  $("grid").replaceChildren();
  $("stats").replaceChildren();
  $("grid-summary").textContent = "Collecting workers for this deployment...";
  $("export-timings").disabled = true;
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
  const counts = summary(snapshot.nodes, image, state.view);
  renderClusterOverview();
  $("export-timings").disabled = false;
  $("grid-title").textContent = state.view === "distribution" ? "Cluster distribution" : state.view === "storage" ? "Node storage" : state.view === "startup" ? "Worker startup state" : "Pod lifecycle";
  $("grid-description").textContent = state.view === "distribution"
    ? "One cell per worker · Image bytes available locally"
    : state.view === "storage" ? "One cell per worker · Filesystem backing Gantry data, across all images and workloads"
      : "One cell per worker · Observed Pod state, independent of image download";
  const active = new Set(snapshot.nodes.map(node => node.name));
  if (state.selected && !active.has(state.selected)) {
    state.eventsRequest?.abort();
    state.selected = null;
    state.detail = null;
  }
  $("grid-summary").textContent = state.view === "distribution"
    ? `${number(snapshot.nodes.length)} workers · ${number(counts.complete || 0)} bytes complete · ${number(counts.pulling || 0)} downloading · ${number(counts.failed || 0)} pull errors`
    : `${number(snapshot.nodes.length)} workers · ${number(counts.ready || 0)} Ready · ${number((counts.preparing || 0) + (counts.queued || 0) + (counts.scheduling || 0))} preparing · ${number(counts.failed || 0)} errors`;
  if (state.view === "storage") {
    const reporting = snapshot.nodes.filter(node => storagePercent(node.storage) !== null);
    const totals = reporting.reduce((sum, node) => ({ used: sum.used + node.storage.used, available: sum.available + node.storage.available }), { used: 0, available: 0 });
    $("grid-summary").textContent = `${number(reporting.length)} / ${number(snapshot.nodes.length)} workers observed · ${reporting.length ? bytes(totals.used) : "--"} used · ${reporting.length ? bytes(totals.available) : "--"} left`;
  }
  for (const [name, cell] of state.cells) {
    if (!active.has(name)) { cell.remove(); state.cells.delete(name); }
  }
  let position = 0;
  $("grid").dataset.empty = "No Linux workers match this pool and architecture.";
  for (const node of snapshot.nodes) {
    let cell = state.cells.get(node.name);
    if (!cell) {
      cell = document.createElement("button");
      cell.type = "button";
      cell.dataset.node = node.name;
      cell.className = "node-cell";
      cell.innerHTML = '<span class="cell-fill"></span><span class="cell-label"></span>';
      state.cells.set(node.name, cell);
    }
    const phase = state.view === "distribution" ? distributionState(node, image) : state.view === "storage" ? storageState(node) : node.phase;
    const value = state.view === "storage" ? storagePercent(node.storage) : percent(node.available, image.size);
    const filter = $("filter").value;
    const before = $("grid").children[position++];
    if (before !== cell) $("grid").insertBefore(cell, before || null);
    const storageLabel = state.view === "storage" ? `, ${bytes(node.storage?.used)} used, ${bytes(node.storage?.available)} left` : "";
    const visualKey = JSON.stringify([phase, value, storageLabel, state.view, state.selected === node.name, node.nodeReady, filter, position === 1]);
    if (cell.dataset.visualKey === visualKey) continue;
    cell.dataset.visualKey = visualKey;
    cell.className = `node-cell ${phase} ${state.selected === node.name ? "selected" : ""} ${node.nodeReady ? "" : "node-unready"}`;
    cell.setAttribute("aria-label", `${node.name}: ${phaseLabel(phase)}${state.view === "distribution" ? `, ${progressText(value)} bytes available` : storageLabel}`);
    cell.setAttribute("aria-pressed", String(state.selected === node.name));
    cell.tabIndex = state.selected === node.name || (!state.selected && position === 1) ? 0 : -1;
    cell.querySelector(".cell-fill").style.width = `${state.view === "lifecycle" ? 100 : value ?? 0}%`;
    cell.querySelector(".cell-label").textContent = state.view === "distribution" ? `${shortName(node.name)} ${progressText(value)}` : state.view === "storage" ? `${shortName(node.name)} ${bytes(node.storage?.available)} left` : shortName(node.name);
    cell.classList.toggle("dimmed", filter !== "all" && (filter === "active" ? !["pulling", "preparing", "scheduling", "running"].includes(phase) : phase !== filter));
  }
  $("grid").classList.toggle("zoomed", state.zoom > 1);
  $("grid").style.gridTemplateColumns = `repeat(${Math.max(8, Math.floor(40 / state.zoom))}, minmax(0, 1fr))`;
  $("grid").style.gridAutoRows = state.zoom > 1 ? `${24 * state.zoom}px` : "minmax(6px, 1fr)";
  const legend = state.view === "distribution" ? ["complete", "pulling", "queued", "unknown", "failed"] : state.view === "storage" ? ["storage-ok", "storage-high", "storage-full", "unknown"] : ["scheduling", "preparing", "running", "ready", "completed", "failed"];
  $("legend").innerHTML = legend.map(phase => `<span class="${phase}"><i></i>${phaseLabel(phase)}</span>`).join("");
  if (state.renderedNode !== selectedNode()) renderDetail();
  if (state.view === "startup" && state.selected) renderTimingsTable();
}

function renderClusterOverview() {
  const activity = state.snapshot?.activity;
  const rendered = clusterActivityHTML(activity);
  if ($("stats").innerHTML !== rendered.cards) $("stats").innerHTML = rendered.cards;
  $("activity-coverage").textContent = rendered.coverage;
  if (state.clusterPanelHTML !== rendered.panel) {
    state.clusterPanelHTML = rendered.panel;
    $("cluster-activity").innerHTML = rendered.panel;
    paintBars($("cluster-activity"));
  }
  const at = Date.parse(activity?.observedAt);
  const last = state.clusterTraffic.at(-1);
  if (!activity?.rateReporting) { drawBandwidth($("cluster-bandwidth"), []); return; }
  if (Number.isFinite(at) && activity.originRate != null && activity.peerRate != null && (!last || at - last.at >= 500)) {
    state.clusterTraffic.push({ at, origin: activity.originRate, peer: activity.peerRate });
    state.clusterTraffic = state.clusterTraffic.filter(point => point.at >= at - 60000).slice(-180);
    drawBandwidth($("cluster-bandwidth"), state.clusterTraffic);
  } else if (!state.clusterTraffic.length) drawBandwidth($("cluster-bandwidth"), []);
}

function selectedNode() { return state.snapshot?.nodes.find(node => node.name === state.selected); }
function observationKey(node) { return JSON.stringify([node?.podUID, node?.updatedAt, node?.telemetryError, node?.storageUpdatedAt]); }

function clearSelection() {
  const previous = state.cells.get(state.selected);
  if (previous) { previous.classList.remove("selected"); previous.setAttribute("aria-pressed", "false"); previous.tabIndex = -1; }
  state.eventsRequest?.abort();
  Object.assign(state, { selected: null, detail: null, events: [], eventsPodUID: null, detailLoading: false, detailObservation: null, traffic: [] });
  if (state.cells.size) state.cells.values().next().value.tabIndex = 0;
  renderDetail();
}

function chooseNode(name) {
  if (state.selected === name) { clearSelection(); return; }
  const previous = state.cells.get(state.selected);
  if (previous) {
    previous.classList.remove("selected");
    previous.setAttribute("aria-pressed", "false");
    previous.tabIndex = -1;
  } else if (state.cells.size) state.cells.values().next().value.tabIndex = -1;
  state.selected = name;
  const cell = state.cells.get(name);
  if (cell) {
    cell.classList.add("selected");
    cell.setAttribute("aria-pressed", "true");
    cell.tabIndex = 0;
  }
  state.detail = null;
  state.events = [];
  state.eventsPodUID = null;
  state.eventsError = "";
  state.detailError = "";
  state.traffic = [];
  $("tooltip").hidden = true;
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
    const previous = state.detail;
    state.detail = detail;
    state.detailError = "";
    if (!detail.telemetryError && detail.updatedAt !== previous?.updatedAt && detail.peerRate !== null && detail.originRate !== null) {
      state.traffic.push({ peer: state.detail.peerRate, origin: state.detail.originRate });
      state.traffic = state.traffic.slice(-40);
    }
    renderDetail();
  }, failure => {
    if (!current()) return;
    state.detailError = failure.message;
    renderDetail();
  })];
  if (["lifecycle", "startup"].includes(state.view) && (!live || state.eventsPodUID !== podUID)) requests.push(api(`${base}/events?node=${encodeURIComponent(name)}`, { signal: controller.signal }).then(events => {
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
  $("cluster-activity").hidden = !!node;
  $("node-detail").hidden = !node;
  $("activity").hidden = !node;
  $("timings-table-panel").hidden = !node || state.view !== "startup";
  if (!node) {
    renderClusterOverview();
    return;
  }
  const image = state.deployment.image;
  const storageView = state.view === "storage";
  const value = storageView ? storagePercent(node.storage) : percent(node.available, image.size);
  const phase = storageView ? storageState(node) : node.phase;
  const restoreClearFocus = document.activeElement?.id === "clear-selection";
  let content = `<div class="detail-heading"><h2>${esc(shortName(node.name))}</h2><span class="phase-badge ${esc(phase)}">${esc(phaseLabel(phase))}</span><button id="clear-selection" class="clear-selection">Clear selection &#215;</button><div class="detail-progress"><span>${storageView ? "Disk used" : "Image bytes available"} <strong>${progressText(value)}</strong></span>${bar(value, storageView ? phase : "")}</div></div><p class="small muted node-name">${esc(node.name)}</p>`;
  if (!node.nodeReady) content += '<p class="notice warning-notice">Kubernetes reports this worker as not Ready.</p>';
  if (state.view === "distribution") {
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
  } else if (storageView) {
    const storage = node.storage;
    if (value === null) content += '<p class="muted">Waiting for the first filesystem observation.</p>';
    else {
      content += `<div class="storage-cards">${[
        ["Used", storage.used], ["Space left", storage.available], ["Total capacity", storage.total], ["Filesystem reserved", storage.total - storage.used - storage.available],
      ].map(([label, amount]) => `<div><span>${label}</span><strong>${bytes(amount)}</strong></div>`).join("")}</div><p class="small muted">Filesystem backing <span class="digest">${esc(storage.path)}</span>. Includes all files on this filesystem, not just Gantry or the selected image. Usage percentage excludes filesystem-reserved space.</p><p class="small muted">Last storage observation: ${esc(time(node.storageUpdatedAt))}.</p>`;
    }
  } else if (state.view === "startup") {
    content += startupHTML(node);
    if (state.eventsError) content += `<p class="notice error-notice">${esc(state.eventsError)}</p>`;
  } else {
    content += `<p class="lifecycle-reason">${esc(node.reason)}</p><div class="timeline">${[
      ["Pod created", node.created], ["Scheduled", node.scheduled], ["Container started", node.started], ["Pod Ready", node.readyAt],
    ].map(([label, at]) => `<div class="${at ? "observed" : ""}"><i></i><strong>${label}</strong><span>${time(at)}</span></div>`).join("")}</div>`;
    content += '<p class="small muted">Download, unpack, and mount work can overlap. "Preparing" is an honest combined phase; Kubernetes does not expose separate unpack percentages.</p><h3>Observed Kubernetes events</h3>';
    if (state.eventsError) content += `<p class="notice error-notice">${esc(state.eventsError)}</p>`;
    else content += `<div class="event-list">${state.events.length ? state.events.map(event => `<article class="${event.type === "Warning" ? "warning-event" : ""}"><time>${time(event.at)}</time><div><strong>${esc(event.reason)}</strong><p>${esc(event.message)}</p>${event.count > 1 ? `<span class="small muted">${event.count} occurrences</span>` : ""}</div></article>`).join("") : '<p class="muted">No retained events for this Pod. Events may expire; missing history is not reconstructed.</p>'}</div>`;
  }
  $("node-detail").innerHTML = content;
  paintBars($("node-detail"));
  for (const element of $("node-detail").querySelectorAll("[data-startup-offset]")) {
    element.style.left = `${Number(element.dataset.startupOffset)}%`;
    element.style.width = `${Number(element.dataset.startupWidth)}%`;
  }
  if (restoreClearFocus) $("clear-selection").focus({ preventScroll: true });
  const detail = state.detail;
  $("activity").innerHTML = `<h2>Node transfer activity</h2><p class="small muted">All layer traffic on ${esc(shortName(node.name))}, not attribution to this image.</p><div class="traffic-cards"><div class="peer"><span>From peers</span><strong>${bytes(detail?.peerRate)}/s</strong></div><div class="origin"><span>From origin</span><strong>${bytes(detail?.originRate)}/s</strong></div></div><svg id="sparkline" class="sparkline" viewBox="0 0 360 110" role="img" aria-label="Recent peer and origin transfer rates"></svg><div class="legend"><span class="pulling"><i></i>Peer traffic</span><span class="preparing"><i></i>Origin traffic</span></div><p class="small muted">${detail?.peerRate == null ? "Rates require two successful observations." : `Observed ${time(detail.updatedAt)}. Retries count as network traffic, not extra progress.`}</p>`;
  drawTraffic();
  if (state.view === "startup") renderTimingsTable();
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

function renderTimingsTable() {
  if (!state.snapshot) return;
  const entries = state.snapshot.nodes.map(node => ({ node, timing: startupTimeline(node, [], state.snapshot.observedAt) }));
  entries.sort((a, b) => (b.timing.total ?? -1) - (a.timing.total ?? -1) || a.node.name.localeCompare(b.node.name));
  const rows = entries.slice(0, 20).map(({ node, timing }) => `<tr class="${state.selected === node.name ? "selected" : ""}"><td><button data-timing-node="${esc(node.name)}">${esc(shortName(node.name))}</button></td><td>${esc(phaseLabel(node.phase))}</td><td>${duration(timing.stages[0].seconds)}</td><td>${duration(timing.stages[4].seconds)}</td><td>${duration(timing.total)}</td><td>${node.restarts || 0}</td></tr>`).join("");
  if (state.timingsHTML !== rows) { state.timingsHTML = rows; $("timings-table").innerHTML = rows; }
}

function drawTraffic() {
  const svg = $("sparkline");
  const peak = Math.max(1, ...state.traffic.flatMap(point => [point.peer, point.origin]));
  for (const [key, color] of [["peer", "#21a5ff"], ["origin", "#ffbe45"]]) {
    const line = document.createElementNS("http://www.w3.org/2000/svg", "polyline");
    line.setAttribute("points", state.traffic.map((point, index) => `${index / Math.max(1, state.traffic.length - 1) * 356 + 2},${106 - point[key] / peak * 96}`).join(" "));
    line.setAttribute("fill", "none");
    line.setAttribute("stroke", color);
    line.setAttribute("stroke-width", "2");
    svg.append(line);
  }
}

function setView(view) {
  state.view = view;
  state.renderedNode = null;
  $("distribution-tab").setAttribute("aria-selected", String(view === "distribution"));
  $("lifecycle-tab").setAttribute("aria-selected", String(view === "lifecycle"));
  $("startup-tab").setAttribute("aria-selected", String(view === "startup"));
  $("storage-tab").setAttribute("aria-selected", String(view === "storage"));
  const previousFilter = $("filter").value;
  const filters = view === "storage"
    ? [["all", "All workers"], ["storage-high", "85-95% used"], ["storage-full", "95-100% used"], ["unknown", "Unknown"]]
    : [["all", "All workers"], ["active", "In progress"], ["failed", "Errors"], ["unknown", "Unknown"]];
  $("filter").innerHTML = filters.map(([value, label]) => `<option value="${value}">${label}</option>`).join("");
  $("filter").value = filters.some(([value]) => value === previousFilter) ? previousFilter : "all";
  $("tooltip").hidden = true;
  renderSnapshot();
  if (state.selected) void loadNode();
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
  $("grid").replaceChildren();
  $("build-status").hidden = true;
  $("tooltip").hidden = true;
  $("export-timings").disabled = true;
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
$("distribution-tab").addEventListener("click", () => setView("distribution"));
$("lifecycle-tab").addEventListener("click", () => setView("lifecycle"));
$("startup-tab").addEventListener("click", () => setView("startup"));
$("storage-tab").addEventListener("click", () => setView("storage"));
$("node-detail").addEventListener("click", event => {
  if (event.target.closest("#clear-selection")) clearSelection();
});
$("timings-table").addEventListener("click", event => {
  const button = event.target.closest("[data-timing-node]");
  if (button) chooseNode(button.dataset.timingNode);
});
document.addEventListener("click", event => {
  if (!$("image-picker").contains(event.target)) $("image-picker").open = false;
});
document.addEventListener("keydown", event => {
  if (event.key !== "Escape") return;
  if ($("image-picker").open) { $("image-picker").open = false; $("image-picker").querySelector("summary").focus(); }
  else if (state.selected) clearSelection();
});
$("export-timings").addEventListener("click", () => {
  if (!state.snapshot) return;
  const fields = ["worker", "pod", "pod_uid", "state", "created", "scheduled", "container_started", "ready", "created_to_ready_seconds", "container_restarts"];
  const rows = state.snapshot.nodes.map(node => {
    const timing = startupTimeline(node, [], state.snapshot.observedAt);
    return [node.name, node.pod, node.podUID, node.phase, node.created, node.scheduled, node.started, node.readyAt, timing.total, node.restarts || 0];
  });
  const csv = [fields, ...rows].map(row => row.map(value => {
    const raw = value == null ? "" : String(value);
    const safe = /^[=+@\-\t\r]/.test(raw) ? `'${raw}` : raw;
    return `"${safe.replaceAll('"', '""')}"`;
  }).join(",")).join("\r\n");
  const url = URL.createObjectURL(new Blob([csv], { type: "text/csv;charset=utf-8" }));
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = `gantry-startup-${state.deployment.id}.csv`;
  anchor.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
});
$("filter").addEventListener("change", renderSnapshot);
$("fit").addEventListener("click", () => { state.zoom = 1; renderSnapshot(); });
$("zoom-in").addEventListener("click", () => { state.zoom = Math.min(4, state.zoom * 1.4); renderSnapshot(); });
$("zoom-out").addEventListener("click", () => { state.zoom = Math.max(1, state.zoom / 1.4); renderSnapshot(); });
$("grid").addEventListener("click", event => {
  const cell = event.target.closest("[data-node]");
  if (cell) chooseNode(cell.dataset.node);
});
$("grid").addEventListener("keydown", event => {
  const cells = [...$("grid").children];
  const index = cells.indexOf(event.target);
  if (index < 0) return;
  const columns = Math.max(8, Math.floor(40 / state.zoom));
  const offsets = { ArrowRight: 1, ArrowLeft: -1, ArrowDown: columns, ArrowUp: -columns };
  let next = index;
  if (event.key in offsets) next += offsets[event.key];
  else if (event.key === "Home") next = 0;
  else if (event.key === "End") next = cells.length - 1;
  else return;
  event.preventDefault();
  cells[Math.max(0, Math.min(cells.length - 1, next))]?.focus();
});
$("grid").addEventListener("pointerover", event => {
  const cell = event.target.closest("[data-node]");
  const node = state.snapshot?.nodes.find(node => node.name === cell?.dataset.node);
  if (!node) return;
  const pct = percent(node.available, state.deployment.image.size);
  const tooltip = $("tooltip");
  tooltip.innerHTML = state.view === "storage"
    ? `<strong>${esc(shortName(node.name))}</strong><span>${bytes(node.storage?.used)} used · ${bytes(node.storage?.available)} left</span><span>${progressText(storagePercent(node.storage))} used · ${bytes(node.storage?.total)} capacity</span><span class="small muted">${node.storageUpdatedAt ? `Observed ${esc(time(node.storageUpdatedAt))}` : "Waiting for a filesystem observation"}</span>`
    : `<strong>${esc(shortName(node.name))}</strong><span>${state.view === "distribution" ? `Image bytes available: ${progressText(pct)}` : esc(phaseLabel(node.phase))}</span><span class="small muted">${state.view === "distribution" && node.updatedAt ? `Last byte observation: ${esc(time(node.updatedAt))}` : "Click for details"}</span>`;
  const rect = cell.getBoundingClientRect();
  tooltip.style.left = `${Math.max(8, Math.min(innerWidth - 290, rect.left + 12))}px`;
  tooltip.style.top = `${Math.max(8, Math.min(innerHeight - 130, rect.bottom + 10))}px`;
  tooltip.hidden = false;
});
$("grid").addEventListener("pointerleave", () => { $("tooltip").hidden = true; });
window.addEventListener("beforeunload", () => { state.stream?.close(); state.eventsRequest?.abort(); state.buildRequest?.abort(); state.reclaimController?.abort(); state.reclaimStartController?.abort(); clearTimeout(state.buildTimer); clearTimeout(state.reclaimTimer); });
setInterval(() => {
  if (["lifecycle", "startup"].includes(state.view) && state.selected && !state.detailLoading) void loadNode();
}, 5000);
setInterval(() => {
  if (!state.lastUpdate) return;
  const age = Math.max(0, Math.floor((Date.now() - state.lastUpdate) / 1000));
  $("updated").textContent = `Last snapshot ${age < 2 ? "just now" : `${age}s ago`}${state.stale ? " · stale" : ""}`;
  if (age > 45) {
    state.stale = true;
    $("grid-panel").classList.add("stale");
    connection("Stale observations");
  }
}, 1000);

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

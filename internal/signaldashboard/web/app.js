// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

"use strict";

const refreshIntervalMs = 7000;
let refreshTimer;

const byId = (id) => document.getElementById(id);
const clear = (node) => { while (node.firstChild) node.removeChild(node.firstChild); };
const text = (tag, value, className) => {
  const node = document.createElement(tag);
  node.textContent = value ?? "";
  if (className) node.className = className;
  return node;
};
const badge = (value) => text("span", value || "unknown", `badge ${(value || "unknown").toLowerCase()}`);
const formatTime = (value) => value ? new Date(value).toLocaleString() : "n/a";
const shortName = (value) => value ? (value.length > 30 ? `${value.slice(0, 27)}...` : value) : "n/a";

async function fetchJSON(url) {
  const response = await fetch(url, { headers: { "Accept": "application/json" } });
  const body = await response.json();
  if (!response.ok) throw new Error(body.error || `Request failed with status ${response.status}`);
  return body;
}

function cell(primary, secondary, className = "") {
  const td = document.createElement("td");
  td.append(text("div", primary, `primary ${className}`.trim()));
  if (secondary) td.append(text("div", secondary, "secondary"));
  return td;
}

function showErrors(...responses) {
  const errors = responses.flatMap((response) => response?.errors || []);
  const box = byId("global-error");
  if (!errors.length) {
    box.classList.add("hidden");
    return;
  }
  box.textContent = `Partial Kubernetes data: ${errors.map((item) => `${item.resource}: ${item.message}`).join("; ")}`;
  box.classList.remove("hidden");
}

function renderSummary(summary) {
  const cards = byId("cards");
  clear(cards);
  [
    ["Running", summary.counts.running],
    ["Pending", summary.counts.pending],
    ["Passed", summary.counts.passed],
    ["Failed", summary.counts.failed],
    ["Inconclusive", summary.counts.inconclusive],
  ].forEach(([label, value]) => {
    const card = text("article", "", "card");
    card.append(text("span", label), text("strong", String(value)));
    cards.append(card);
  });

  const health = byId("health");
  clear(health);
  const controller = badge(`Controller ${summary.controller.status}`);
  controller.title = summary.controller.message || "";
  const argo = badge(`Argo ${summary.argo.status}`);
  argo.title = summary.argo.message || "";
  health.append(controller, argo);

  const reasons = byId("reason-grid");
  clear(reasons);
  Object.entries(summary.reasonExplanations).forEach(([reason, explanation]) => {
    const article = document.createElement("article");
    article.append(text("h3", reason), text("p", explanation));
    reasons.append(article);
  });
}

function renderRuns(runs) {
  const body = byId("runs-body");
  clear(body);
  byId("runs-empty").classList.toggle("hidden", runs.items.length > 0);

  runs.items.forEach((run) => {
    const row = document.createElement("tr");
    row.dataset.run = run.name;
    row.tabIndex = 0;
    row.setAttribute("role", "button");
    row.setAttribute("aria-label", `Open details for ${run.name}`);
    row.append(
      cell(shortName(run.name), run.profile, "mono"),
      cell(run.nodeAliases.join(", ") || "approved-node", run.hardwareClass),
    );

    const phase = document.createElement("td");
    phase.append(badge(run.phase));
    phase.append(text("div", run.explanation, "secondary"));
    row.append(phase);

    const result = document.createElement("td");
    result.append(badge(run.verdict || run.execution || "awaiting result"));
    result.append(text("div", `${run.execution || "Pending"} / ${run.reason || "No typed reason yet"}`, "secondary"));
    row.append(result);

    row.append(
      cell(shortName(run.workflowName), `${run.workflowPhase || "not created"} · ${shortName(run.mpiJobName)} ${run.mpiJobPhase || ""}`, "mono"),
      cell(run.duration || "n/a", `Started ${formatTime(run.startedAt)} | next due ${formatTime(run.nextDueAt)}`),
      cell(run.trigger, `Attempt ${run.attempt || 1} | script ${run.scriptDigest || "n/a"}`),
    );
    row.addEventListener("click", () => openRun(run.name));
    row.addEventListener("keydown", (event) => {
      if (event.key === "Enter" || event.key === " ") openRun(run.name);
    });
    body.append(row);
  });
}

function renderNodes(nodes) {
  const body = byId("nodes-body");
  clear(body);
  byId("nodes-empty").classList.toggle("hidden", nodes.items.length > 0);
  nodes.items.forEach((node) => {
    const row = document.createElement("tr");
    row.append(
      cell(node.alias, node.hardwareClass),
      cell(node.profile || "n/a", node.activeRun ? `Active: ${shortName(node.activeRun)}` : "No active reservation"),
    );
    const state = document.createElement("td");
    state.append(badge(node.state));
    row.append(state);
    const eligibility = document.createElement("td");
    eligibility.append(badge(node.eligibility));
    row.append(eligibility);
    row.append(
      cell(node.lastVerdict || "n/a", `${node.lastReason || "No terminal result"} | ${formatTime(node.lastRunAt)}`),
      cell(formatTime(node.nextDueAt), `${node.recurringReason || "No policy hold"}${node.recurringSuspended ? " · suspended" : ""}`),
    );
    body.append(row);
  });
}

function filterQuery() {
  const params = new URLSearchParams();
  [
    ["phase", "phase-filter"],
    ["verdict", "verdict-filter"],
    ["profile", "profile-filter"],
    ["hardware", "hardware-filter"],
  ].forEach(([name, id]) => {
    const value = byId(id).value.trim();
    if (value) params.set(name, value);
  });
  return params.toString();
}

async function refresh() {
  clearTimeout(refreshTimer);
  byId("refresh-state").textContent = "Refreshing...";
  try {
    const query = filterQuery();
    const [summary, runs, nodes] = await Promise.all([
      fetchJSON("/api/summary"),
      fetchJSON(`/api/runs?limit=100${query ? `&${query}` : ""}`),
      fetchJSON("/api/nodes"),
    ]);
    renderSummary(summary);
    renderRuns(runs);
    renderNodes(nodes);
    showErrors(summary, runs, nodes);
    byId("refresh-state").textContent = `Updated ${new Date(summary.lastRefresh).toLocaleTimeString()} · auto-refresh 7s`;
  } catch (error) {
    const box = byId("global-error");
    box.textContent = `Dashboard refresh failed: ${error.message}`;
    box.classList.remove("hidden");
    byId("refresh-state").textContent = "Refresh failed";
  } finally {
    refreshTimer = setTimeout(refresh, refreshIntervalMs);
  }
}

function renderStages(stages) {
  const list = byId("stage-list");
  clear(list);
  stages.forEach((stage) => {
    const article = text("article", "", "stage");
    article.append(text("h3", stage.name), badge(stage.phase));
    const fallback = stage.phase === "Succeeded" ? "Stage completed." :
      stage.phase === "Running" ? "Stage is running." : "Waiting for this stage.";
    article.append(text("p", stage.message || stage.reason || fallback));
    article.append(text("p", stage.source, "mono"));
    list.append(article);
  });
}

function renderWorkflowNodes(nodes) {
  const list = byId("workflow-nodes");
  clear(list);
  if (!nodes.length) {
    list.append(text("p", "No Argo status nodes are available yet."));
    return;
  }
  nodes.forEach((node) => {
    const article = document.createElement("article");
    const heading = text("div", "", "primary");
    heading.append(document.createTextNode(node.displayName || node.template || node.name), document.createTextNode(" "), badge(node.phase));
    article.append(heading);
    article.append(text("p", `${node.type || "Node"} · ${formatTime(node.startedAt)} to ${formatTime(node.finishedAt)}`));
    if (node.reason || node.message) article.append(text("p", `${node.reason || ""} ${node.message || ""}`.trim(), "mono"));
    list.append(article);
  });
}

function renderRunDetail(detail) {
  byId("detail-title").textContent = detail.name;
  byId("detail-subtitle").textContent = `${detail.profile} · ${detail.nodeAliases.join(", ")} · ${detail.hardwareClass}`;
  renderStages(detail.stages);
  renderWorkflowNodes(detail.workflowNodes);

  const typed = byId("typed-result");
  clear(typed);
  typed.append(
    text("div", detail.outcomeType, "eyebrow"),
    text("div", `execution=${detail.execution || "Pending"}`, "mono"),
    text("div", `verdict=${detail.verdict || "Pending"}`, "mono"),
    text("div", `reason=${detail.reason || "Pending"}`, "mono"),
    text("p", detail.explanation),
  );
  Object.entries(detail.outputs || {}).forEach(([name, value]) => typed.append(text("div", `${name}=${value}`, "mono")));

  const mpi = byId("mpi-status");
  clear(mpi);
  if (!detail.mpiJob) {
    mpi.append(text("p", "The MPIJob has not been created or is no longer retained."));
  } else {
    mpi.append(text("div", `${detail.mpiJob.name} · ${detail.mpiJob.phase || "Pending"}`, "primary"));
    detail.mpiJob.conditions.forEach((condition) => {
      mpi.append(text("p", `${condition.type}=${condition.status} ${condition.reason || ""} ${condition.message || ""}`.trim(), "mono"));
    });
    Object.entries(detail.mpiJob.replicaStatuses || {}).forEach(([name, value]) => {
      mpi.append(text("p", `${name}: active=${value.active} succeeded=${value.succeeded} failed=${value.failed}`, "mono"));
    });
  }

  const participants = byId("participant-list");
  clear(participants);
  detail.participants.forEach((participant) => {
    const article = document.createElement("article");
    article.append(text("div", participant.alias, "primary"));
    article.append(text("p", `state=${participant.state || "unknown"} eligibility=${participant.eligibility || "unknown"} reserved=${participant.reserved}`, "mono"));
    participants.append(article);
  });
  const cleanup = document.createElement("article");
  cleanup.append(text("div", "Cleanup", "primary"));
  cleanup.append(text("p", `reservationsReleased=${detail.cleanup.reservationsReleased} snapshotDeleted=${detail.cleanup.snapshotDeleted}`, "mono"));
  participants.append(cleanup);

  const error = byId("detail-error");
  if (detail.errors?.length) {
    error.textContent = `Partial data: ${detail.errors.map((item) => `${item.resource}: ${item.message}`).join("; ")}`;
    error.classList.remove("hidden");
  } else {
    error.classList.add("hidden");
  }
}

async function openRun(name) {
  const dialog = byId("run-dialog");
  try {
    renderRunDetail(await fetchJSON(`/api/runs/${encodeURIComponent(name)}`));
    dialog.showModal();
  } catch (error) {
    const box = byId("global-error");
    box.textContent = `Could not load run details: ${error.message}`;
    box.classList.remove("hidden");
  }
}

byId("refresh-now").addEventListener("click", refresh);
byId("close-detail").addEventListener("click", () => byId("run-dialog").close());
["phase-filter", "verdict-filter"].forEach((id) => byId(id).addEventListener("change", refresh));
["profile-filter", "hardware-filter"].forEach((id) => byId(id).addEventListener("input", () => {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(refresh, 350);
}));
refresh();

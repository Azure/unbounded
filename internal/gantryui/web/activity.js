// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { bytes, escape as esc } from "./model.js";

const number = value => value == null ? "--" : new Intl.NumberFormat().format(value);
const rate = value => value == null ? "--" : `${bytes(value)}/s`;
const errorRate = value => value == null ? "--" : `${value.toFixed(value < 10 ? 1 : 0)}/s`;

export function clusterActivityHTML(activity) {
  const total = activity?.originRate != null && activity?.peerRate != null ? activity.originRate + activity.peerRate : null;
  const partial = activity && !activity.complete;
  const cards = [
    ["Aggregate download", rate(total), "Origin + peer receive bandwidth", "complete"],
    ["From origin", rate(activity?.originRate), "Registry ingress across agents", "preparing"],
    ["From peers", rate(activity?.peerRate), "Counted at the receivers", "pulling"],
    ["Origin pulls in progress", number(activity?.originActive), "Active origin layer transfers", "preparing"],
    ["Peer pulls in progress", number(activity?.peerActive), "Active peer layer transfers", "complete"],
  ].map(([label, value, note, cls]) => `<article class="stat ${cls}"><span class="stat-symbol"></span><span class="metric-label">${label}</span><strong>${esc(value)}</strong><span>${note}</span></article>`).join("");
  const coverage = !activity?.agents
    ? "Discovering Gantry agents..."
    : `${partial ? "Partial totals · " : ""}${number(activity.reporting)} / ${number(activity.agents)} discovered agents reporting · ${number(activity.rateReporting)} bandwidth samples · ${number(activity.activityReporting)} source gauges`;
  const peerShare = total > 0 ? activity.peerRate / total * 100 : null;
  const originActive = activity?.originActive;
  const peerActive = activity?.peerActive;
  const active = originActive != null && peerActive != null ? number(originActive + peerActive) : "--";
  const panel = `<div class="aggregate-heading"><div><p class="eyebrow">GANTRY CLUSTER ACTIVITY</p><h2>What Gantry is doing right now</h2><p class="small muted">All discovered agents and all images, not just this deployment.</p></div><div class="aggregate-total"><strong>${active}</strong><span>active layer pulls</span></div></div>
    <div class="pull-source-cards"><div class="origin-source"><span>ORIGIN PULLS</span><strong>${number(originActive)} <small>in progress</small></strong><p>${esc(rate(activity?.originRate))} from registries</p></div><div class="peer-source"><span>PEER PULLS</span><strong>${number(peerActive)} <small>in progress</small></strong><p>${esc(rate(activity?.peerRate))} from peer agents</p></div></div>
    <div class="split-heading"><span>Received traffic split</span><span>${peerShare === null ? "No current traffic split" : `${peerShare.toFixed(1)}% from peers`}</span></div>
    <div class="traffic-split ${peerShare === null ? "inactive" : ""}"><span data-progress="${peerShare ?? 0}"></span></div>
    <div class="transfer-health"><div><span>Reported origin failures</span><strong>${errorRate(activity?.originFailures)}</strong></div><div><span>Peer busy responses</span><strong>${errorRate(activity?.peerBusy)}</strong></div><div><span>Peer dial failures</span><strong>${errorRate(activity?.peerDialFailures)}</strong></div></div>
    <p class="small muted">Totals reflect reporting agents. Receiver-side layer bytes only. Peer uploads are not added again. Retries count as network traffic; traffic share is not cache-hit rate or saved bandwidth.</p>
    <div class="cluster-selection-hint"><strong>Cluster activity first. Worker startup on selection.</strong><p>Select a worker to inspect its layers, Pod lifecycle, or startup waterfall. Clear selection to return here.</p></div>`;
  return { cards, coverage, panel, partial };
}

export function drawBandwidth(svg, samples) {
  svg.replaceChildren();
  const add = (name, attributes, text = "") => {
    const element = document.createElementNS("http://www.w3.org/2000/svg", name);
    for (const [key, value] of Object.entries(attributes)) element.setAttribute(key, value);
    element.textContent = text;
    svg.append(element);
  };
  const points = samples.filter(sample => sample.peer != null && sample.origin != null);
  if (!points.length) { add("text", { x: 320, y: 70, "text-anchor": "middle", class: "chart-label" }, "Waiting for two fresh observations per agent"); return; }
  const end = points.at(-1).at;
  const start = end - 60000;
  const shown = points.filter(point => point.at >= start);
  const peak = Math.max(0, ...shown.map(point => point.peer + point.origin));
  if (peak === 0) { add("text", { x: 320, y: 70, "text-anchor": "middle", class: "chart-label" }, "No received layer traffic in the observed window"); return; }
  const x = value => 45 + (value - start) / 60000 * 565;
  const y = value => 110 - value / peak * 90;
  for (let index = 0; index <= 4; index++) {
    const value = peak * index / 4;
    add("line", { x1: 45, x2: 610, y1: y(value), y2: y(value), class: "chart-grid" });
    add("text", { x: 39, y: y(value) + 3, "text-anchor": "end", class: "chart-label" }, bytes(value));
  }
  const origin = shown.map(point => `${x(point.at)},${y(point.origin)}`);
  const total = shown.map(point => `${x(point.at)},${y(point.origin + point.peer)}`);
  add("polygon", { points: `${x(shown[0].at)},110 ${origin.join(" ")} ${x(shown.at(-1).at)},110`, class: "origin-area" });
  add("polygon", { points: `${total.join(" ")} ${[...origin].reverse().join(" ")}`, class: "peer-area" });
  add("polyline", { points: origin.join(" "), class: "origin-line" });
  add("polyline", { points: total.join(" "), class: "peer-line" });
  for (const [value, label, anchor] of [[45, "60s ago", "start"], [327, "30s ago", "middle"], [610, "Now", "end"]]) add("text", { x: value, y: 132, "text-anchor": anchor, class: "chart-label" }, label);
}

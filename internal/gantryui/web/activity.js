// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { bytes, escape as esc } from "./model.js";
import { historySeries, historyWindow } from "./history.js";

const number = value => value == null ? "--" : new Intl.NumberFormat().format(value);
const rate = value => value == null ? "--" : `${bytes(value)}/s`;
const errorRate = value => value == null ? "--" : `${value.toFixed(value < 10 ? 1 : 0)}/s`;

export function clusterActivityHTML(activity, history) {
  const total = activity?.originRate != null && activity?.peerRate != null ? activity.originRate + activity.peerRate : null;
  const partial = activity && !activity.complete;
  const cards = [
    ["Aggregate download", rate(total), "Origin + peer receive bandwidth", "complete"],
    ["From origin", rate(activity?.originRate), "Registry ingress across agents", "preparing"],
    ["From peers", rate(activity?.peerRate), "Counted at the receivers", "pulling"],
    ["Origin pulls in progress", number(activity?.originActive), "Active origin layer transfers", "preparing"],
    ["Peer pulls in progress", number(activity?.peerActive), "Active peer layer transfers", "complete"],
    ["Origin received this run", bytes(history?.originBytes), "Observed cumulative receiver traffic", "preparing"],
    ["Peer received this run", bytes(history?.peerBytes), "Observed cumulative receiver traffic", "pulling"],
  ].map(([label, value, note, cls]) => `<article class="stat ${cls}"><span class="stat-symbol"></span><span class="metric-label">${label}</span><strong>${esc(value)}</strong><span>${note}</span></article>`).join("");
  const coverage = !activity?.agents
    ? "Discovering Gantry agents..."
    : `${partial ? "Partial totals · " : ""}${number(activity.reporting)} / ${number(activity.agents)} discovered agents reporting · ${number(activity.rateReporting)} bandwidth samples · ${number(activity.activityReporting)} source gauges`;
  const peerShare = total > 0 ? activity.peerRate / total * 100 : null;
  const panel = `<div class="compact-activity-heading"><h2>Cluster-wide traffic observed during the selected run</h2></div>
    <div class="split-heading"><span>Received traffic split</span><span>${peerShare === null ? "No current traffic split" : `${peerShare.toFixed(1)}% from peers`}</span></div>
    <div class="traffic-split ${peerShare === null ? "inactive" : ""}"><span data-progress="${peerShare ?? 0}"></span></div>
    <div class="transfer-health"><div><span>Reported origin failures</span><strong>${errorRate(activity?.originFailures)}</strong></div><div><span>Peer busy responses</span><strong>${errorRate(activity?.peerBusy)}</strong></div><div><span>Peer dial failures</span><strong>${errorRate(activity?.peerDialFailures)}</strong></div></div>
    <p class="small muted">All images, not exact selected-image traffic. Totals reflect reporting agents. Receiver layer bytes only; peer uploads excluded. Retries count; traffic share is not cache-hit rate or saved bandwidth. Collection begins ${history?.observedFrom ? esc(new Date(history.observedFrom).toLocaleTimeString()) : "when telemetry becomes available"}; earlier traffic is not reconstructed.</p>`;
  return { cards, coverage, panel, partial };
}

export function drawHistoryChart(svg, history, observedAt, range, readiness = false) {
  svg.replaceChildren();
  const add = (name, attributes, text = "") => {
    const element = document.createElementNS("http://www.w3.org/2000/svg", name);
    for (const [key, value] of Object.entries(attributes)) element.setAttribute(key, value);
    element.textContent = text;
    svg.append(element);
  };
  const window = historyWindow(history, observedAt, range);
  const keys = readiness ? ["ready"] : ["origin", "peer"];
  const series = keys.map(key => historySeries(window, key));
  const peak = series.reduce((max, segments) => segments.reduce((max, segment) =>
    segment.reduce((max, point) => Math.max(max, point.value), max), max),
  readiness ? window.points.reduce((max, point) => Math.max(max, point.total || 0), 1) : 1);
  const x = value => 45 + (value - window.start) / (window.end - window.start) * 565;
  const y = value => 110 - value / peak * 90;
  for (let index = 0; index <= 4; index++) {
    const value = peak * index / 4;
    add("line", { x1: 45, x2: 610, y1: y(value), y2: y(value), class: "chart-grid" });
    add("text", { x: 39, y: y(value) + 3, "text-anchor": "end", class: "chart-label" }, readiness ? String(Math.round(value)) : bytes(value));
  }
  // The hatched baseline remains visible wherever no valid series was observed.
  add("line", { x1: 45, x2: 610, y1: 115, y2: 115, class: "chart-missing" });
  series.forEach((segments, index) => {
    for (const segment of segments) {
      add("line", { x1: x(segment[0].at), x2: x(segment.at(-1).at), y1: 115, y2: 115, class: "chart-observed" });
      const points = segment.flatMap((point, position) => readiness && position
        ? [`${x(point.at)},${y(segment[position - 1].value)}`, `${x(point.at)},${y(point.value)}`]
        : [`${x(point.at)},${y(point.value)}`]).join(" ");
      add("polyline", { points, class: readiness || index ? "peer-line" : "origin-line" });
      if (segment.length === 1) add("circle", { cx: x(segment[0].at), cy: y(segment[0].value), r: 2, class: readiness || index ? "peer-point" : "origin-point" });
    }
  });
  if (!series.some(segments => segments.length)) add("text", { x: 320, y: 65, "text-anchor": "middle", class: "chart-label" }, "No telemetry observed in this range");
  for (const [at, anchor] of [[window.start, "start"], [(window.start + window.end) / 2, "middle"], [window.end, "end"]]) {
    const elapsed = Math.max(0, (at - window.fullStart) / 1000);
    add("text", { x: x(at), y: 132, "text-anchor": anchor, class: "chart-label" }, `+${elapsed < 60 ? elapsed.toFixed(0) + "s" : (elapsed / 60).toFixed(1) + "m"}`);
  }
}

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { test } from "node:test";
import assert from "node:assert/strict";
import { historySeries, historyWindow, mergeHistory, readinessMilestones } from "./history.js";
import { mergeNodeUpdates } from "./model.js";
import { drawHistoryChart } from "./activity.js";

const start = "2026-11-10T12:00:00Z";
const at = seconds => new Date(Date.parse(start) + seconds * 1000).toISOString();
const point = (seconds, values = {}) => ({ at: at(seconds), origin: 10, peer: 20, originBytes: 100, peerBytes: 200, ready: 1, total: 2, ...values });

test("full-run history retains more than a minute and does not invent an initial baseline", () => {
  const history = { start, observedFrom: at(10), points: Array.from({ length: 171 }, (_, i) => point(i + 10)) };
  const window = historyWindow(history, at(185));
  assert.equal(window.start, Date.parse(start));
  assert.equal(window.end, Date.parse(at(185)));
  const series = historySeries(window, "origin");
  assert.equal(series[0].length, 171);
  assert.equal(series[0][0].at, Date.parse(at(10)));
  assert.equal(series[0].at(-1).at, Date.parse(at(180)));
  assert.equal(history.points.length, 171);
});

test("delta merging accepts multiple points, orders them, deduplicates timestamps, and keeps null gaps", () => {
  const previous = { start, observedFrom: at(10), points: [point(10), point(11)], originBytes: 100, peerBytes: 200 };
  const update = { start, observedFrom: at(10), points: [point(13), point(11, { origin: null }), point(12)], originBytes: 400, peerBytes: 500 };
  const merged = mergeHistory(previous, update);
  assert.deepEqual(merged.points.map(p => p.at), [10, 11, 12, 13].map(at));
  assert.equal(merged.points[1].origin, null);
  assert.equal(merged.originBytes, 400);
  assert.equal(merged.peerBytes, 500);
  assert.equal(previous.points[1].origin, 10);
  assert.deepEqual(mergeHistory(merged, update), merged);
  assert.equal(mergeHistory(merged, undefined), merged);
  const snapshot = { nodes: [], history: previous };
  assert.deepEqual(mergeNodeUpdates(snapshot, { nodes: [], history: update, observedAt: at(13) }).history, merged);
});

test("unknown rates and collection pauses break series while measured zero stays visible", () => {
  const history = { start, points: [point(10, { origin: 0 }), point(11), point(12, { origin: null }), point(13), point(20), point(21)] };
  const window = historyWindow(history, at(22));
  const segments = historySeries(window, "origin");
  assert.deepEqual(segments.map(segment => segment.map(p => (p.at - Date.parse(start)) / 1000)), [[10, 11], [13], [20, 21]]);
  assert.equal(segments[0][0].value, 0);
  assert.deepEqual(historySeries(window, "peer").map(segment => segment.length), [4, 2]);
  assert.deepEqual(historySeries(window, "ready").map(segment => segment.length), [4, 2]);
});

test("zoom clamps the shared time domain and reset restores the entire run", () => {
  const history = { start, points: [point(10), point(180)] };
  const zoom = historyWindow(history, at(180), [Date.parse(at(90)), Date.parse(at(150))]);
  assert.equal(zoom.start, Date.parse(at(90)));
  assert.equal(zoom.end, Date.parse(at(150)));
  assert.equal(zoom.fullStart, Date.parse(start));
  assert.equal(historySeries(zoom, "ready").length, 0);
  assert.equal(historyWindow(history, at(180)).start, Date.parse(start));
  assert.equal(historyWindow(history, at(180), [0, Infinity]).end, Date.parse(at(180)));
});

test("missing legacy history, invalid samples, and empty histories stay unavailable", () => {
  assert.equal(historyWindow(undefined, at(180)).available, false);
  assert.deepEqual(historySeries(historyWindow(undefined, at(180)), "ready"), []);
  assert.deepEqual(mergeHistory(null, { points: [{ at: "bad" }, point(1)] }).points, [point(1)]);
  assert.equal(historyWindow({ start, points: [] }, at(180)).available, true);
  assert.deepEqual(readinessMilestones([], start, at(180)).map(m => m.seconds), [null, null, null]);
});

test("readiness milestones use ceiling thresholds and only valid current Ready timestamps", () => {
  const nodes = Array.from({ length: 20 }, (_, i) => ({ phase: "ready", readyAt: at(i + 1), available: null }));
  assert.deepEqual(readinessMilestones(nodes, start, at(30)).map(m => [m.percent, m.target, m.seconds]), [[50, 10, 10], [95, 19, 19], [100, 20, 20]]);
  nodes[19].phase = "preparing";
  assert.equal(readinessMilestones(nodes, start, at(30))[2].seconds, null);
  nodes[18].readyAt = at(-1);
  assert.equal(readinessMilestones(nodes, start, at(30))[1].seconds, null);
  nodes[17].readyAt = "bad";
  nodes[16].readyAt = at(31);
  assert.equal(readinessMilestones(nodes, undefined, at(30))[0].seconds, null);
  assert.equal(readinessMilestones([{ phase: "ready", readyAt: at(2) }], start, at(30))[2].seconds, 2);
});

test("SVG history charts preserve gaps and show measured idle samples, with no DOM dependency", () => {
  const original = globalThis.document;
  const svg = { children: [], replaceChildren() { this.children = []; }, append(element) { this.children.push(element); } };
  globalThis.document = { createElementNS() { return { attrs: {}, setAttribute(key, value) { this.attrs[key] = value; } }; } };
  try {
    drawHistoryChart(svg, { start, points: [point(10), point(11), point(12, { origin: null }), point(13), point(14), point(180, { origin: 0, peer: 0 })] }, at(180), null);
    assert.equal(svg.children.filter(el => el.attrs.class === "origin-line").length, 3);
    assert(svg.children.some(el => el.attrs.class === "chart-missing"));
    assert(svg.children.some(el => el.textContent === "+3.0m"));
    assert(svg.children.filter(el => el.attrs.class === "origin-line")[0].attrs.points.startsWith("76."));
    drawHistoryChart(svg, undefined, at(180), null, true);
    assert(svg.children.some(el => el.textContent === "No telemetry observed in this range"));
    assert(!svg.children.some(el => String(el.attrs.points).includes("NaN")));
  } finally { globalThis.document = original; }
});

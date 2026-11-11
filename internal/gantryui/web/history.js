// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

const instant = value => typeof value === "number" ? value : Date.parse(value);
const known = value => typeof value === "number" && Number.isFinite(value) && value >= 0;

export function mergeHistory(previous, update) {
  if (!update) return previous;
  const points = new Map();
  for (const point of [...(previous?.points || []), ...(update.points || [])]) {
    const at = instant(point.at);
    if (Number.isFinite(at)) points.set(at, point);
  }
  return { ...previous, ...update, points: [...points.values()].sort((a, b) => instant(a.at) - instant(b.at)) };
}

export function historyWindow(history, observedAt, range = null) {
  const points = (history?.points || []).map(point => ({ ...point, at: instant(point.at) }))
    .filter(point => Number.isFinite(point.at)).sort((a, b) => a.at - b.at);
  const suppliedStart = instant(history?.start);
  const observed = instant(history?.completedAt || observedAt);
  const start = Number.isFinite(suppliedStart) ? suppliedStart : points[0]?.at;
  const end = Math.max(start ?? 0, Number.isFinite(observed) ? observed : 0, points.at(-1)?.at ?? 0);
  if (!Number.isFinite(start)) return { start: 0, end: 1, fullStart: 0, fullEnd: 1, points: [], available: false };
  const from = range ? Math.max(start, Math.min(end, range[0])) : start;
  const to = range ? Math.max(from, Math.min(end, range[1])) : end;
  return { start: from, end: Math.max(from + 1, to), fullStart: start, fullEnd: end, points, available: true };
}

// Keep missing observations and collection pauses as gaps, never zero-fill them.
export function historySeries(window, key, maxGap = 2500) {
  const segments = [];
  let segment = [];
  let previous = null;
  for (const point of window.points) {
    const valid = known(point[key]);
    if (!valid || previous !== null && point.at - previous > maxGap) {
      if (segment.length) segments.push(segment);
      segment = [];
    }
    if (valid && point.at >= window.start && point.at <= window.end) segment.push({ at: point.at, value: point[key] });
    previous = point.at;
  }
  if (segment.length) segments.push(segment);
  return segments;
}

export function readinessMilestones(nodes, start, observedAt) {
  const from = instant(start);
  const end = instant(observedAt);
  const ready = nodes.filter(node => node.phase === "ready").map(node => instant(node.readyAt))
    .filter(at => Number.isFinite(at) && Number.isFinite(from) && at >= from && (!Number.isFinite(end) || at <= end))
    .sort((a, b) => a - b);
  return [50, 95, 100].map(percent => {
    const target = Math.ceil(nodes.length * percent / 100);
    const at = target > 0 ? ready[target - 1] ?? null : null;
    return { percent, target, at, seconds: at === null ? null : (at - from) / 1000 };
  });
}

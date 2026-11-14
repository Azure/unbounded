// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { mergeHistory } from "./history.js";

export function percent(available, total) {
  if (available === null || available === undefined || !Number.isFinite(available)) return null;
  return total === 0 ? 100 : Math.min(100, Math.max(0, available / total * 100));
}

export function distributionState(node, image) {
  if (/ErrImagePull|ImagePullBackOff/.test(node.reason)) return "failed";
  const value = percent(node.available, image.size);
  if (value === null) return "unknown";
  if (value === 100) return "complete";
  return value > 0 || !["queued", "scheduling"].includes(node.phase) ? "pulling" : "queued";
}

export function storagePercent(storage) {
  if (!storage || ![storage.total, storage.used, storage.available].every(value => Number.isSafeInteger(value) && value >= 0) ||
      storage.total === 0 || storage.used > storage.total || storage.available > storage.total - storage.used) return null;
  const usable = storage.used + storage.available;
  return usable === 0 ? 100 : percent(storage.used, usable);
}

export function storageState(node) {
  const value = storagePercent(node.storage);
  if (value === null) return "unknown";
  return value >= 95 ? "storage-full" : value >= 85 ? "storage-high" : "storage-ok";
}

export function summary(nodes, image, view) {
  const counts = {};
  for (const node of nodes) {
    const state = view === "distribution" ? distributionState(node, image) : view === "storage" ? storageState(node) : node.phase;
    counts[state] = (counts[state] || 0) + 1;
  }
  return counts;
}

export function sortWorkers(nodes, image, view) {
  const order = view === "distribution"
    ? ["failed", "unknown", "queued", "pulling", "complete"]
    : view === "storage"
      ? ["storage-full", "storage-high", "unknown", "storage-ok"]
      : ["failed", "unknown", "terminating", "scheduling", "queued", "preparing", "running", "completed", "ready"];
  const rank = node => {
    const phase = view === "distribution" ? distributionState(node, image) : view === "storage" ? storageState(node) : node.phase;
    const index = order.indexOf(phase);
    return index < 0 ? 1 : index;
  };
  return [...nodes].sort((a, b) => rank(a) - rank(b) ||
    (view === "storage" ? (storagePercent(b.storage) ?? -1) - (storagePercent(a.storage) ?? -1) : 0) ||
    a.name.localeCompare(b.name, "en", { numeric: true }));
}

export function bytes(value) {
  if (value === null || value === undefined || !Number.isFinite(value)) return "--";
  if (value < 1000) return `${value.toFixed(0)} B`;
  const units = ["kB", "MB", "GB", "TB"];
  let amount = value / 1000;
  let index = 0;
  while (amount >= 1000 && index < units.length - 1) { amount /= 1000; index++; }
  return `${amount.toFixed(amount < 10 ? 2 : 1)} ${units[index]}`;
}

export function escape(value) {
  return String(value ?? "").replace(/[&<>"']/g, character => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[character]);
}

export function phaseLabel(phase) {
  return ({
    complete: "Bytes available", pulling: "Downloading", queued: "Queued", unknown: "Unknown",
    scheduling: "Scheduling", preparing: "Preparing", running: "Running", ready: "Ready",
    completed: "Completed", terminating: "Terminating", failed: "Failed",
    "storage-ok": "Below 85% used", "storage-high": "85-95% used", "storage-full": "95-100% used",
  })[phase] || phase;
}

export function buildRequestError(layers, layerMB, limits) {
  if (!Number.isInteger(layers) || layers < 1 || layers > limits.maxLayers) return `Choose 1 to ${limits.maxLayers} whole payload layers.`;
  if (!Number.isInteger(layerMB) || layerMB < 1 || layerMB > limits.maxTotalMB || layers * layerMB > limits.maxTotalMB) return `Use whole MB values with at most ${bytes(limits.maxTotalMB * 1000000)} of total payload.`;
  return "";
}

export function mergeNodeUpdates(snapshot, update) {
  const changes = new Map(update.nodes.map(node => [node.name, node]));
  const nodes = snapshot.nodes.map(node => {
    const current = changes.get(node.name);
    changes.delete(node.name);
    return current || node;
  });
  if (changes.size) throw new Error("Worker topology changed without a full snapshot");
  return { ...snapshot, nodes, observedAt: update.observedAt, activity: update.activity ?? snapshot.activity,
    history: mergeHistory(snapshot.history, update.history) };
}

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { test } from "node:test";
import assert from "node:assert/strict";
import { buildRequestError, bytes, distributionState, escape, mergeNodeUpdates, percent, sortWorkers, storagePercent, storageState, summary } from "./model.js";

test("grids independently group attention-needed workers first with numeric name ties", () => {
  const nodes = [
    {name:"flex10",phase:"ready",available:100},
    {name:"flex2",phase:"failed",available:100},
    {name:"flex1",phase:"ready",available:null},
    {name:"flex3",phase:"preparing",available:50},
  ];
  assert.deepEqual(sortWorkers(nodes,{size:100},"lifecycle").map(n=>n.name),["flex2","flex3","flex1","flex10"]);
  assert.deepEqual(sortWorkers(nodes,{size:100},"distribution").map(n=>n.name),["flex1","flex3","flex2","flex10"]);
  assert.equal(nodes[0].name,"flex10");
  const storage = used => ({total:100,used,available:100-used});
  assert.deepEqual(sortWorkers([{name:"a",storage:storage(20)},{name:"b",storage:storage(99)},{name:"c",storage:storage(90)}],{},"storage").map(n=>n.name),["b","c","a"]);
});

test("streaming readiness is independent of byte availability", () => {
  const image = { size: 100, streaming: true };
  const node = { phase: "ready", available: 12, telemetryError: "", reason: "Pod Ready" };
  assert.equal(distributionState(node, image), "pulling");
  assert.equal(summary([node], image, "lifecycle").ready, 1);
  assert.equal(percent(node.available, image.size), 12);
});

test("unknown observations never appear as zero or complete", () => {
  assert.equal(percent(null, 100), null);
  assert.equal(distributionState({ available: null, telemetryError: "upgrade required", reason: "" }, { size: 100 }), "unknown");
  assert.equal(bytes(undefined), "--");
});

test("agent failures preserve the last reported distribution state", () => {
  const image = { size: 100 };
  const nodes = [0, 37, 100].map(available => ({
    available, phase: "queued", reason: "", telemetryError: "cannot reach Gantry agent progress endpoint",
  }));
  assert.deepEqual(nodes.map(node => distributionState(node, image)), ["queued", "pulling", "complete"]);
  assert.deepEqual(summary(nodes, image, "distribution"), { queued: 1, pulling: 1, complete: 1 });
  assert.equal(distributionState({ ...nodes[1], available: 12, telemetryError: "" }, image), "pulling");
});

test("storage usage distinguishes empty, reserved, full, and unavailable filesystems", () => {
  const node = { storage: { total: 100, used: 0, available: 90 }, telemetryError: "agent unreachable", phase: "failed" };
  assert.equal(storagePercent(node.storage), 0);
  assert.equal(storageState(node), "storage-ok");
  assert.equal(storagePercent({ total: 100, used: 45, available: 45 }), 50);
  node.storage = { total: 100, used: 90, available: 10 };
  assert.equal(storageState(node), "storage-high");
  node.storage = { total: 100, used: 99, available: 0 };
  assert.equal(storagePercent(node.storage), 100);
  assert.equal(storageState(node), "storage-full");
  assert.equal(storagePercent({ total: 100, used: 0, available: 0 }), 100);
  assert.equal(summary([node], { size: 1 }, "storage")["storage-full"], 1);
  for (const storage of [null, {}, { total: 0, used: 0, available: 0 }, { total: 100, used: 80, available: 30 }, { total: 100, used: -1, available: 50 }, { total: Infinity, used: 1, available: 2 }]) {
    assert.equal(storagePercent(storage), null);
    assert.equal(storageState({ storage }), "unknown");
  }
});

test("application failures do not erase successfully distributed bytes", () => {
  const node = { available: 100, telemetryError: "", phase: "failed", reason: "CrashLoopBackOff" };
  assert.equal(distributionState(node, { size: 100 }), "complete");
  node.reason = "ImagePullBackOff";
  assert.equal(distributionState(node, { size: 100 }), "failed");
});

test("registry and event text cannot become HTML", () => {
  assert.equal(escape('<img src=x onerror="bad">'), "&lt;img src=x onerror=&quot;bad&quot;&gt;");
});

test("generated image sizes are bounded whole-MB payloads", () => {
  const limits = { maxLayers: 100, maxTotalMB: 100000 };
  for (const [layers, mb] of [[1, 100000], [100, 1000], [100, 1], [20, 101], [3, 50]]) assert.equal(buildRequestError(layers, mb, limits), "");
  for (const [layers, mb] of [[0, 10], [101, 1], [1.5, 10], [1, 0], [1, 0.5], [100, 1001], [1, 100001], [1, Infinity]]) assert.notEqual(buildRequestError(layers, mb, limits), "");
  assert.match(buildRequestError(100, 1001, limits), /100.0 GB/);
});

test("incremental observations update only named workers and preserve order", () => {
  const unchanged = { name: "worker-a", available: 2 };
  const snapshot = { nodes: [unchanged, { name: "worker-b", available: 0 }], observedAt: "old" };
  const fresh = { name: "worker-b", available: 53 };
  const merged = mergeNodeUpdates(snapshot, { nodes: [fresh], observedAt: "new" });
  assert.deepEqual(merged.nodes, [unchanged, fresh]);
  assert.equal(merged.nodes[0], unchanged);
  assert.equal(snapshot.nodes[1].available, 0);
  assert.equal(merged.observedAt, "new");
  assert.throws(() => mergeNodeUpdates(snapshot, { nodes: [{ name: "foreign" }] }), /topology/);
});

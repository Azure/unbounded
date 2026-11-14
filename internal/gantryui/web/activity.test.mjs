// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { test } from "node:test";
import assert from "node:assert/strict";
import { clusterActivityHTML } from "./activity.js";

test("cluster cards sum receiver bandwidth and retain explicit zero activity", () => {
  const activity = { agents: 2, reporting: 2, rateReporting: 2, activityReporting: 2, complete: true, originRate: 1000000000, peerRate: 3000000000, originActive: 0, peerActive: 4, originFailures: 0, peerBusy: 0, peerDialFailures: 0 };
  const result = clusterActivityHTML(activity);
  assert.match(result.cards, /4.00 GB\/s/);
  assert.match(result.cards, /Origin pulls in progress<\/span><strong>0<\/strong>/);
  assert.match(result.panel, /75.0% from peers/);
  assert.equal(result.partial, false);
  assert.doesNotMatch(result.panel, /Partial aggregate:/);
});

test("missing and partial telemetry cannot appear as complete zero totals", () => {
  const missing = clusterActivityHTML(undefined);
  assert.match(missing.cards, /<strong>--<\/strong>/);
  const partial = clusterActivityHTML({ agents: 1001, reporting: 1000, rateReporting: 999, activityReporting: 0, complete: false, originRate: 10, peerRate: 0, originActive: null, peerActive: null });
  assert.match(partial.coverage, /Partial totals/);
  assert.match(partial.panel, /Totals reflect reporting agents/);
  assert.match(partial.cards, /Origin pulls in progress<\/span><strong>--<\/strong>/);
  assert.match(partial.coverage, /1000|1,000/);
  assert.doesNotMatch(partial.panel, /warning-notice|Partial aggregate:/);
});

test("an idle but observed cluster has no fabricated traffic split", () => {
  const result = clusterActivityHTML({ agents: 1, reporting: 1, rateReporting: 1, activityReporting: 1, complete: true, originRate: 0, peerRate: 0, originActive: 0, peerActive: 0 });
  assert.match(result.panel, /No current traffic split/);
  assert.match(result.panel, /traffic-split inactive/);
  assert.doesNotMatch(result.panel, /NaN|Infinity|100.0% from peers/);
});

test("run counters are cumulative received bytes, separate from live rates and unknown history", () => {
  const activity = { originRate: 0, peerRate: 0, complete: true };
  const result = clusterActivityHTML(activity, { originBytes: 3000000000, peerBytes: 12000000000, observedFrom: "2026-11-10T12:00:00Z" });
  assert.match(result.cards, /Origin received this run<\/span><strong>3.00 GB/);
  assert.match(result.cards, /Peer received this run<\/span><strong>12.0 GB/);
  assert.match(result.panel, /Cluster-wide traffic observed during the selected run/);
  assert.match(result.panel, /not exact selected-image traffic/);
  assert.match(clusterActivityHTML(activity).cards, /Origin received this run<\/span><strong>--/);
});

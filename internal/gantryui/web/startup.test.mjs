// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { test } from "node:test";
import assert from "node:assert/strict";
import { runtimePullSeconds, startupTimeline } from "./startup.js";

const at = seconds => new Date(Date.UTC(2026, 10, 10) + seconds * 1000).toISOString();
const node = { podUID: "pod-a", created: at(0), scheduled: at(2), started: at(15), readyAt: at(20), restarts: 0 };
const event = (reason, seconds, message = "", count = 1) => ({ podUID: "pod-a", reason, firstAt: at(seconds), at: at(seconds), message, count });

test("startup durations come from observed milestones, independent of image bytes", () => {
  const events = [event("Pulling", 4), event("Pulled", 14, 'Successfully pulled image "image" in 10s (10s including waiting).')];
  const timeline = startupTimeline({ ...node, available: 12 }, events, at(21));
  assert.equal(timeline.total, 20);
  assert.deepEqual(timeline.stages.map(stage => stage.seconds), [2, 2, 10, 1, 5]);
  assert.equal(timeline.attempts, 1);
  assert.equal(timeline.runtime, 10);
  assert.deepEqual(timeline, startupTimeline({ ...node, available: 100 }, events, at(21)));
});

test("a retained runtime duration remains useful without inventing a queue timestamp", () => {
  const timeline = startupTimeline(node, [event("Pulled", 14, "Successfully pulled image in 3.2s (4s including waiting).")], at(21));
  assert.equal(timeline.stages[1].seconds, null);
  assert.equal(timeline.stages[2].seconds, 3.2);
  assert.equal(timeline.stages[2].source, "runtime reported");
  assert.equal(timeline.stages[2].inferredPosition, true);
  assert.equal(timeline.attempts, null);
});

test("aggregated retries do not become fabricated per-attempt timings", () => {
  const timeline = startupTimeline(node, [event("Pulling", 4, "", 3), event("Pulled", 14, "Successfully pulled image in 3s.")], at(21));
  assert.equal(timeline.attempts, 3);
  assert.equal(timeline.stages[2].name, "Pull + retries");
  assert.equal(timeline.stages[2].seconds, 10);
  assert.match(timeline.warnings.join(" "), /do not preserve/);
});

test("foreign Pod events, missing data, and invalid chronology stay unavailable", () => {
  const timeline = startupTimeline(node, [{ ...event("Pulled", 14), podUID: "foreign" }], at(21));
  assert.equal(timeline.stages[2].seconds, null);
  assert.equal(timeline.stages[3].seconds, null);
  const invalid = startupTimeline({ ...node, scheduled: at(-3), readyAt: at(-1) }, [], at(21));
  assert.equal(invalid.total, null);
  assert.equal(invalid.stages[0].seconds, null);
});

test("in-progress stages are bounded by the observation, and cache hits are explicit", () => {
  const ongoing = startupTimeline({ ...node, started: null, readyAt: null }, [event("Pulling", 4)], at(10));
  assert.equal(ongoing.total, null);
  assert.equal(ongoing.stages[2].seconds, 6);
  assert.equal(ongoing.stages[2].ongoing, true);
  const cached = startupTimeline(node, [event("Pulled", 14, 'Container image "image" already present on machine')], at(21));
  assert.equal(cached.cached, true);
  assert.equal(cached.stages[2].seconds, null);
});

test("runtime duration parser accepts kubelet formats and rejects unsupported values", () => {
  assert.equal(runtimePullSeconds("Successfully pulled image in 3m26.399s (3m26.399s including waiting)."), 206.399);
  assert.equal(runtimePullSeconds("Successfully pulled image in 400ms (1s including waiting)."), .4);
  assert.equal(runtimePullSeconds("Successfully pulled image in 0s (1s including waiting)."), 0);
  assert.equal(runtimePullSeconds("Successfully pulled image in nonsense."), null);
});

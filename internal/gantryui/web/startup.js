// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

const instant = value => {
  const parsed = value ? Date.parse(value) : NaN;
  return Number.isFinite(parsed) ? parsed : null;
};

export function runtimePullSeconds(message) {
  const raw = message.match(/\bin ([0-9][^\s]*)/)?.[1];
  if (!raw) return null;
  const units = { h: 3600, m: 60, s: 1, ms: .001, us: .000001, "\u00b5s": .000001, "\u03bcs": .000001, ns: .000000001 };
  const tokens = [...raw.matchAll(/([0-9]+(?:\.[0-9]+)?)(ns|us|\u00b5s|\u03bcs|ms|s|m|h)/g)];
  if (tokens.map(token => token[0]).join("") !== raw) return null;
  const seconds = tokens.reduce((sum, token) => sum + Number(token[1]) * units[token[2]], 0);
  return Number.isFinite(seconds) ? seconds : null;
}

export function startupTimeline(node, events, observedAt) {
  const created = instant(node.created);
  const scheduled = instant(node.scheduled);
  const started = instant(node.started);
  const ready = instant(node.readyAt);
  const observed = instant(observedAt);
  const owned = node.podUID ? events.filter(event => event.podUID === node.podUID) : [];
  const pulling = owned.filter(event => event.reason === "Pulling");
  const pullTimes = pulling.map(event => instant(event.firstAt)).filter(at => at !== null && created !== null && at >= created);
  const firstPull = pullTimes.length ? Math.min(...pullTimes) : null;
  const successes = owned.filter(event => event.reason === "Pulled").map(event => ({ event, at: instant(event.firstAt) }))
    .filter(({ at }) => at !== null && created !== null && at >= created && (started === null || at <= started))
    .sort((a, b) => a.at - b.at);
  const success = successes[0] || null;
  const pulled = success?.at ?? null;
  const runtime = success ? runtimePullSeconds(success.event.message) : null;
  const warnings = [];
  const maxEnd = ready ?? observed;
  const span = (name, start, end, source, active = false) => {
    let ongoing = false;
    if (end === null && active && observed !== null) { end = observed; ongoing = true; }
    const valid = created !== null && start !== null && end !== null && start >= created && end >= start && (maxEnd === null || end <= maxEnd);
    return { name, seconds: valid ? (end - start) / 1000 : null, offset: valid ? (start - created) / 1000 : null, source: valid ? source : "unavailable", ongoing, inferredPosition: false };
  };
  const preparation = span(pulling.some(event => event.count > 1) || pulling.length > 1 ? "Pull + retries" : "Image preparation",
    firstPull, pulled, "event timestamps", firstPull !== null && pulled === null && started === null);
  if (preparation.seconds === null && runtime !== null && pulled !== null && created !== null && runtime <= (pulled - created) / 1000) {
    Object.assign(preparation, { seconds: runtime, offset: (pulled - created) / 1000 - runtime, source: "runtime reported", ongoing: false, inferredPosition: true });
    warnings.push("The runtime reports preparation duration; its start position is anchored to the Pulled event, not an observed Pulling timestamp.");
  }
  const stages = [
    span("Scheduling", created, scheduled, "Pod timestamps", scheduled === null && started === null),
    span("Waiting to pull", scheduled, firstPull, "event timestamps", scheduled !== null && firstPull === null && pulled === null && started === null),
    preparation,
    span("Container start", pulled, started, "event + Pod timestamps", pulled !== null && started === null),
    span("Pod readiness", started, ready, "Pod timestamps", started !== null && ready === null),
  ];
  const total = created !== null && ready !== null && ready >= created ? (ready - created) / 1000 : null;
  const elapsed = created !== null && maxEnd !== null && maxEnd >= created ? (maxEnd - created) / 1000 : null;
  const attempts = pulling.length ? pulling.reduce((sum, event) => sum + Math.max(1, event.count || 1), 0) : null;
  if (attempts > 1) warnings.push("Pulling events report multiple attempts. Aggregated events do not preserve every attempt's duration or exact retry backoff.");
  if (node.restarts > 0) warnings.push("This container has restarted. These are current reported milestones, not a reconstruction of its first startup.");
  if (stages.some(stage => stage.seconds === null)) warnings.push("Some stage timestamps are unavailable. Gaps are not inferred from byte progress or treated as zero.");
  return { stages, total, elapsed, runtime, attempts, cached: success ? /already present on machine/i.test(success.event.message) : false, warnings };
}

export function duration(seconds) {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) return "--";
  if (seconds < 1) return `${Math.round(seconds * 1000)} ms`;
  return `${seconds.toFixed(1)} s`;
}

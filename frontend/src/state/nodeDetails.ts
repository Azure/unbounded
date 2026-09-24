// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import type { NodeDetailResult, NodeDetailSnapshot, NodeSummary } from '../types';

export type DetailView = {
  state: 'not-loaded' | 'loading' | 'loaded' | 'expired' | 'error';
  snapshot?: NodeDetailSnapshot;
  error?: string;
  deadline?: string;
};
type Timer = ReturnType<typeof setTimeout>;
type Operation = { abort: AbortController; deadline: number; requestId?: string; timer?: Timer };
type Transport = {
  request: (name: string, forceRefresh: boolean, signal: AbortSignal) => Promise<NodeDetailResult>;
  poll: (name: string, requestId: string, signal: AbortSignal) => Promise<NodeDetailResult>;
};
type Clock = {
  now: () => number;
  setTimeout: (callback: () => void, delay: number) => Timer;
  clearTimeout: (timer: Timer) => void;
};
const browserClock: Clock = {
  now: Date.now,
  // Browser timers require their global receiver, not the injected clock object.
  setTimeout: (callback, delay) => globalThis.setTimeout(callback, delay),
  clearTimeout: (timer) => globalThis.clearTimeout(timer),
};
const notLoaded: DetailView = { state: 'not-loaded' };

// Only this store owns retained snapshots. React subscribes to a version, not a
// second cache. Timers and async operations capture names/IDs, never old data.
export class NodeDetails {
  private views = new Map<string, DetailView>();
  private operations = new Map<string, Operation>();
  private expiryTimers = new Map<string, Timer>();
  private identities = new Map<string, string>();
  private transport: Transport;
  private changed: () => void;
  private clock: Clock;

  constructor(transport: Transport, changed: () => void, clock: Clock = browserClock) {
    this.transport = transport;
    this.changed = changed;
    this.clock = clock;
  }

  read(name: string): DetailView {
    this.expire(name);
    return this.views.get(name) || notLoaded;
  }

  private expire(name: string) {
    const view = this.views.get(name);
    if (view?.snapshot && !(Date.parse(view.snapshot.expiresAt) > this.clock.now())) {
      this.clearExpiry(name);
      this.views.set(name, {
        state: view.state === 'loading' ? 'loading' : view.error ? 'error' : 'expired',
        error: view.error, deadline: view.deadline,
      });
    }
  }

  private clearExpiry(name: string) {
    const timer = this.expiryTimers.get(name);
    if (timer !== undefined) this.clock.clearTimeout(timer);
    this.expiryTimers.delete(name);
  }

  private scheduleExpiry(name: string) {
    this.clearExpiry(name);
    const snapshot = this.views.get(name)?.snapshot;
    if (!snapshot) return;
    const delay = Date.parse(snapshot.expiresAt) - this.clock.now();
    this.expiryTimers.set(name, this.clock.setTimeout(() => {
      this.expire(name);
      if (this.views.get(name)?.snapshot) this.scheduleExpiry(name);
      this.changed();
    }, Math.min(Math.max(0, delay), 2147483647)));
  }

  private publish(name: string, view: DetailView) {
    this.views.set(name, view);
    this.changed();
  }

  cancel(name: string) {
    const op = this.operations.get(name);
    if (!op) return;
    this.operations.delete(name);
    op.abort.abort();
    if (op.timer !== undefined) this.clock.clearTimeout(op.timer);
    const view = this.read(name);
    this.publish(name, { state: view.snapshot ? 'loaded' : 'not-loaded', snapshot: view.snapshot });
  }

  syncNodes(nodes: NodeSummary[]) {
    const next = new Map<string, string>();
    for (const node of nodes) {
      if (!node.name) continue;
      const info = node.nodeInfo || {};
      next.set(node.name, JSON.stringify([
        info.providerId || '',
        [...(info.internalIPs || [])].sort(),
        info.wireGuard?.publicKey || '',
      ]));
    }
    for (const [name, identity] of this.identities) {
      if (!next.has(name) || next.get(name) !== identity) this.invalidate(name);
    }
    this.identities = next;
  }

  private invalidate(name: string) {
    const op = this.operations.get(name);
    if (op) {
      this.operations.delete(name);
      op.abort.abort();
      if (op.timer !== undefined) this.clock.clearTimeout(op.timer);
    }
    this.clearExpiry(name);
    const changed = this.views.delete(name);
    if (op || changed) this.changed();
  }

  dispose() {
    for (const op of this.operations.values()) {
      op.abort.abort();
      if (op.timer !== undefined) this.clock.clearTimeout(op.timer);
    }
    for (const timer of this.expiryTimers.values()) this.clock.clearTimeout(timer);
    this.operations.clear();
    this.expiryTimers.clear();
    this.views.clear();
    this.identities.clear();
  }

  load(name: string, forceRefresh = false) {
    if (!name) return;
    if (!forceRefresh && this.operations.has(name)) return;
    const view = this.read(name);
    if (!forceRefresh && view.snapshot) {
      this.publish(name, { state: 'loaded', snapshot: view.snapshot });
      return;
    }
    this.cancel(name);
    const op: Operation = { abort: new AbortController(), deadline: Infinity };
    this.operations.set(name, op);
    this.publish(name, { state: 'loading', snapshot: this.read(name).snapshot });
    // Bound an unresponsive initial POST too; once pending arrives, only the
    // controller's fixed deadline governs polling.
    op.timer = this.clock.setTimeout(() => this.fail(name, op, 'Detail request timed out'), 120000);
    void this.transport.request(name, forceRefresh, op.abort.signal)
      .then((result) => this.accept(name, op, result))
      .catch((error) => this.fail(name, op, String(error.message || error)));
  }

  private current(name: string, op: Operation) {
    return this.operations.get(name) === op && !op.abort.signal.aborted;
  }

  private finish(name: string, op: Operation) {
    if (op.timer !== undefined) this.clock.clearTimeout(op.timer);
    this.operations.delete(name);
    op.abort.abort();
  }

  private fail(name: string, op: Operation, error: string, state: 'error' | 'expired' = 'error') {
    if (!this.current(name, op)) return;
    this.finish(name, op);
    this.publish(name, { state, error, snapshot: this.read(name).snapshot });
  }

  private armDeadline(name: string, op: Operation) {
    op.timer = this.clock.setTimeout(() => {
      if (!this.current(name, op)) return;
      if (this.clock.now() >= op.deadline) {
        this.fail(name, op, 'Detail request deadline expired', 'expired');
      } else {
        this.armDeadline(name, op);
      }
    }, Math.min(Math.max(0, op.deadline - this.clock.now()), 2147483647));
  }

  private accept(name: string, op: Operation, result: NodeDetailResult) {
    if (!this.current(name, op)) return;
    if (this.clock.now() >= op.deadline) {
      this.fail(name, op, 'Detail request deadline expired', 'expired');
      return;
    }
    if (result.nodeName !== name || (op.requestId && result.requestId !== op.requestId)) {
      this.fail(name, op, 'Mismatched node or detail request ID');
      return;
    }
    if (result.state === 'pending') {
      const deadline = Date.parse(result.deadline || '');
      if (!result.requestId || !Number.isFinite(deadline)) {
        this.fail(name, op, 'Pending detail response is missing a request ID or deadline');
        return;
      }
      op.requestId = result.requestId;
      op.deadline = Math.min(op.deadline, deadline);
      if (op.timer !== undefined) this.clock.clearTimeout(op.timer);
      const remaining = op.deadline - this.clock.now();
      if (remaining <= 0) {
        this.fail(name, op, 'Detail request deadline expired', 'expired');
        return;
      }
      this.publish(name, {
        ...this.read(name), state: 'loading', error: result.error,
        deadline: new Date(op.deadline).toISOString(),
      });
      op.timer = this.clock.setTimeout(() => {
        if (!this.current(name, op)) return;
        if (this.clock.now() >= op.deadline) {
          this.fail(name, op, 'Detail request deadline expired', 'expired');
          return;
        }
        this.armDeadline(name, op);
        void this.transport.poll(name, op.requestId!, op.abort.signal)
          .then((next) => this.accept(name, op, next))
          .catch((error) => this.fail(name, op, String(error.message || error)));
      }, Math.min(1000, remaining));
      return;
    }
    if (result.state !== 'complete') {
      if (result.state === 'expired') {
        this.finish(name, op);
        this.clearExpiry(name);
        this.publish(name, { state: 'expired', error: result.error || 'Detail request expired' });
        return;
      }
      this.fail(name, op, result.error || `Detail request ${result.state}`);
      return;
    }
    const snapshot = result.details;
    if (!snapshot || !result.requestId || snapshot.nodeName !== name ||
        snapshot.status?.nodeInfo?.name !== name || snapshot.requestId !== result.requestId) {
      this.fail(name, op, 'Invalid detail snapshot identity');
      return;
    }
    const expiry = Date.parse(snapshot.expiresAt);
    if (!(expiry > this.clock.now())) {
      this.fail(name, op, 'Detail snapshot expired', 'expired');
      return;
    }
    this.finish(name, op);
    this.publish(name, { state: 'loaded', snapshot });
    this.scheduleExpiry(name);
  }
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

type DiagEntry = {
  lastLogAt: number;
  suppressed: number;
};

const uiDiagEntries = new Map<string, DiagEntry>();

function resolveUiDiagEnabled() {
  if (typeof window === 'undefined') return false;
  try {
    const params = new URLSearchParams(window.location.search);
    const query = params.get('uiDiag');
    if (query === '1' || query === 'true') return true;
    if (query === '0' || query === 'false') return false;

    const stored = window.localStorage.getItem('uiDiag');
    if (!stored) return false;
    return stored === '1' || stored.toLowerCase() === 'true';
  } catch {
    return false;
  }
}

const UI_DIAG_ENABLED = resolveUiDiagEnabled();

function uiDiag(
  key: string,
  message: string,
  data?: Record<string, unknown>,
  options?: { minIntervalMs?: number; level?: 'log' | 'warn' }
) {
  if (!UI_DIAG_ENABLED) return;
  if (typeof window === 'undefined') return;
  const minIntervalMs = options?.minIntervalMs ?? 300;
  const level = options?.level ?? 'log';
  const now = performance.now();
  const entry = uiDiagEntries.get(key) || { lastLogAt: 0, suppressed: 0 };

  if (now - entry.lastLogAt < minIntervalMs) {
    entry.suppressed += 1;
    uiDiagEntries.set(key, entry);
    return;
  }

  const payload: Record<string, unknown> = {
    ...(data || {}),
    suppressed: entry.suppressed,
    at: new Date().toISOString()
  };

  if (level === 'warn') {
    console.warn(`[UI-DIAG] ${message}`, payload);
  } else {
    console.log(`[UI-DIAG] ${message}`, payload);
  }

  entry.lastLogAt = now;
  entry.suppressed = 0;
  uiDiagEntries.set(key, entry);
}

export { uiDiag };

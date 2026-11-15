// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { clampPageIndex } from '../src/components/nodes/shared/pagination.ts';

const pageCountFor = (rowCount: number, pageSize: number) => Math.max(1, Math.ceil(rowCount / pageSize));

test('clamping the peer page index after an Auto page size change keeps rows visible', () => {
  const rows = Array.from({ length: 100 }, (_, index) => `peer-${index}`);
  const manualPageSize = 10;
  const autoPageSize = 25;
  const lastManualPageIndex = 4; // Page 5 of 10 at 10 rows per page.

  const autoPageCount = pageCountFor(rows.length, autoPageSize);
  assert.equal(autoPageCount, 4);

  const unclampedRows = rows.slice(
    lastManualPageIndex * autoPageSize,
    (lastManualPageIndex + 1) * autoPageSize
  );
  assert.equal(unclampedRows.length, 0, 'page 5 is empty once Auto fits 25 rows on 4 pages');

  const clamped = clampPageIndex(lastManualPageIndex, autoPageCount);
  assert.equal(clamped, autoPageCount - 1);
  const visibleRows = rows.slice(clamped * autoPageSize, (clamped + 1) * autoPageSize);
  assert.equal(visibleRows.length, 25);
  assert.equal(visibleRows[0], 'peer-75');

  const rangeStart = clamped * autoPageSize + 1;
  const rangeEnd = Math.min(rows.length, (clamped + 1) * autoPageSize);
  assert.equal(`${rangeStart}-${rangeEnd} of ${rows.length}`, '76-100 of 100');
});

test('clamping leaves in-range page indexes alone and clamps empty tables to the first page', () => {
  assert.equal(clampPageIndex(2, 4), 2);
  assert.equal(clampPageIndex(0, 1), 0);
  assert.equal(clampPageIndex(4, 1), 0);
  assert.equal(clampPageIndex(0, 0), 0);
  assert.equal(clampPageIndex(-1, 4), 0);
});

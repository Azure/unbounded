// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

export function clampPageIndex(pageIndex: number, pageCount: number): number {
  if (pageCount <= 0) return 0;
  return Math.min(Math.max(pageIndex, 0), pageCount - 1);
}

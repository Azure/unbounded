// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import type { NodeStatus, NodeSummary } from '../types';

export function nodeForDetailView(name: string, summary?: NodeSummary, details?: NodeStatus): NodeStatus {
  const info = { ...details?.nodeInfo, ...summary?.nodeInfo };
  return {
    ...details,
    nodeInfo: {
      ...info,
      name: summary?.name ?? info.name ?? name,
      siteName: summary?.siteName ?? info.siteName,
      isGateway: summary?.isGateway ?? info.isGateway,
      k8sReady: summary?.k8sReady ?? info.k8sReady,
    },
    lastPushTime: summary?.lastPushTime ?? details?.lastPushTime,
    statusSource: summary?.statusSource ?? details?.statusSource,
    fetchError: summary ? summary.fetchError : details?.fetchError,
  };
}

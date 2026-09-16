// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useRef, useState } from 'react';
import { pollNodeDetails, requestNodeDetails } from '../api';
import { NodeDetails } from '../state/nodeDetails';

export default function useNodeDetails(selectedNodeName: string | null) {
  const [, setVersion] = useState(0);
  const storeRef = useRef<NodeDetails | null>(null);
  if (!storeRef.current) {
    storeRef.current = new NodeDetails(
      { request: requestNodeDetails, poll: pollNodeDetails },
      () => setVersion((version) => version + 1)
    );
  }
  const store = storeRef.current;
  useEffect(() => () => store.dispose(), [store]);
  // Selection only cancels obsolete waiters. It never initiates collection.
  useEffect(() => () => {
    if (selectedNodeName) store.cancel(selectedNodeName);
  }, [store, selectedNodeName]);
  const load = useCallback((forceRefresh = false) => {
    if (selectedNodeName) store.load(selectedNodeName, forceRefresh);
  }, [store, selectedNodeName]);
  return {
    detail: selectedNodeName ? store.read(selectedNodeName) : { state: 'not-loaded' as const },
    load,
  };
}

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import * as React from 'react';

type ReagraphModule = {
  GraphCanvas: React.ComponentType<any>;
  darkTheme: Record<string, any>;
  lightTheme: Record<string, any>;
};

export type { ReagraphModule };

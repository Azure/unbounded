// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

import React from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import './styles.css';

const rootEl = document.getElementById('root');
if (rootEl) {
  createRoot(rootEl).render(
    <React.StrictMode>
      <App />
    </React.StrictMode>
  );
}

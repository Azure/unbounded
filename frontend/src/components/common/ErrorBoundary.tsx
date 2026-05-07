// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Component, type ReactNode } from 'react';

class ErrorBoundary extends Component<
  { fallback?: ReactNode; children: ReactNode },
  { hasError: boolean }
> {
  state = { hasError: false };

  static getDerivedStateFromError() {
    return { hasError: true };
  }

  componentDidCatch(error: Error) {
    console.error('ErrorBoundary caught an error', error);
  }

  render() {
    if (this.state.hasError) {
      return this.props.fallback ?? (
        <div className="card error-panel">
          <div className="section-title">Render Error</div>
          <div>Something went wrong while rendering the dashboard.</div>
        </div>
      );
    }
    return this.props.children;
  }
}

export default ErrorBoundary;

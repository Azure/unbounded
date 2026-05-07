// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';
import { useEffect } from 'react';

function FunnelIcon({ filled = false }: { filled?: boolean }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className="filter-icon">
      <path
        d="M3 5h18l-7 8v5l-4 2v-7L3 5Z"
        fill={filled ? 'currentColor' : 'none'}
        stroke={filled ? 'none' : 'currentColor'}
        strokeWidth={filled ? 0 : 1.8}
        strokeLinecap={filled ? undefined : 'round'}
        strokeLinejoin={filled ? undefined : 'round'}
      />
    </svg>
  );
}

function MagnifyPlusIcon() {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className="action-icon">
      <circle cx="9.5" cy="9.5" r="7.5" fill="none" stroke="currentColor" strokeWidth="3" />
      <path d="M9.5 5.8v7.4M5.8 9.5h7.4" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
      <path d="m15.2 15.2 7.2 7.2" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
    </svg>
  );
}

function CloseXIcon() {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className="action-icon">
      <path d="m2.5 2.5 19 19M21.5 2.5l-19 19" stroke="currentColor" strokeWidth="3.2" strokeLinecap="round" />
    </svg>
  );
}

function useDismissOnOutside(
  open: boolean,
  refs: Array<React.RefObject<HTMLElement | null>>,
  onDismiss: () => void
) {
  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target as Node | null;
      if (!target) return;
      const clickedInside = refs.some((ref) => ref.current?.contains(target));
      if (!clickedInside) {
        onDismiss();
      }
    };
    window.addEventListener('mousedown', onPointerDown);
    return () => window.removeEventListener('mousedown', onPointerDown);
  }, [open, refs, onDismiss]);
}

const TableFilterButton = React.forwardRef<HTMLButtonElement, {
  active: boolean;
  open: boolean;
  onToggle: () => void;
  title?: string;
}>(function TableFilterButton({ active, open, onToggle, title }, ref) {
  return (
    <button
      ref={ref}
      className={`button filter-button ${active ? 'active' : ''}`}
      onClick={onToggle}
      aria-label={title || 'Filters'}
      aria-expanded={open}
      title={title || 'Filters'}
      type="button"
    >
      <FunnelIcon filled={active} />
      <span className="filter-button-label">Filter</span>
    </button>
  );
});

export { CloseXIcon, MagnifyPlusIcon, TableFilterButton, useDismissOnOutside };

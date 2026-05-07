// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';

declare module '@tanstack/react-table' {
  export type ColumnDef<TData = unknown, TValue = unknown> = any;
  export type SortingState = Array<{ id: string; desc: boolean }>;
  export type Renderable<TProps> = React.ReactNode | React.ComponentType<TProps>;
  export function getCoreRowModel<TData = unknown>(): any;
  export function getFilteredRowModel<TData = unknown>(): any;
  export function getPaginationRowModel<TData = unknown>(): any;
  export function getSortedRowModel<TData = unknown>(): any;
  export function flexRender<TProps extends object>(comp: Renderable<TProps>, props: TProps): React.ReactNode;
  export function useReactTable<TData = unknown>(options: any): any;
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  ColumnDef,
  flexRender,
  getCoreRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  SortingState,
  useReactTable
} from '@tanstack/react-table';
import { NodeStatus } from '../../types';
import {
  CloseXIcon,
  TableFilterButton,
  firstUsableIpFromCidr,
  formatBytes,
  formatDateAndAge,
  formatTime,
  getNodeCniProblemMessages,
  getCniStatus,
  getCniStatusTooltip,
  getNodeStatus,
  getNodeTypeFromProviderId,
  getPeerHealthCheckState,
  hostCandidatesFromAllowedIPs,
  hostIpFromRouteDestination,
  isHostRoute,
  isImdsRoute,
  mixedDestinationSortKey,
  normalizeIpAddress,
  parseAzureProviderId,
  parseEndpointHost,
  uiDiag,
  useDismissOnOutside,
  withCommaBreaks
} from './shared/index';
import NodeInfoPanel from './detail/NodeInfoPanel';
import NodeDetailTabsHeader from './detail/NodeDetailTabsHeader';

function NodeDetailModal({
  nodeName,
  node,
  allNodeNames,
  gatewayByNode,
  nodeK8sStatusMap,
  azureTenantId,
  pullEnabled,
  theme,
  detailTab,
  onDetailTabChange,
  onSelectNode,
  onClose
}: {
  nodeName: string | null;
  node: NodeStatus | null;
  allNodeNames: string[];
  gatewayByNode: Map<string, string>;
  nodeK8sStatusMap?: Map<string, string>;
  azureTenantId?: string;
  pullEnabled?: boolean;
  theme: 'dark' | 'light';
  detailTab: 'peerings' | 'routes' | 'bpf';
  onDetailTabChange: (tab: 'peerings' | 'routes' | 'bpf') => void;
  onSelectNode: (nodeName: string) => void;
  onClose: () => void;
}) {
  useEffect(() => {
    if (!nodeName) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        onClose();
      }
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [nodeName, onClose]);

  // Track loading state -- but don't return early here; hooks must run first.
  const isLoadingDetail = Boolean(nodeName && !node);

  const status = node ? getNodeStatus(node) : 'warning';
  const peers = node?.peers || [];
  const nodeDisplayName = node?.nodeInfo?.name || nodeName || 'Unknown node';
  const nodeInfo = node?.nodeInfo;
  const isGateway = nodeInfo?.isGateway || (nodeDisplayName ? gatewayByNode.has(nodeDisplayName) : false);
  const siteOrPool = (nodeDisplayName ? gatewayByNode.get(nodeDisplayName) : undefined) || nodeInfo?.siteName || '-';
  const cniStatus = node ? getCniStatus(node, pullEnabled) : { label: 'No data', tone: 'danger' as const };
  const cniProblemMessages = node ? getNodeCniProblemMessages(node) : [];

  // Collect all node errors and health check issues for the errors card.
  const nodeErrorMessages = useMemo(() => {
    if (!node) return [];
    const messages: string[] = [];
    for (const ne of node.nodeErrors || []) {
      const msg = (ne.message || '').trim();
      if (msg) messages.push(msg);
    }
    if (node.healthCheck && !node.healthCheck.healthy) {
      const summary = (node.healthCheck.summary || '').trim();
      if (summary) messages.push(summary);
    }
    return messages;
  }, [node]);

  const nodeType = getNodeTypeFromProviderId(nodeInfo?.providerId);
  const providerMeta = parseAzureProviderId(nodeInfo?.providerId);
  const providerIsVmss = Boolean(providerMeta && /virtualmachinescalesets/i.test(providerMeta.type));
  const providerName = providerMeta
    ? (providerIsVmss && providerMeta.instanceId && providerMeta.instanceId !== '-')
      ? `${providerMeta.object}/${providerMeta.instanceId}`
      : providerMeta.object
    : '-';
  const nodeLabels = nodeInfo?.k8sLabels || {};
  const tenantId = (azureTenantId || '').trim();
  const tenantPath = tenantId ? `/@${encodeURIComponent(tenantId)}` : '';
  const tenantHashPrefix = tenantId ? `@${encodeURIComponent(tenantId)}/` : '';
  const providerPortalUrl = providerMeta
    ? (() => {
      if (providerIsVmss) {
        if (providerMeta.instanceId && providerMeta.instanceId !== '-') {
          const vmssInstanceResourceId = [
            '/subscriptions',
            providerMeta.subscription,
            'resourceGroups',
            providerMeta.resourceGroup,
            'providers',
            'Microsoft.Compute',
            'virtualMachineScaleSets',
            providerMeta.object,
            'virtualMachines',
            providerMeta.instanceId
          ].join('/');
          return `https://portal.azure.com${tenantPath}/#view/Microsoft_Azure_Compute/VirtualMachineInstancesMenuBlade/~/overview/instanceId/${encodeURIComponent(vmssInstanceResourceId)}`;
        }
        return `https://portal.azure.com/#${tenantHashPrefix}resource/subscriptions/${encodeURIComponent(providerMeta.subscription)}/resourceGroups/${encodeURIComponent(providerMeta.resourceGroup)}/providers/${encodeURIComponent(providerMeta.provider)}/${encodeURIComponent(providerMeta.type)}/${encodeURIComponent(providerMeta.object)}/instances`;
      }
      return `https://portal.azure.com/#${tenantHashPrefix}resource/subscriptions/${encodeURIComponent(providerMeta.subscription)}/resourceGroups/${encodeURIComponent(providerMeta.resourceGroup)}/providers/${encodeURIComponent(providerMeta.provider)}/${encodeURIComponent(providerMeta.type)}/${encodeURIComponent(providerMeta.object)}`;
    })()
    : '';
  const nodeImage = nodeLabels['kubernetes.azure.com/node-image-version'] || nodeInfo?.osImage || '-';
  const nodeAgentBuild = (() => {
    const version = node?.nodeInfo?.buildInfo?.version?.trim() || '';
    const commit = node?.nodeInfo?.buildInfo?.commit?.trim() || '';
    const buildTime = node?.nodeInfo?.buildInfo?.buildTime?.trim() || '';
    const parts: string[] = [];
    if (version) parts.push(`Version: ${version}`);
    if (commit) parts.push(`Commit: ${commit}`);
    if (buildTime) parts.push(`Build Time: ${buildTime}`);
    return parts.length > 0 ? parts.join(' | ') : '-';
  })();
  const podCidrs = nodeInfo?.podCIDRs || [];
  const externalIps = nodeInfo?.externalIPs || [];
  const podCidrFirstIps = podCidrs
    .map((cidr) => firstUsableIpFromCidr(cidr))
    .filter((ip) => Boolean(ip));
  const instanceType = nodeLabels['node.kubernetes.io/instance-type'] || '-';
  const region = nodeLabels['topology.kubernetes.io/region'] || '-';
  const zoneRaw = nodeLabels['topology.kubernetes.io/zone'] || '';
  const availabilityZone = (() => {
    if (!zoneRaw) return '-';
    const parts = zoneRaw.split(/[-_/]/).filter(Boolean);
    const zoneId = parts.length > 0 ? parts[parts.length - 1] : zoneRaw;
    if (zoneId === '0') return 'Regional';
    return zoneRaw;
  })();
  const k8sUpdated = formatDateAndAge(nodeInfo?.k8sUpdatedAt);
  const pushUpdated = formatDateAndAge(node?.lastPushTime);
  const dataCondition = useMemo(() => {
    if (!node) return null;
    if (node.statusSource === 'stale-cache') {
      return {
        tone: 'warning' as const,
        title: 'Warning',
        message: node.fetchError || `Showing cached node status from ${pushUpdated.age}.`
      };
    }
    if (node.statusSource === 'error') {
      return {
        tone: 'danger' as const,
        title: 'Error',
        message: node.fetchError || 'No node status is currently available.'
      };
    }
    return null;
  }, [node, pushUpdated.age]);

  const knownNodeNames = useMemo(() => {
    return new Set(allNodeNames);
  }, [allNodeNames]);

  const allPeerTypeOptions = ['Gateway', 'Site'];
  const allHealthCheckStatusOptions = ['Up', 'Down', 'Mixed', 'Unknown'];
  const allRouteKindOptions = [
    'Local',
    'Tunnel peer',
    'Tunnel gateway',
    'Self',
    'IMDS',
    'Other'
  ];
  const allRouteValidationOptions = ['Matching', 'Mismatch', 'N/A'];
  const defaultRouteKindOptions = allRouteKindOptions.filter((kind) => kind !== 'Local' && kind !== 'IMDS');
  const allRouteInfoOptions = useMemo(() => {
    const types = new Set<string>();
    const routes = node?.routingTable?.routes || [];
    for (const route of routes) {
      for (const hop of route.nextHops || []) {
        const routeType = (hop.info?.routeType || '').trim();
        if (routeType) {
          types.add(routeType);
        }
      }
    }
    return Array.from(types).sort((a, b) => a.localeCompare(b, undefined, { sensitivity: 'base' }));
  }, [node?.routingTable?.routes]);

  const [selectedPeerTypes, setSelectedPeerTypes] = useState<Set<string>>(new Set(allPeerTypeOptions));
  const [selectedHealthCheckStatuses, setSelectedHealthCheckStatuses] = useState<Set<string>>(new Set(allHealthCheckStatusOptions));
  const [selectedRouteKinds, setSelectedRouteKinds] = useState<Set<string>>(new Set(defaultRouteKindOptions));
  const [selectedRouteValidationStates, setSelectedRouteValidationStates] = useState<Set<string>>(new Set(allRouteValidationOptions));
  const [selectedRouteInfoTypes, setSelectedRouteInfoTypes] = useState<Set<string>>(new Set());
  const [detailFilterOpen, setDetailFilterOpen] = useState(false);
  const [peerFilter, setPeerFilter] = useState('');
  const [routeFilter, setRouteFilter] = useState('');
  type RouteSortKey = 'destination' | 'type' | 'destinationNode' | 'nextHops' | 'devices' | 'distance' | 'mtu' | 'info' | 'expected' | 'present';
  const [routeSort, setRouteSort] = useState<{ key: RouteSortKey; direction: 'asc' | 'desc' }>({ key: 'destination', direction: 'asc' });
  const [peerPageSizeMode, setPeerPageSizeMode] = useState<'auto' | number>('auto');
  const [routePageSizeMode, setRoutePageSizeMode] = useState<'auto' | number>('auto');
  const [peerAutoPageSize, setPeerAutoPageSize] = useState(10);
  const [routeAutoPageSize, setRouteAutoPageSize] = useState(10);
  const detailFilterButtonRef = useRef<HTMLButtonElement | null>(null);
  const detailFilterPopoverRef = useRef<HTMLDivElement | null>(null);
  const peerTableWrapperRef = useRef<HTMLDivElement | null>(null);
  const routeTableWrapperRef = useRef<HTMLDivElement | null>(null);

  const allSelectedPeerTypes = selectedPeerTypes.size === allPeerTypeOptions.length;
  const allSelectedHealthCheckStatuses = selectedHealthCheckStatuses.size === allHealthCheckStatusOptions.length;
  const allSelectedRouteKinds = selectedRouteKinds.size === allRouteKindOptions.length;
  const allSelectedRouteValidationStates = selectedRouteValidationStates.size === allRouteValidationOptions.length;
  const allSelectedRouteInfoTypes = allRouteInfoOptions.length === 0 || selectedRouteInfoTypes.size === allRouteInfoOptions.length;

  useEffect(() => {
    setSelectedRouteInfoTypes((prev) => {
      if (allRouteInfoOptions.length === 0) {
        return prev.size === 0 ? prev : new Set();
      }
      if (prev.size === 0) {
        return new Set(allRouteInfoOptions);
      }

      const next = new Set(Array.from(prev).filter((option) => allRouteInfoOptions.includes(option)));
      if (next.size === 0) {
        return new Set(allRouteInfoOptions);
      }
      if (next.size === prev.size && Array.from(next).every((option) => prev.has(option))) {
        return prev;
      }
      return next;
    });
  }, [allRouteInfoOptions]);

  const toggleAllDetailSelection = (
    allSelected: boolean,
    options: string[],
    setter: React.Dispatch<React.SetStateAction<Set<string>>>
  ) => {
    setter(allSelected ? new Set() : new Set(options));
  };

  const resetDetailFilters = () => {
    if (detailTab === 'peerings') {
      setPeerFilter('');
      setSelectedPeerTypes(new Set(allPeerTypeOptions));
      setSelectedHealthCheckStatuses(new Set(allHealthCheckStatusOptions));
      return;
    }

    setRouteFilter('');
    setSelectedRouteKinds(new Set(allRouteKindOptions));
    setSelectedRouteValidationStates(new Set(allRouteValidationOptions));
    setSelectedRouteInfoTypes(new Set(allRouteInfoOptions));
  };

  useDismissOnOutside(detailFilterOpen, [detailFilterButtonRef, detailFilterPopoverRef], () => setDetailFilterOpen(false));

  const isSelfTunnelRoute = (
    destination?: string,
    hop?: { gateway?: string; device?: string; routeTypes?: Array<{ type?: string }> }
  ) => {
    if (!isHostRoute(destination)) return false;
    if (!hop) return false;
    const device = (hop.device || '').trim().toLowerCase();
    if (!isTunnelDevice(device)) return false;
    if ((hop.gateway || '').trim() !== '') return false;
    return (hop.routeTypes || []).some((routeType) => (routeType?.type || '').trim().toLowerCase() === 'connected');
  };

  const isTunnelDevice = (device: string) =>
    device.startsWith('wg') || device.startsWith('gn') || device.startsWith('vxlan') || device.startsWith('ipip');

  const getRouteKind = (destination?: string, nextHops?: Array<{ gateway?: string; device?: string; routeTypes?: Array<{ type?: string }> }>) => {
    if (isImdsRoute(destination)) return 'IMDS';

    const devices = (nextHops || [])
      .map((hop) => (hop.device || '').trim().toLowerCase())
      .filter((device) => Boolean(device));

    const hasLocalDevice = devices.some((device) => (
      device === 'lo'
      || device.startsWith('eth')
      || device.startsWith('ens')
      || device.startsWith('enp')
    ));
    if (hasLocalDevice) return 'Local';

    const hasTunnelDev = devices.some((device) => isTunnelDevice(device));
    const hasSelfTunnel = (nextHops || []).some((hop) => isSelfTunnelRoute(destination, hop));
    if (hasSelfTunnel) {
      return 'Self';
    }
    if (hasTunnelDev && isHostRoute(destination)) {
      return 'Tunnel peer';
    }
    if (hasTunnelDev && !isHostRoute(destination)) {
      return 'Tunnel gateway';
    }

    return 'Other';
  };

  const isWireGuardRouteKind = (kind: string) => kind === 'Tunnel peer' || kind === 'Tunnel gateway'
    || kind === 'Wireguard peer' || kind === 'Wireguard gateway';

  const renderRoutePresence = (kind: string, value: boolean | undefined | 'mixed', aggregate = false) => {
    if (!isWireGuardRouteKind(kind)) {
      return <span className="route-presence-na">N/A</span>;
    }
    if (aggregate && value === 'mixed') {
      return (
        <span className="route-presence route-presence-warning" title="ECMP next hops have mixed values">
          ⚠
        </span>
      );
    }
    if (aggregate && value === undefined) {
      return <span className="route-presence-na">N/A</span>;
    }
    if (value) {
      return <span className="route-presence route-presence-success">✓</span>;
    }
    return <span className="route-presence route-presence-danger">✗</span>;
  };

  const getUniformRoutePresenceValue = (
    hops: Array<{ expected?: boolean; present?: boolean }>,
    key: 'expected' | 'present'
  ): boolean | undefined | 'mixed' => {
    if (hops.length === 0) return undefined;
    const firstValue = hops[0]?.[key];
    const allSame = hops.every((hop) => hop?.[key] === firstValue);
    if (!allSame) return 'mixed';
    return firstValue;
  };

  const splitRouteObjectNames = (value?: string) => {
    if (!value) {
      return [];
    }
    return value
      .split(',')
      .map((part) => part.trim())
      .filter((part) => part.length > 0);
  };

  const resolveRouteSourceMetadata = useCallback((hop?: {
    info?: { objectName?: string; objectType?: string; routeType?: string };
    peerDestinations?: string[];
  }) => {
    const routeType = (hop?.info?.routeType || '').trim();
    const objectType = (hop?.info?.objectType || '').trim();
    const objectNames = splitRouteObjectNames(hop?.info?.objectName || '');

    return {
      routeType,
      objectType,
      objectNames
    };
  }, []);

  const getRouteInfoLabel = (
    hop?: { info?: { objectName?: string; objectType?: string; routeType?: string }; peerDestinations?: string[] },
    aggregate = false,
    destination?: string
  ) => {
    const source = resolveRouteSourceMetadata(hop);
    const routeType = source.routeType;
    const objectType = source.objectType;
    if (routeType) {
      return routeType;
    }
    if (objectType) {
      return objectType;
    }
    if (aggregate && destination) {
      return 'destination';
    }
    return '-';
  };

  const getRouteInfoTooltip = (
    hop?: { info?: { objectName?: string; objectType?: string; routeType?: string }; peerDestinations?: string[] },
    aggregate = false,
    destination?: string
  ) => {
    const source = resolveRouteSourceMetadata(hop);
    const routeType = source.routeType;
    const objectType = source.objectType;
    const objectName = source.objectNames.join(',');
    const objectLabel = [objectType, objectName].filter((part) => part.length > 0).join(': ');
    if (objectLabel && routeType) {
      return `${routeType} -- ${objectLabel}`;
    }
    if (objectLabel) {
      return objectLabel;
    }
    if (routeType) {
      return routeType;
    }
    if (aggregate && destination) {
      return `destination:${destination}`;
    }
    return '';
  };

  const getRouteTypeDisplay = (kind: string): { label: string; tone: '' | 'success' | 'info' } => {
    if (kind === 'Tunnel peer' || kind === 'Wireguard peer') return { label: 'Peer', tone: 'success' };
    if (kind === 'Tunnel gateway' || kind === 'Wireguard gateway') return { label: 'Gateway', tone: 'info' };
    if (kind === 'Self') return { label: 'Self', tone: '' };
    return { label: kind, tone: '' };
  };

  const routeMinDistance = (route: { nextHops?: Array<{ distance?: number }> }) => {
    const distances = (route.nextHops || [])
      .map((hop) => (typeof hop.distance === 'number' && hop.distance > 0 ? hop.distance : undefined))
      .filter((distance): distance is number => distance !== undefined);
    if (distances.length === 0) {
      return Number.POSITIVE_INFINITY;
    }
    return Math.min(...distances);
  };

  const routeDistanceLabel = (distance: number) => {
    if (!Number.isFinite(distance)) {
      return '-';
    }
    return String(distance);
  };

  const routeSortValue = (
    route: { destination?: string; nextHops?: Array<{ gateway?: string; device?: string; distance?: number; mtu?: number; expected?: boolean; present?: boolean; info?: { objectName?: string; objectType?: string; routeType?: string } }> },    key: RouteSortKey,
    options?: { useMixedDestinationSort?: boolean }
  ) => {
    const nextHops = route.nextHops || [];
    const firstHop = nextHops[0];
    const kind = getRouteKind(route.destination, nextHops);
    const isECMP = nextHops.length > 1;
    switch (key) {
      case 'destination':
        if (options?.useMixedDestinationSort) {
          return mixedDestinationSortKey(route.destination);
        }
        return route.destination || '';
      case 'type':
        return getRouteTypeDisplay(kind).label;
      case 'destinationNode':
        return isECMP ? '' : resolveRouteDestinationNodes(route);
      case 'nextHops':
        return isECMP ? `ecmp-${nextHops.length}` : (firstHop?.gateway || '');
      case 'devices':
        return isECMP ? '' : (firstHop?.device || '');
      case 'distance': {
        const minimumDistance = routeMinDistance(route);
        if (!Number.isFinite(minimumDistance)) {
          return '999999';
        }
        return minimumDistance.toString().padStart(6, '0');
      }
      case 'mtu': {
        const mtuValues = nextHops
          .map((hop) => hop.mtu)
          .filter((v): v is number => typeof v === 'number' && v > 0);
        if (mtuValues.length === 0) return '000000';
        return Math.max(...mtuValues).toString().padStart(6, '0');
      }
      case 'info':
        return getRouteInfoLabel(firstHop, isECMP, route.destination || undefined);
      case 'expected': {
        const value = firstHop?.expected;
        return value === true ? '1' : value === false ? '0' : '-1';
      }
      case 'present': {
        const value = firstHop?.present;
        return value === true ? '1' : value === false ? '0' : '-1';
      }
      default:
        return '';
    }
  };

  const sortRoutes = (
    routes: Array<{ destination?: string; nextHops?: Array<{ gateway?: string; device?: string; distance?: number; mtu?: number; expected?: boolean; present?: boolean; info?: { objectName?: string; objectType?: string; routeType?: string } }> }>,
    sort: { key: RouteSortKey; direction: 'asc' | 'desc' },
    options?: { useMixedDestinationSort?: boolean }
  ) => {
    const multiplier = sort.direction === 'asc' ? 1 : -1;
    return [...routes].sort((a, b) => {
      const left = String(routeSortValue(a, sort.key, options) || '');
      const right = String(routeSortValue(b, sort.key, options) || '');
      const compare = left.localeCompare(right, undefined, { numeric: true, sensitivity: 'base' });
      return compare * multiplier;
    });
  };

  const toggleRouteSort = (
    key: RouteSortKey,
    setter: React.Dispatch<React.SetStateAction<{ key: RouteSortKey; direction: 'asc' | 'desc' }>>
  ) => {
    setter((prev) => {
      if (prev.key !== key) return { key, direction: 'asc' };
      return { key, direction: prev.direction === 'asc' ? 'desc' : 'asc' };
    });
  };

  const renderRouteSortArrow = (sort: { key: RouteSortKey; direction: 'asc' | 'desc' }, key: RouteSortKey) => {
    if (sort.key !== key) return '';
    return sort.direction === 'asc' ? '↑' : '↓';
  };

  const summarizeWireGuardRouteValidation = useCallback((routes: Array<{ destination?: string; nextHops?: Array<{ gateway?: string; device?: string; expected?: boolean; present?: boolean }> }>) => {
    let total = 0;
    let expected = 0;
    let present = 0;
    let mismatch = 0;

    for (const route of routes || []) {
      const kind = getRouteKind(route.destination, route.nextHops || []);
      if (!isWireGuardRouteKind(kind)) {
        continue;
      }

      for (const hop of route.nextHops || []) {
        total += 1;
        const hopExpected = hop.expected === true;
        const hopPresent = hop.present === true;
        if (hopExpected) expected += 1;
        if (hopPresent) present += 1;
        if (hopExpected !== hopPresent) mismatch += 1;
      }
    }

    return { total, expected, present, mismatch };
  }, [getRouteKind]);

  const getRouteValidationCategory = (route: { destination?: string; nextHops?: Array<{ gateway?: string; device?: string; expected?: boolean; present?: boolean }> }) => {
    const kind = getRouteKind(route.destination, route.nextHops || []);
    if (!isWireGuardRouteKind(kind)) {
      return 'N/A';
    }
    const hops = route.nextHops || [];
    const hasMismatch = hops.some((hop) => (hop.expected === true) !== (hop.present === true));
    return hasMismatch ? 'Mismatch' : 'Matching';
  };

  const getRouteInfoTypes = (route: { nextHops?: Array<{ info?: { routeType?: string } }> }) => {
    const infoTypes = new Set<string>();
    for (const hop of route.nextHops || []) {
      const routeType = (hop.info?.routeType || '').trim();
      if (routeType) {
        infoTypes.add(routeType);
      }
    }
    return infoTypes;
  };

  const allRoutes = useMemo(() => {
    return node?.routingTable?.routes || [];
  }, [node?.routingTable?.routes]);

  const filteredRoutes = useMemo(() => {
    const needle = routeFilter.trim().toLowerCase();
    const hasRouteInfoFilter = allRouteInfoOptions.length > 0 && selectedRouteInfoTypes.size !== allRouteInfoOptions.length;
    return allRoutes.filter((route) => {
      const destination = route.destination || '';
      const kind = getRouteKind(destination, route.nextHops);
      const validationCategory = getRouteValidationCategory(route);
      if (!selectedRouteKinds.has(kind)) return false;
      if (!selectedRouteValidationStates.has(validationCategory)) return false;

      if (hasRouteInfoFilter) {
        const routeInfoTypes = getRouteInfoTypes(route);
        if (routeInfoTypes.size === 0) return false;
        const hasSelectedInfoType = Array.from(routeInfoTypes).some((routeInfoType) => selectedRouteInfoTypes.has(routeInfoType));
        if (!hasSelectedInfoType) return false;
      }

      if (!needle) return true;
      const destinationNodes = (route.nextHops || [])
        .map((hop) => `${hop.info?.objectName || ''} ${(hop.peerDestinations || []).join(' ')}`)
        .join(' ')
        .toLowerCase();
      const nextHops = (route.nextHops || [])
        .map((hop) => `${hop.gateway || ''}`)
        .join(' ')
        .toLowerCase();
      const devices = (route.nextHops || [])
        .map((hop) => `${hop.device || ''}`)
        .join(' ')
        .toLowerCase();
      const mixedDestination = mixedDestinationSortKey(destination);
      if (!destination.toLowerCase().includes(needle) && !mixedDestination.includes(needle) && !destinationNodes.includes(needle) && !nextHops.includes(needle) && !devices.includes(needle)) return false;
      return true;
    });
  }, [allRouteInfoOptions, allRoutes, getRouteValidationCategory, routeFilter, selectedRouteInfoTypes, selectedRouteKinds, selectedRouteValidationStates]);
  const routesValidationSummary = useMemo(
    () => summarizeWireGuardRouteValidation(allRoutes),
    [allRoutes, summarizeWireGuardRouteValidation]
  );

  const peerNamesByOverlayIp = useMemo(() => {
    const map = new Map<string, Set<string>>();
    for (const peer of peers) {
      const peerName = (peer.name || '').trim();
      if (!peerName) {
        continue;
      }
      for (const rawGateway of peer.podCidrGateways || []) {
        const overlayIp = normalizeIpAddress(rawGateway);
        if (!overlayIp) {
          continue;
        }
        if (!map.has(overlayIp)) {
          map.set(overlayIp, new Set<string>());
        }
        map.get(overlayIp)?.add(peerName);
      }
    }
    return map;
  }, [peers]);

  const peerCandidatesByInterface = useMemo(() => {
    const map = new Map<string, Array<{ name: string; endpointHost: string; overlayIps: Set<string>; allowedHosts: Set<string> }>>();
    for (const peer of peers) {
      const iface = (peer.tunnel?.interface || '').trim();
      const peerName = (peer.name || '').trim();
      if (!iface || !peerName) {
        continue;
      }
      if (!map.has(iface)) {
        map.set(iface, []);
      }
      map.get(iface)?.push({
        name: peerName,
        endpointHost: normalizeIpAddress(parseEndpointHost(peer.tunnel?.endpoint)),
        overlayIps: new Set((peer.podCidrGateways || []).map((gateway) => normalizeIpAddress(gateway)).filter((gateway) => gateway.length > 0)),
        allowedHosts: hostCandidatesFromAllowedIPs(peer.tunnel?.allowedIPs)
      });
    }
    return map;
  }, [peers]);

  const resolveDestinationNodesForHops = useCallback((destination: string | undefined, hops: Array<{ gateway?: string; device?: string; info?: { objectName?: string; objectType?: string; routeType?: string }; peerDestinations?: string[] }>) => {
    if (!isHostRoute(destination)) {
      const routeType = (hops.find((hop) => {
        const routeTypeValue = (hop.info?.routeType || '').trim().toLowerCase();
        return routeTypeValue === 'nodecidr' || routeTypeValue === 'routedcidr';
      })?.info?.routeType || '').trim().toLowerCase();

      if (routeType === 'nodecidr' || routeType === 'routedcidr') {
        // Resolve each hop's gateway IP to the specific peer node name.
        const peerNodes = new Set<string>();
        for (const hop of hops) {
          const nextHopIp = (hop.gateway || '').trim();
          if (nextHopIp) {
            const normalizedIp = normalizeIpAddress(nextHopIp);
            for (const peerName of peerNamesByOverlayIp.get(normalizedIp) || []) {
              peerNodes.add(peerName);
            }
          }
        }
        if (peerNodes.size > 0) {
          return Array.from(peerNodes)
            .sort((a, b) => a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' }))
            .join(', ');
        }
        // Fallback to source metadata if gateway IP lookup fails
        const sourceNames = new Set<string>();
        for (const hop of hops) {
          const source = resolveRouteSourceMetadata(hop);
          for (const objectName of source.objectNames) {
            sourceNames.add(objectName);
          }
        }
        if (sourceNames.size > 0) {
          return Array.from(sourceNames)
            .sort((a, b) => a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' }))
            .join(', ');
        }
      }
    }

    const metadataNodes = new Set<string>();
    const heuristicNodes = new Set<string>();
    let hasSiteMetadata = false;

    for (const hop of hops) {
      const objectName = (hop.info?.objectName || '').trim();
      const objectType = (hop.info?.objectType || '').trim().toLowerCase();
      if (objectType === 'site') {
        hasSiteMetadata = true;
      }
      if (!objectName) {
        continue;
      }
      for (const part of objectName.split(',')) {
        const name = part.trim();
        if (name) {
          metadataNodes.add(name);
        }
      }
    }

    const isInterfaceRoute = isHostRoute(destination);
    const routeHost = hostIpFromRouteDestination(destination);
    const normalizedRouteHost = normalizeIpAddress(routeHost);

    for (const hop of hops) {
      if (isInterfaceRoute) {
        const iface = (hop.device || '').trim();
        if (!iface) {
          continue;
        }

        const candidates = peerCandidatesByInterface.get(iface) || [];
        if (candidates.length === 0) {
          continue;
        }

        let selected = candidates;
        if (normalizedRouteHost && candidates.length > 1) {
          const endpointMatches = candidates.filter(
            (candidate) => candidate.endpointHost === normalizedRouteHost
          );
          if (endpointMatches.length > 0) {
            selected = endpointMatches;
          } else {
            const overlayMatches = candidates.filter(
              (candidate) => candidate.overlayIps.has(normalizedRouteHost)
            );
            if (overlayMatches.length > 0) {
              selected = overlayMatches;
            } else {
              const allowedMatches = candidates.filter(
                (candidate) => candidate.allowedHosts.has(normalizedRouteHost)
              );
              selected = allowedMatches;
            }
          }
        }

        for (const candidate of selected) {
          heuristicNodes.add(candidate.name);
        }
        continue;
      }

      const nextHopIp = (hop.gateway || '').trim();
      if (!nextHopIp) {
        continue;
      }
      const normalizedNextHopIp = normalizeIpAddress(nextHopIp);
      for (const peerName of peerNamesByOverlayIp.get(normalizedNextHopIp) || []) {
        heuristicNodes.add(peerName);
      }
    }

    const singleHop = hops.length === 1;
    // When metadata names are site/pool objects rather than node names,
    // prefer the heuristic node resolution (gateway IP -> peer name).
    const metadataIsNodeName = !hasSiteMetadata;
    const preferredNodes = metadataIsNodeName && metadataNodes.size > 0
      ? metadataNodes
      : (singleHop && heuristicNodes.size > 0) || (isInterfaceRoute && heuristicNodes.size > 0)
        ? heuristicNodes
        : metadataNodes.size > 0
          ? metadataNodes
          : heuristicNodes;

    if (preferredNodes.size === 0) {
      return '-';
    }
    return Array.from(preferredNodes)
      .sort((a, b) => a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' }))
      .join(', ');
  }, [peerCandidatesByInterface, peerNamesByOverlayIp, resolveRouteSourceMetadata]);

  const resolveRouteDestinationNodes = useCallback((route: { destination?: string; nextHops?: Array<{ gateway?: string; device?: string; info?: { objectName?: string; objectType?: string } }> }) => {
    return resolveDestinationNodesForHops(route.destination, route.nextHops || []);
  }, [resolveDestinationNodesForHops]);

  type PeerRow = {
    id: string;
    destination: string;
    destinationNodeAvailable: boolean;
    peerTypeLabel: string;
    peerTypeTone: '' | 'info' | 'success';
    peerTypeSort: string;
    site: string;
    pool: string;
    tunnelProtocol: string;
    destinationReady: string | null;
    healthCheckStatus: string;
    healthCheckUptime: string;
    healthCheckRtt: string;
    localInterface: string;
    endpoint: string;
    podCidrGateways: string;
    rxBytes: number;
    txBytes: number;
    lastHandshake?: string;
    allowedIPs: string[];
    allowedRoutes: Array<{ route: string; type: string; destination: string }>;
  };

  const routeDetailsByPeer = useMemo(() => {
    const byPeer = new Map<string, Map<string, { tags: Set<string> }>>();
    const allRoutes = node?.routingTable?.routes || [];

    for (const route of allRoutes) {
      const destination = (route.destination || '').trim();
      if (!destination) continue;
      for (const hop of route.nextHops || []) {
        const source = resolveRouteSourceMetadata(hop);
        const routeType = source.routeType;
        const objectType = source.objectType;
        const objectName = source.objectNames.join(',');
        const tags: string[] = [];
        if (routeType) {
          tags.push(routeType);
        }
        if (objectName) {
          if (objectType && objectType.toLowerCase() !== 'site') {
            tags.push(`${objectType}:${objectName}`);
          } else {
            tags.push(objectName);
          }
        }

        for (const peerName of hop.peerDestinations || []) {
          const normalizedPeerName = (peerName || '').trim();
          if (!normalizedPeerName) continue;
          if (!byPeer.has(normalizedPeerName)) {
            byPeer.set(normalizedPeerName, new Map());
          }
          const peerRoutes = byPeer.get(normalizedPeerName);
          if (!peerRoutes) continue;
          if (!peerRoutes.has(destination)) {
            peerRoutes.set(destination, { tags: new Set() });
          }
          const routeDetails = peerRoutes.get(destination);
          if (!routeDetails) continue;
          for (const tag of tags) {
            routeDetails.tags.add(tag);
          }
        }
      }
    }

    return byPeer;
  }, [node?.routingTable?.routes]);

  const peerRows = useMemo<PeerRow[]>(() => {
    return peers.map((peer, index) => {
      const destination = peer.name || '-';
      const inferredGateway = Boolean(peer.name && gatewayByNode.has(peer.name));
      const peerTypeRaw = (peer.peerType || '').trim().toLowerCase();
      const peerTypeLabel = peerTypeRaw === 'gateway' || inferredGateway
        ? 'Gateway'
        : 'Site';
      const peerTypeTone: '' | 'info' | 'success' =
        peerTypeLabel.toLowerCase() === 'gateway'
          ? 'info'
          : peerTypeLabel.toLowerCase() === 'site'
            ? 'success'
            : '';
      const site = peer.siteName || '-';
      const pool = peerTypeLabel === 'Gateway'
        ? (peer.name ? (gatewayByNode.get(peer.name) || '-') : '-')
        : '';
      const peerInterface = (peer.tunnel?.interface || '').trim();
      const tunnelProtocol = (peer.tunnel?.protocol || 'WireGuard').trim();
      const sortedRouteDestinations = [...(peer.routeDestinations || [])];
      sortedRouteDestinations.sort((a, b) => a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' }));
      const routeDetails = peer.name ? routeDetailsByPeer.get(peer.name) : undefined;
      const allowedRoutes = sortedRouteDestinations.map((route) => {
        const normalizedRoute = route.trim();
        if (!normalizedRoute) {
          return { route, type: '', destination: '' };
        }
        const routeDetail = routeDetails?.get(normalizedRoute);
        if (!routeDetail) {
          return { route, type: '', destination: '' };
        }
        const rawParts = Array.from(routeDetail.tags)
          .filter((tag) => !tag.toLowerCase().startsWith('multiple:'));
        const hasNodeCidr = rawParts.some((tag) => tag.toLowerCase() === 'nodecidr');
        const hasPodCidr = rawParts.some((tag) => tag.toLowerCase() === 'podcidr');
        const routeType = hasNodeCidr
          ? 'nodeCidr'
          : hasPodCidr
            ? 'podCidr'
            : '';

        const targetCandidates = rawParts
          .filter((tag) => tag.toLowerCase() !== 'nodecidr' && tag.toLowerCase() !== 'podcidr')
          .map((tag) => tag.replace(/^gateway:/i, '').replace(/^node:/i, ''))
          .filter((tag) => Boolean(tag));

        const target = hasNodeCidr
          ? (site && site !== '-' ? site : (targetCandidates[0] || ''))
          : (targetCandidates[0] || '');

        return {
          route,
          type: routeType,
          destination: target
        };
      });
      const localInterface = typeof node?.nodeInfo?.wireGuard?.interface === 'string'
        ? node.nodeInfo.wireGuard.interface
        : node?.nodeInfo?.wireGuard?.interface
          ? 'enabled'
          : '-';
      const allowedIPs = [...(peer.tunnel?.allowedIPs || [])];
      allowedIPs.sort((a, b) => a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' }));
      const hcState = getPeerHealthCheckState(peer);

      return {
        id: `${destination}-${peer.tunnel?.endpoint || index}`,
        destination,
        destinationNodeAvailable: Boolean(peer.name && knownNodeNames.has(peer.name)),
        peerTypeLabel,
        peerTypeTone,
        peerTypeSort: peerTypeLabel.toLowerCase(),
        site,
        pool,
        tunnelProtocol,
        destinationReady: peer.name ? (nodeK8sStatusMap?.get(peer.name) || null) : null,
        healthCheckStatus: hcState.status,
        healthCheckUptime: hcState.enabled ? (peer.healthCheck?.uptime || '-') : '-',
        healthCheckRtt: hcState.enabled ? (peer.healthCheck?.rtt || '-') : '-',
        localInterface: peerInterface || localInterface,
        endpoint: peer.tunnel?.endpoint || '-',
        podCidrGateways: (peer.podCidrGateways || []).length > 0 ? (peer.podCidrGateways || []).join(', ') : '-',
        rxBytes: Number(peer.tunnel?.rxBytes || 0),
        txBytes: Number(peer.tunnel?.txBytes || 0),
        lastHandshake: peer.tunnel?.lastHandshake,
        allowedIPs,
        allowedRoutes
      };
    });
  }, [routeDetailsByPeer, knownNodeNames, gatewayByNode, node?.nodeInfo?.wireGuard?.interface, peers]);

  const filteredPeerRows = useMemo(() => {
    const needle = peerFilter.trim().toLowerCase();
    return peerRows.filter((row) => {
      const hc = row.healthCheckStatus.toLowerCase() === 'up'
        ? 'Up'
        : row.healthCheckStatus.toLowerCase() === 'mixed'
          ? 'Mixed'
        : row.healthCheckStatus.toLowerCase() === 'down'
          ? 'Down'
          : 'Unknown';

      if (!selectedPeerTypes.has(row.peerTypeLabel)) return false;
      if (!selectedHealthCheckStatuses.has(hc)) return false;
      if (!needle) return true;
      const haystack = `${row.destination} ${row.site} ${row.pool} ${row.endpoint} ${row.localInterface}`.toLowerCase();
      if (!haystack.includes(needle)) return false;

      return true;
    });
  }, [peerFilter, peerRows, selectedHealthCheckStatuses, selectedPeerTypes]);

  const [peerSorting, setPeerSorting] = useState<SortingState>([
    { id: 'destination', desc: false }
  ]);
  const [peerPagination, setPeerPagination] = useState({ pageIndex: 0, pageSize: 10 });
  const [routePagination, setRoutePagination] = useState({ pageIndex: 0, pageSize: 10 });
  const [expandedPeerRows, setExpandedPeerRows] = useState<Record<string, boolean>>({});
  const [expandedRouteEcmpRows, setExpandedRouteEcmpRows] = useState<Record<string, boolean>>({});
  const identicalPeerPaginationUpdateRef = useRef(0);

  const peerColumns = useMemo<ColumnDef<PeerRow>[]>(
    () => [
      {
        id: 'expand',
        enableSorting: false,
        header: '',
        cell: ({ row }) => (
          <button
            className="details-toggle-button"
            aria-label={expandedPeerRows[row.original.id] ? 'Collapse peering details' : 'Expand peering details'}
            type="button"
            onClick={(event) => {
              event.stopPropagation();
              togglePeerRowExpanded(row.original.id);
            }}
          >
            {expandedPeerRows[row.original.id] ? '▾' : '▸'}
          </button>
        )
      },
      {
        id: 'destination',
        header: 'Destination Node',
        accessorKey: 'destination',
        cell: ({ row }) => {
          const canNavigate = row.original.destinationNodeAvailable;
          return (
            <div className="peer-destination-cell">
              <span>{row.original.destination}</span>
              {canNavigate ? (
                <button
                  className="details-toggle-button"
                  aria-label={`Open details for ${row.original.destination}`}
                  type="button"
                  onClick={(event) => {
                    event.stopPropagation();
                    onSelectNode(row.original.destination);
                  }}
                  title={`Open ${row.original.destination}`}
                >
                  {'>'}
                </button>
              ) : (
                <span style={{ opacity: 0.5 }}>-</span>
              )}
            </div>
          );
        }
      },
      {
        id: 'peerType',
        header: 'Type',
        accessorFn: (row) => row.peerTypeSort,
        cell: ({ row }) => (
          <span className={`badge${row.original.peerTypeTone ? ` ${row.original.peerTypeTone}` : ''}`}>
            {row.original.peerTypeLabel}
          </span>
        )
      },
      {
        id: 'site',
        header: 'Site',
        accessorKey: 'site',
        cell: ({ row }) => <span className="badge site-pool-badge">{row.original.site}</span>
      },
      {
        id: 'pool',
        header: 'Pool',
        accessorKey: 'pool',
        cell: ({ row }) => {
          if (!row.original.pool) {
            return <span></span>;
          }
          return <span className="badge site-pool-badge">{row.original.pool}</span>;
        }
      },
      {
        id: 'tunnelProtocol',
        header: 'Protocol',
        accessorKey: 'tunnelProtocol'
      },
      {
        id: 'k8sStatus',
        header: 'K8s',
        accessorFn: (row) => row.destinationReady || '-',
        cell: ({ row }) => {
          const status = row.original.destinationReady;
          if (!status) return <span className="badge">Unknown</span>;
          const tone = status === 'Ready' ? 'success' : 'danger';
          return (
            <span className={`badge ${tone}`}>
              {status}
            </span>
          );
        }
      },
      {
        id: 'healthCheckStatus',
        header: 'HC',
        accessorKey: 'healthCheckStatus',
        cell: ({ row }) => {
          const value = row.original.healthCheckStatus.toLowerCase();
          const tone = value === 'up' ? 'success' : value === 'mixed' ? 'warning' : value === 'down' ? 'danger' : '';
          return <span className={`badge${tone ? ` ${tone}` : ''}`}>{row.original.healthCheckStatus}</span>;
        }
      },
      { id: 'endpoint', header: 'Endpoint', accessorKey: 'endpoint' },
      {
        id: 'allowedIpCount',
        header: 'Allowed IPs',
        accessorFn: (row) => row.allowedIPs.length,
        cell: ({ row }) => row.original.allowedIPs.length
      },
      {
        id: 'allowedRouteCount',
        header: 'Routes',
        accessorFn: (row) => row.allowedRoutes.length,
        cell: ({ row }) => row.original.allowedRoutes.length
      }
    ],
    [expandedPeerRows, knownNodeNames, onSelectNode]
  );

  const peerTable = useReactTable({
    data: filteredPeerRows,
    columns: peerColumns,
    autoResetPageIndex: false,
    state: {
      sorting: peerSorting,
      pagination: peerPagination
    },
    onSortingChange: setPeerSorting,
    onPaginationChange: (updater) => {
      setPeerPagination((prev) => {
        const next = typeof updater === 'function' ? updater(prev) : updater;
        const unchanged = prev.pageIndex === next.pageIndex && prev.pageSize === next.pageSize;
        if (unchanged) {
          identicalPeerPaginationUpdateRef.current += 1;
          if (identicalPeerPaginationUpdateRef.current % 200 === 0) {
            uiDiag(
              'peer-pagination-identical-loop',
              'repeated identical peer pagination updates observed',
              {
                count: identicalPeerPaginationUpdateRef.current,
                pageIndex: prev.pageIndex,
                pageSize: prev.pageSize
              },
              { minIntervalMs: 1000, level: 'log' }
            );
          }
          return prev;
        }
        identicalPeerPaginationUpdateRef.current = 0;
        return next;
      });
    },
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getPaginationRowModel: getPaginationRowModel()
  });

  const peerFilteredCount = filteredPeerRows.length;
  const peerPageCount = peerTable.getPageCount() || 1;
  const peerPageIndex = peerTable.getState().pagination.pageIndex;
  const peerPageSize = peerTable.getState().pagination.pageSize;

  const routesFilteredCount = filteredRoutes.length;
  const sortedRoutes = useMemo(
    () => sortRoutes(filteredRoutes, routeSort, { useMixedDestinationSort: routeSort.key === 'destination' }),
    [filteredRoutes, routeSort]
  );
  const routesPageCount = Math.max(1, Math.ceil(routesFilteredCount / routePagination.pageSize));
  const routesPageIndex = routePagination.pageIndex;
  const routesPageSize = routePagination.pageSize;
  const pagedRoutes = useMemo(() => {
    const start = routePagination.pageIndex * routePagination.pageSize;
    return sortedRoutes.slice(start, start + routePagination.pageSize);
  }, [sortedRoutes, routePagination.pageIndex, routePagination.pageSize]);

  useEffect(() => {
    peerTable.setPageIndex(0);
  }, [peerFilter, filteredPeerRows.length, selectedPeerTypes, selectedHealthCheckStatuses, peerTable]);

  useEffect(() => {
    setRoutePagination((prev) => (prev.pageIndex === 0 ? prev : { ...prev, pageIndex: 0 }));
  }, [filteredRoutes.length, routeFilter, selectedRouteInfoTypes, selectedRouteKinds]);

  useEffect(() => {
    setDetailFilterOpen(false);
  }, [detailTab]);

  useEffect(() => {
    if (detailTab !== 'peerings' || peerPageSizeMode !== 'auto') {
      return;
    }
    const wrapper = peerTableWrapperRef.current;
    if (!wrapper) return;

    let rafId = 0;
    let pending = false;

    const updateAutoPageSize = () => {
      if (pending) return;
      pending = true;
      rafId = window.requestAnimationFrame(() => {
        pending = false;
        const header = wrapper.querySelector('thead');
        const headerHeight = header instanceof HTMLElement ? header.getBoundingClientRect().height : 0;
        const rowHeights = Array.from(wrapper.querySelectorAll('tbody tr'))
          .slice(0, 5)
          .map((row) => (row instanceof HTMLElement ? row.getBoundingClientRect().height : 0))
          .filter((height) => height > 0);
        const measuredRowHeight = rowHeights.length > 0
          ? Math.max(24, rowHeights.reduce((sum, h) => sum + h, 0) / rowHeights.length)
          : 40;
        const wrapperTop = wrapper.getBoundingClientRect().top;
        const available = Math.max(0, window.innerHeight - wrapperTop - 28 - headerHeight);
        const nextSize = Math.max(10, Math.floor(available / measuredRowHeight));
        setPeerAutoPageSize(nextSize);
        setPeerPagination((prev) => (prev.pageSize === nextSize ? prev : { ...prev, pageSize: nextSize }));
      });
    };

    updateAutoPageSize();
    window.addEventListener('resize', updateAutoPageSize);

    return () => {
      if (rafId) window.cancelAnimationFrame(rafId);
      window.removeEventListener('resize', updateAutoPageSize);
    };
  }, [detailTab, peerFilteredCount, peerPageSizeMode]);

  useEffect(() => {
    if (detailTab !== 'routes' || routePageSizeMode !== 'auto') {
      return;
    }
    const wrapper = routeTableWrapperRef.current;
    if (!wrapper) return;

    let rafId = 0;
    let pending = false;
    const updateAutoPageSize = () => {
      if (pending) return;
      pending = true;
      rafId = window.requestAnimationFrame(() => {
        pending = false;
        const header = wrapper.querySelector('thead');
        const headerHeight = header instanceof HTMLElement ? header.getBoundingClientRect().height : 0;
        const rowHeights = Array.from(wrapper.querySelectorAll('tbody tr'))
          .slice(0, 5)
          .map((row) => (row instanceof HTMLElement ? row.getBoundingClientRect().height : 0))
          .filter((height) => height > 0);
        const measuredRowHeight = rowHeights.length > 0
          ? Math.max(24, rowHeights.reduce((sum, h) => sum + h, 0) / rowHeights.length)
          : 38;
        const wrapperTop = wrapper.getBoundingClientRect().top;
        const available = Math.max(0, window.innerHeight - wrapperTop - 28 - headerHeight);
        const nextSize = Math.max(10, Math.floor(available / measuredRowHeight));
        setRouteAutoPageSize(nextSize);
        setRoutePagination((prev) => (prev.pageSize === nextSize ? prev : { ...prev, pageSize: nextSize }));
      });
    };

    updateAutoPageSize();
    window.addEventListener('resize', updateAutoPageSize);

    return () => {
      if (rafId) window.cancelAnimationFrame(rafId);
      window.removeEventListener('resize', updateAutoPageSize);
    };
  }, [detailTab, routePageSizeMode, routesFilteredCount]);

  useEffect(() => {
    setRoutePagination((prev) => {
      if (routesPageCount <= 0 || prev.pageIndex < routesPageCount) {
        return prev;
      }
      return { ...prev, pageIndex: routesPageCount - 1 };
    });
  }, [routesPageCount]);

  useEffect(() => {
    setRouteSort({ key: 'destination', direction: 'asc' });
  }, [nodeName]);

  const togglePeerRowExpanded = (id: string) => {
    setExpandedPeerRows((prev) => ({ ...prev, [id]: !prev[id] }));
  };

  const toggleRouteEcmpRowExpanded = (id: string) => {
    setExpandedRouteEcmpRows((prev) => ({ ...prev, [id]: !prev[id] }));
  };

  const renderDetailPagination = () => {
    if (detailTab === 'peerings') {
      return (
        <div className="pagination">
          <span className="pagination-info">
            {peerFilteredCount === 0 ? '0' : peerPageIndex * peerPageSize + 1}
            -{Math.min(peerFilteredCount, (peerPageIndex + 1) * peerPageSize)} of {peerFilteredCount}
          </span>
          <div className="filter-menu-anchor">
            <TableFilterButton
              ref={detailFilterButtonRef}
              active={isDetailFilterActive}
              open={detailFilterOpen}
              onToggle={() => setDetailFilterOpen((open) => !open)}
              title="Detail table filters"
            />
            {detailFilterOpen && (
              <div className="filter-popover" ref={detailFilterPopoverRef}>
                {renderDetailFilterContent()}
              </div>
            )}
          </div>
          <select
            className="select"
            value={peerPageSizeMode === 'auto' ? 'auto' : String(peerPageSizeMode)}
            onChange={(event) => {
              const value = event.target.value;
              if (value === 'auto') {
                setPeerPageSizeMode('auto');
              } else {
                const size = Number(value);
                setPeerPageSizeMode(size);
                peerTable.setPageSize(size);
              }
            }}
          >
            <option value="auto">Auto ({peerAutoPageSize} / page)</option>
            {[10, 25, 50, 100, 250].map((size) => (
              <option key={size} value={size}>{size} / page</option>
            ))}
            <option value="99999">All</option>
          </select>
          <button className="button" onClick={() => peerTable.previousPage()} disabled={!peerTable.getCanPreviousPage()}>
            Prev
          </button>
          <button
            className="button"
            onClick={() => {
              const input = window.prompt(`Go to page (1-${peerPageCount})`, String(peerPageIndex + 1));
              if (input == null) return;
              const parsed = Number.parseInt(input.trim(), 10);
              if (!Number.isFinite(parsed)) return;
              const clamped = Math.min(Math.max(parsed, 1), peerPageCount);
              peerTable.setPageIndex(clamped - 1);
            }}
            disabled={peerFilteredCount === 0 || peerPageCount <= 1}
          >
            Page {peerPageIndex + 1} / {peerPageCount}
          </button>
          <button className="button" onClick={() => peerTable.nextPage()} disabled={!peerTable.getCanNextPage()}>
            Next
          </button>
        </div>
      );
    }

    if (detailTab === 'routes') {
      return (
        <div className="pagination">
          <span className="pagination-info">
            {routesFilteredCount === 0 ? '0' : routesPageIndex * routesPageSize + 1}
            -{Math.min(routesFilteredCount, (routesPageIndex + 1) * routesPageSize)} of {routesFilteredCount}
          </span>
          <div className="filter-menu-anchor">
            <TableFilterButton
              ref={detailFilterButtonRef}
              active={isDetailFilterActive}
              open={detailFilterOpen}
              onToggle={() => setDetailFilterOpen((open) => !open)}
              title="Detail table filters"
            />
            {detailFilterOpen && (
              <div className="filter-popover" ref={detailFilterPopoverRef}>
                {renderDetailFilterContent()}
              </div>
            )}
          </div>
          <select
            className="select"
            value={routePageSizeMode === 'auto' ? 'auto' : String(routePageSizeMode)}
            onChange={(event) => {
              const value = event.target.value;
              if (value === 'auto') {
                setRoutePageSizeMode('auto');
              } else {
                const size = Number(value);
                setRoutePageSizeMode(size);
                setRoutePagination((prev) => ({ ...prev, pageSize: size }));
              }
            }}
          >
            <option value="auto">Auto ({routeAutoPageSize} / page)</option>
            {[10, 25, 50, 100, 250].map((size) => (
              <option key={size} value={size}>{size} / page</option>
            ))}
            <option value="99999">All</option>
          </select>
          <button
            className="button"
            onClick={() => setRoutePagination((prev) => ({ ...prev, pageIndex: Math.max(0, prev.pageIndex - 1) }))}
            disabled={routesPageIndex <= 0}
          >
            Prev
          </button>
          <button
            className="button"
            onClick={() => {
              const input = window.prompt(`Go to page (1-${routesPageCount})`, String(routesPageIndex + 1));
              if (input == null) return;
              const parsed = Number.parseInt(input.trim(), 10);
              if (!Number.isFinite(parsed)) return;
              const clamped = Math.min(Math.max(parsed, 1), routesPageCount);
              setRoutePagination((prev) => ({ ...prev, pageIndex: clamped - 1 }));
            }}
            disabled={routesFilteredCount === 0 || routesPageCount <= 1}
          >
            Page {routesPageIndex + 1} / {routesPageCount}
          </button>
          <button
            className="button"
            onClick={() => setRoutePagination((prev) => ({ ...prev, pageIndex: Math.min(routesPageCount - 1, prev.pageIndex + 1) }))}
            disabled={routesPageIndex >= routesPageCount - 1 || routesFilteredCount === 0}
          >
            Next
          </button>
        </div>
      );
    }

    return null;
  };

  const isDetailFilterActive = detailTab === 'peerings'
    ? peerFilter.trim().length > 0
      || selectedPeerTypes.size !== allPeerTypeOptions.length
      || selectedHealthCheckStatuses.size !== allHealthCheckStatusOptions.length
    : routeFilter.trim().length > 0
      || selectedRouteKinds.size !== allRouteKindOptions.length
      || selectedRouteValidationStates.size !== allRouteValidationOptions.length
      || (allRouteInfoOptions.length > 0 && selectedRouteInfoTypes.size !== allRouteInfoOptions.length);

  const renderDetailFilterContent = () => {
    if (detailTab === 'peerings') {
      return (
        <>
          <div className="filter-popover-first-row">
            <div className="filter-section filter-first-row-section">
              <div className="filter-section-title">Search</div>
              <input
                className="input filter-search-input"
                placeholder="Search peerings"
                value={peerFilter}
                onChange={(event) => setPeerFilter(event.target.value)}
              />
            </div>
            <button
              type="button"
              className="button filter-reset-button filter-reset-docked"
              onClick={resetDetailFilters}
            >
              Reset
            </button>
          </div>
          <div className="filter-section">
            <div className="filter-section-title">Peer Type</div>
            <div className="filter-badge-row">
              <button
                type="button"
                className="filter-badge-button"
                onClick={() => toggleAllDetailSelection(allSelectedPeerTypes, allPeerTypeOptions, setSelectedPeerTypes)}
                style={{ opacity: allSelectedPeerTypes ? 1 : 0.5 }}
              >
                <span className="badge all">All</span>
              </button>
              {allPeerTypeOptions.map((option) => (
                <button
                  key={option}
                  type="button"
                  className="filter-badge-button"
                  onClick={() => {
                    setSelectedPeerTypes((prev) => {
                      const next = new Set(prev);
                      if (next.has(option)) next.delete(option);
                      else next.add(option);
                      return next;
                    });
                  }}
                  style={{ opacity: selectedPeerTypes.has(option) ? 1 : 0.5 }}
                >
                  <span className={`badge ${option === 'Gateway' ? 'info' : 'success'}`}>{option}</span>
                </button>
              ))}
            </div>
          </div>
          <div className="filter-section">
            <div className="filter-section-title">Health Check Status</div>
            <div className="filter-badge-row">
              <button
                type="button"
                className="filter-badge-button"
                onClick={() => toggleAllDetailSelection(allSelectedHealthCheckStatuses, allHealthCheckStatusOptions, setSelectedHealthCheckStatuses)}
                style={{ opacity: allSelectedHealthCheckStatuses ? 1 : 0.5 }}
              >
                <span className="badge all">All</span>
              </button>
              {allHealthCheckStatusOptions.map((option) => {
                const tone = option === 'Up' ? 'success' : option === 'Mixed' ? 'warning' : option === 'Down' ? 'danger' : '';
                return (
                  <button
                    key={option}
                    type="button"
                    className="filter-badge-button"
                    onClick={() => {
                      setSelectedHealthCheckStatuses((prev) => {
                        const next = new Set(prev);
                        if (next.has(option)) next.delete(option);
                        else next.add(option);
                        return next;
                      });
                    }}
                    style={{ opacity: selectedHealthCheckStatuses.has(option) ? 1 : 0.5 }}
                  >
                    <span className={`badge${tone ? ` ${tone}` : ''}`}>{option}</span>
                  </button>
                );
              })}
            </div>
          </div>
        </>
      );
    }

    return (
      <>
        <div className="filter-popover-first-row">
          <div className="filter-section filter-first-row-section">
            <div className="filter-section-title">Search</div>
            <input
              className="input filter-search-input"
              placeholder="Search routes"
              value={routeFilter}
              onChange={(event) => setRouteFilter(event.target.value)}
            />
          </div>
          <button
            type="button"
            className="button filter-reset-button filter-reset-docked"
            onClick={resetDetailFilters}
          >
            Reset
          </button>
        </div>
        <div className="filter-section">
          <div className="filter-section-title">Route Type</div>
          <div className="filter-badge-row">
            <button
              type="button"
              className="filter-badge-button"
              onClick={() => toggleAllDetailSelection(allSelectedRouteKinds, allRouteKindOptions, setSelectedRouteKinds)}
              style={{ opacity: allSelectedRouteKinds ? 1 : 0.5 }}
            >
              <span className="badge all">All</span>
            </button>
            {allRouteKindOptions.map((option) => (
              <button
                key={option}
                type="button"
                className="filter-badge-button"
                onClick={() => {
                  setSelectedRouteKinds((prev) => {
                    const next = new Set(prev);
                    if (next.has(option)) next.delete(option);
                    else next.add(option);
                    return next;
                  });
                }}
                style={{ opacity: selectedRouteKinds.has(option) ? 1 : 0.5 }}
              >
                <span className="badge">{option}</span>
              </button>
            ))}
          </div>
        </div>
        <div className="filter-section">
          <div className="filter-section-title">Route Validation</div>
          <div className="filter-badge-row">
            <button
              type="button"
              className="filter-badge-button"
              onClick={() => toggleAllDetailSelection(allSelectedRouteValidationStates, allRouteValidationOptions, setSelectedRouteValidationStates)}
              style={{ opacity: allSelectedRouteValidationStates ? 1 : 0.5 }}
            >
              <span className="badge all">All</span>
            </button>
            {allRouteValidationOptions.map((option) => {
              const tone = option === 'Matching' ? 'success' : option === 'Mismatch' ? 'warning' : '';
              return (
                <button
                  key={option}
                  type="button"
                  className="filter-badge-button"
                  onClick={() => {
                    setSelectedRouteValidationStates((prev) => {
                      const next = new Set(prev);
                      if (next.has(option)) next.delete(option);
                      else next.add(option);
                      return next;
                    });
                  }}
                  style={{ opacity: selectedRouteValidationStates.has(option) ? 1 : 0.5 }}
                >
                  <span className={`badge${tone ? ` ${tone}` : ''}`}>{option}</span>
                </button>
              );
            })}
          </div>
        </div>
        {allRouteInfoOptions.length > 0 && (
          <div className="filter-section">
            <div className="filter-section-title">Info</div>
            <div className="filter-badge-row">
              <button
                type="button"
                className="filter-badge-button"
                onClick={() => toggleAllDetailSelection(allSelectedRouteInfoTypes, allRouteInfoOptions, setSelectedRouteInfoTypes)}
                style={{ opacity: allSelectedRouteInfoTypes ? 1 : 0.5 }}
              >
                <span className="badge all">All</span>
              </button>
              {allRouteInfoOptions.map((option) => (
                <button
                  key={option}
                  type="button"
                  className="filter-badge-button"
                  onClick={() => {
                    setSelectedRouteInfoTypes((prev) => {
                      const next = new Set(prev);
                      if (next.has(option)) next.delete(option);
                      else next.add(option);
                      return next;
                    });
                  }}
                  style={{ opacity: selectedRouteInfoTypes.has(option) ? 1 : 0.5 }}
                >
                  <span className="badge">{option}</span>
                </button>
              ))}
            </div>
          </div>
        )}
      </>
    );
  };

  if (!node) {
    if (isLoadingDetail) {
      return (
        <div className="modal-backdrop" onClick={onClose}>
          <div className="modal" onClick={(event) => event.stopPropagation()}>
            <div className="modal-header">
              <div className="modal-title">{nodeName}</div>
              <button className="button zoom-action-button" onClick={onClose} aria-label="Close" title="Close">
                <CloseXIcon />
              </button>
            </div>
            <div className="modal-body" style={{ padding: '2rem', textAlign: 'center' }}>
              Loading node details...
            </div>
          </div>
        </div>
      );
    }
    return null;
  }

  return (
    <div className="modal-backdrop modal-node-detail-backdrop" onClick={onClose}>
      <div className="modal modal-node-detail" onClick={(event) => event.stopPropagation()}>
        <div className="modal-header">
          <div className="node-modal-header-main">
            <div className="node-modal-header-title-row">
              <div className="modal-title">{nodeDisplayName}</div>
              <div className="node-modal-header-badges">
                <span className={`badge ${isGateway ? 'info' : 'success'}`}>{isGateway ? 'Gateway' : 'Worker'}</span>
                <span className="badge">{siteOrPool}</span>
                <span className="badge" title={nodeType.title}>{nodeType.label}</span>
                <span className={`badge ${nodeInfo?.k8sReady === 'Ready' ? 'success' : 'danger'}`}>
                  {nodeInfo?.k8sReady || 'NotReady'}
                </span>
                <span className={`badge ${cniStatus.tone}`} title={node ? getCniStatusTooltip(node, pullEnabled) : undefined}>{cniStatus.label}</span>
              </div>
            </div>
          </div>
          <button className="button zoom-action-button" onClick={onClose} aria-label="Close" title="Close">
            <CloseXIcon />
          </button>
        </div>

        <div className="node-detail-grid">
          <div className="node-detail-left">
            <NodeInfoPanel
              node={node}
              nodeAgentBuild={nodeAgentBuild}
              nodeInfo={nodeInfo}
              externalIps={externalIps}
              podCidrs={podCidrs}
              podCidrFirstIps={podCidrFirstIps}
              nodeImage={nodeImage}
              instanceType={instanceType}
              region={region}
              availabilityZone={availabilityZone}
              k8sUpdated={k8sUpdated}
              pushUpdated={pushUpdated}
              providerMeta={providerMeta}
              providerPortalUrl={providerPortalUrl}
              providerName={providerName}
              providerIsVmss={providerIsVmss}
            />
          </div>

          <div className="node-detail-right">
            {dataCondition && (
              <div className={`card node-modal-card data-condition-card ${dataCondition.tone}`}>
                <div className="section-title">{dataCondition.title}</div>
                <div>{dataCondition.message}</div>
              </div>
            )}
            {cniProblemMessages.length > 0 && (
              <div className="card node-modal-card data-condition-card warning">
                <div className="section-title">Warning</div>
                <div>This node reported CNI configuration problems:</div>
                <ul style={{ margin: '8px 0 0', paddingLeft: 20 }}>
                  {cniProblemMessages.map((message, index) => (
                    <li key={`${index}-${message}`}>{message}</li>
                  ))}
                </ul>
              </div>
            )}
            {nodeErrorMessages.length > 0 && (
              <div className="card node-modal-card data-condition-card danger">
                <div className="section-title">Errors</div>
                <ul style={{ margin: '4px 0 0', paddingLeft: 20 }}>
                  {nodeErrorMessages.map((message, index) => (
                    <li key={`err-${index}-${message}`}>{message}</li>
                  ))}
                </ul>
              </div>
            )}
            <div className="card node-modal-card">
              <NodeDetailTabsHeader
                detailTab={detailTab}
                routesValidationSummary={routesValidationSummary}
                bpfEntryCount={node?.bpfEntries?.length ?? 0}
                paginationControls={renderDetailPagination()}
                onDetailTabChange={onDetailTabChange}
              />

              {detailTab === 'peerings' && (
                <>
                  <div className="detail-table-wrapper" ref={peerTableWrapperRef}>
                    <table className="table sticky-table-header modal-sticky-table-header">
                      <thead>
                        {peerTable.getHeaderGroups().map((group) => (
                          <tr key={group.id}>
                            {group.headers.map((header) => (
                              <th
                                key={header.id}
                                className={[
                                  header.column.id === 'expand' ? 'peer-expand-col' : '',
                                  header.column.id === 'destination' ? 'peer-col-destination' : '',
                                  header.column.id === 'endpoint' ? 'peer-col-endpoint' : ''
                                ].filter(Boolean).join(' ') || undefined}
                                onClick={header.column.getToggleSortingHandler()}
                                style={{ cursor: header.column.getCanSort() ? 'pointer' : 'default' }}
                              >
                                {flexRender(header.column.columnDef.header, header.getContext())}
                                {header.column.getCanSort() && (
                                  <span style={{ marginLeft: 6 }}>
                                    {header.column.getIsSorted() === 'asc'
                                      ? '↑'
                                      : header.column.getIsSorted() === 'desc'
                                        ? '↓'
                                        : ''}
                                  </span>
                                )}
                              </th>
                            ))}
                          </tr>
                        ))}
                      </thead>
                      <tbody>
                        {peerTable.getRowModel().rows.map((row) => {
                          const expanded = expandedPeerRows[row.original.id];
                          return (
                            <React.Fragment key={row.id}>
                              <tr
                                onClick={() => togglePeerRowExpanded(row.original.id)}
                                style={{ cursor: 'pointer' }}
                              >
                                {row.getVisibleCells().map((cell) => (
                                  <td
                                    key={cell.id}
                                    className={[
                                      cell.column.id === 'expand' ? 'peer-expand-col' : '',
                                      cell.column.id === 'destination' ? 'peer-col-destination' : '',
                                      cell.column.id === 'endpoint' ? 'peer-col-endpoint' : ''
                                    ].filter(Boolean).join(' ') || undefined}
                                  >
                                    {flexRender(cell.column.columnDef.cell, cell.getContext())}
                                  </td>
                                ))}
                              </tr>
                              {expanded && (
                                <tr>
                                  <td colSpan={peerColumns.length}>
                                    <div className="peer-dropdown-card">
                                      <div className="node-info-grid peer-dropdown-grid">
                                        <div className="node-info-card peer-dropdown-section">
                                          <div className="node-info-label">Link Details</div>
                                          <table className="table peer-dropdown-table">
                                            <tbody>
                                              <tr>
                                                <td>Interface</td>
                                                <td>{row.original.localInterface}</td>
                                              </tr>
                                              <tr>
                                                <td>Endpoint</td>
                                                <td>{row.original.endpoint}</td>
                                              </tr>
                                              <tr>
                                                <td>Pod CIDR Gateways</td>
                                                <td>{row.original.podCidrGateways}</td>
                                              </tr>
                                              <tr>
                                                <td>Health Check Status</td>
                                                <td>{row.original.healthCheckStatus}</td>
                                              </tr>
                                            </tbody>
                                          </table>
                                        </div>
                                        <div className="node-info-card peer-dropdown-section">
                                          <div className="node-info-label">Link Statistics</div>
                                          <table className="table peer-dropdown-table">
                                            <tbody>
                                              <tr>
                                                <td>RX Bytes</td>
                                                <td>{formatBytes(row.original.rxBytes)}</td>
                                              </tr>
                                              <tr>
                                                <td>TX Bytes</td>
                                                <td>{formatBytes(row.original.txBytes)}</td>
                                              </tr>
                                              <tr>
                                                <td>Latest Handshake</td>
                                                <td>{formatTime(row.original.lastHandshake)}</td>
                                              </tr>
                                              <tr>
                                                <td>Health Check Uptime</td>
                                                <td>{row.original.healthCheckUptime}</td>
                                              </tr>
                                              <tr>
                                                <td>Health Check RTT</td>
                                                <td>{row.original.healthCheckRtt}</td>
                                              </tr>
                                            </tbody>
                                          </table>
                                        </div>
                                        <div className="node-info-card peer-dropdown-section">
                                          <div className="node-info-label">Allowed IPs ({row.original.allowedIPs.length})</div>
                                          <table className="table peer-dropdown-table peer-dropdown-table-values">
                                            <tbody>
                                              {row.original.allowedIPs.length > 0
                                                ? row.original.allowedIPs.map((allowedIp, idx) => (
                                                  <tr key={`${row.original.id}-allowed-${allowedIp}-${idx}`}>
                                                    <td>{allowedIp}</td>
                                                  </tr>
                                                ))
                                                : (
                                                  <tr>
                                                    <td>No allowed IPs</td>
                                                  </tr>
                                                )}
                                            </tbody>
                                          </table>
                                        </div>
                                        <div className="node-info-card peer-dropdown-section">
                                          <div className="node-info-label">Routes ({row.original.allowedRoutes.length})</div>
                                          <table className="table peer-dropdown-table peer-dropdown-table-values">
                                            <tbody>
                                              {row.original.allowedRoutes.length > 0
                                                ? row.original.allowedRoutes.map((route, idx) => (
                                                  <tr key={`${row.original.id}-route-${route.route}-${idx}`}>
                                                    <td>{route.route}</td>
                                                    <td>{route.type}</td>
                                                    <td>{route.destination}</td>
                                                  </tr>
                                                ))
                                                : (
                                                  <tr>
                                                    <td colSpan={3}>No matched routes</td>
                                                  </tr>
                                                )}
                                            </tbody>
                                          </table>
                                        </div>
                                      </div>
                                    </div>
                                  </td>
                                </tr>
                              )}
                            </React.Fragment>
                          );
                        })}
                      </tbody>
                    </table>
                  </div>
                </>
              )}

              {detailTab === 'routes' && (
                <>
                  <div className="detail-table-wrapper" ref={routeTableWrapperRef}>
                    <table className="table sticky-table-header modal-sticky-table-header">
                      <thead>
                        <tr>
                          <th onClick={() => toggleRouteSort('destination', setRouteSort)} style={{ cursor: 'pointer' }}>Destination <span style={{ marginLeft: 6 }}>{renderRouteSortArrow(routeSort, 'destination')}</span></th>
                          <th onClick={() => toggleRouteSort('destinationNode', setRouteSort)} style={{ cursor: 'pointer' }}>Destination Node <span style={{ marginLeft: 6 }}>{renderRouteSortArrow(routeSort, 'destinationNode')}</span></th>
                          <th onClick={() => toggleRouteSort('nextHops', setRouteSort)} style={{ cursor: 'pointer' }}>Next Hops <span style={{ marginLeft: 6 }}>{renderRouteSortArrow(routeSort, 'nextHops')}</span></th>
                          <th onClick={() => toggleRouteSort('devices', setRouteSort)} style={{ cursor: 'pointer' }}>Devices <span style={{ marginLeft: 6 }}>{renderRouteSortArrow(routeSort, 'devices')}</span></th>
                          <th onClick={() => toggleRouteSort('mtu', setRouteSort)} style={{ cursor: 'pointer' }}>MTU <span style={{ marginLeft: 6 }}>{renderRouteSortArrow(routeSort, 'mtu')}</span></th>
                          <th onClick={() => toggleRouteSort('info', setRouteSort)} style={{ cursor: 'pointer' }}>Info <span style={{ marginLeft: 6 }}>{renderRouteSortArrow(routeSort, 'info')}</span></th>
                          <th onClick={() => toggleRouteSort('expected', setRouteSort)} style={{ cursor: 'pointer' }}>Expected <span style={{ marginLeft: 6 }}>{renderRouteSortArrow(routeSort, 'expected')}</span></th>
                          <th onClick={() => toggleRouteSort('present', setRouteSort)} style={{ cursor: 'pointer' }}>Present <span style={{ marginLeft: 6 }}>{renderRouteSortArrow(routeSort, 'present')}</span></th>
                        </tr>
                      </thead>
                      <tbody>
                        {pagedRoutes.flatMap((route, idx) => {
                          const nextHops = route.nextHops || [];
                          const kind = getRouteKind(route.destination, nextHops);
                          const isECMP = nextHops.length > 1;
                          const routeFamily = (route as { family?: number }).family;
                          const rowId = `route-${route.destination || idx}-${routeFamily || 'x'}`;
                          const isExpanded = Boolean(expandedRouteEcmpRows[rowId]);
                          const aggregateExpected = getUniformRoutePresenceValue(nextHops, 'expected');
                          const aggregatePresent = getUniformRoutePresenceValue(nextHops, 'present');
                          const parentRow = (
                            <tr
                              key={rowId}
                              className={isECMP ? 'route-ecmp-row' : undefined}
                              onClick={isECMP ? () => toggleRouteEcmpRowExpanded(rowId) : undefined}
                              onKeyDown={isECMP ? (event) => {
                                if (event.key === 'Enter' || event.key === ' ') {
                                  event.preventDefault();
                                  toggleRouteEcmpRowExpanded(rowId);
                                }
                              } : undefined}
                              role={isECMP ? 'button' : undefined}
                              tabIndex={isECMP ? 0 : undefined}
                              aria-expanded={isECMP ? isExpanded : undefined}
                              aria-label={isECMP ? `${isExpanded ? 'Collapse' : 'Expand'} ECMP route ${route.destination || ''}` : undefined}
                            >
                              <td>{route.destination || '-'}</td>
                              {isECMP ? (
                                <td colSpan={3} style={{ paddingLeft: 0 }}>
                                  <button
                                    className="route-ecmp-toggle"
                                    type="button"
                                    onClick={(event) => {
                                      event.stopPropagation();
                                      toggleRouteEcmpRowExpanded(rowId);
                                    }}
                                    aria-label={isExpanded ? 'Collapse ECMP next hops' : 'Expand ECMP next hops'}
                                  >
                                    {isExpanded ? '▾' : '▸'} ECMP ({nextHops.length} next hops)
                                  </button>
                                  {(() => {
                                    const infoHop = nextHops.find((h) => h.info?.objectType && h.info?.objectName);
                                    if (!infoHop?.info) return null;
                                    const label = `${infoHop.info.objectType}: ${infoHop.info.objectName}`;
                                    return <span className="badge secondary" style={{ marginLeft: '8px' }}>{label}</span>;
                                  })()}
                                </td>
                              ) : (
                                <>
                                  <td>{resolveRouteDestinationNodes(route)}</td>
                                  <td>{nextHops[0]?.gateway || '-'}</td>
                                  <td>{nextHops[0]?.device || '-'}</td>
                                </>
                              )}
                              <td>{(() => { const mtu = nextHops.find(h => h.mtu && h.mtu > 0)?.mtu; return mtu ? String(mtu) : '-'; })()}</td>
                              <td>
                                <span title={getRouteInfoTooltip(nextHops[0], isECMP, route.destination || undefined)}>
                                  {getRouteInfoLabel(nextHops[0], isECMP, route.destination || undefined)}
                                </span>
                              </td>
                              <td>{renderRoutePresence(kind, isECMP ? aggregateExpected : nextHops[0]?.expected, isECMP)}</td>
                              <td>{renderRoutePresence(kind, isECMP ? aggregatePresent : nextHops[0]?.present, isECMP)}</td>
                            </tr>
                          );

                          if (!isECMP || !isExpanded) {
                            return [parentRow];
                          }

                          const childRows = nextHops.map((hop, hopIdx) => (
                            <tr key={`${rowId}-hop-${hopIdx}`}>
                              <td style={{ paddingLeft: '24px' }}>{`distance ${routeDistanceLabel(typeof hop.distance === 'number' && hop.distance > 0 ? hop.distance : Number.POSITIVE_INFINITY)}`}</td>
                              <td>{resolveDestinationNodesForHops(route.destination, [hop])}</td>
                              <td>{hop.gateway || '-'}</td>
                              <td>{hop.device || '-'}</td>
                              <td></td>
                              <td>
                                <span title={getRouteInfoTooltip(hop)}>{getRouteInfoLabel(hop)}</span>
                              </td>
                              <td>{renderRoutePresence(kind, hop.expected)}</td>
                              <td>{renderRoutePresence(kind, hop.present)}</td>
                            </tr>
                          ));

                          return [parentRow, ...childRows];
                        })}
                      </tbody>
                    </table>
                  </div>
                </>
              )}

              {detailTab === 'bpf' && (
                <>
                  <div className="detail-table-wrapper">
                    <table className="table sticky-table-header modal-sticky-table-header">
                      <thead>
                        <tr>
                          <th>CIDR</th>
                          <th>Remote</th>
                          <th>Node</th>
                          <th>Interface</th>
                          <th>Protocol</th>
                          <th>VNI</th>
                          <th>MTU</th>
                        </tr>
                      </thead>
                      <tbody>
                        {(node?.bpfEntries ?? [])
                          .slice()
                          .sort((a, b) => (a.cidr ?? '').localeCompare(b.cidr ?? ''))
                          .map((entry, idx) => (
                            <tr key={idx}>
                              <td style={{ fontFamily: 'monospace' }}>{entry.cidr ?? '-'}</td>
                              <td style={{ fontFamily: 'monospace' }}>{entry.remote ?? '-'}</td>
                              <td>{entry.node ?? '-'}</td>
                              <td>{entry.interface ?? '-'}</td>
                              <td>{entry.protocol ?? '-'}</td>
                              <td>{entry.vni ?? 0}</td>
                              <td>{entry.mtu ?? 0}</td>
                            </tr>
                          ))}
                      </tbody>
                    </table>
                  </div>
                </>
              )}
            </div>
          </div>
        </div>

      </div>
    </div>
  );
}


export default NodeDetailModal;

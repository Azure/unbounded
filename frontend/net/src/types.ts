// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

export type ClusterStatus = {
  timestamp?: string;
  nodeCount?: number;
  siteCount?: number;
  azureTenantId?: string;
  nodes?: NodeStatus[];
  sites?: SiteStatus[];
  gatewayPools?: GatewayPoolStatus[];
  peerings?: PeeringStatus[];
  connectivityMatrix?: Record<string, SiteMatrix>;
  buildInfo?: BuildInfo;
  leaderInfo?: LeaderInfo;
  errors?: string[];
  warnings?: string[];
  problems?: StatusProblem[];
  pullEnabled?: boolean;
};

export type StatusProblem = {
  name?: string;
  type?: string;
  errors?: string[];
};

export type BuildInfo = {
  version?: string;
  commit?: string;
  buildTime?: string;
};

export type LeaderInfo = {
  podName?: string;
  nodeName?: string;
};

export type NodeInfo = {
  name?: string;
  siteName?: string;
  isGateway?: boolean;
  k8sReady?: string;
  podCIDRs?: string[];
  internalIPs?: string[];
  externalIPs?: string[];
  providerId?: string;
  osImage?: string;
  kernel?: string;
  kubelet?: string;
  arch?: string;
  nodeOs?: string;
  k8sLabels?: Record<string, string>;
  k8sUpdatedAt?: string;
  buildInfo?: BuildInfo;
  wireGuard?: WireguardStatus;
};

export type WireguardStatus = {
  interface?: string | boolean;
  publicKey?: string;
  peerCount?: number;
};

export type WireguardPeer = {
  tunnel?: {
    protocol?: string;
    interface?: string;
    publicKey?: string;
    endpoint?: string;
    rxBytes?: number;
    txBytes?: number;
    lastHandshake?: string;
    allowedIPs?: string[];
  };
  name?: string;
  peerType?: string;
  siteName?: string;
  healthCheck?: HealthCheckPeerStatus;
  podCidrGateways?: string[];
  routeDistances?: Record<string, number>;
  routeDestinations?: string[];
};

export type RouteType = {
  type?: string;
  attributes?: string[];
};

export type NextHop = {
  gateway?: string;
  device?: string;
  distance?: number;
  weight?: number;
  mtu?: number;
  routeTypes?: RouteType[];
  expected?: boolean;
  present?: boolean;
  peerDestinations?: string[];
  info?: {
    objectName?: string;
    objectType?: string;
    routeType?: string;
  };
};

export type RouteEntry = {
  family?: string;
  destination?: string;
  table?: number;
  nextHops?: NextHop[];
};

export type RoutingTable = {
  routes?: RouteEntry[];
  managedRouteCount?: number;
  pendingRouteCount?: number;
};

export type HealthCheckPeerStatus = {
  enabled?: boolean;
  status?: string;
  uptime?: string;
  rtt?: string;
};

export type HealthCheckStatus = {
  healthy?: boolean;
  summary?: string;
  peerCount?: number;
  checkedAt?: string;
};

export type NodeStatus = {
  nodeInfo?: NodeInfo;
  peers?: WireguardPeer[];
  routingTable?: RoutingTable;
  healthCheck?: HealthCheckStatus;
  nodeErrors?: NodeError[];
  lastPushTime?: string;
  statusSource?: string;
  fetchError?: string;
  bpfEntries?: BpfEntry[];
};

export type BpfEntry = {
  cidr?: string;
  remote?: string;
  node?: string;
  interface?: string;
  protocol?: string;
  vni?: number;
  mtu?: number;
  ifindex?: number;
};

export type NodeError = {
  type?: string;
  message?: string;
};

export type SiteStatus = {
  name?: string;
  nodeCount?: number;
  onlineCount?: number;
  offlineCount?: number;
  nodeCidrs?: string[];
  manageCniPlugin?: boolean;
};

export type GatewayPoolStatus = {
  name?: string;
  siteName?: string;
  nodeCount?: number;
  gateways?: string[];
  nodes?: GatewayPoolNodeStatus[];
  connectedSites?: string[];
  reachableSites?: string[];
};

export type GatewayPoolNodeStatus = {
  name?: string;
  siteName?: string;
  internalIPs?: string[];
  externalIPs?: string[];
  healthEndpoints?: string[];
  wireGuardPublicKey?: string;
  podCIDRs?: string[];
};

export type PeeringStatus = {
  name?: string;
  sites?: string[];
  gatewayPools?: string[];
  healthCheckEnabled?: boolean;
};

export type SiteMatrix = {
  nodes?: string[];
  results?: Record<string, Record<string, string>>;
};

export type ClusterStatusDelta = {
  seq?: number;
  timestamp?: string;
  nodeCount?: number;
  siteCount?: number;
  azureTenantId?: string;
  nodes?: NodeStatus[];
  removedNodes?: string[];
  updatedNodes?: NodeStatus[];
  sites?: SiteStatus[];
  gatewayPools?: GatewayPoolStatus[];
  peerings?: PeeringStatus[];
  connectivityMatrix?: Record<string, SiteMatrix> | null;
  buildInfo?: BuildInfo;
  leaderInfo?: LeaderInfo;
  errors?: string[];
  warnings?: string[];
  problems?: StatusProblem[];
  pullEnabled?: boolean;
};

export type ClusterSummary = {
  seq?: number;
  timestamp?: string;
  nodeCount?: number;
  siteCount?: number;
  azureTenantId?: string;
  leaderInfo?: LeaderInfo;
  buildInfo?: BuildInfo;
  sites?: SiteStatus[];
  gatewayPools?: GatewayPoolStatus[];
  peerings?: PeeringStatus[];
  errors?: string[];
  warnings?: string[];
  problems?: StatusProblem[];
  pullEnabled?: boolean;
  nodeSummaries?: NodeSummary[];
  connectivityMatrix?: Record<string, SiteMatrix>;
};

export type ClusterSummaryDelta = {
  seq?: number;
  timestamp?: string;
  nodeCount?: number;
  siteCount?: number;
  azureTenantId?: string;
  leaderInfo?: LeaderInfo;
  buildInfo?: BuildInfo;
  sites?: SiteStatus[];
  gatewayPools?: GatewayPoolStatus[];
  peerings?: PeeringStatus[];
  errors?: string[];
  warnings?: string[];
  problems?: StatusProblem[];
  pullEnabled?: boolean;
  nodeSummaries?: NodeSummary[];
  removedNodes?: string[];
  connectivityMatrix?: Record<string, SiteMatrix>;
};

export type NodeSummary = {
  name?: string;
  siteName?: string;
  isGateway?: boolean;
  k8sReady?: string;
  statusSource?: string;
  cniStatus?: string;
  cniTone?: string;
  errorCount?: number;
  firstError?: string;
  peerCount?: number;
  healthyPeers?: number;
  routeCount?: number;
  routeMismatch?: boolean;
  fetchError?: string;
};

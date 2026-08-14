export const projectVersion = 1 as const;
export const siteSelectorKey = 'unbounded-cloud.io/site';

export type Position = { x: number; y: number };
export type Size = { width: number; height: number };
export type TunnelProtocol = '' | 'WireGuard' | 'IPIP' | 'GENEVE' | 'VXLAN' | 'None' | 'Auto';
export type OptionalBoolean = boolean | null;

export interface HealthCheckSettings {
  enabled: OptionalBoolean;
  detectMultiplier: number | null;
  receiveInterval: string;
  transmitInterval: string;
}

export interface PodCidrAssignment {
  assignmentEnabled: OptionalBoolean;
  cidrBlocks: string[];
  nodeBlockSizes: { ipv4: number | null; ipv6: number | null };
  nodeRegex: string[];
  priority: number | null;
}

export interface SiteSpec {
  nodeCidrs: string[];
  podCidrAssignments: PodCidrAssignment[];
  manageCniPlugin: OptionalBoolean;
  nonMasqueradeCIDRs: string[];
  localCidrs: string[];
  healthCheckSettings: HealthCheckSettings;
  tunnelProtocol: TunnelProtocol;
  tunnelMTU: number | null;
  components: {
    machina: OptionalBoolean;
    metalman: OptionalBoolean;
    metalmanDHCPAutoInterface: OptionalBoolean;
    metalmanReplicas: number | null;
    storage: OptionalBoolean;
    gantry: OptionalBoolean;
  };
}

export interface GatewayPoolSpec {
  type: '' | 'External' | 'Internal';
  nodeSelector: Record<string, string>;
  routedCidrs: string[];
  healthCheckSettings: HealthCheckSettings;
  tunnelProtocol: TunnelProtocol;
  tunnelMTU: number | null;
}

export interface RelationshipSpec {
  enabled: OptionalBoolean;
  meshNodes: OptionalBoolean;
  healthCheckSettings: HealthCheckSettings;
  tunnelProtocol: TunnelProtocol;
  tunnelMTU: number | null;
}

export interface SiteResource {
  id: string;
  name: string;
  position: Position;
  size: Size;
  spec: SiteSpec;
}

export interface GatewayPoolResource {
  id: string;
  name: string;
  siteId: string;
  position: Position;
  spec: GatewayPoolSpec;
}

export type RelationshipType = 'site-site' | 'site-gateway-pool' | 'gateway-pool-gateway-pool';

export interface Relationship {
  id: string;
  name: string;
  type: RelationshipType;
  source: string;
  target: string;
  spec: RelationshipSpec;
}

export interface NetProject {
  version: typeof projectVersion;
  name: string;
  sites: SiteResource[];
  gatewayPools: GatewayPoolResource[];
  relationships: Relationship[];
}

export const emptyHealthCheck = (): HealthCheckSettings => ({
  enabled: null,
  detectMultiplier: null,
  receiveInterval: '',
  transmitInterval: ''
});

export const defaultSiteSpec = (siteIndex = 0): SiteSpec => ({
  nodeCidrs: [`10.${siteIndex % 256}.0.0/16`],
  podCidrAssignments: [{
    assignmentEnabled: null,
    cidrBlocks: [`172.${16 + Math.floor((siteIndex % 224) / 16)}.${(siteIndex % 16) * 16}.0/20`],
    nodeBlockSizes: { ipv4: 24, ipv6: null },
    nodeRegex: [],
    priority: null
  }],
  manageCniPlugin: null,
  nonMasqueradeCIDRs: [],
  localCidrs: [],
  healthCheckSettings: emptyHealthCheck(),
  tunnelProtocol: 'Auto',
  tunnelMTU: null,
  components: {
    machina: null,
    metalman: null,
    metalmanDHCPAutoInterface: null,
    metalmanReplicas: null,
    storage: null,
    gantry: null
  }
});

export const defaultGatewayPoolSpec = (siteName?: string): GatewayPoolSpec => ({
  type: 'External',
  nodeSelector: {
    ...(siteName ? { [siteSelectorKey]: siteName } : {}),
    'unbounded-cloud.io/unbounded-net-gateway': 'true'
  },
  routedCidrs: [],
  healthCheckSettings: emptyHealthCheck(),
  tunnelProtocol: 'Auto',
  tunnelMTU: null
});

export const defaultRelationshipSpec = (): RelationshipSpec => ({
  enabled: null,
  meshNodes: null,
  healthCheckSettings: emptyHealthCheck(),
  tunnelProtocol: 'Auto',
  tunnelMTU: null
});

export const createProject = (): NetProject => ({
  version: projectVersion,
  name: 'unbounded-network',
  sites: [],
  gatewayPools: [],
  relationships: []
});

export const defaultSiteSize = (): Size => ({ width: 520, height: 350 });

export const createAKSQuickstartProject = (): NetProject => {
  const clusterSpec = defaultSiteSpec();
  clusterSpec.nodeCidrs = ['10.224.0.0/16'];
  clusterSpec.tunnelProtocol = '';
  clusterSpec.podCidrAssignments = [{
    assignmentEnabled: null,
    cidrBlocks: ['10.244.0.0/16'],
    nodeBlockSizes: { ipv4: null, ipv6: null },
    nodeRegex: [],
    priority: null
  }];
  clusterSpec.components = {
    machina: true,
    metalman: false,
    metalmanDHCPAutoInterface: null,
    metalmanReplicas: null,
    storage: false,
    gantry: null
  };

  const remoteSpec = defaultSiteSpec(1);
  remoteSpec.nodeCidrs = ['192.168.1.0/24'];
  remoteSpec.tunnelProtocol = '';
  remoteSpec.podCidrAssignments = [{
    assignmentEnabled: null,
    cidrBlocks: ['10.245.0.0/16'],
    nodeBlockSizes: { ipv4: null, ipv6: null },
    nodeRegex: [],
    priority: null
  }];
  remoteSpec.components = {
    machina: false,
    metalman: false,
    metalmanDHCPAutoInterface: null,
    metalmanReplicas: null,
    storage: false,
    gantry: null
  };

  const gatewayPoolSpec = defaultGatewayPoolSpec('cluster');
  gatewayPoolSpec.tunnelProtocol = '';
  const associationSpec = defaultRelationshipSpec();
  associationSpec.tunnelProtocol = '';

  return {
    version: projectVersion,
    name: 'aks-quickstart',
    sites: [
      {
        id: 'site-cluster',
        name: 'cluster',
        position: { x: 80, y: 80 },
        size: defaultSiteSize(),
        spec: clusterSpec
      },
      {
        id: 'site-remote',
        name: 'remote',
        position: { x: 720, y: 80 },
        size: defaultSiteSize(),
        spec: remoteSpec
      }
    ],
    gatewayPools: [{
      id: 'gateway-pool-main',
      name: 'gw-main',
      siteId: 'site-cluster',
      position: { x: 130, y: 160 },
      spec: gatewayPoolSpec
    }],
    relationships: [
      {
        id: 'association-cluster',
        name: 'cluster',
        type: 'site-gateway-pool',
        source: 'site-cluster',
        target: 'gateway-pool-main',
        spec: structuredClone(associationSpec)
      },
      {
        id: 'association-remote',
        name: 'remote',
        type: 'site-gateway-pool',
        source: 'site-remote',
        target: 'gateway-pool-main',
        spec: structuredClone(associationSpec)
      }
    ]
  };
};

export function createId(prefix: string): string {
  return `${prefix}-${crypto.randomUUID()}`;
}

export function parseList(value: string): string[] {
  return value.split(/[\n,]/).map((item) => item.trim()).filter(Boolean);
}

export function formatList(value: string[]): string {
  return value.join('\n');
}

export function relationshipType(
  sourceId: string,
  targetId: string,
  project: NetProject
): RelationshipType | null {
  const sourceSite = project.sites.some((site) => site.id === sourceId);
  const targetSite = project.sites.some((site) => site.id === targetId);
  const sourcePool = project.gatewayPools.some((pool) => pool.id === sourceId);
  const targetPool = project.gatewayPools.some((pool) => pool.id === targetId);
  if (sourceSite && targetSite) return 'site-site';
  if ((sourceSite && targetPool) || (sourcePool && targetSite)) return 'site-gateway-pool';
  if (sourcePool && targetPool) return 'gateway-pool-gateway-pool';
  return null;
}

export function canCreateRelationship(
  sourceId: string,
  targetId: string,
  project: NetProject
): boolean {
  if (sourceId === targetId) return false;
  const type = relationshipType(sourceId, targetId, project);
  if (!type) return false;
  const endpoints = [sourceId, targetId].sort().join(':');
  return !project.relationships.some((relationship) =>
    relationship.type === type &&
    [relationship.source, relationship.target].sort().join(':') === endpoints
  );
}

export function nextGatewayPoolName(siteName: string, gatewayPools: GatewayPoolResource[]): string {
  const base = `gw-${siteName}`;
  const names = new Set(gatewayPools.map((pool) => pool.name));
  if (!names.has(base)) return base;
  let index = 2;
  while (names.has(`${base}${index}`)) index++;
  return `${base}${index}`;
}

export function relationshipDefaultName(
  type: RelationshipType,
  sourceId: string,
  targetId: string,
  project: NetProject
): string {
  const names = new Map([
    ...project.sites.map((item) => [item.id, item.name] as const),
    ...project.gatewayPools.map((item) => [item.id, item.name] as const)
  ]);
  const suffix = type === 'site-site' ? 'peering' : type === 'site-gateway-pool' ? 'assignment' : 'pool-peering';
  return `${names.get(sourceId) ?? 'source'}-${names.get(targetId) ?? 'target'}-${suffix}`
    .toLowerCase().replace(/[^a-z0-9.-]+/g, '-').slice(0, 63).replace(/[-.]$/, '');
}

export function parseProject(text: string): NetProject {
  const value: unknown = JSON.parse(text);
  if (!isRecord(value) || value.version !== projectVersion || typeof value.name !== 'string' ||
      !Array.isArray(value.sites) || !Array.isArray(value.gatewayPools) ||
      !Array.isArray(value.relationships)) {
    throw new Error('Unsupported or invalid Architecture Tool project JSON.');
  }
  try {
    return {
      version: projectVersion,
      name: value.name,
      sites: value.sites.map((item) => {
        if (!isRecord(item) || typeof item.id !== 'string' || typeof item.name !== 'string' ||
            !isPosition(item.position) || !isRecord(item.spec)) throw new Error('site');
        const defaults = defaultSiteSpec();
        const components = isRecord(item.spec.components) ? item.spec.components : {};
        const assignments = Array.isArray(item.spec.podCidrAssignments)
          ? item.spec.podCidrAssignments : defaults.podCidrAssignments;
        const healthCheckSettings = parseHealthCheck(
          item.spec.healthCheckSettings,
          defaults.healthCheckSettings
        );
        return {
          id: item.id,
          name: item.name,
          position: item.position,
          size: isSize(item.size) ? item.size : defaultSiteSize(),
          spec: {
            ...defaults,
            nodeCidrs: stringArray(item.spec.nodeCidrs, 'site node CIDRs'),
            manageCniPlugin: optionalBoolean(item.spec.manageCniPlugin, 'manage CNI plugin'),
            nonMasqueradeCIDRs: stringArray(item.spec.nonMasqueradeCIDRs, 'non-masquerade CIDRs'),
            localCidrs: stringArray(item.spec.localCidrs, 'local CIDRs'),
            healthCheckSettings,
            tunnelProtocol: tunnelProtocol(item.spec.tunnelProtocol),
            tunnelMTU: optionalNumber(item.spec.tunnelMTU, 'site tunnel MTU'),
            components: {
              machina: optionalBoolean(components.machina, 'machina enabled'),
              metalman: optionalBoolean(components.metalman, 'metalman enabled'),
              metalmanDHCPAutoInterface: optionalBoolean(
                components.metalmanDHCPAutoInterface,
                'metalman DHCP auto interface'
              ),
              metalmanReplicas: optionalNumber(components.metalmanReplicas, 'metalman replicas'),
              storage: optionalBoolean(components.storage, 'storage enabled'),
              gantry: optionalBoolean(components.gantry, 'gantry enabled')
            },
            podCidrAssignments: assignments.map((assignment) => {
              if (!isRecord(assignment)) throw new Error('assignment');
              return {
                assignmentEnabled: optionalBoolean(
                  assignment.assignmentEnabled,
                  'pod assignment enabled'
                ),
                cidrBlocks: stringArray(assignment.cidrBlocks, 'pod CIDR blocks'),
                nodeBlockSizes: {
                  ipv4: optionalNumber(
                    isRecord(assignment.nodeBlockSizes) ? assignment.nodeBlockSizes.ipv4 : null,
                    'IPv4 node block size'
                  ),
                  ipv6: optionalNumber(
                    isRecord(assignment.nodeBlockSizes) ? assignment.nodeBlockSizes.ipv6 : null,
                    'IPv6 node block size'
                  )
                },
                nodeRegex: stringArray(assignment.nodeRegex, 'node regex'),
                priority: optionalNumber(assignment.priority, 'pod assignment priority')
              };
            })
          }
        };
      }),
      gatewayPools: value.gatewayPools.map((item) => {
        if (!isRecord(item) || typeof item.id !== 'string' || typeof item.name !== 'string' ||
            typeof item.siteId !== 'string' || !isPosition(item.position) || !isRecord(item.spec)) {
          throw new Error('gateway pool');
        }
        const defaults = defaultGatewayPoolSpec();
        return {
          id: item.id,
          name: item.name,
          siteId: item.siteId,
          position: item.position,
          spec: {
            type: gatewayPoolType(item.spec.type),
            nodeSelector: stringRecord(item.spec.nodeSelector, 'gateway pool node selector'),
            routedCidrs: stringArray(item.spec.routedCidrs, 'gateway pool routed CIDRs'),
            healthCheckSettings: parseHealthCheck(
              item.spec.healthCheckSettings,
              defaults.healthCheckSettings
            ),
            tunnelProtocol: tunnelProtocol(item.spec.tunnelProtocol),
            tunnelMTU: optionalNumber(item.spec.tunnelMTU, 'gateway pool tunnel MTU')
          }
        };
      }),
      relationships: value.relationships.map((item) => {
        if (!isRecord(item) || typeof item.id !== 'string' || typeof item.name !== 'string' ||
            typeof item.source !== 'string' || typeof item.target !== 'string' ||
            !isRecord(item.spec)) throw new Error('relationship');
        const defaults = defaultRelationshipSpec();
        return {
          id: item.id,
          name: item.name,
          type: relationshipKind(item.type),
          source: item.source,
          target: item.target,
          spec: {
            enabled: optionalBoolean(item.spec.enabled, 'relationship enabled'),
            meshNodes: optionalBoolean(item.spec.meshNodes, 'mesh nodes'),
            healthCheckSettings: parseHealthCheck(
              item.spec.healthCheckSettings,
              defaults.healthCheckSettings
            ),
            tunnelProtocol: tunnelProtocol(item.spec.tunnelProtocol),
            tunnelMTU: optionalNumber(item.spec.tunnelMTU, 'relationship tunnel MTU')
          }
        };
      })
    };
  } catch {
    throw new Error('Unsupported or invalid Architecture Tool project JSON.');
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isPosition(value: unknown): value is Position {
  return isRecord(value) && typeof value.x === 'number' && typeof value.y === 'number';
}

function isSize(value: unknown): value is Size {
  return isRecord(value) && typeof value.width === 'number' && value.width > 0 &&
    typeof value.height === 'number' && value.height > 0;
}

function stringArray(value: unknown, field: string): string[] {
  if (!Array.isArray(value) || !value.every((item) => typeof item === 'string')) {
    throw new Error(field);
  }
  return value;
}

function stringRecord(value: unknown, field: string): Record<string, string> {
  if (!isRecord(value) || !Object.values(value).every((item) => typeof item === 'string')) {
    throw new Error(field);
  }
  return value as Record<string, string>;
}

function optionalBoolean(value: unknown, field: string): OptionalBoolean {
  if (value === null || typeof value === 'boolean') return value;
  throw new Error(field);
}

function optionalNumber(value: unknown, field: string): number | null {
  if (value === null || (typeof value === 'number' && Number.isFinite(value))) return value;
  throw new Error(field);
}

function tunnelProtocol(value: unknown): TunnelProtocol {
  if (typeof value === 'string' &&
      ['', 'WireGuard', 'IPIP', 'GENEVE', 'VXLAN', 'None', 'Auto'].includes(value)) {
    return value as TunnelProtocol;
  }
  throw new Error('tunnel protocol');
}

function gatewayPoolType(value: unknown): GatewayPoolSpec['type'] {
  if (value === '' || value === 'External' || value === 'Internal') return value;
  throw new Error('gateway pool type');
}

function relationshipKind(value: unknown): RelationshipType {
  if (value === 'site-site' || value === 'site-gateway-pool' ||
      value === 'gateway-pool-gateway-pool') {
    return value;
  }
  throw new Error('relationship type');
}

function parseHealthCheck(value: unknown, defaults: HealthCheckSettings): HealthCheckSettings {
  if (!isRecord(value)) throw new Error('health check settings');
  return {
    enabled: optionalBoolean(value.enabled, 'health check enabled'),
    detectMultiplier: optionalNumber(value.detectMultiplier, 'health check detect multiplier'),
    receiveInterval: typeof value.receiveInterval === 'string'
      ? value.receiveInterval : defaults.receiveInterval,
    transmitInterval: typeof value.transmitInterval === 'string'
      ? value.transmitInterval : defaults.transmitInterval
  };
}

import JSZip from 'jszip';
import { stringify } from 'yaml';
import type {
  GatewayPoolResource,
  HealthCheckSettings,
  NetProject,
  Relationship,
  SiteResource
} from './model';

const fixedDate = new Date('2000-01-01T00:00:00.000Z');
const comparePaths = (left: GeneratedFile, right: GeneratedFile): number =>
  left.path < right.path ? -1 : left.path > right.path ? 1 : 0;
const compareNames = (
  left: { name: string; id: string },
  right: { name: string; id: string }
): number => left.name < right.name ? -1
  : left.name > right.name ? 1
    : left.id < right.id ? -1 : left.id > right.id ? 1 : 0;

function compact<T extends Record<string, unknown>>(value: T): T {
  return Object.fromEntries(Object.entries(value).filter(([, item]) =>
    item !== null && item !== '' && item !== undefined &&
    (!Array.isArray(item) || item.length > 0) &&
    (typeof item !== 'object' || Array.isArray(item) || Object.keys(item as object).length > 0)
  )) as T;
}

function health(settings: HealthCheckSettings): Record<string, unknown> | undefined {
  const result = compact({
    enabled: settings.enabled,
    detectMultiplier: settings.detectMultiplier,
    receiveInterval: settings.receiveInterval,
    transmitInterval: settings.transmitInterval
  });
  return Object.keys(result).length > 0 ? result : undefined;
}

function document(apiVersion: string, kind: string, name: string, spec: object): object {
  return { apiVersion, kind, metadata: { name }, spec };
}

function siteDocument(site: SiteResource): object {
  const components = compact({
    machina: site.spec.components.machina === null ? undefined : { enabled: site.spec.components.machina },
    metalman: site.spec.components.metalman === null &&
      site.spec.components.metalmanDHCPAutoInterface === null &&
      site.spec.components.metalmanReplicas === null ? undefined : compact({
        enabled: site.spec.components.metalman,
        dhcpAutoInterface: site.spec.components.metalmanDHCPAutoInterface,
        replicas: site.spec.components.metalmanReplicas
      }),
    storage: site.spec.components.storage === null ? undefined : { enabled: site.spec.components.storage },
    gantry: site.spec.components.gantry === null ? undefined : { enabled: site.spec.components.gantry }
  });
  return {
    apiVersion: 'unbounded-cloud.io/v1alpha3',
    kind: 'Site',
    metadata: {
      name: site.name,
      labels: { 'unbounded-cloud.io/site': site.name }
    },
    spec: compact({
      nodeCidrs: site.spec.nodeCidrs,
      podCidrAssignments: site.spec.podCidrAssignments.map((assignment) => compact({
        assignmentEnabled: assignment.assignmentEnabled,
        cidrBlocks: assignment.cidrBlocks,
        nodeBlockSizes: compact({
          ipv4: assignment.nodeBlockSizes.ipv4,
          ipv6: assignment.nodeBlockSizes.ipv6
        }),
        nodeRegex: assignment.nodeRegex,
        priority: assignment.priority
      })),
      manageCniPlugin: site.spec.manageCniPlugin,
      nonMasqueradeCIDRs: site.spec.nonMasqueradeCIDRs,
      localCidrs: site.spec.localCidrs,
      healthCheckSettings: health(site.spec.healthCheckSettings),
      tunnelProtocol: site.spec.tunnelProtocol,
      tunnelMTU: site.spec.tunnelMTU,
      components
    })
  };
}

function gatewayPoolDocument(pool: GatewayPoolResource): object {
  return document('net.unbounded-cloud.io/v1alpha1', 'GatewayPool', pool.name, compact({
    type: pool.spec.type,
    nodeSelector: pool.spec.nodeSelector,
    routedCidrs: pool.spec.routedCidrs,
    healthCheckSettings: health(pool.spec.healthCheckSettings),
    tunnelProtocol: pool.spec.tunnelProtocol,
    tunnelMTU: pool.spec.tunnelMTU
  }));
}

function relationshipDocument(relationship: Relationship, project: NetProject): object {
  const names = new Map([
    ...project.sites.map((resource) => [resource.id, resource.name] as const),
    ...project.gatewayPools.map((resource) => [resource.id, resource.name] as const)
  ]);
  const base = compact({
    enabled: relationship.spec.enabled,
    healthCheckSettings: health(relationship.spec.healthCheckSettings),
    tunnelProtocol: relationship.spec.tunnelProtocol,
    tunnelMTU: relationship.spec.tunnelMTU
  });
  if (relationship.type === 'site-site') {
    return document('net.unbounded-cloud.io/v1alpha1', 'SitePeering', relationship.name, {
      ...base,
      sites: [names.get(relationship.source), names.get(relationship.target)],
      ...(relationship.spec.meshNodes === null ? {} : { meshNodes: relationship.spec.meshNodes })
    });
  }
  if (relationship.type === 'gateway-pool-gateway-pool') {
    return document('net.unbounded-cloud.io/v1alpha1', 'GatewayPoolPeering', relationship.name, {
      ...base,
      gatewayPools: [names.get(relationship.source), names.get(relationship.target)]
    });
  }
  const siteId = project.sites.some((site) => site.id === relationship.source)
    ? relationship.source : relationship.target;
  const poolId = siteId === relationship.source ? relationship.target : relationship.source;
  return document('net.unbounded-cloud.io/v1alpha1', 'SiteGatewayPoolAssignment', relationship.name, {
    ...base,
    sites: [names.get(siteId)],
    gatewayPools: [names.get(poolId)]
  });
}

function yaml(value: object): string {
  return stringify(value, { lineWidth: 0, sortMapEntries: false }).trimEnd() + '\n';
}

export interface GeneratedFile {
  path: string;
  contents: string;
}

export interface GeneratedManifestDocument {
  resourceId: string;
  contents: string;
}

interface GeneratedResourceFile extends GeneratedFile {
  resourceId: string;
}

function generateResourceFiles(project: NetProject): GeneratedResourceFile[] {
  const files = new Map<string, GeneratedResourceFile>([
    ...project.sites.map((site) => ({
      resourceId: site.id,
      path: `resources/site-${site.name}.yaml`,
      contents: yaml(siteDocument(site))
    })).map((file) => [file.resourceId, file] as const),
    ...project.gatewayPools.map((pool) => ({
      resourceId: pool.id,
      path: `resources/gatewaypool-${pool.name}.yaml`,
      contents: yaml(gatewayPoolDocument(pool))
    })).map((file) => [file.resourceId, file] as const),
    ...project.relationships.map((relationship) => ({
      resourceId: relationship.id,
      path: `resources/${relationship.type}-${relationship.name}.yaml`,
      contents: yaml(relationshipDocument(relationship, project))
    })).map((file) => [file.resourceId, file] as const)
  ]);
  const ordered: GeneratedResourceFile[] = [];
  const emitted = new Set<string>();
  let pendingRelationships = [...project.relationships].sort(compareNames);
  const emit = (resourceId: string): void => {
    const file = files.get(resourceId);
    if (!file || emitted.has(resourceId)) return;
    ordered.push(file);
    emitted.add(resourceId);
  };
  const emitReadyRelationships = (): void => {
    const waiting: Relationship[] = [];
    pendingRelationships.forEach((relationship) => {
      if (emitted.has(relationship.source) && emitted.has(relationship.target)) {
        emit(relationship.id);
      } else {
        waiting.push(relationship);
      }
    });
    pendingRelationships = waiting;
  };

  const sites = [...project.sites].sort(compareNames);
  const knownSiteIds = new Set(sites.map((site) => site.id));
  sites.forEach((site) => {
    emit(site.id);
    [...project.gatewayPools]
      .filter((pool) => pool.siteId === site.id)
      .sort(compareNames)
      .forEach((pool) => {
        emit(pool.id);
        emitReadyRelationships();
      });
    emitReadyRelationships();
  });
  [...project.gatewayPools]
    .filter((pool) => !knownSiteIds.has(pool.siteId))
    .sort(compareNames)
    .forEach((pool) => {
      emit(pool.id);
      emitReadyRelationships();
    });
  pendingRelationships.forEach((relationship) => emit(relationship.id));

  return ordered;
}

export function generateManifestDocuments(project: NetProject): GeneratedManifestDocument[] {
  return generateResourceFiles(project).map(({ resourceId, contents }) => ({
    resourceId,
    contents
  }));
}

export function generateManifest(project: NetProject): string {
  return generateManifestDocuments(project)
    .map((document) => document.contents.trimEnd())
    .join('\n---\n') + '\n';
}

export function generateFiles(project: NetProject): GeneratedFile[] {
  const resources = generateResourceFiles(project);
  const kustomization = yaml({
    apiVersion: 'kustomize.config.k8s.io/v1beta1',
    kind: 'Kustomization',
    resources: resources.map((file) => file.path)
  });
  const readme = [
    `Unbounded Architecture Tool project: ${project.name}`,
    '',
    'Review the generated resources, then apply them with:',
    '',
    '  kubectl apply -k .',
    '',
    'This archive was generated entirely in your browser.',
    ''
  ].join('\n');
  return [
    { path: 'README.txt', contents: readme },
    { path: 'kustomization.yaml', contents: kustomization },
    { path: 'project.json', contents: JSON.stringify(project, null, 2) + '\n' },
    ...resources.map(({ path, contents }) => ({ path, contents }))
  ].sort(comparePaths);
}

export async function generateZip(project: NetProject): Promise<Uint8Array> {
  const zip = new JSZip();
  generateFiles(project).forEach((file) => {
    zip.file(file.path, file.contents, { date: fixedDate, createFolders: false });
  });
  return zip.generateAsync({
    type: 'uint8array',
    compression: 'STORE',
    platform: 'DOS'
  });
}

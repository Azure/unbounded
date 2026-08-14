import {
  BaseEdge,
  ConnectionMode,
  ControlButton,
  Controls,
  EdgeLabelRenderer,
  Handle,
  MiniMap,
  NodeResizer,
  Position as HandlePosition,
  ReactFlow,
  ReactFlowProvider,
  getStraightPath,
  type Connection,
  type Edge,
  type EdgeProps,
  type EdgeTypes,
  type FitViewOptions,
  type Node,
  type NodeChange,
  type NodeProps,
  type ReactFlowInstance
} from '@xyflow/react';
import { useCallback, useEffect, useMemo, useRef, useState, type ReactElement } from 'react';
import {
  canCreateRelationship,
  createId,
  createAKSQuickstartProject,
  createProject,
  defaultSiteSize,
  defaultGatewayPoolSpec,
  defaultRelationshipSpec,
  defaultSiteSpec,
  formatList,
  nextGatewayPoolName,
  parseList,
  parseProject,
  relationshipDefaultName,
  relationshipType,
  siteSelectorKey,
  type GatewayPoolResource,
  type HealthCheckSettings,
  type NetProject,
  type OptionalBoolean,
  type Relationship,
  type SiteResource,
  type TunnelProtocol
} from './model';
import { generateManifest, generateManifestDocuments } from './generation';
import { validateProject } from './validation';

const storageKey = 'unbounded-net-quickstart-v1';
const poolWidth = 260;
const poolHeight = 110;
const snapGridSize = 10;
const protocols: TunnelProtocol[] = ['', 'Auto', 'WireGuard', 'IPIP', 'GENEVE', 'VXLAN', 'None'];

type ResourceNodeData = {
  label: string;
  subtitle: string;
  details: string[];
};
type Selection = { kind: 'site' | 'pool' | 'relationship'; id: string } | null;
type CanvasBounds = { x: number; y: number; width: number; height: number };
type ContextMenuState = {
  x: number;
  y: number;
  target: Selection;
  flowPosition: { x: number; y: number };
};

const externalHandles = [
  { id: 'external-top', position: HandlePosition.Top },
  { id: 'external-right', position: HandlePosition.Right },
  { id: 'external-bottom', position: HandlePosition.Bottom },
  { id: 'external-left', position: HandlePosition.Left }
] as const;

function relationshipLabel(type: Relationship['type']): string {
  switch (type) {
    case 'site-site':
      return 'SitePeering';
    case 'site-gateway-pool':
      return 'SiteGatewayPoolAssociation';
    case 'gateway-pool-gateway-pool':
      return 'GatewayPoolPeering';
  }
}

function resourceBounds(id: string, project: NetProject): CanvasBounds | null {
    const site = project.sites.find((item) => item.id === id);
    if (site) {
      return { ...site.position, ...site.size };
    }
    const pool = project.gatewayPools.find((item) => item.id === id);
    if (!pool) return null;
    const parent = project.sites.find((item) => item.id === pool.siteId);
    return {
      x: (parent?.position.x ?? 0) + pool.position.x,
      y: (parent?.position.y ?? 0) + pool.position.y,
      width: poolWidth,
      height: poolHeight
    };
}

function closestExternalHandle(bounds: CanvasBounds, other: CanvasBounds): string {
    const dx = other.x + other.width / 2 - (bounds.x + bounds.width / 2);
    const dy = other.y + other.height / 2 - (bounds.y + bounds.height / 2);
    if (Math.abs(dx) >= Math.abs(dy)) return dx >= 0 ? 'external-right' : 'external-left';
    return dy >= 0 ? 'external-bottom' : 'external-top';
}

function relationshipHandles(
  relationship: Relationship,
  project: NetProject
): { sourceHandle?: string; targetHandle?: string } {
    const sourceBounds = resourceBounds(relationship.source, project);
    const targetBounds = resourceBounds(relationship.target, project);
    if (!sourceBounds || !targetBounds) return {};

    const sourceSite = project.sites.find((site) => site.id === relationship.source);
    const targetSite = project.sites.find((site) => site.id === relationship.target);
    const sourcePool = project.gatewayPools.find((pool) => pool.id === relationship.source);
    const targetPool = project.gatewayPools.find((pool) => pool.id === relationship.target);

    return {
      sourceHandle: sourceSite && targetPool?.siteId === sourceSite.id
        ? 'internal' : closestExternalHandle(sourceBounds, targetBounds),
      targetHandle: targetSite && sourcePool?.siteId === targetSite.id
        ? 'internal' : closestExternalHandle(targetBounds, sourceBounds)
    };
}

function ResourceNode({ data, selected }: NodeProps<Node<ResourceNodeData>>): ReactElement {
  return (
    <div className={`resource-node ${selected ? 'selected' : ''}`}>
      {externalHandles.map((handle) => <Handle key={handle.id} id={handle.id}
        type="source" position={handle.position} />)}
      <div className="resource-node-title">
        <strong>{data.label}</strong>
        <span>{data.subtitle}</span>
      </div>
      <div className="resource-node-body">
        {data.details.map((detail) => <code key={detail}>{detail}</code>)}
      </div>
    </div>
  );
}

function SiteNode({ data, selected }: NodeProps<Node<ResourceNodeData>>): ReactElement {
  return (
    <div className={`site-node ${selected ? 'selected' : ''}`}>
      <NodeResizer isVisible={selected} minWidth={420} minHeight={260} />
      {externalHandles.filter((handle) =>
        handle.position === HandlePosition.Top || handle.position === HandlePosition.Bottom
      ).map((handle) => <Handle key={handle.id} id={handle.id}
        type="source" position={handle.position} />)}
      <div className="site-node-title">
        <strong>{data.label}</strong>
        <span>{data.subtitle}</span>
        {data.details.map((detail) => <code key={detail}>{detail}</code>)}
        <Handle id="internal" type="source" position={HandlePosition.Bottom}
          className="site-internal-handle" />
      </div>
      <div className="site-node-body">
        <Handle id="external-left" type="source" position={HandlePosition.Left} />
        <Handle id="external-right" type="source" position={HandlePosition.Right} />
        <div className="site-node-drop-hint">Drop GatewayPools inside this site</div>
      </div>
    </div>
  );
}

function RelationshipEdge(props: EdgeProps): ReactElement {
  const {
    id, sourceX, sourceY, targetX, targetY, label, style,
    markerStart, markerEnd, interactionWidth, selected
  } = props;
  const [hovered, setHovered] = useState(false);
  const [path, labelX, labelY] = getStraightPath({
    sourceX,
    sourceY,
    targetX,
    targetY
  });
  const lineLength = Math.hypot(targetX - sourceX, targetY - sourceY) || 1;
  let labelOffsetX = -(targetY - sourceY) / lineLength * 14;
  let labelOffsetY = (targetX - sourceX) / lineLength * 14;
  if (labelOffsetY > 0) {
    labelOffsetX *= -1;
    labelOffsetY *= -1;
  }

  return (
    <>
      <g onMouseEnter={() => setHovered(true)} onMouseLeave={() => setHovered(false)}>
        <BaseEdge id={id} path={path} style={selected ? {
          ...style,
          strokeWidth: 5,
          filter: 'drop-shadow(0 0 4px rgba(255, 255, 255, 0.9))'
        } : style} markerStart={markerStart}
          markerEnd={markerEnd} interactionWidth={interactionWidth} />
      </g>
      {label && <EdgeLabelRenderer>
        <div className={`relationship-label ${hovered ? 'visible' : ''}`} style={{
          transform: `translate(-50%, -50%) translate(` +
            `${labelX + labelOffsetX}px, ${labelY + labelOffsetY}px)`
        }}>{label}</div>
      </EdgeLabelRenderer>}
    </>
  );
}

const nodeTypes = { site: SiteNode, pool: ResourceNode };
const edgeTypes: EdgeTypes = { relationship: RelationshipEdge };

function withSiteSelectors(project: NetProject): NetProject {
  return {
    ...project,
    gatewayPools: project.gatewayPools.map((pool) => {
      const site = project.sites.find((item) => item.id === pool.siteId);
      if (!site || pool.spec.nodeSelector[siteSelectorKey] === site.name) return pool;
      return {
        ...pool,
        spec: {
          ...pool.spec,
          nodeSelector: {
            [siteSelectorKey]: site.name,
            ...Object.fromEntries(
              Object.entries(pool.spec.nodeSelector).filter(([key]) => key !== siteSelectorKey)
            )
          }
        }
      };
    })
  };
}

function loadProject(): NetProject {
  const saved = localStorage.getItem(storageKey);
  if (!saved) return createAKSQuickstartProject();
  try {
    return withSiteSelectors(parseProject(saved));
  } catch {
    return createProject();
  }
}

function OptionalBooleanField(props: {
  label: string;
  value: OptionalBoolean;
  onChange: (value: OptionalBoolean) => void;
}): ReactElement {
  const choices: { label: string; value: OptionalBoolean }[] = [
    { label: 'Default', value: null },
    { label: 'True', value: true },
    { label: 'False', value: false }
  ];
  return (
    <div className="optional-boolean-field">
      <span>{props.label}</span>
      <div className="segmented-control" role="group" aria-label={props.label}>
        {choices.map((choice) => <button key={choice.label} type="button"
          className={props.value === choice.value ? 'selected' : ''}
          aria-pressed={props.value === choice.value}
          onClick={() => props.onChange(choice.value)}>{choice.label}</button>)}
      </div>
    </div>
  );
}

function NumberField(props: {
  label: string;
  value: number | null;
  onChange: (value: number | null) => void;
}): ReactElement {
  return (
    <label>
      <span>{props.label}</span>
      <input type="number" value={props.value ?? ''} onChange={(event) =>
        props.onChange(event.target.value === '' ? null : Number(event.target.value))} />
    </label>
  );
}

function ListField(props: {
  label: string;
  value: string[];
  onChange: (value: string[]) => void;
  placeholder?: string;
}): ReactElement {
  return (
    <label>
      <span>{props.label}</span>
      <textarea value={formatList(props.value)} placeholder={props.placeholder}
        onChange={(event) => props.onChange(parseList(event.target.value))} />
    </label>
  );
}

function HealthFields(props: {
  value: HealthCheckSettings;
  onChange: (value: HealthCheckSettings) => void;
}): ReactElement {
  const update = <K extends keyof HealthCheckSettings>(key: K, value: HealthCheckSettings[K]) =>
    props.onChange({ ...props.value, [key]: value });
  return (
    <fieldset>
      <legend>Health checks</legend>
      <OptionalBooleanField label="Enabled" value={props.value.enabled}
        onChange={(value) => update('enabled', value)} />
      <NumberField label="Detect multiplier" value={props.value.detectMultiplier}
        onChange={(value) => update('detectMultiplier', value)} />
      <label><span>Receive interval</span><input value={props.value.receiveInterval}
        placeholder="300ms" onChange={(event) => update('receiveInterval', event.target.value)} /></label>
      <label><span>Transmit interval</span><input value={props.value.transmitInterval}
        placeholder="300ms" onChange={(event) => update('transmitInterval', event.target.value)} /></label>
    </fieldset>
  );
}

function TunnelFields(props: {
  protocol: TunnelProtocol;
  mtu: number | null;
  onProtocol: (value: TunnelProtocol) => void;
  onMTU: (value: number | null) => void;
}): ReactElement {
  return (
    <>
      <label><span>Tunnel protocol</span><select value={props.protocol}
        onChange={(event) => props.onProtocol(event.target.value as TunnelProtocol)}>
        {protocols.map((protocol) => <option key={protocol || 'default'} value={protocol}>
          {protocol || 'Default'}
        </option>)}
      </select></label>
      <NumberField label="Tunnel MTU" value={props.mtu} onChange={props.onMTU} />
    </>
  );
}

function SiteEditor(props: {
  site: SiteResource;
  update: (site: SiteResource) => void;
}): ReactElement {
  const { site, update } = props;
  const spec = site.spec;
  const updateSpec = <K extends keyof typeof spec>(key: K, value: (typeof spec)[K]) =>
    update({ ...site, spec: { ...spec, [key]: value } });
  return (
    <>
      <label><span>Name</span><input value={site.name}
        onChange={(event) => update({ ...site, name: event.target.value })} /></label>
      <ListField label="Node CIDRs" value={spec.nodeCidrs}
        onChange={(value) => updateSpec('nodeCidrs', value)} />
      <fieldset>
        <legend>Pod CIDR assignments</legend>
        {spec.podCidrAssignments.map((assignment, index) => {
          const updateAssignment = (next: typeof assignment) => updateSpec(
            'podCidrAssignments',
            spec.podCidrAssignments.map((item, itemIndex) => itemIndex === index ? next : item)
          );
          return <div className="assignment" key={index}>
            <ListField label="CIDR blocks" value={assignment.cidrBlocks}
              onChange={(value) => updateAssignment({ ...assignment, cidrBlocks: value })} />
            <OptionalBooleanField label="Assignment enabled" value={assignment.assignmentEnabled}
              onChange={(value) => updateAssignment({ ...assignment, assignmentEnabled: value })} />
            <NumberField label="IPv4 node block size" value={assignment.nodeBlockSizes.ipv4}
              onChange={(value) => updateAssignment({
                ...assignment, nodeBlockSizes: { ...assignment.nodeBlockSizes, ipv4: value }
              })} />
            <NumberField label="IPv6 node block size" value={assignment.nodeBlockSizes.ipv6}
              onChange={(value) => updateAssignment({
                ...assignment, nodeBlockSizes: { ...assignment.nodeBlockSizes, ipv6: value }
              })} />
            <ListField label="Node regex" value={assignment.nodeRegex}
              onChange={(value) => updateAssignment({ ...assignment, nodeRegex: value })} />
            <NumberField label="Priority" value={assignment.priority}
              onChange={(value) => updateAssignment({ ...assignment, priority: value })} />
            <button onClick={() => updateSpec('podCidrAssignments',
              spec.podCidrAssignments.filter((_, itemIndex) => itemIndex !== index))}>
              Remove assignment
            </button>
          </div>;
        })}
        <button onClick={() => updateSpec('podCidrAssignments', [...spec.podCidrAssignments, {
          assignmentEnabled: null,
          cidrBlocks: ['10.245.0.0/16'],
          nodeBlockSizes: { ipv4: 24, ipv6: null },
          nodeRegex: [],
          priority: null
        }])}>Add assignment</button>
      </fieldset>
      <OptionalBooleanField label="Manage CNI plugin" value={spec.manageCniPlugin}
        onChange={(value) => updateSpec('manageCniPlugin', value)} />
      <ListField label="Non-masquerade CIDRs" value={spec.nonMasqueradeCIDRs}
        onChange={(value) => updateSpec('nonMasqueradeCIDRs', value)} />
      <ListField label="Local CIDRs" value={spec.localCidrs}
        onChange={(value) => updateSpec('localCidrs', value)} />
      <TunnelFields protocol={spec.tunnelProtocol} mtu={spec.tunnelMTU}
        onProtocol={(value) => updateSpec('tunnelProtocol', value)}
        onMTU={(value) => updateSpec('tunnelMTU', value)} />
      <HealthFields value={spec.healthCheckSettings}
        onChange={(value) => updateSpec('healthCheckSettings', value)} />
      <fieldset>
        <legend>Components</legend>
        <OptionalBooleanField label="Machina enabled" value={spec.components.machina}
          onChange={(value) => updateSpec('components', { ...spec.components, machina: value })} />
        <OptionalBooleanField label="Metalman enabled" value={spec.components.metalman}
          onChange={(value) => updateSpec('components', { ...spec.components, metalman: value })} />
        <OptionalBooleanField label="Metalman DHCP auto interface"
          value={spec.components.metalmanDHCPAutoInterface}
          onChange={(value) => updateSpec('components', {
            ...spec.components, metalmanDHCPAutoInterface: value
          })} />
        <NumberField label="Metalman replicas" value={spec.components.metalmanReplicas}
          onChange={(value) => updateSpec('components', { ...spec.components, metalmanReplicas: value })} />
        <OptionalBooleanField label="Storage enabled" value={spec.components.storage}
          onChange={(value) => updateSpec('components', { ...spec.components, storage: value })} />
        <OptionalBooleanField label="Gantry enabled" value={spec.components.gantry}
          onChange={(value) => updateSpec('components', { ...spec.components, gantry: value })} />
      </fieldset>
    </>
  );
}

function PoolEditor(props: {
  pool: GatewayPoolResource;
  update: (pool: GatewayPoolResource) => void;
}): ReactElement {
  const { pool, update } = props;
  const spec = pool.spec;
  const updateSpec = <K extends keyof typeof spec>(key: K, value: (typeof spec)[K]) =>
    update({ ...pool, spec: { ...spec, [key]: value } });
  return (
    <>
      <label><span>Name</span><input value={pool.name}
        onChange={(event) => update({ ...pool, name: event.target.value })} /></label>
      <label><span>Type</span><select value={spec.type}
        onChange={(event) => updateSpec('type', event.target.value as typeof spec.type)}>
        <option value="">Default</option><option value="External">External</option>
        <option value="Internal">Internal</option>
      </select></label>
      <label><span>Node selector (one key=value per line)</span><textarea
        value={Object.entries(spec.nodeSelector).map(([key, value]) => `${key}=${value}`).join('\n')}
        onChange={(event) => updateSpec('nodeSelector', Object.fromEntries(
          event.target.value.split('\n').map((line) => line.trim()).filter(Boolean).map((line) => {
            const separator = line.indexOf('=');
            return separator < 0 ? [line, ''] : [line.slice(0, separator), line.slice(separator + 1)];
          })
        ))} /></label>
      <ListField label="Routed CIDRs" value={spec.routedCidrs}
        onChange={(value) => updateSpec('routedCidrs', value)} />
      <TunnelFields protocol={spec.tunnelProtocol} mtu={spec.tunnelMTU}
        onProtocol={(value) => updateSpec('tunnelProtocol', value)}
        onMTU={(value) => updateSpec('tunnelMTU', value)} />
      <HealthFields value={spec.healthCheckSettings}
        onChange={(value) => updateSpec('healthCheckSettings', value)} />
    </>
  );
}

function RelationshipEditor(props: {
  relationship: Relationship;
  update: (relationship: Relationship) => void;
}): ReactElement {
  const { relationship, update } = props;
  const spec = relationship.spec;
  const updateSpec = <K extends keyof typeof spec>(key: K, value: (typeof spec)[K]) =>
    update({ ...relationship, spec: { ...spec, [key]: value } });
  return (
    <>
      <label><span>Name</span><input value={relationship.name}
        onChange={(event) => update({ ...relationship, name: event.target.value })} /></label>
      <OptionalBooleanField label="Enabled" value={spec.enabled}
        onChange={(value) => updateSpec('enabled', value)} />
      {relationship.type === 'site-site' &&
        <OptionalBooleanField label="Mesh nodes" value={spec.meshNodes}
          onChange={(value) => updateSpec('meshNodes', value)} />}
      <TunnelFields protocol={spec.tunnelProtocol} mtu={spec.tunnelMTU}
        onProtocol={(value) => updateSpec('tunnelProtocol', value)}
        onMTU={(value) => updateSpec('tunnelMTU', value)} />
      <HealthFields value={spec.healthCheckSettings}
        onChange={(value) => updateSpec('healthCheckSettings', value)} />
    </>
  );
}

function download(name: string, data: BlobPart, type: string): void {
  const url = URL.createObjectURL(new Blob([data], { type }));
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = name;
  anchor.click();
  URL.revokeObjectURL(url);
}

function QuickstartWorkspace(): ReactElement {
  const [project, setProject] = useState(loadProject);
  const [selection, setSelection] = useState<Selection>(null);
  const [snapEnabled, setSnapEnabled] = useState(true);
  const [connectSourceId, setConnectSourceId] = useState<string | null>(null);
  const [contextMenu, setContextMenu] = useState<ContextMenuState | null>(null);
  const [loadMenuOpen, setLoadMenuOpen] = useState(false);
  const [propertiesCollapsed, setPropertiesCollapsed] = useState(false);
  const [previewCollapsed, setPreviewCollapsed] = useState(false);
  const [legendCollapsed, setLegendCollapsed] = useState(false);
  const [flow, setFlow] = useState<ReactFlowInstance<Node<ResourceNodeData>, Edge> | null>(null);
  const [frozenEdgeHandles, setFrozenEdgeHandles] =
    useState<Map<string, { sourceHandle?: string; targetHandle?: string }> | null>(null);
  const importRef = useRef<HTMLInputElement>(null);
  const previewRef = useRef<HTMLDivElement>(null);
  const selectedManifestRef = useRef<HTMLElement>(null);
  const issues = useMemo(() => validateProject(project), [project]);
  const manifest = useMemo(() => generateManifest(project), [project]);
  const manifestDocuments = useMemo(() => generateManifestDocuments(project), [project]);
  const selectedManifestLineCount = useMemo(() => {
    if (!selection) return 0;
    const document = manifestDocuments.find((item) => item.resourceId === selection.id);
    return document ? document.contents.trimEnd().split('\n').length : 0;
  }, [manifestDocuments, selection]);
  const fitViewOptions = useMemo<FitViewOptions<Node<ResourceNodeData>>>(() => ({
    padding: {
      top: '52px',
      right: '16px',
      bottom: '52px',
      left: '16px'
    }
  }), []);

  useEffect(() => {
    localStorage.setItem(storageKey, JSON.stringify(project));
  }, [project]);

  useEffect(() => {
    if (!selection || previewCollapsed) return;
    window.requestAnimationFrame(() => {
      const preview = previewRef.current;
      const selected = selectedManifestRef.current;
      if (!preview || !selected) return;
      const previewTop = preview.getBoundingClientRect().top;
      const selectedTop = selected.getBoundingClientRect().top;
      preview.scrollTo({
        top: preview.scrollTop + selectedTop - previewTop,
        behavior: 'smooth'
      });
    });
  }, [previewCollapsed, selectedManifestLineCount, selection?.id]);

  const connectResources = useCallback((sourceId: string, targetId: string): void => {
    setProject((current) => {
      if (!canCreateRelationship(sourceId, targetId, current)) return current;
      const type = relationshipType(sourceId, targetId, current);
      if (!type) return current;
      const relationship: Relationship = {
        id: createId('relationship'),
        name: relationshipDefaultName(type, sourceId, targetId, current),
        type,
        source: sourceId,
        target: targetId,
        spec: defaultRelationshipSpec()
      };
      return { ...current, relationships: [...current.relationships, relationship] };
    });
  }, []);

  const deleteItem = useCallback((target: NonNullable<Selection>): void => {
    setProject((current) => {
      if (target.kind === 'relationship') {
        return {
          ...current,
          relationships: current.relationships.filter((item) => item.id !== target.id)
        };
      }
      const removed = new Set([target.id]);
      if (target.kind === 'site') {
        current.gatewayPools.filter((pool) => pool.siteId === target.id)
          .forEach((pool) => removed.add(pool.id));
      }
      return {
        ...current,
        sites: current.sites.filter((site) => !removed.has(site.id)),
        gatewayPools: current.gatewayPools.filter((pool) => !removed.has(pool.id)),
        relationships: current.relationships.filter((relationship) =>
          !removed.has(relationship.source) && !removed.has(relationship.target))
      };
    });
    setSelection((current) =>
      current?.kind === target.kind && current.id === target.id ? null : current);
  }, []);

  useEffect(() => {
    if (!contextMenu) return;
    const close = () => setContextMenu(null);
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') close();
    };
    window.addEventListener('click', close);
    window.addEventListener('keydown', onKeyDown);
    return () => {
      window.removeEventListener('click', close);
      window.removeEventListener('keydown', onKeyDown);
    };
  }, [contextMenu]);

  useEffect(() => {
    if (!loadMenuOpen) return;
    const close = () => setLoadMenuOpen(false);
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') close();
    };
    window.addEventListener('click', close);
    window.addEventListener('keydown', onKeyDown);
    return () => {
      window.removeEventListener('click', close);
      window.removeEventListener('keydown', onKeyDown);
    };
  }, [loadMenuOpen]);

  const nodes = useMemo<Node<ResourceNodeData>[]>(() => [
    ...project.sites.map((site) => ({
      id: site.id,
      type: 'site',
      position: site.position,
      data: {
        label: site.name,
        subtitle: 'Site',
        details: [
          `Node CIDRs: ${site.spec.nodeCidrs.join(', ') || '-'}`,
          `Pod CIDRs: ${site.spec.podCidrAssignments
            .flatMap((assignment) => assignment.cidrBlocks).join(', ') || '-'}`
        ]
      },
      width: site.size.width,
      height: site.size.height,
      measured: { width: site.size.width, height: site.size.height },
      style: { width: site.size.width, height: site.size.height },
      selected: selection?.kind === 'site' && selection.id === site.id
    })),
    ...project.gatewayPools.map((pool) => ({
      id: pool.id,
      type: 'pool',
      parentId: pool.siteId,
      extent: 'parent' as const,
      position: pool.position,
      data: {
        label: pool.name,
        subtitle: 'GatewayPool',
        details: Object.entries(pool.spec.nodeSelector)
          .map(([key, value]) => `${key}=${value}`)
      },
      width: poolWidth,
      height: poolHeight,
      measured: { width: poolWidth, height: poolHeight },
      style: { width: poolWidth, height: poolHeight },
      selected: selection?.kind === 'pool' && selection.id === pool.id
    }))
  ], [project, selection]);

  const edges = useMemo<Edge[]>(() => project.relationships.map((relationship) => ({
    id: relationship.id,
    source: relationship.source,
    target: relationship.target,
    ...(frozenEdgeHandles?.get(relationship.id) ?? relationshipHandles(relationship, project)),
    type: 'relationship',
    label: relationshipLabel(relationship.type),
    selected: selection?.kind === 'relationship' && selection.id === relationship.id,
    style: {
      stroke: relationship.type === 'site-site' ? '#7c3aed' :
        relationship.type === 'site-gateway-pool' ? '#0284c7' : '#16a34a',
      strokeWidth: 3
    }
  })), [frozenEdgeHandles, project, selection]);

  const onConnect = useCallback((connection: Connection) => {
    if (!connection.source || !connection.target) return;
    connectResources(connection.source, connection.target);
  }, [connectResources]);

  const onNodesChange = useCallback((changes: NodeChange<Node<ResourceNodeData>>[]) => {
    const positions = new Map(changes.flatMap((change) =>
      change.type === 'position' && change.position ? [[change.id, change.position] as const] : []
    ));
    const sizes = new Map(changes.flatMap((change) =>
      change.type === 'dimensions' && change.dimensions
        ? [[change.id, change.dimensions] as const] : []
    ));
    if (positions.size === 0 && sizes.size === 0) return;
    setProject((current) => ({
      ...current,
      sites: current.sites.map((site) => {
        const position = positions.get(site.id);
        const size = sizes.get(site.id);
        return position || size ? {
          ...site,
          ...(position ? { position } : {}),
          ...(size ? { size } : {})
        } : site;
      }),
      gatewayPools: current.gatewayPools.map((pool) => {
        const nextPosition = positions.get(pool.id);
        if (!nextPosition) return pool;
        const site = current.sites.find((item) => item.id === pool.siteId);
        if (!snapEnabled || !site) return { ...pool, position: nextPosition };
        const centeredX = (site.size.width - poolWidth) / 2;
        const position = Math.abs(nextPosition.x - centeredX) <= snapGridSize
          ? { ...nextPosition, x: centeredX }
          : nextPosition;
        return { ...pool, position };
      })
    }));
  }, [snapEnabled]);

  const onDrop = useCallback((event: React.DragEvent) => {
    event.preventDefault();
    const kind = event.dataTransfer.getData('application/unbounded-resource');
    if (!flow || (kind !== 'site' && kind !== 'pool')) return;
    const position = flow.screenToFlowPosition({ x: event.clientX, y: event.clientY });
    setProject((current) => {
      if (kind === 'site') {
        const index = current.sites.length;
        return {
          ...current,
          sites: [...current.sites, {
            id: createId('site'),
            name: `site-${index + 1}`,
            position,
            size: defaultSiteSize(),
            spec: defaultSiteSpec(index)
          }]
        };
      }
      const parent = [...current.sites].reverse().find((site) =>
        position.x >= site.position.x && position.x <= site.position.x + site.size.width &&
        position.y >= site.position.y && position.y <= site.position.y + site.size.height);
      if (!parent) return current;
      const poolId = createId('gateway-pool');
      const poolName = nextGatewayPoolName(parent.name, current.gatewayPools);
      const relationship: Relationship = {
        id: createId('relationship'),
        name: relationshipDefaultName('site-gateway-pool', parent.id, poolId, {
          ...current,
          gatewayPools: [...current.gatewayPools, {
            id: poolId,
            name: poolName,
            siteId: parent.id,
            position: { x: 0, y: 0 },
            spec: defaultGatewayPoolSpec(parent.name)
          }]
        }),
        type: 'site-gateway-pool',
        source: parent.id,
        target: poolId,
        spec: defaultRelationshipSpec()
      };
      return {
        ...current,
        gatewayPools: [...current.gatewayPools, {
          id: poolId,
          name: poolName,
          siteId: parent.id,
          position: {
            x: Math.max(20, Math.min(
              position.x - parent.position.x,
              parent.size.width - poolWidth - 20
            )),
            y: Math.max(70, Math.min(
              position.y - parent.position.y,
              parent.size.height - poolHeight - 20
            ))
          },
          spec: defaultGatewayPoolSpec(parent.name)
        }],
        relationships: [...current.relationships, relationship]
      };
    });
  }, [flow]);

  const addSiteAt = useCallback((position: { x: number; y: number }): void => {
    setProject((current) => {
      const index = current.sites.length;
      return {
        ...current,
        sites: [...current.sites, {
          id: createId('site'),
          name: `site-${index + 1}`,
          position,
          size: defaultSiteSize(),
          spec: defaultSiteSpec(index)
        }]
      };
    });
  }, []);

  const addGatewayPoolToSite = useCallback((siteId: string): void => {
    setProject((current) => {
      const site = current.sites.find((item) => item.id === siteId);
      if (!site) return current;
      const poolId = createId('gateway-pool');
      const poolName = nextGatewayPoolName(site.name, current.gatewayPools);
      const pool: GatewayPoolResource = {
        id: poolId,
        name: poolName,
        siteId,
        position: {
          x: (site.size.width - poolWidth) / 2,
          y: Math.min(
            90 + current.gatewayPools.filter((item) => item.siteId === siteId).length * 130,
            site.size.height - poolHeight - 20
          )
        },
        spec: defaultGatewayPoolSpec(site.name)
      };
      const relationship: Relationship = {
        id: createId('relationship'),
        name: relationshipDefaultName('site-gateway-pool', siteId, poolId, {
          ...current,
          gatewayPools: [...current.gatewayPools, pool]
        }),
        type: 'site-gateway-pool',
        source: siteId,
        target: poolId,
        spec: defaultRelationshipSpec()
      };
      return {
        ...current,
        gatewayPools: [...current.gatewayPools, pool],
        relationships: [...current.relationships, relationship]
      };
    });
  }, []);

  const openContextMenu = useCallback((
    event: MouseEvent | React.MouseEvent,
    target: Selection
  ): void => {
    event.preventDefault();
    event.stopPropagation();
    setContextMenu({
      x: event.clientX,
      y: event.clientY,
      target,
      flowPosition: flow?.screenToFlowPosition({
        x: event.clientX,
        y: event.clientY
      }) ?? { x: 0, y: 0 }
    });
  }, [flow]);

  const selectedSite = selection?.kind === 'site'
    ? project.sites.find((site) => site.id === selection.id) : undefined;
  const selectedPool = selection?.kind === 'pool'
    ? project.gatewayPools.find((pool) => pool.id === selection.id) : undefined;
  const selectedRelationship = selection?.kind === 'relationship'
    ? project.relationships.find((relationship) => relationship.id === selection.id) : undefined;
  const selectManifestResource = (resourceId: string): void => {
    const kind = project.sites.some((site) => site.id === resourceId)
      ? 'site'
      : project.gatewayPools.some((pool) => pool.id === resourceId)
        ? 'pool'
        : 'relationship';
    setSelection({ kind, id: resourceId });
  };

  return (
    <div className="app-shell">
      <header className="app-header">
        <a href="/" className="brand">Unbounded</a>
        <h1>Architecture Tool</h1>
        <input className="header-project-name" value={project.name}
          aria-label="Project name"
          onChange={(event) => setProject({ ...project, name: event.target.value })} />
        <div className="toolbar">
          {connectSourceId && <button className="connect-mode"
            onClick={() => setConnectSourceId(null)}>
            Select destination (cancel)
          </button>}
          <button onClick={() => download(`${project.name}.json`,
            JSON.stringify(project, null, 2) + '\n', 'application/json')}>Save</button>
          <div className="split-button">
            <button className="split-button-main" onClick={() => {
              setLoadMenuOpen(false);
              importRef.current?.click();
            }}>Load</button>
            <button className="split-button-arrow" aria-label="Choose load source"
              aria-expanded={loadMenuOpen}
              onClick={(event) => {
                event.stopPropagation();
                setLoadMenuOpen((open) => !open);
              }}><span className="dropdown-arrow" aria-hidden="true" /></button>
            <input ref={importRef} type="file" accept="application/json" hidden
              onChange={async (event) => {
                const file = event.target.files?.[0];
                if (!file) return;
                try {
                  setProject(withSiteSelectors(parseProject(await file.text())));
                  setSelection(null);
                } catch (error) {
                  window.alert(error instanceof Error ? error.message : 'Could not load design.');
                }
                event.target.value = '';
              }} />
            {loadMenuOpen && <div className="load-menu"
              onClick={(event) => event.stopPropagation()}>
              <button onClick={() => {
                setLoadMenuOpen(false);
                importRef.current?.click();
              }}>Saved design</button>
              <button onClick={() => {
                setLoadMenuOpen(false);
                if (window.confirm(
                  'Replace the current project with the default configuration?'
                )) {
                  setProject(createAKSQuickstartProject());
                  setSelection(null);
                  window.requestAnimationFrame(() => flow?.fitView(fitViewOptions));
                }
              }}>Default configuration</button>
            </div>}
          </div>
          <button className="primary" disabled={issues.length > 0} onClick={() =>
            download(`${project.name}.yaml`, manifest, 'application/yaml')
          }>Download manifests</button>
          <button className="danger" onClick={() => {
            if (window.confirm('Clear all resources and relationships?')) {
              setProject(createProject());
              setSelection(null);
            }
          }}>Clear all</button>
        </div>
      </header>
      <main className="workspace">
        <section className="canvas" onDrop={onDrop}
          onDragOver={(event) => { event.preventDefault(); event.dataTransfer.dropEffect = 'move'; }}>
          <ReactFlow nodes={nodes} edges={edges} nodeTypes={nodeTypes} edgeTypes={edgeTypes}
            connectionMode={ConnectionMode.Loose}
            connectionRadius={240}
            snapToGrid={snapEnabled}
            snapGrid={[snapGridSize, snapGridSize]}
            isValidConnection={(connection) => Boolean(
              connection.source && connection.target &&
              canCreateRelationship(connection.source, connection.target, project)
            )}
            onInit={setFlow} onConnect={onConnect} onNodesChange={onNodesChange}
            onNodeDragStart={() => setFrozenEdgeHandles(new Map(
              project.relationships.map((relationship) => [
                relationship.id,
                relationshipHandles(relationship, project)
              ])
            ))}
            onNodeDragStop={() => setFrozenEdgeHandles(null)}
            onNodeClick={(_, node) => {
              const target = {
                kind: project.sites.some((site) => site.id === node.id) ? 'site' : 'pool',
                id: node.id
              } as const;
              if (connectSourceId) {
                connectResources(connectSourceId, node.id);
                setConnectSourceId(null);
              }
              setSelection(target);
            }}
            onEdgeClick={(_, edge) => setSelection({ kind: 'relationship', id: edge.id })}
            onNodeContextMenu={(event, node) => {
              const target: NonNullable<Selection> = {
                kind: project.sites.some((site) => site.id === node.id) ? 'site' : 'pool',
                id: node.id
              };
              setSelection(target);
              openContextMenu(event, target);
            }}
            onEdgeContextMenu={(event, edge) => {
              const target: NonNullable<Selection> = { kind: 'relationship', id: edge.id };
              setSelection(target);
              openContextMenu(event, target);
            }}
            onPaneContextMenu={(event) => openContextMenu(event, null)}
            fitViewOptions={fitViewOptions}
            onPaneClick={() => {
              setSelection(null);
              setContextMenu(null);
            }} fitView>
            <Controls className="canvas-controls" position="top-left"
              orientation="horizontal" fitViewOptions={fitViewOptions}>
              <ControlButton
                className={snapEnabled ? 'snap-enabled' : ''}
                title={snapEnabled ? 'Disable snap to grid' : 'Enable snap to grid'}
                aria-label={snapEnabled ? 'Disable snap to grid' : 'Enable snap to grid'}
                aria-pressed={snapEnabled}
                onClick={() => setSnapEnabled((enabled) => !enabled)}
              >
                <svg viewBox="0 0 16 16" aria-hidden="true">
                  <path d="M1 1h5v5H1V1Zm9 0h5v5h-5V1ZM1 10h5v5H1v-5Zm9 0h5v5h-5v-5Z" />
                </svg>
              </ControlButton>
            </Controls>
            <MiniMap
              pannable
              zoomable
              bgColor="#18181b"
              maskColor="rgba(9, 9, 11, 0.35)"
              maskStrokeColor="#a1a1aa"
              maskStrokeWidth={2}
              nodeColor={(node) => node.type === 'site' ? '#3f3f46' : '#0369a1'}
              nodeStrokeColor={(node) => node.type === 'site' ? '#71717a' : '#38bdf8'}
              nodeStrokeWidth={3}
            />
          </ReactFlow>
          <section className={`canvas-overlay legend-overlay ${legendCollapsed ? 'collapsed' : ''}`}>
              <button className="canvas-overlay-header"
                aria-expanded={!legendCollapsed}
                onClick={() => setLegendCollapsed((collapsed) => !collapsed)}>
                <span>Legend</span>
                <span aria-hidden="true">{legendCollapsed ? '+' : '-'}</span>
              </button>
              {!legendCollapsed && <ul className="legend">
                <li><i className="site-site" /> SitePeering</li>
                <li><i className="site-pool" /> SiteGatewayPoolAssociation</li>
                <li><i className="pool-pool" /> GatewayPoolPeering</li>
              </ul>}
          </section>
          <section className={`canvas-overlay validation ${issues.length ? 'has-errors' : ''}`}>
            <strong>{issues.length
              ? `${issues.length} validation issue${issues.length === 1 ? '' : 's'}`
              : 'Project is valid and ready to export.'}</strong>
            {issues.length > 0 && <ul>{issues.map((issue, index) =>
              <li key={`${issue.path}-${index}`}>
                <code>{issue.path}</code>: {issue.message}
              </li>)}</ul>}
          </section>
        </section>
        <aside className="inspector">
          {selection && <section
            className={`inspector-panel ${propertiesCollapsed ? 'collapsed' : ''}`}>
            <button className="inspector-panel-header"
              aria-expanded={!propertiesCollapsed}
              onClick={() => setPropertiesCollapsed((collapsed) => !collapsed)}>
              <span>Properties</span>
              <span aria-hidden="true">{propertiesCollapsed ? '+' : '-'}</span>
            </button>
            {!propertiesCollapsed && <div className="inspector-panel-content">
              {selectedSite && <SiteEditor site={selectedSite} update={(site) =>
                setProject((current) => ({
                  ...current,
                  sites: current.sites.map((item) => item.id === site.id ? site : item),
                  gatewayPools: current.gatewayPools.map((pool) => pool.siteId === site.id ? {
                    ...pool,
                    spec: {
                      ...pool.spec,
                      nodeSelector: {
                        [siteSelectorKey]: site.name,
                        ...Object.fromEntries(
                          Object.entries(pool.spec.nodeSelector)
                            .filter(([key]) => key !== siteSelectorKey)
                        )
                      }
                    }
                  } : pool)
                }))} />}
              {selectedPool && <PoolEditor pool={selectedPool} update={(pool) =>
                setProject((current) => {
                  const site = current.sites.find((item) => item.id === pool.siteId);
                  const updatedPool = {
                    ...pool,
                    spec: {
                      ...pool.spec,
                      nodeSelector: {
                        ...(site ? { [siteSelectorKey]: site.name } : {}),
                        ...Object.fromEntries(
                          Object.entries(pool.spec.nodeSelector)
                            .filter(([key]) => key !== siteSelectorKey)
                        )
                      }
                    }
                  };
                  return {
                    ...current,
                    gatewayPools: current.gatewayPools.map((item) =>
                      item.id === updatedPool.id ? updatedPool : item)
                  };
                })} />}
              {selectedRelationship && <RelationshipEditor relationship={selectedRelationship}
                update={(relationship) => setProject({
                  ...project,
                  relationships: project.relationships.map((item) =>
                    item.id === relationship.id ? relationship : item)
                })} />}
            </div>}
          </section>}
          <section className={`inspector-panel preview-panel ${previewCollapsed ? 'collapsed' : ''}`}>
            <button className="inspector-panel-header"
              aria-expanded={!previewCollapsed}
              onClick={() => setPreviewCollapsed((collapsed) => !collapsed)}>
              <span>Preview</span>
              <span aria-hidden="true">{previewCollapsed ? '+' : '-'}</span>
            </button>
            {!previewCollapsed && <div ref={previewRef}
              className="inspector-panel-content manifest-preview">
              <pre>{manifestDocuments.map((document, documentIndex) => {
                const selected = selection?.id === document.resourceId;
                return <span key={document.resourceId}>
                  {documentIndex > 0 && <span className="manifest-separator">---</span>}
                  <code ref={selected ? selectedManifestRef : undefined}
                    className={`manifest-document ${selected ? 'selected' : ''}`}
                    onClick={() => selectManifestResource(document.resourceId)}
                    onKeyDown={(event) => {
                      if (event.key === 'Enter' || event.key === ' ') {
                        event.preventDefault();
                        selectManifestResource(document.resourceId);
                      }
                    }}
                    role="button"
                    tabIndex={0}>
                    {document.contents.trimEnd().split('\n').map((line, lineIndex) =>
                      <span className="manifest-line"
                        key={`${document.resourceId}-${lineIndex}`}>{line || ' '}</span>)}
                  </code>
                </span>;
              })}</pre>
            </div>}
          </section>
        </aside>
      </main>
      {contextMenu && <div className="context-menu" style={{
        left: contextMenu.x,
        top: contextMenu.y
      }} onClick={(event) => event.stopPropagation()}
      onContextMenu={(event) => event.preventDefault()}>
        {contextMenu.target && contextMenu.target.kind !== 'relationship' &&
          <button onClick={() => {
            setConnectSourceId(contextMenu.target!.id);
            setContextMenu(null);
          }}>Connect</button>}
        {contextMenu.target?.kind !== 'relationship' && <div className="context-submenu">
          <button>Add <span aria-hidden="true">&gt;</span></button>
          <div className="context-submenu-panel">
            {!contextMenu.target && <button onClick={() => {
              addSiteAt(contextMenu.flowPosition);
              setContextMenu(null);
            }}>Site</button>}
            {contextMenu.target?.kind === 'site' && <button onClick={() => {
              addGatewayPoolToSite(contextMenu.target!.id);
              setContextMenu(null);
            }}>GatewayPool</button>}
            {contextMenu.target?.kind === 'pool' &&
              <button disabled>No child objects</button>}
          </div>
        </div>}
        {contextMenu.target && <button className="context-delete" onClick={() => {
          deleteItem(contextMenu.target!);
          setContextMenu(null);
        }}>Delete</button>}
      </div>}
    </div>
  );
}

export function QuickstartApp(): ReactElement {
  return <ReactFlowProvider><QuickstartWorkspace /></ReactFlowProvider>;
}

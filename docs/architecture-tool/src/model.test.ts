import { describe, expect, it } from 'vitest';
import {
  canCreateRelationship, createAKSQuickstartProject, createProject, defaultGatewayPoolSpec,
  defaultRelationshipSpec, defaultSiteSpec,
  defaultSiteSize, nextGatewayPoolName, parseProject, relationshipType
} from './model';

describe('relationshipType', () => {
  it('infers each supported endpoint pair', () => {
    const project = createProject();
    project.sites.push({
      id: 'site-a', name: 'a', position: { x: 0, y: 0 },
      size: defaultSiteSize(), spec: defaultSiteSpec()
    });

    project.sites.push({
      id: 'site-b', name: 'b', position: { x: 0, y: 0 },
      size: defaultSiteSize(), spec: defaultSiteSpec()
    });
    project.gatewayPools.push({
      id: 'pool-a', name: 'pool-a', siteId: 'site-a', position: { x: 0, y: 0 },
      spec: defaultGatewayPoolSpec()
    });

    project.gatewayPools.push({
      id: 'pool-b', name: 'pool-b', siteId: 'site-b', position: { x: 0, y: 0 },
      spec: defaultGatewayPoolSpec()
    });

    expect(relationshipType('site-a', 'site-b', project)).toBe('site-site');
    expect(relationshipType('pool-a', 'site-b', project)).toBe('site-gateway-pool');
    expect(relationshipType('pool-a', 'pool-b', project)).toBe('gateway-pool-gateway-pool');
    expect(relationshipType('missing', 'site-a', project)).toBeNull();
  });
});

describe('canCreateRelationship', () => {
  it('rejects self-links and duplicate endpoint pairs in either direction', () => {
    const project = createAKSQuickstartProject();
    expect(canCreateRelationship('site-cluster', 'site-cluster', project)).toBe(false);
    expect(canCreateRelationship('site-cluster', 'gateway-pool-main', project)).toBe(false);
    expect(canCreateRelationship('gateway-pool-main', 'site-cluster', project)).toBe(false);

    project.relationships = [];
    expect(canCreateRelationship('site-cluster', 'gateway-pool-main', project)).toBe(true);
    project.relationships.push({
      id: 'association',
      name: 'association',
      type: 'site-gateway-pool',
      source: 'site-cluster',
      target: 'gateway-pool-main',
      spec: defaultRelationshipSpec()
    });
    expect(canCreateRelationship('gateway-pool-main', 'site-cluster', project)).toBe(false);
  });
});

describe('nextGatewayPoolName', () => {
  it('uses the Site name and adds an index only when needed', () => {
    const project = createAKSQuickstartProject();
    expect(nextGatewayPoolName('remote', project.gatewayPools)).toBe('gw-remote');
    project.gatewayPools.push({
      ...project.gatewayPools[0],
      id: 'gateway-pool-remote',
      name: 'gw-remote',
      siteId: 'site-remote'
    });
    expect(nextGatewayPoolName('remote', project.gatewayPools)).toBe('gw-remote2');
    project.gatewayPools.push({
      ...project.gatewayPools[0],
      id: 'gateway-pool-remote2',
      name: 'gw-remote2',
      siteId: 'site-remote'
    });
    expect(nextGatewayPoolName('remote', project.gatewayPools)).toBe('gw-remote3');
  });
});

describe('parseProject', () => {
  it('rejects structurally invalid imported projects', () => {
    expect(() => parseProject(JSON.stringify({
      version: 1, name: 'demo', sites: [{}], gatewayPools: [], relationships: []
    }))).toThrow('Unsupported or invalid');
  });

  describe('createAKSQuickstartProject', () => {
    it('matches the resources created by aks-quickstart.sh', () => {
      const project = createAKSQuickstartProject();

      expect(project.sites.map((site) => site.name)).toEqual(['cluster', 'remote']);
      expect(project.sites[0].spec.nodeCidrs).toEqual(['10.224.0.0/16']);
      expect(project.sites[0].spec.podCidrAssignments[0].cidrBlocks).toEqual(['10.244.0.0/16']);
      expect(project.sites[1].spec.nodeCidrs).toEqual(['192.168.1.0/24']);
      expect(project.sites[1].spec.podCidrAssignments[0].cidrBlocks).toEqual(['10.245.0.0/16']);
      expect(project.gatewayPools).toMatchObject([{
        name: 'gw-main',
        siteId: 'site-cluster',
        position: { x: 130, y: 160 },
        spec: {
          type: 'External',
          nodeSelector: {
            'unbounded-cloud.io/site': 'cluster',
            'unbounded-cloud.io/unbounded-net-gateway': 'true'
          }
        }
      }]);
      expect(project.relationships.map((relationship) => relationship.name)).toEqual([
        'cluster',
        'remote'
      ]);
      expect(project.relationships.every(
        (relationship) => relationship.spec.tunnelProtocol === ''
      )).toBe(true);
    });
  });

  it('rejects invalid nested field types before they reach the editor', () => {
    const project = createProject();
    project.sites.push({
      id: 'site-a',
      name: 'site-a',
      position: { x: 0, y: 0 },
      size: defaultSiteSize(),
      spec: defaultSiteSpec()
    });
    const value = JSON.parse(JSON.stringify(project));
    value.sites[0].spec.nodeCidrs = '10.0.0.0/16';
    expect(() => parseProject(JSON.stringify(value))).toThrow('Unsupported or invalid');
  });
});

import { describe, expect, it } from 'vitest';
import {
  createProject, defaultGatewayPoolSpec, defaultRelationshipSpec, defaultSiteSize, defaultSiteSpec
} from './model';
import { validateProject } from './validation';

function validProject() {
  const project = createProject();
  project.sites = [
    {
      id: 'site-a', name: 'site-a', position: { x: 0, y: 0 },
      size: defaultSiteSize(), spec: defaultSiteSpec()
    },
    {
      id: 'site-b', name: 'site-b', position: { x: 600, y: 0 },
      size: defaultSiteSize(), spec: defaultSiteSpec(1)
    }
  ];
  project.gatewayPools = [{
    id: 'pool-a',
    name: 'pool-a',
    siteId: 'site-a',
    position: { x: 20, y: 80 },
    spec: defaultGatewayPoolSpec()
  }];
  project.relationships = [{
    id: 'rel-a',
    name: 'site-a-pool-a',
    type: 'site-gateway-pool',
    source: 'site-a',
    target: 'pool-a',
    spec: defaultRelationshipSpec()
  }];
  return project;
}

describe('validateProject', () => {
  it('accepts a valid project', () => {
    expect(validateProject(validProject())).toEqual([]);
  });

  it('reports names, CIDRs, required fields, duplicates, and dangling relationships', () => {
    const project = validProject();
    project.sites[0].name = 'Invalid Name';
    project.sites[0].spec.nodeCidrs = ['10.0.0.999/16'];
    project.sites[0].spec.podCidrAssignments = [];
    project.gatewayPools[0].spec.nodeSelector = {};
    project.relationships.push({
      ...project.relationships[0],
      id: 'rel-b',
      name: 'site-a-pool-a',
      source: 'missing'
    });

    const messages = validateProject(project).map((issue) => issue.message);
    expect(messages).toContain('Use a lowercase DNS name of at most 63 characters.');
    expect(messages.some((message) => message.includes('valid CIDR'))).toBe(true);
    expect(messages).toContain('At least one pod CIDR assignment is required.');
    expect(messages).toContain('At least one node selector label is required.');
    expect(messages).toContain('Relationship has a dangling endpoint.');
    expect(messages).toContain('Duplicate relationships name "site-a-pool-a".');
  });

  it('rejects overlapping site CIDRs and fractional integer fields', () => {
    const project = validProject();
    project.sites[1].spec.nodeCidrs = ['10.0.0.0/24'];
    project.sites[1].spec.podCidrAssignments[0].cidrBlocks = ['172.16.1.0/24'];
    project.sites[0].spec.tunnelMTU = 1400.5;

    const messages = validateProject(project).map((issue) => issue.message);
    expect(messages.some((message) => message.includes('Node CIDR'))).toBe(true);
    expect(messages.some((message) => message.includes('Pod CIDR'))).toBe(true);
    expect(messages).toContain('Value must be an integer.');
  });
});

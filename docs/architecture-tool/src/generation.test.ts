import JSZip from 'jszip';
import { describe, expect, it } from 'vitest';
import {
  createAKSQuickstartProject, createProject, defaultGatewayPoolSpec,
  defaultRelationshipSpec, defaultSiteSize, defaultSiteSpec
} from './model';
import {
  generateFiles, generateManifest, generateManifestDocuments, generateZip
} from './generation';

function projectFixture() {
  const project = createProject();
  project.name = 'demo';
  project.sites = [
    {
      id: 'site-a', name: 'site-a', position: { x: 0, y: 0 },
      size: defaultSiteSize(), spec: defaultSiteSpec()
    },
    {
      id: 'site-b', name: 'site-b', position: { x: 600, y: 0 },
      size: defaultSiteSize(), spec: defaultSiteSpec()
    }
  ];
  project.gatewayPools = [{
    id: 'pool-a', name: 'pool-a', siteId: 'site-a', position: { x: 20, y: 80 },
    spec: defaultGatewayPoolSpec()
  }];
  project.relationships = [{
    id: 'relationship-a',
    name: 'site-a-pool-a',
    type: 'site-gateway-pool',
    source: 'site-a',
    target: 'pool-a',
    spec: defaultRelationshipSpec()
  }];
  return project;
}

describe('manifest generation', () => {
  it('generates sorted resources and a kustomization', () => {
    const files = generateFiles(projectFixture());
    expect(files.map((file) => file.path)).toEqual([...files.map((file) => file.path)].sort());
    expect(files.find((file) => file.path === 'resources/site-site-a.yaml')?.contents)
      .toContain('apiVersion: unbounded-cloud.io/v1alpha3');
    expect(files.find((file) => file.path === 'resources/site-site-a.yaml')?.contents)
      .toContain('unbounded-cloud.io/site: site-a');
    expect(files.find((file) => file.path === 'kustomization.yaml')?.contents)
      .toContain('resources/site-site-a.yaml');
  });

  it('generates one multi-document YAML manifest', () => {
    const project = projectFixture();
    const documents = generateManifestDocuments(project);
    const manifest = generateManifest(project);
    expect(documents.map((document) => document.resourceId)).toEqual([
      'site-a',
      'pool-a',
      'relationship-a',
      'site-b'
    ]);
    expect(manifest).toContain('kind: Site');
    expect(manifest).toContain('kind: GatewayPool');
    expect(manifest).toContain('kind: SiteGatewayPoolAssignment');
    expect(manifest.match(/^---$/gm)).toHaveLength(3);
  });

  it('orders resources by parentage before their relationships', () => {
    const documents = generateManifestDocuments(createAKSQuickstartProject());
    expect(documents.map((document) => document.resourceId)).toEqual([
      'site-cluster',
      'gateway-pool-main',
      'association-cluster',
      'site-remote',
      'association-remote'
    ]);
  });

  it('produces a deterministic ZIP with every generated file', async () => {
    const project = projectFixture();
    const first = await generateZip(project);
    const second = await generateZip(project);
    expect(first).toEqual(second);

    const archive = await JSZip.loadAsync(first);
    expect(Object.keys(archive.files).sort()).toEqual([
      'README.txt',
      'kustomization.yaml',
      'project.json',
      'resources/gatewaypool-pool-a.yaml',
      'resources/site-gateway-pool-site-a-pool-a.yaml',
      'resources/site-site-a.yaml',
      'resources/site-site-b.yaml'
    ]);
  });
});

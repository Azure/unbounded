import ipaddr from 'ipaddr.js';
import type { HealthCheckSettings, NetProject, Relationship, RelationshipType } from './model';
import { relationshipType } from './model';

export interface ValidationIssue {
  path: string;
  message: string;
}

const dnsName = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;

function validateName(name: string, path: string, issues: ValidationIssue[]): void {
  if (!name) issues.push({ path, message: 'Name is required.' });
  else if (name.length > 63 || !dnsName.test(name)) {
    issues.push({ path, message: 'Use a lowercase DNS name of at most 63 characters.' });
  }
}

function validateCIDRs(cidrs: string[], path: string, required: boolean, issues: ValidationIssue[]): void {
  if (required && cidrs.length === 0) issues.push({ path, message: 'At least one CIDR is required.' });
  cidrs.forEach((cidr, index) => {
    try {
      const [address, prefix] = ipaddr.parseCIDR(cidr);
      const maximum = address.kind() === 'ipv4' ? 32 : 128;
      if (prefix < 0 || prefix > maximum) throw new Error('range');
    } catch {
      issues.push({ path: `${path}[${index}]`, message: `"${cidr}" is not a valid CIDR.` });
    }
  });
}

function validateHealth(settings: HealthCheckSettings, path: string, issues: ValidationIssue[]): void {
  if (settings.detectMultiplier !== null &&
      (settings.detectMultiplier < 1 || settings.detectMultiplier > 255)) {
    issues.push({ path: `${path}.detectMultiplier`, message: 'Detect multiplier must be between 1 and 255.' });
  }
  validateInteger(settings.detectMultiplier, `${path}.detectMultiplier`, issues);
}

function validateMTU(value: number | null, path: string, issues: ValidationIssue[]): void {
  if (value !== null && (value < 576 || value > 9000)) {
    issues.push({ path, message: 'Tunnel MTU must be between 576 and 9000.' });
  }
  validateInteger(value, path, issues);
}

function validateInteger(value: number | null, path: string, issues: ValidationIssue[]): void {
  if (value !== null && !Number.isInteger(value)) {
    issues.push({ path, message: 'Value must be an integer.' });
  }
}

function cidrsOverlap(left: string, right: string): boolean {
  try {
    const [leftAddress, leftPrefix] = ipaddr.parseCIDR(left);
    const [rightAddress, rightPrefix] = ipaddr.parseCIDR(right);
    if (leftAddress.kind() !== rightAddress.kind()) return false;
    return leftAddress.match(rightAddress, rightPrefix) ||
      rightAddress.match(leftAddress, leftPrefix);
  } catch {
    return false;
  }
}

function expectedEndpoints(type: RelationshipType, actual: RelationshipType | null): boolean {
  return type === actual;
}

function validateRelationship(
  relationship: Relationship,
  project: NetProject,
  issues: ValidationIssue[]
): void {
  const path = `relationships.${relationship.id}`;
  validateName(relationship.name, `${path}.name`, issues);
  const actual = relationshipType(relationship.source, relationship.target, project);
  if (actual === null) {
    issues.push({ path, message: 'Relationship has a dangling endpoint.' });
  } else if (!expectedEndpoints(relationship.type, actual)) {
    issues.push({ path, message: `Endpoints do not match relationship type ${relationship.type}.` });
  }
  if (relationship.source === relationship.target) {
    issues.push({ path, message: 'A relationship cannot connect a resource to itself.' });
  }
  validateHealth(relationship.spec.healthCheckSettings, `${path}.healthCheckSettings`, issues);
  validateMTU(relationship.spec.tunnelMTU, `${path}.tunnelMTU`, issues);
}

function duplicateNames(
  entries: Array<{ name: string; id: string }>,
  kind: string,
  issues: ValidationIssue[]
): void {
  const seen = new Set<string>();
  entries.forEach((entry) => {
    if (seen.has(entry.name)) {
      issues.push({ path: `${kind}.${entry.id}.name`, message: `Duplicate ${kind} name "${entry.name}".` });
    }
    seen.add(entry.name);
  });
}

export function validateProject(project: NetProject): ValidationIssue[] {
  const issues: ValidationIssue[] = [];
  validateName(project.name, 'name', issues);
  duplicateNames(project.sites, 'sites', issues);
  duplicateNames(project.gatewayPools, 'gatewayPools', issues);

  project.sites.forEach((site) => {
    const path = `sites.${site.id}`;
    validateName(site.name, `${path}.name`, issues);
    validateCIDRs(site.spec.nodeCidrs, `${path}.nodeCidrs`, true, issues);
    if (site.spec.podCidrAssignments.length === 0) {
      issues.push({ path: `${path}.podCidrAssignments`, message: 'At least one pod CIDR assignment is required.' });
    }
    site.spec.podCidrAssignments.forEach((assignment, index) => {
      validateCIDRs(assignment.cidrBlocks, `${path}.podCidrAssignments[${index}].cidrBlocks`, true, issues);
      if (assignment.nodeBlockSizes.ipv4 !== null &&
          (assignment.nodeBlockSizes.ipv4 < 1 || assignment.nodeBlockSizes.ipv4 > 32)) {
        issues.push({ path, message: 'IPv4 node block size must be between 1 and 32.' });
      }
      validateInteger(
        assignment.nodeBlockSizes.ipv4,
        `${path}.podCidrAssignments[${index}].nodeBlockSizes.ipv4`,
        issues
      );
      if (assignment.nodeBlockSizes.ipv6 !== null &&
          (assignment.nodeBlockSizes.ipv6 < 1 || assignment.nodeBlockSizes.ipv6 > 128)) {
        issues.push({ path, message: 'IPv6 node block size must be between 1 and 128.' });
      }
      validateInteger(
        assignment.nodeBlockSizes.ipv6,
        `${path}.podCidrAssignments[${index}].nodeBlockSizes.ipv6`,
        issues
      );
      validateInteger(
        assignment.priority,
        `${path}.podCidrAssignments[${index}].priority`,
        issues
      );
    });
    validateCIDRs(site.spec.nonMasqueradeCIDRs, `${path}.nonMasqueradeCIDRs`, false, issues);
    validateCIDRs(site.spec.localCidrs, `${path}.localCidrs`, false, issues);
    validateHealth(site.spec.healthCheckSettings, `${path}.healthCheckSettings`, issues);
    validateMTU(site.spec.tunnelMTU, `${path}.tunnelMTU`, issues);
    validateInteger(site.spec.components.metalmanReplicas, `${path}.components.metalmanReplicas`, issues);
    if (site.spec.components.metalmanReplicas !== null &&
        site.spec.components.metalmanReplicas < 0) {
      issues.push({
        path: `${path}.components.metalmanReplicas`,
        message: 'Metalman replicas cannot be negative.'
      });
    }
  });

  project.sites.forEach((site, siteIndex) => {
    project.sites.slice(siteIndex + 1).forEach((otherSite) => {
      site.spec.nodeCidrs.forEach((cidr) => {
        otherSite.spec.nodeCidrs.forEach((otherCIDR) => {
          if (cidrsOverlap(cidr, otherCIDR)) {
            issues.push({
              path: `sites.${otherSite.id}.nodeCidrs`,
              message: `Node CIDR ${otherCIDR} overlaps ${cidr} in Site ${site.name}.`
            });
          }
        });
      });
      const podCIDRs = site.spec.podCidrAssignments.flatMap((assignment) => assignment.cidrBlocks);
      const otherPodCIDRs = otherSite.spec.podCidrAssignments.flatMap(
        (assignment) => assignment.cidrBlocks
      );
      podCIDRs.forEach((cidr) => {
        otherPodCIDRs.forEach((otherCIDR) => {
          if (cidrsOverlap(cidr, otherCIDR)) {
            issues.push({
              path: `sites.${otherSite.id}.podCidrAssignments`,
              message: `Pod CIDR ${otherCIDR} overlaps ${cidr} in Site ${site.name}.`
            });
          }
        });
      });
    });
  });

  project.gatewayPools.forEach((pool) => {
    const path = `gatewayPools.${pool.id}`;
    validateName(pool.name, `${path}.name`, issues);
    if (!project.sites.some((site) => site.id === pool.siteId)) {
      issues.push({ path: `${path}.siteId`, message: 'Gateway pool must be nested in an existing site.' });
    }
    if (Object.keys(pool.spec.nodeSelector).length === 0) {
      issues.push({ path: `${path}.nodeSelector`, message: 'At least one node selector label is required.' });
    }
    validateCIDRs(pool.spec.routedCidrs, `${path}.routedCidrs`, false, issues);
    validateHealth(pool.spec.healthCheckSettings, `${path}.healthCheckSettings`, issues);
    validateMTU(pool.spec.tunnelMTU, `${path}.tunnelMTU`, issues);
  });

  const relationshipKeys = new Set<string>();
  project.relationships.forEach((relationship) => {
    validateRelationship(relationship, project, issues);
    const endpoints = [relationship.source, relationship.target].sort().join(':');
    const key = `${relationship.type}:${endpoints}`;
    if (relationshipKeys.has(key)) {
      issues.push({ path: `relationships.${relationship.id}`, message: 'Duplicate relationship.' });
    }
    relationshipKeys.add(key);
  });
  duplicateNames(project.relationships, 'relationships', issues);
  return issues;
}

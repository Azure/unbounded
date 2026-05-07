// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { NodeStatus } from '../../../types';

function parseEndpointHost(endpoint?: string) {
  if (!endpoint) return '';
  const trimmed = endpoint.trim();
  if (!trimmed) return '';
  if (trimmed.startsWith('[')) {
    const end = trimmed.indexOf(']');
    return end > 0 ? trimmed.slice(1, end) : '';
  }
  const lastColon = trimmed.lastIndexOf(':');
  if (lastColon <= 0) return trimmed;
  const maybeHost = trimmed.slice(0, lastColon);
  if (maybeHost.includes(':')) {
    return trimmed;
  }
  return maybeHost;
}

function normalizeIpAddress(value?: string) {
  const trimmed = (value || '').trim().toLowerCase();
  if (!trimmed) return '';

  let raw = trimmed;
  if (raw.startsWith('[') && raw.endsWith(']')) {
    raw = raw.slice(1, -1);
  }

  const zoneIndex = raw.indexOf('%');
  if (zoneIndex >= 0) {
    raw = raw.slice(0, zoneIndex);
  }

  if (!raw.includes(':')) {
    const parts = raw.split('.');
    if (parts.length === 4 && parts.every((part) => /^\d+$/.test(part))) {
      const normalized = parts.map((part) => String(Number(part)));
      if (normalized.every((part) => Number(part) >= 0 && Number(part) <= 255)) {
        return normalized.join('.');
      }
    }
    return raw;
  }

  const split = raw.split('::');
  if (split.length > 2) {
    return raw;
  }

  const parseHextets = (valuePart: string) => {
    if (!valuePart) return [] as number[];
    const tokens = valuePart.split(':').filter((token) => token.length > 0);
    const result: number[] = [];
    for (const token of tokens) {
      if (token.includes('.')) {
        const octets = token.split('.');
        if (octets.length !== 4 || octets.some((octet) => !/^\d+$/.test(octet))) {
          return null;
        }
        const nums = octets.map((octet) => Number(octet));
        if (nums.some((num) => num < 0 || num > 255)) {
          return null;
        }
        result.push((nums[0] << 8) + nums[1], (nums[2] << 8) + nums[3]);
        continue;
      }
      if (!/^[0-9a-f]{1,4}$/.test(token)) {
        return null;
      }
      result.push(Number.parseInt(token, 16));
    }
    return result;
  };

  const left = parseHextets(split[0] || '');
  const right = parseHextets(split.length === 2 ? split[1] || '' : '');
  if (!left || !right) {
    return raw;
  }

  let hextets: number[];
  if (split.length === 2) {
    const missing = 8 - (left.length + right.length);
    if (missing < 0) {
      return raw;
    }
    hextets = [...left, ...new Array(missing).fill(0), ...right];
  } else {
    hextets = left;
  }

  if (hextets.length !== 8) {
    return raw;
  }

  return hextets.map((part) => part.toString(16)).join(':');
}

function expandedIpv6DestinationForSort(destination?: string) {
  const trimmed = (destination || '').trim().toLowerCase();
  if (!trimmed) return '';

  const [addressPart, prefixPart] = trimmed.split('/');
  if (!addressPart || !addressPart.includes(':')) {
    return trimmed;
  }

  const normalized = normalizeIpAddress(addressPart);
  const hextets = normalized.split(':');
  if (hextets.length !== 8 || hextets.some((hextet) => hextet.length === 0 || hextet.length > 4)) {
    return trimmed;
  }

  const expandedAddress = hextets.map((hextet) => hextet.padStart(4, '0')).join(':');
  if (!prefixPart) {
    return expandedAddress;
  }

  const prefix = Number(prefixPart);
  if (!Number.isFinite(prefix) || prefix < 0 || prefix > 128) {
    return expandedAddress;
  }

  return `${expandedAddress}/${Math.trunc(prefix).toString().padStart(3, '0')}`;
}

function mixedDestinationSortKey(destination?: string) {
  const trimmed = (destination || '').trim().toLowerCase();
  if (!trimmed) return '';

  const [addressPart, prefixPart] = trimmed.split('/');
  if (!addressPart) return trimmed;

  if (addressPart.includes(':')) {
    return `6|${expandedIpv6DestinationForSort(trimmed)}`;
  }

  const octets = addressPart.split('.');
  if (octets.length === 4 && octets.every((octet) => /^\d+$/.test(octet))) {
    const values = octets.map((octet) => Number(octet));
    if (values.every((value) => value >= 0 && value <= 255)) {
      const paddedIp = values.map((value) => String(value).padStart(3, '0')).join('.');
      const prefix = Number(prefixPart);
      const paddedPrefix = Number.isFinite(prefix) && prefix >= 0 && prefix <= 32
        ? Math.trunc(prefix).toString().padStart(3, '0')
        : '000';
      return `4|${paddedIp}/${paddedPrefix}`;
    }
  }

  return `4|${trimmed}`;
}

function withCommaBreaks(value?: string) {
  if (!value) return '-';
  return value.replace(/,\s*/g, ',\u200b ');
}

function isHostRoute(destination?: string) {
  if (!destination) return false;
  return /\/(32|128)$/.test(destination.trim());
}

function hostIpFromRouteDestination(destination?: string) {
  if (!isHostRoute(destination)) return '';
  const trimmed = (destination || '').trim();
  const slashIndex = trimmed.lastIndexOf('/');
  if (slashIndex <= 0) return '';
  return trimmed.slice(0, slashIndex).trim();
}

function firstUsableIpFromCidr(cidr?: string) {
  if (!cidr) return '';
  const [ip, prefixRaw] = cidr.split('/');
  if (!ip) return '';
  const octets = ip.split('.');
  if (octets.length !== 4) return ip;

  const parts = octets.map((part) => Number(part));
  if (parts.some((part) => !Number.isInteger(part) || part < 0 || part > 255)) {
    return ip;
  }

  const prefix = Number(prefixRaw);
  if (!Number.isFinite(prefix) || prefix < 0 || prefix > 32) {
    return ip;
  }

  const addr = ((parts[0] << 24) >>> 0)
    | ((parts[1] << 16) >>> 0)
    | ((parts[2] << 8) >>> 0)
    | (parts[3] >>> 0);
  const mask = prefix === 0 ? 0 : ((0xffffffff << (32 - prefix)) >>> 0);
  const network = addr & mask;
  const hostBits = 32 - prefix;

  // /32 has only one address; /31 point-to-point has two usable addresses.
  const firstUsable = hostBits <= 1 ? network : ((network + 1) >>> 0);

  return [
    (firstUsable >>> 24) & 255,
    (firstUsable >>> 16) & 255,
    (firstUsable >>> 8) & 255,
    firstUsable & 255
  ].join('.');
}

function hostCandidatesFromAllowedIPs(allowedIPs?: string[]) {
  const hosts = new Set<string>();
  for (const allowedIP of allowedIPs || []) {
    const normalizedAllowedIP = (allowedIP || '').trim();
    if (!normalizedAllowedIP) {
      continue;
    }

    if (isHostRoute(normalizedAllowedIP)) {
      const host = hostIpFromRouteDestination(normalizedAllowedIP);
      const normalizedHost = normalizeIpAddress(host);
      if (normalizedHost) hosts.add(normalizedHost);
      continue;
    }

    if (normalizedAllowedIP.includes('/')) {
      const firstUsable = firstUsableIpFromCidr(normalizedAllowedIP);
      if (firstUsable) {
        const normalizedFirstUsable = normalizeIpAddress(firstUsable);
        if (normalizedFirstUsable) {
          hosts.add(normalizedFirstUsable);
        }
      }
      continue;
    }

    const normalized = normalizeIpAddress(normalizedAllowedIP);
    if (normalized) {
      hosts.add(normalized);
    }
  }
  return hosts;
}

function isImdsRoute(destination?: string) {
  if (!destination) return false;
  const normalized = destination.trim();
  return normalized === '168.63.129.16/32' || normalized === '169.254.169.254/32';
}

function parseAzureProviderId(providerId?: string): {
  subscription: string;
  resourceGroup: string;
  provider: string;
  type: string;
  object: string;
  instanceId: string;
} | null {
  const raw = (providerId || '').trim();
  if (!raw) return null;

  const normalized = raw.startsWith('azure://')
    ? raw.replace(/^azure:\/\//i, '')
    : raw;
  const path = normalized.replace(/^\/+/, '');
  if (!/^(subscriptions)\//i.test(path)) {
    return null;
  }

  const parts = path.split('/').filter(Boolean);
  const find = (name: string) => parts.findIndex((p) => p.toLowerCase() === name.toLowerCase());

  const subIdx = find('subscriptions');
  const rgIdx = find('resourcegroups');
  const provIdx = find('providers');
  if (subIdx < 0 || rgIdx < 0 || provIdx < 0) return null;

  const subscription = parts[subIdx + 1] || '';
  const resourceGroup = parts[rgIdx + 1] || '';
  const providerNamespace = parts[provIdx + 1] || '';
  const type = parts[provIdx + 2] || '';
  const object = parts[provIdx + 3] || '';

  let instanceId = '';
  for (let i = provIdx + 4; i < parts.length - 1; i++) {
    if (parts[i].toLowerCase() === 'virtualmachines') {
      instanceId = parts[i + 1] || '';
      break;
    }
  }

  if (!subscription || !resourceGroup || !providerNamespace || !type || !object) {
    return null;
  }

  return {
    subscription,
    resourceGroup,
    provider: providerNamespace,
    type,
    object,
    instanceId: instanceId || '-'
  };
}

function assessAksNode(node?: NodeStatus | null) {
  if (!node) {
    return {
      isAks: false,
      reasons: ['node data unavailable']
    };
  }

  const nodeName = node.nodeInfo?.name || '';
  const providerId = node.nodeInfo?.providerId || '';
  const labels = node.nodeInfo?.k8sLabels || {};
  const nameLooksAks = /^aks-[a-z0-9]+-\d{8}-vmss/i.test(nodeName);
  const managedLabel = labels['kubernetes.azure.com/managed'];
  const explicitlyUnmanaged = managedLabel?.toLowerCase() === 'false';
  const providerLooksAzureCompute = /providers\/microsoft\.compute\/(virtualmachinescalesets|virtualmachines)\//i.test(providerId);

  const reasons: string[] = [];
  if (!nameLooksAks) reasons.push('name does not match aks-* VMSS pattern');
  if (explicitlyUnmanaged) reasons.push('kubernetes.azure.com/managed is false');
  if (providerId && !providerLooksAzureCompute) reasons.push('providerID does not indicate Azure VM/VMSS');

  return {
    isAks: reasons.length === 0,
    reasons
  };
}

export {
  assessAksNode,
  expandedIpv6DestinationForSort,
  firstUsableIpFromCidr,
  hostCandidatesFromAllowedIPs,
  hostIpFromRouteDestination,
  isHostRoute,
  isImdsRoute,
  mixedDestinationSortKey,
  normalizeIpAddress,
  parseAzureProviderId,
  parseEndpointHost,
  withCommaBreaks
};

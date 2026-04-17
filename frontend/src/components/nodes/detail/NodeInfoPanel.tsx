// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { NodeStatus } from '../../../types';
import { withCommaBreaks } from '../shared/index';

type ProviderMeta = {
  subscription: string;
  resourceGroup: string;
  provider: string;
  type: string;
  object: string;
  instanceId: string;
};

type NodeInfoPanelProps = {
  node: NodeStatus;
  nodeAgentBuild: string;
  nodeInfo: NodeStatus['nodeInfo'];
  externalIps: string[];
  podCidrs: string[];
  podCidrFirstIps: string[];
  nodeImage: string;
  instanceType: string;
  region: string;
  availabilityZone: string;
  k8sUpdated: { absolute: string; age: string };
  pushUpdated: { absolute: string; age: string };
  providerMeta: ProviderMeta | null;
  providerPortalUrl: string;
  providerName: string;
  providerIsVmss: boolean;
};

function NodeInfoPanel({
  node,
  nodeAgentBuild,
  nodeInfo,
  externalIps,
  podCidrs,
  podCidrFirstIps,
  nodeImage,
  instanceType,
  region,
  availabilityZone,
  k8sUpdated,
  pushUpdated,
  providerMeta,
  providerPortalUrl,
  providerName,
  providerIsVmss
}: NodeInfoPanelProps) {
  return (
    <div className="card node-modal-card node-modal-info-card">
      <div className="section-title">Node Info</div>
      <div className="node-info-grid">
        <div className="node-info-card full">
          <div className="node-info-label">WireGuard Public Key</div>
          <div className="node-info-value long-wrap">{node.nodeInfo?.wireGuard?.publicKey || '-'}</div>
        </div>
        <div className="node-info-card full">
          <div className="node-info-label">Node Agent Build</div>
          <div className="node-info-value long-wrap">{nodeAgentBuild}</div>
        </div>
        <div className="node-info-card">
          <div className="node-info-label">Internal IPs</div>
          <div className="node-info-value comma-wrap">{withCommaBreaks((nodeInfo?.internalIPs || []).join(', '))}</div>
        </div>
        <div className="node-info-card">
          {externalIps.length > 0 && <div className="node-info-label">External IPs</div>}
          <div className="node-info-value comma-wrap">
            {externalIps.length > 0 ? withCommaBreaks(externalIps.join(', ')) : '\u00A0'}
          </div>
        </div>
        <div className="node-info-card">
          <div className="node-info-label">Pod CIDRs</div>
          <div className="node-info-value comma-wrap">{withCommaBreaks(podCidrs.join(', '))}</div>
        </div>
        <div className="node-info-card">
          <div className="node-info-label">Pod CIDR Gateways</div>
          <div className="node-info-value comma-wrap">{withCommaBreaks(podCidrFirstIps.join(', '))}</div>
        </div>
        <div className="node-info-card">
          <div className="node-info-label">Node Image</div>
          <div className="node-info-value comma-wrap">{withCommaBreaks(nodeImage)}</div>
        </div>
        <div className="node-info-card">
          <div className="node-info-label">Kubelet Version</div>
          <div className="node-info-value">{nodeInfo?.kubelet || '-'}</div>
        </div>
        <div className="node-info-card">
          <div className="node-info-label">Kernel</div>
          <div className="node-info-value">{nodeInfo?.kernel || '-'}</div>
        </div>
        <div className="node-info-card">
          <div className="node-info-label">Instance Type</div>
          <div className="node-info-value comma-wrap">{withCommaBreaks(instanceType)}</div>
        </div>
        <div className="node-info-card">
          <div className="node-info-label">Region</div>
          <div className="node-info-value comma-wrap">{withCommaBreaks(region)}</div>
        </div>
        <div className="node-info-card">
          <div className="node-info-label">Availability Zone</div>
          <div className="node-info-value comma-wrap">{withCommaBreaks(availabilityZone)}</div>
        </div>
        <div className="node-info-card">
          <div className="node-info-label">K8s Node Updated</div>
          <div className="node-info-value">{k8sUpdated.age}</div>
        </div>
        <div className="node-info-card">
          <div className="node-info-label">Status Push Updated</div>
          <div className="node-info-value">{pushUpdated.age}</div>
        </div>
        {providerMeta && (
          <>
            <div className="node-info-card full">
              <div className="node-info-label">Subscription</div>
              <div className="node-info-value comma-wrap">{withCommaBreaks(providerMeta.subscription)}</div>
            </div>
            <div className="node-info-card full">
              <div className="node-info-label">Resource Group</div>
              <div className="node-info-value comma-wrap">{withCommaBreaks(providerMeta.resourceGroup)}</div>
            </div>
            <div className="node-info-card">
              <div className="node-info-label">Type</div>
              <div className="node-info-value comma-wrap">{withCommaBreaks(providerMeta.type)}</div>
            </div>
            <div className="node-info-card">
              <div className="node-info-label">Name</div>
              <div className="node-info-value comma-wrap">
                {providerPortalUrl
                  ? (
                    <a href={providerPortalUrl} target="_blank" rel="noreferrer">
                      {withCommaBreaks(providerName)}
                    </a>
                  )
                  : withCommaBreaks(providerName)}
              </div>
            </div>
            {!providerIsVmss && (
              <div className="node-info-card full">
                <div className="node-info-label">Instance ID</div>
                <div className="node-info-value">{providerMeta.instanceId}</div>
              </div>
            )}
          </>
        )}
      </div>
    </div>
  );
}

export default NodeInfoPanel;

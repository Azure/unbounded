---
title: "Networking Reference"
weight: 3
description: "Complete reference documentation for unbounded-net, the multi-site networking system."
---

This section contains detailed reference documentation for
**[unbounded-net](https://github.com/project-unbounded/unbounded-net)**, the
networking layer of Project Unbounded.

For a conceptual introduction, see [Networking Concepts]({{< relref "concepts/networking" >}}).

- **[Architecture]({{< relref "reference/networking/architecture" >}})** --
  System design, eBPF and netlink dataplanes, data flows, and security model.
- **[Custom Resources]({{< relref "reference/networking/custom-resources" >}})** --
  Specifications for all 7 CRDs (Site, SiteNodeSlice, GatewayPool, and more).
- **[Configuration]({{< relref "reference/networking/configuration" >}})** --
  All flags, environment variables, ConfigMap settings, and tuning guidance.
- **[Routing Flows]({{< relref "reference/networking/routing-flows" >}})** --
  Packet-level routing for eBPF and netlink dataplanes, protocol selection.
- **[Operations]({{< relref "reference/networking/operations" >}})** --
  Deployment, monitoring, troubleshooting, and operational procedures.

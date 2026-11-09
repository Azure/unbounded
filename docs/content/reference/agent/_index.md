---
title: "Agent Reference"
weight: 2
description: "Reference documentation for the unbounded-agent."
---

The agent reference covers host-side `unbounded-agent` configuration and
bootstrap behavior. The agent always runs on the host; an
[isolation mode]({{< relref "reference/agent/isolation" >}}) decides where the
node stack (containerd, kubelet, CNI, CoreDNS) runs: in a systemd-nspawn machine
(the default), or directly on the host.

- [Configuration]({{< relref "reference/agent/configuration" >}})
- [Preflight]({{< relref "reference/agent/preflight" >}})
- [Isolation Modes]({{< relref "reference/agent/isolation" >}}): [nspawn]({{< relref "reference/agent/nspawn" >}}) and [none]({{< relref "reference/agent/isolation-none" >}})

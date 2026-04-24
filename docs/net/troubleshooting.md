<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Troubleshooting Guide

## Diagnostic Tools

### kubectl plugin

```
kubectl unbounded net node list          # cluster overview
kubectl unbounded net node show <name>   # detailed node status
kubectl unbounded net node show <name> bpf    # BPF trie entries
kubectl unbounded net node show <name> routes # kernel routes
kubectl unbounded net node show <name> json   # raw JSON status
```

### unroute -- BPF map inspector

Available inside the node agent container:

```
kubectl exec -n unbounded-net <pod> -c node -- unroute           # dump all entries
kubectl exec -n unbounded-net <pod> -c node -- unroute 10.244.1.5  # LPM lookup
kubectl exec -n unbounded-net <pod> -c node -- unroute -j         # JSON output
kubectl exec -n unbounded-net <pod> -c node -- unroute --local    # local CIDRs map
```

Columns: CIDR, REMOTE (underlay IP), NODE (destination node), IFACE, PROTO, VNI, MTU, HEALTHY

The HEALTHY column shows `Y` or `N` per nexthop, indicating whether the BPF
program considers the nexthop healthy. Unhealthy nexthops are skipped during
HRW-based flow hashing. Healthcheck probes (UDP 9997) are always forwarded.

### unping -- health check probe tool

Available inside the node agent container, `unping` sends health check probes
to a remote node's overlay IP and prints round-trip times (similar to standard
ping but uses the unbounded-net health check protocol over UDP port 9997):

```
kubectl exec -n unbounded-net <pod> -c node -- unping 100.80.1.1
kubectl exec -n unbounded-net <pod> -c node -- unping -c 5 100.80.1.1          # send 5 probes
kubectl exec -n unbounded-net <pod> -c node -- unping -i 0.5s 100.80.1.1       # 500ms interval
kubectl exec -n unbounded-net <pod> -c node -- unping -I 100.80.6.1 100.80.1.1 # specify source IP
kubectl exec -n unbounded-net <pod> -c node -- unping -p 9997 100.80.1.1       # explicit port
```

Options:
- `-c, --count N` -- number of probes (0 = until stopped)
- `-i, --interval DURATION` -- interval between probes (default 1s)
- `-w, --timeout SECONDS` -- timeout per probe (default 5s)
- `-I, --interface ADDR` -- source IP or interface name
- `-p, --port PORT` -- UDP port (default 9997)
- `--src-hostname NAME` -- source hostname in probes (default: OS hostname)
- `--dst-hostname NAME` -- destination hostname in probes (default: target address)

`unping` is useful for verifying overlay connectivity independently of the
health check manager. If `unping` succeeds but health checks show Down, the
issue is likely in the health check session configuration (hostname mismatch,
port conflict, or flap backoff).

### Node status endpoint

Each node agent exposes status on port 9998:

- `GET /status/json` -- full node status (peers, routes, health checks, BPF entries)
- `GET /metrics` -- Prometheus metrics (healthcheck_packets_sent/received, etc.)

## Common Issues

### Peers showing Down / 0 online

1. Check BPF program attachment: `tc filter show dev unbounded0 egress`
2. Check rp_filter: `cat /proc/sys/net/ipv4/conf/all/rp_filter` (must be 0)
3. Check tunnel interface MAC: `ip link show geneve0` -- MAC must be `02:<underlay_ip_bytes>:FF`
4. Verify BPF map has entries: `unroute` should show peer entries
5. Check health check probes: `tcpdump -i geneve0 -n udp port 9997`

### Route mismatch

- In eBPF mode, routes should be on unbounded0 (scope global), not on per-peer tunnel interfaces
- Check: `ip route show dev unbounded0` -- should show supernet routes
- Phantom routes on wg*/geneve0/vxlan0 that are "expected but not present" are normal in eBPF mode and should be suppressed by the annotation system

### Cross-site connectivity not working

1. Verify gateway route advertisement: `kubectl get gatewaypoolnode <name> -o yaml` -- check status.routes
2. Verify SiteGatewayPoolAssignment exists for both sites
3. Check WG handshake on gateway: SSH to gateway, check `sudo wg show`
4. Verify supernet routes on workers: `ip route show dev unbounded0` should include remote site CIDRs
5. Check forwarding on gateways: `cat /proc/sys/net/ipv4/ip_forward` (must be 1)
6. Check FORWARD ACCEPT rules on gateways: `iptables -L FORWARD -v -n` should show per-interface ACCEPT rules for `geneve0`, `wg*` gateway interfaces

> **Note:** Policy-based routing (PBR) is deprecated. If `enablePolicyRouting`
> is `false` (the new default), FORWARD ACCEPT rules handle transit forwarding.
> You should not need fwmark/connmark/ip-rule configuration.

### Tunnel protocol not taking effect

- SGPA tunnelProtocol uses composite key (siteName|poolName) -- verify both site and pool names match
- SGPA overrides Site for gateway peers; falls back to Site when Auto/nil
- Check which protocol a peer is using: `kubectl unbounded net node show <name>` -- look at PROTO column

### GENEVE/VXLAN packets arriving but not delivered

1. Check rp_filter: must be 0 on both the interface AND `net.ipv4.conf.all` (kernel uses max). Note: the node agent writes through `/proc/1/root/proc/sys/` to bypass container procfs overlay.
2. Check inner Ethernet dst MAC matches receiver's tunnel interface MAC
3. Check scope of routes on unbounded0: must be scope global (not link) for cross-interface forwarding

### WireGuard tunnel not passing traffic

1. Check WG handshake: `sudo wg show` -- latest handshake should be recent
2. Check AllowedIPs match the traffic's source/destination CIDRs
3. Check NSG/firewall allows UDP on the WG port (default 51820 for mesh, 51821+ for gateways)

### IPIP blocked on Azure

Azure NSGs block IP protocol 4 (IPIP). Use GENEVE or VXLAN instead. IPIP can be validated with tcpdump but will not pass through Azure networking.

## Log Verbosity

The node agent uses klog. Increase verbosity with `-v=4` for detailed logs:

- V(2): tunnel configuration, BPF reconciliation, health check state changes
- V(4): individual route operations, health check packets, BPF map updates

## Health Check Debugging

Health checks use UDP probes on port 9997 over the overlay network:

- Check probe flow: `tcpdump -i geneve0 -n udp port 9997`
- Check metrics: `curl http://localhost:9998/metrics | grep healthcheck`
- packets_sent_total vs packets_received_total shows if probes are flowing bidirectionally
- Required consecutive replies before Up transition (default 3, increases with flap backoff)

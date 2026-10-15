# Experimental peer TCP NODELAY

`RACER_PEER_TCP_NODELAY` accepts exactly `true` or `false` and defaults to
`false`. This is a hypothesis discriminator, not a proven live performance fix.

With `true`, the dataplane enables TCP_NODELAY on newly created outbound peer
TCP sockets before connect and on accepted peer TCP sockets before handoff or
HTTP/authentication I/O. Unix client/origin sockets, control connections, and
diagnostics are not configured by this flag. False leaves socket defaults
untouched. Change the environment and restart the dataplane to apply or reverse
the experiment; existing pooled sockets are not dynamically reconfigured.

The experiment changes no framing, signatures, encryption, admission budgets,
thread limits, deadlines, or connection limits. Header/body boundaries remain
unchanged. Any latency or throughput improvement requires separate measurement.

# Native inventory lifecycle

Automatic rail IDs are initially assigned to eligible active ports in PCI BDF,
port order. Enrollment keeps a bounded physical device/port reservation journal
in the private identity directory (`rdma-rails.json`). Withdrawn ports retain
their reservation; new ports append rather than renumbering survivors. A GID
change does not change the physical port's rail. The journal is persisted before
an enrollment report is sent and restored before subsequent reports, including
after restart. Corrupt journals fail enrollment rather than silently reassigning
rails. Do not remove the journal independently of coordinated topology changes.

This does not infer cable/fabric identity. On a node's first enrollment, ports
that have never been active cannot be matched to an automatic rail. Asymmetric
initial hardware, changed device names, or different cabling requires explicit
administrator `rdma-nics` bindings. Persisting reservations prevents subsequent
outages and restarts from silently compacting the initial assignments.

Enrollment refreshes one process-wide versioned inventory on each issuance or
renewal. Workers consume that same snapshot. A changed generation cancels pending
activation, closes old devices, and lets the native owner drain/fence retained
allocations before reopening. New selection still requires the local member's
authenticated device/port and optional GID. Discovery errors withdraw eligibility
without recycling reservations; a later successful renewal can recover it.

`RACER_ENABLE_RDMA=auto` enables native resource reservation only when startup
discovery returns at least one usable port. No library or no ports preserves HTTP
worker counts and budgets, even with a tiny unused registered-memory budget.
Such a process remains HTTP-only until restart. Use `RACER_ENABLE_RDMA=true` to
reserve native capacity at startup even without hardware; later discovery can
then activate hot-added devices without resizing workers or taking HTTP budgets.
Native provider and discovery failures remain HTTP fallbacks in that mode.

The reservation journal retains at most 1024 physical identities within 64 KiB
of encoded JSON, including escaping; inventory reports at most 64 active ports.
Exhausting either journal bound omits unknown ports without preventing renewal or
updates to known ports. It never reuses a withdrawn port's rail; previously
unknown ports remain HTTP-only.

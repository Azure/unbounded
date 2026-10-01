# Racer startup

The operator deploys `racer-controller` after reserving its permanent installation
claim and marker and establishing serving TLS, configuration, admission policy,
and RBAC. No initialization Job or `initialize` CLI command is used. Dataplanes
remain gated on valid, installation-UID-bound version state; normal serving
readiness still requires validated replicated state.

Every controller replica runs the same startup guard before recovery and manager
startup. A valid consumed, immutable marker and valid version record require no
writes. For a fresh marker and absent version, one resource-version CAS winner
consumes and freezes the marker, then makes exactly one version Create attempt.
Concurrent losers only reread, waiting up to five seconds for the winner's gap.

A consumed marker with missing or corrupt version state never authorizes creation.
A crash or lost response after marker consumption can therefore leave an unusable
installation. This ambiguity is intentional: never reset or recreate a marker to
retry, and never recreate lost version counters under the same cluster identity.
Restore consistent durable state or explicitly rebootstrap with a new cluster UUID.

For standalone manifests, use `InitializationState=fresh` only with a genuinely new
cluster UUID and absent version state. After startup, retain `consumed` in
declarative configuration and preserve the marker and version record. Constructors
do not initialize state; the running controller owns this guard.

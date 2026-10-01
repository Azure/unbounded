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

## Atomic credentials

The controller owns one `racer-credentials` Secret containing `issuer.json`
(private issuer keys and certificates), `bundle.json` (public roots and cache
keys), and `rotation.json` (rotation deadlines and issuer roles). Configure its
name with `RACER_CREDENTIALS_SECRET_NAME`; custom names also require matching RBAC
and create-restriction policy. Dataplanes never receive or mount this Secret.
They receive only the validated public-root/cache-key bundle through the control
API. Every rotation publishes all three entries in one resource-version CAS.
Only an authoritative postwrite reread can install serving trust.

The permanent credentials annotation on the version ConfigMap records
`secretName/initialRootFingerprint`. Its CAS authorizes exactly one Secret Create
attempt. Claimed missing, corrupt, or mismatched credentials never authorize
regeneration, even after an ambiguous Create response. Preserve the claim and
consistent durable state. The persisted format is intentionally breaking; there
is no compatibility reader or migration from the former split Secrets.

Bundle generation is a publication version, not a rotation count. Catalog
admission, preparation, activation, and root retirement each consume one version
only when state changes. RKG1 key IDs bind their creation publication, which never
exceeds the containing bundle generation. Unchanged reconciliation does not write,
and exhausted generations cannot wrap or publish changes.

Preparation publishes exact-ID active and prepared cache keys. Activation removes
replaced symmetric keys immediately; already-held dataplane leases may finish.
Only issuer root fingerprints have retirement deadlines. Roots overlap for the
retention period, and expired roots and their private keys are removed atomically.
Interval, preparation, retention, and leaf-lifetime policy settings remain in
effect. Catalog admission reserves two symmetric key generations plus all
overlapping roots, retains admitted UIDs first, and never evicts existing caches
to admit new ones.

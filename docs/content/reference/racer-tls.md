---
title: "Racer serving TLS"
---

The operator manages `racer-controller-tls` and publishes its CA bundle in
`racer-bootstrap-trust` in the operator namespace. These certificates protect
the controller endpoint; they are separate from Racer dataplane credentials.

New CAs last 10 years and rotate after 180 days. Serving certificates last one
year and renew with 30 days left, or when the CA rotates. Rotation retains up to
two previous roots for 360 days, capped by certificate expiry. Cross certificates
let clients with a retained root verify the new endpoint. The operator also
accepts and rotates its older 28-day CAs.

For an established installation, the operator maintains TLS even with no
ClusterCaches. Admission guard, cache, or dataplane credential failures do not
block TLS maintenance. Guard containment can run in the same pass and does not
depend on a successful TLS write. Installation identity and TLS ownership must
still be valid. TLS maintenance does not repair workloads or reset identity.

## Recovery after expiry

If the CA expires while the operator cannot renew it, restore operator access
and leave the Secret in place. The operator validates the stored keys,
certificates, installation binding, and rotation state, then issues a new CA
and serving certificate. It persists the Secret before publishing the new
bundle in `racer-bootstrap-trust`.

An expired CA cannot provide a valid trust path. The endpoint remains unavailable
to clients using the old trust until they load the new bundle. Controller pods
also need their Secret projection to update. Clients that copy the bootstrap
bundle rather than watch it must refresh their copy.

Do not delete the Secret to force renewal. A missing established Secret, broken
installation binding, invalid key pair, or corrupt rotation state still requires
restoring consistent state from backup. Expiry alone does not require a manual
certificate or rotation-state edit.

# Racer identity

`racer-identity` owns certificate identity acceptance, CSR/private-key recovery,
signing, peer verification, atomic key epochs, rotation/retirement, and immutable
leases. Dependencies are the existing crypto, control-wire, and runtime
environment components plus the existing certificate and zeroization libraries.
It has no dependency on the dataplane or its worker/service graph.

Wire bundles move directly into the installer. Private staged records wipe
transferred secrets on every return path. A lease exposes only epoch identity
and fixed-purpose page/credential AEAD and request-MAC operations. Inputs are
borrowed, outputs caller-owned, and no payload staging copy is introduced.
Purpose, cache, and expected key ID are checked without reconsulting current
admission. Removed epochs remain usable only by previously admitted owners.

Callers retain nonce uniqueness, canonical AAD/messages, quotas, cancellation,
CRC checks, telemetry, and completion ownership. PKCS8 persistence export is
explicit and zeroizing. VerifiedPeer has private construction; certificate
caches remain worker-local and non-Send. Errors contain no secret/input data
and the application maps every variant explicitly.

From `cmd/racer-dataplane`:

```sh
timeout --signal=TERM --kill-after=10s 300s cargo test -p racer-identity
timeout --signal=TERM --kill-after=10s 300s cargo clippy -p racer-identity --all-targets --no-deps -- -D warnings
timeout --signal=TERM --kill-after=10s 300s cargo test -p racer-dataplane --test identity_integration
```

Certificate and atomic-epoch tests live here. Cross-component page-engine and
decode/BundleInstaller scenarios live in the application's top-level tests.

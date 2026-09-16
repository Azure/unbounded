# Bootstrap ownership v1 fixtures

Produced by the P6 candidate on base `50286b2c`, using
`TestBootstrapV1CompatibilityFixtures`. The input uses synthetic credentials.
The producer runs the actual JSON loader, normalization, fingerprint, and
`installstate.Store.Save`; only the random installation ID is fixed.

These files freeze the default-path contract for later releases. Consume the
original input when checking compatibility. Do not regenerate these fixtures to
make a changed serializer pass. Adding a new format requires new fixtures.

Initial production command:

```sh
UPDATE_BOOTSTRAP_V1_FIXTURES=1 go test ./cmd/agent/internal/cmd -run TestBootstrapV1CompatibilityFixtures -count=1
```

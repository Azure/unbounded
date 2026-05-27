# unbounded-s3

## Relationship to unbounded-storage

`unbounded-s3` is the second Rust crate in this repository. It provides an
S3-compatible read-only API frontend for the p2p storage layer that
`unbounded-storage` implements.

For code style, testing conventions, and file layout, defer to
`cmd/unbounded-storage/AGENTS.md` - the same rules apply here.

## Notable differences from unbounded-storage

- **Async runtime.** `unbounded-s3` uses `tokio` (multi-thread for the HTTP
  server). This is intentional: the upstream crate is runtime-agnostic; the
  S3 binary ships with tokio.
- **No module integration tests (`tests/` at crate root).** The end-to-end
  HTTP test lives in `src/tests.rs` and uses the `#[cfg(test)]`
  `MemoryObjectSource`. Integration tests that require a real `BlockStore`
  implementation are run manually.

## Storage backend

The production `ObjectSource` is `BlockStoreObjectSource<S>` in
`src/storage_backend.rs`, where `S` implements the crate-local
`SendBlockStore` adapter trait (a thin `Send`-bound wrapper over
upstream `BlockStore`, needed because the upstream `read_page` future
is not `Send`). It owns a small fixed ring of 2 MiB slots registered
with the `BlockStore` at construction; `read_range` `read_page`s into
a slot, copies the requested sub-range into a fresh `Bytes`, then
releases the slot back to the ring before yielding. The ring is
therefore only ever a `read_page` destination; live response chunks
are independent heap allocations, matching the `ObjectSource` trait
contract ("each item is an independent heap allocation copied from
the pool page"). The crate has no topology, transport, or P2P logic -
those belong to the `BlockStore` implementation that `main.rs`
constructs.

Today `main.rs` wires `unbounded_storage::bufferpool::NullBlockStore`,
which always reports a miss, so the daemon will not serve any object
content until upstream publishes a P2P-aware `BlockStore`. Swapping
in a real implementor requires (1) a one-line change in `main.rs` to
construct it instead of `NullBlockStore`, and (2) a small
`impl SendBlockStore for NewStore { ... }` adapter block in
`storage_backend.rs`. The adapter is required because `SendBlockStore`
can't be blanket-impl'd over `BlockStore` (the `Send` bound applies to
the opaque `read_page` future). The rest of the data path is already
in place.

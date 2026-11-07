# Racer parser fuzz targets

Decode-only [cargo-fuzz](https://github.com/rust-fuzz/cargo-fuzz) targets for
Racer parsers. Every target must return normally on any input; a panic is a bug.

| Target              | Parser                                                                   |
| ------------------- | ------------------------------------------------------------------------ |
| `http_head`         | `http1::Codec::decode_head` (default and `trim_ows`, with opaque fields) |
| `small_body`        | `http1::small::Decoder` (chunked, close-delimited, fixed length)         |
| `peer_envelope`     | `peer_wire::decode_signed`, `decode_envelope`, and related decoders      |
| `control_documents` | `racer_wire::control` publication, keyring bundle, and enrollment decode  |
| `store_record`      | `racer::store::parse_bytes` and `parse_prefix` (disk record headers)     |
| `checkpoint`        | `racer::store::checkpoint::decode`                                       |

## Layout

This crate is intentionally not a member of the Racer workspace. Its
`Cargo.toml` declares its own `[workspace]`, so the Racer workspace,
its `Cargo.lock`, and the NOTICE generator (`hack/cmd/notice`, which rejects
`workspace.exclude`) are unaffected. The fuzz-only dependency
(`libfuzzer-sys`) is tracked in `fuzz/Cargo.lock`.

Seed inputs live in `corpus/<target>/seed-*` and are committed. Inputs that
the fuzzer discovers, plus `artifacts/`, are ignored by `.gitignore`.

`tests/seeds.rs` checks that every seed decodes with the current parsers and
that encoder-built seeds (publication, keyring bundle, and enrollment
documents copied from `wire/testdata`, peer v2 envelopes, the checkpoint
image, and the store record) are current. Regenerate them after a format
change with:

```sh
RUSTC_BOOTSTRAP=1 cargo +1.96.0 test --test seeds -- --ignored regenerate
```

## Running

The root `racer` crate uses language features newer than the pinned nightly,
so targets build with the Racer stable toolchain and `RUSTC_BOOTSTRAP=1` to
enable the sanitizer flags that cargo-fuzz passes:

```sh
make racer-fuzz                  # every target, 30 seconds each
make racer-fuzz FUZZ_SECONDS=300 RACER_FUZZ_TARGETS=small_body

cd cmd/racer/fuzz
RUSTC_BOOTSTRAP=1 cargo +1.96.0 fuzz run http_head corpus/http_head -- -max_total_time=60
```

The `http_head` target treats `HeadTooLarge` from re-encoding as expected:
encoding inserts one separator space per field, so an accepted head can
exceed the wire limit when re-encoded.

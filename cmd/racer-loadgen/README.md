# Racer load generator

Test-only synthetic origin and full-object download workers using the Racer Go
SDK. Origin and cache traffic use Unix sockets; downloaded bytes are counted and
discarded.

## Build and try it

From the repository root:

```sh
make racer-loadgen-build
mkdir -p "$PWD/tmp/loadgen"
./bin/racer-loadgen \
  -endpoint="$PWD/tmp/loadgen/origin" -origin-socket="$PWD/tmp/loadgen/origin" \
  -listen=127.0.0.1:8080 -footprint=32MiB -object-size=8MiB -duration=15s
```

This smoke test downloads directly from its own origin. During the run,
`http://127.0.0.1:8080/metrics` exposes Prometheus metrics and `/healthz` reports
process health. Shutdown logs include byte and download totals.

## Use with a cache

Point `-endpoint` at an existing Racer cache socket and `-origin-socket` at the
origin socket configured for that cache. The origin socket's parent directory
must exist and be writable. Keep `-footprint` and `-object-size` identical across
origins sharing the dataset; footprint must be an exact multiple of object size.

Use `./bin/racer-loadgen -h` for all flags and defaults, including concurrency,
Zipf sampling, and timeouts. Omit `-duration` to run until interrupted.

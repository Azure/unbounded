# Racer Unix HTTP transport measurement

Measured the production HTTP transport through `http-bench`, with 4 MiB
responses, eight persistent connections, two seconds of warmup, and ten seconds
of measured traffic. Both transports validate payload bytes during warmup.

Environment: Linux 7.0.0-30-generic, workspace ext-family filesystem, one server
worker pinned to CPU 2 (physical core 1) and one client worker pinned to CPU 4
(physical core 2), NUMA node 0. Locked-memory limit: 12,356,396 KiB. Release build
with the committed Cargo.lock. No cold-origin or network-peer traffic is included.

| Body | Transport | Gbit/s | p50 ms | p95 ms | p99 ms | Combined CPU seconds |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| File splice | TCP loopback | 53.989 | 4.187 | 9.523 | 13.657 | 23.724 |
| File splice | Unix | 85.322 | 3.130 | 3.325 | 3.472 | 23.761 |
| Immutable buffer | TCP SEND_ZC | 25.944 | 10.290 | 11.312 | 13.483 | 22.117 |
| Immutable buffer | Unix SEND | 50.348 | 5.309 | 5.660 | 6.155 | 23.971 |

CPU seconds are summed user plus system time for both child processes, including
startup, warmup, and shutdown; throughput and latency cover only the measured
window. Thus these are whole-run CPU costs, not precisely window-aligned CPU per
request. All four trials completed with zero errors. A preceding trial showed
the same direction: file TCP/Unix 56.204/83.389 Gbit/s and buffer TCP/Unix
24.320/50.429 Gbit/s. These local measurements are not a general hardware claim.

Reproduce using `cmd/racer-dataplane/README.md`'s benchmark instructions with
`taskset -c 2` for the server, `taskset -c 4` for the client,
`--connections-per-worker 8 --warmup 2 --duration 10`. Compare `--listen`/`--connect`
with `--unix` using a short path in an existing workspace directory, for both
`--body file` and `--body buffer`. Stop the server with SIGTERM and collect both
processes' resource usage after they exit.

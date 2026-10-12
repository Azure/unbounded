# Racer load generator

`racer-loadgen` serves synthetic content and reads it through Gantry, the Racer
SDK, or S3. Run `racer-loadgen --help` for workload and transport options.

## Timed runs and final metrics

`--duration` limits the load phase after catalog startup and `--start-delay`.
Zero runs until a signal. When a nonzero duration ends, workers stop and finish
recording their metrics. The metrics endpoint and origins then stay available
for `--metrics-shutdown-grace` (default `30s`) before shutdown. Readiness stays
healthy during this grace period so scrape discovery can still find the process.

Set the grace longer than your Prometheus scrape interval, with room for scrape
delays. For example, use `--duration=5m --metrics-shutdown-grace=45s` for a
30-second scrape interval. The grace is bounded; it does not wait for a scraper
to connect. Set `--metrics-shutdown-grace=0` to disable it. Negative values are
rejected.

SIGINT or SIGTERM skips or interrupts the grace. Startup and server failures
also exit without waiting for it. HTTP shutdown may then take up to five seconds
to drain active requests. Keep the process running through the grace period if
you need the final scrape; stopping it with a signal can lose that scrape.

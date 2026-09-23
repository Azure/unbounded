#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Summarize persisted live campaign TLS changes and nonzero error counters."""

import json
import sys


def main():
    previous = {}
    with open(sys.argv[1], encoding="utf-8") as observations:
        for line in observations:
            entry = json.loads(line)
            event = entry["event"]
            if event != "sample":
                print(entry["at"], event, "public-generation", entry["trust"]["generation"])
            for worker, data in entry.items():
                if not worker.startswith("worker-"):
                    continue
                status = data.get("status") or {}
                tls = status.get("tls")
                key = (worker, "tls")
                if tls != previous.get(key):
                    print(entry["at"], worker, "TLS", json.dumps(tls, sort_keys=True))
                    previous[key] = tls
                for metric in data.get("metrics", "").splitlines():
                    if not metric.startswith((
                        "racer_dataplane_http_error_responses_total{",
                        "racer_dataplane_http_stream_aborts_total{",
                        "racer_dataplane_http_pressure_failures_total{",
                    )):
                        continue
                    name, value = metric.rsplit(" ", 1)
                    key = (worker, name)
                    count = float(value)
                    if count != previous.get(key, 0):
                        print(entry["at"], worker, metric)
                    previous[key] = count


if __name__ == "__main__":
    main()

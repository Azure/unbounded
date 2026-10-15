"""Offline checks for the fixed-config Direct Zipf dashboard."""

import json
import pathlib
import re
import unittest

ROOT = pathlib.Path(__file__).resolve().parent
DASHBOARD = json.loads((ROOT / "grafana-direct-zipf.json").read_text())


class DashboardTest(unittest.TestCase):
    def test_ten_panels_and_twenty_one_queries(self):
        self.assertEqual(DASHBOARD["uid"], "racer-direct-zipf")
        panels = DASHBOARD["panels"]
        self.assertEqual([p["id"] for p in panels], list(range(1, 11)))
        self.assertEqual(sum(len(p["targets"]) for p in panels), 21)
        for panel in panels:
            self.assertEqual(panel["type"], "timeseries")
            self.assertEqual(panel["datasource"], {"type": "prometheus", "uid": "prometheus"})
            self.assertEqual(panel["fieldConfig"]["defaults"]["noValue"], "No data")
            for target in panel["targets"]:
                self.assertNotIn("or vector(0)", target["expr"])
                self.assertIn('job="', target["expr"])

    def test_tail_queries_and_safeguards(self):
        base = ('job="kubernetes-pods",namespace="${namespace}",'
                'app_kubernetes_io_name="racer-loadgen",'
                'racer_unbounded_cloud_io_benchmark="zipf-16t-v3"')
        success = base + ',result="success"'
        count_rate = ('rate(racer_loadgen_pull_duration_seconds_count{'
                      + success + '}[5m])')
        total = 'sum(' + count_rate + ')'
        latency, thresholds, outcomes, progress = DASHBOARD["panels"][6:]
        self.assertEqual(latency["fieldConfig"]["defaults"]["unit"], "s")
        self.assertEqual([t["legendFormat"] for t in latency["targets"]],
                         ["p50", "p95", "p99", "p99.9"])
        for target, quantile in zip(latency["targets"], ("0.50", "0.95", "0.99", "0.999")):
            self.assertEqual(target["expr"],
                             'histogram_quantile(' + quantile + ', sum by (le) '
                             '(rate(racer_loadgen_pull_duration_seconds_bucket{'
                             + success + '}[5m])))')
        self.assertEqual(thresholds["fieldConfig"]["defaults"]["unit"], "percentunit")
        for target, boundary in zip(thresholds["targets"], ("5.0", "10.0", "30.0")):
            bucket = ('sum(rate(racer_loadgen_pull_duration_seconds_bucket{'
                      + success + ',le="' + boundary + '"}[5m]))')
            self.assertEqual(target["expr"], '(' + total + ' - ' + bucket + ') / ' + total)
            self.assertEqual(target["legendFormat"], '>' + boundary[:-2] + 's')
        self.assertEqual(outcomes["targets"][0]["expr"],
                         'sum by (result) (rate(racer_loadgen_pulls_total{' + base + '}[5m]))')
        self.assertEqual(outcomes["targets"][0]["legendFormat"], "{{result}}")
        for panel, matcher in ((outcomes, {"id": "byRegexp", "options": "^(error|canceled)$"}),
                               (progress, {"id": "byName", "options": "Successful completions / 5m"})):
            override, = panel["fieldConfig"]["overrides"]
            self.assertEqual(override["matcher"], matcher)
            self.assertIn({"id": "custom.axisPlacement", "value": "right"},
                          override["properties"])
        self.assertEqual([t["expr"] for t in progress["targets"]], [
            'sum(racer_loadgen_in_flight{' + base + '})',
            'sum(increase(racer_loadgen_pull_duration_seconds_count{' + success + '}[5m]))',
            'count(' + count_rate + ')',
        ])
        for panel, x, y in ((latency, 0, 36), (thresholds, 12, 36),
                            (outcomes, 0, 45), (progress, 12, 45)):
            self.assertEqual(panel["gridPos"], {"h": 9, "w": 12, "x": x, "y": y})
            for target in panel["targets"]:
                for forbidden in ("or vector(0)", "clamp_min", "gantry_racer_",
                                  "request_duration_seconds", "avg("):
                    self.assertNotIn(forbidden, target["expr"])

    def test_tail_scope_and_missing_data_descriptions(self):
        latency, thresholds, outcomes, progress = DASHBOARD["panels"][6:]
        for text in ("completed 1 GiB", "Not 16 MiB page latency", "interpolation",
                     "300 seconds", "Failed, canceled, and still-running reads",
                     "undefined"):
            self.assertIn(text, latency["description"])
        for text in ("strictly above", "5.0, 10.0, and 30.0", "Zero completions",
                     "undefined", "missing buckets"):
            self.assertIn(text.lower(), thresholds["description"].lower())
        for text in ("Timeouts count as error", "first event", "does not prove health"):
            self.assertIn(text, outcomes["description"])
        for text in ("Still-running reads", "100 contributing loadgens",
                     "absent samples do not", "old samples", "scrape health"):
            self.assertIn(text, progress["description"])
        readme = (ROOT / "grafana-direct-zipf-README.md").read_text()
        for text in ("ten panels and 21 queries", "p99.9", "interpolation",
                     "1 GiB", "300 seconds", "5.0", "10.0", "30.0",
                     "still-running", "Timeouts", "contributing loadgens"):
            self.assertIn(text, readme)

    def test_physical_disk_pair_and_units(self):
        panel = DASHBOARD["panels"][4]
        self.assertEqual(panel["fieldConfig"]["defaults"]["unit"], "bytes")
        selector = ('{job="kubernetes-pods",namespace="${namespace}",'
                    'app_kubernetes_io_name="racer-dataplane"}')
        self.assertEqual([t["expr"] for t in panel["targets"]], [
            "sum(racer_disk_size_bytes" + selector + ")",
            "sum(racer_disk_used_bytes" + selector + ")",
        ])
        self.assertIn("physical", panel["title"])
        self.assertIn("not indexed payload or effective payload capacity", panel["description"])
        self.assertIn("Missing disk scrapes", panel["description"])
        self.assertEqual(panel["gridPos"], {"h": 9, "w": 24, "x": 0, "y": 18})

    def test_cpu_is_average_busy_logical_cores_per_racer_host(self):
        panel = DASHBOARD["panels"][3]
        self.assertEqual(panel["title"], "Average busy logical cores per Racer node")
        defaults = panel["fieldConfig"]["defaults"]
        self.assertEqual(defaults["unit"], "suffix:cores")
        self.assertEqual(defaults["min"], 0)
        self.assertNotIn("max", defaults)
        self.assertEqual(panel["targets"][0]["expr"],
                         'avg(sum by (node) (1 - rate(node_cpu_seconds_total'
                         '{job="node-exporter",mode="idle"}[5m])) '
                         'and on (node) group by (node) '
                         '(up{job="kubernetes-pods",namespace="${namespace}",'
                         'app_kubernetes_io_name="racer-dataplane"}))')
        for text in ("not a percentage or total cluster cores",
                     "Missing host CPU series are not zero",
                     "System-only nodes are excluded", "I/O wait"):
            self.assertIn(text, panel["description"])
        # Only CPU changes units; cache-hit shares remain fractions.
        self.assertEqual(DASHBOARD["panels"][2]["fieldConfig"]["defaults"]["unit"],
                         "percentunit")

    def test_experiment_buffer_descriptions(self):
        readme = (ROOT / "grafana-direct-zipf-README.md").read_text()
        self.assertEqual(DASHBOARD["version"], 10)
        for text in (DASHBOARD["description"], readme):
            self.assertIn("48 GiB plaintext, 64 GiB ciphertext", text)
            self.assertIn("2 GiB each for dirty and", text)
            for stale in ("4 GiB plaintext", "8 GiB plaintext", "32 GiB plaintext",
                          "8 GiB ciphertext", "16 GiB ciphertext"):
                self.assertNotRegex(text, r"\b" + re.escape(stale))

    def test_network_tx_rx_scope_conversion_and_single_interface(self):
        panel = DASHBOARD["panels"][5]
        self.assertEqual(panel["title"], "Total Racer-node network TX/RX (Gbit/s)")
        self.assertEqual(panel["gridPos"], {"h": 9, "w": 24, "x": 0, "y": 27})
        self.assertEqual(panel["fieldConfig"]["defaults"]["unit"], "suffix:Gbit/s")
        self.assertEqual(panel["fieldConfig"]["defaults"]["min"], 0)
        self.assertEqual([t["legendFormat"] for t in panel["targets"]],
                         ["TX (ens1)", "RX (ens1)"])
        join = (' and on (node) group by (node) '
                '(up{job="kubernetes-pods",namespace="${namespace}",'
                'app_kubernetes_io_name="racer-dataplane"})')
        readme = (ROOT / "grafana-direct-zipf-README.md").read_text()
        for target, direction in zip(panel["targets"], ("transmit", "receive")):
            with self.subTest(direction=direction):
                rate = ('rate(node_network_' + direction + '_bytes_total'
                        '{job="node-exporter",device="ens1"}[5m])')
                self.assertEqual(target["expr"], 'sum(' + rate + join + ') * 8 / 1e9')
                # No duplicate interfaces, fleet multiplier, healthy-only gate, or zero fill.
                for forbidden in ('device=~', 'eth0', 'geneve0', '* 100', '== 1',
                                  'or vector(0)', 'clamp_min'):
                    self.assertNotIn(forbidden, target["expr"])
                if direction == "transmit":
                    self.assertIn('count(' + rate + join + ')', readme)
        for text in (panel["description"], " ".join(readme.split())):
            for required in ("non-Racer and external traffic", "not peer-only",
                             "node-pair attribution", "do not add them as unique traffic",
                             "Missing network samples reduce totals"):
                self.assertIn(required.lower(), text.lower())
        self.assertIn("all 100 Racer nodes", panel["description"])
        self.assertIn("down dataplane scrape targets", panel["description"])
        self.assertIn("100 Racer nodes among 103 node-exporter nodes",
                      " ".join(readme.split()))
        self.assertIn("Zero traffic still contributes a series", readme)
        self.assertIn("absent samples do not", readme)

    def test_cache_queries_remain_event_shares(self):
        selector = ('{job="kubernetes-pods",namespace="${namespace}",'
                    'app_kubernetes_io_name="racer-dataplane"}')
        rates = {tier: 'sum(rate(racer_' + tier + '_hits_total' + selector + '[5m]))'
                 for tier in ("memory", "disk", "peer")}
        denominator = '(' + ' + '.join(rates.values()) + ')'
        self.assertEqual([t["expr"] for t in DASHBOARD["panels"][2]["targets"]],
                         [rates[tier] + ' / ' + denominator
                          for tier in ("peer", "disk", "memory")])

    def test_second_sight_retention_descriptions(self):
        readme = (ROOT / "grafana-direct-zipf-README.md").read_text()
        for text in (DASHBOARD["description"], readme):
            with self.subTest(source="dashboard" if text == DASHBOARD["description"] else "readme"):
                normalized = " ".join(text.lower().split())
                for required in ("second-sight local disk retention", "c16",
                                 "racer_admission_mode=second-sight", "runtime default",
                                 "repeated nonowner reads can admit verified ciphertext",
                                 "subject to resource limits", "warm-up, not steady state"):
                    self.assertIn(required, normalized)
                for stale in ("owned-only", "admission disabled", "balanced-config"):
                    self.assertNotIn(stale, normalized)
        self.assertIn("same logical read is not a second sight", readme)
        self.assertIn("does not bypass dirty-buffer limits", readme)

    def test_benchmark_gate_excludes_old_or_unlabeled_loadgens(self):
        selector = ('{job="kubernetes-pods",namespace="${namespace}",'
                    'app_kubernetes_io_name="racer-loadgen",'
                    'racer_unbounded_cloud_io_benchmark="zipf-16t-v3",result="success"}')
        rate = 'rate(racer_loadgen_pulls_total' + selector + '[5m])'
        total = 'sum(' + rate + ') * 1073741824 * 8 / 1e9'
        self.assertEqual(DASHBOARD["panels"][0]["targets"][0]["expr"], total)
        self.assertEqual(DASHBOARD["panels"][1]["targets"][0]["expr"],
                         '(' + total + ') / count(group by (node) '
                         '(up{job="kubernetes-pods",namespace="${namespace}",'
                         'app_kubernetes_io_name="racer-dataplane"}))')
        readme = (ROOT / "grafana-direct-zipf-README.md").read_text()
        # Count only new loadgens with enough successful-counter samples for rate.
        self.assertIn('count(' + rate + ')', readme)
        for text in (json.dumps(DASHBOARD), readme):
            self.assertNotIn("1791576350", text)
            self.assertNotIn("process_start_time_seconds", text)
            self.assertNotIn("2147483648", text)
        self.assertIn("16 TiB catalog", DASHBOARD["title"])
        self.assertIn("16384 x 1073741824-byte", DASHBOARD["description"])
        for text in (DASHBOARD["description"], readme):
            normalized = " ".join(text.split())
            for required in ("zipf-balanced-v2", "exponent 1.1", "32 catalog workers",
                             "catalog hashing", "nonexclusive",
                             "no data", "not zero"):
                self.assertIn(required, normalized)
            for stale in ("not known yet", "before rollout", "120 minutes", "130 minutes"):
                self.assertNotIn(stale, normalized)


if __name__ == "__main__":
    unittest.main()

"""Offline checks for the fixed-config Direct Zipf dashboard."""

import datetime
import json
import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parent
DASHBOARD = json.loads((ROOT / "grafana-direct-zipf.json").read_text())


class DashboardTest(unittest.TestCase):
    def test_five_basic_panels_and_eight_queries(self):
        panels = DASHBOARD["panels"]
        self.assertEqual([p["id"] for p in panels], [1, 2, 3, 4, 5])
        self.assertEqual(sum(len(p["targets"]) for p in panels), 8)
        for panel in panels:
            self.assertEqual(panel["type"], "timeseries")
            self.assertEqual(panel["datasource"], {"type": "prometheus", "uid": "prometheus"})
            self.assertEqual(panel["fieldConfig"]["defaults"]["noValue"], "No data")
            for target in panel["targets"]:
                self.assertNotIn("or vector(0)", target["expr"])
                self.assertIn('job="', target["expr"])

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

    def test_rollout_gate_excludes_old_size_and_missing_process_metrics(self):
        boundary = int(datetime.datetime(2026, 10, 9, 20, 5, 50,
                                        tzinfo=datetime.timezone.utc).timestamp())
        self.assertEqual(boundary, 1791576350)
        gate = ('and on (job, namespace, pod) '
                '(process_start_time_seconds{job="kubernetes-pods",'
                'namespace="${namespace}",app_kubernetes_io_name="racer-loadgen"}'
                f' >= {boundary})')
        for panel in DASHBOARD["panels"][:2]:
            expression = panel["targets"][0]["expr"]
            self.assertIn(gate, expression)
            self.assertIn('* 2147483648 * 8 / 1e9', expression)
            self.assertIn('result="success"}[5m]', expression)
            self.assertNotIn(" bool ", expression)
            self.assertNotIn("536870912", expression)
        self.assertIn("1 TiB catalog", DASHBOARD["title"])
        self.assertIn("512 x 2147483648-byte", DASHBOARD["description"])


if __name__ == "__main__":
    unittest.main()

import copy
import unittest

import ready
import watcher as w
from test_adapters import FakeAPI


class ReadyTest(unittest.TestCase):
    def test_exact_current_fleet_and_reboot_rejection(self):
        api = FakeAPI()
        w.cycle(api, "a", lambda: 100)
        a, _ = w.authority(api.cm, 110)
        proofs = []
        for node in api.nodes:
            node["status"]["nodeInfo"] = dict(bootID="boot-" + node["metadata"]["uid"])
            proofs.append(dict(node=node["metadata"]["name"], nodeUID=node["metadata"]["uid"],
                ip=node["status"]["addresses"][0]["address"], bootID=node["status"]["nodeInfo"]["bootID"],
                sequence=a["sequence"], contentDigest=a["contentDigest"], valid_until=160, verified=110))
        self.assertEqual(1500, ready.fleet(api.cm, proofs, api.nodes, 111)["verified"])
        for change in (lambda p: p.update(bootID="previous"), lambda p: p.update(contentDigest="old"),
                       lambda p: p.update(verified=50), lambda p: p.update(valid_until=110)):
            broken = copy.deepcopy(proofs)
            change(broken[0])
            with self.assertRaises(ValueError):
                ready.fleet(api.cm, broken, api.nodes, 111)


if __name__ == "__main__":
    unittest.main()

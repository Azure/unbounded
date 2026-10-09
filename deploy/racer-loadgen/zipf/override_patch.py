"""Print a scoped merge patch from a live ConfigMap JSON snapshot on stdin."""

import json
import pathlib
import sys

KEY = "racer-zipf.yaml"


def make_patch(current, document):
    metadata = current["metadata"]
    if (current.get("kind"), metadata.get("name"), metadata.get("namespace")) != (
        "ConfigMap", "unbounded-component-overrides", "unbounded-system"
    ):
        raise ValueError("expected the unbounded-system overrides ConfigMap")
    data = current.get("data", {})
    if KEY in data and data[KEY] != document:
        raise ValueError("refusing to replace a different racer-zipf.yaml entry")
    return {
        "metadata": {"resourceVersion": metadata["resourceVersion"]},
        "data": {KEY: document},
    }


if __name__ == "__main__":
    document = pathlib.Path(__file__).with_name("racer-overrides.yaml").read_text()
    print(json.dumps(make_patch(json.load(sys.stdin), document)))

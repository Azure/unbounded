"""Exercise vLLM's real S3 weight iterator and parameter loader on CPU."""

import json
import os
import select
import socket
import socketserver
import threading

import torch
from vllm.model_executor.model_loader.weight_utils import (
    default_weight_loader,
    runai_safetensors_weights_iterator,
)


# The external S3 iterator accepts TCP endpoints only. This test-owned adapter
# forwards bytes unchanged to Racer's Unix socket; Racer exposes no TCP ingress.
class UnixBridge(socketserver.BaseRequestHandler):
    def handle(self):
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as upstream:
            upstream.connect("/dev/racer/s3-cache/cache")
            while True:
                for source in select.select([self.request, upstream], [], [])[0]:
                    data = source.recv(65536)
                    if not data:
                        return
                    (upstream if source is self.request else self.request).sendall(data)


bridge = socketserver.ThreadingTCPServer(("127.0.0.1", 0), UnixBridge)
bridge.daemon_threads = True
threading.Thread(target=bridge.serve_forever, daemon=True).start()
os.environ["AWS_ENDPOINT_URL"] = f"http://127.0.0.1:{bridge.server_address[1]}"

model = torch.nn.Linear(1024, 1024)
parameters = dict(model.named_parameters())
loaded = set()
for name, tensor in runai_safetensors_weights_iterator(
    ["s3://models/model.safetensors"], use_tqdm_on_load=False
):
    assert name not in loaded, f"duplicate tensor: {name}"
    default_weight_loader(parameters[name], tensor)
    loaded.add(name)
assert loaded == set(parameters), f"missing weights: {set(parameters) - loaded}"

expected_weight = (torch.arange(1024 * 1024) % 97).float().reshape(1024, 1024) / 16
expected_bias = torch.arange(1024).float() / 16
torch.testing.assert_close(model.weight, expected_weight, rtol=0, atol=0)
torch.testing.assert_close(model.bias, expected_bias, rtol=0, atol=0)
with torch.no_grad():
    output = model(torch.ones(1, 1024))
torch.testing.assert_close(
    output, expected_weight.sum(dim=1).unsqueeze(0) + expected_bias, rtol=0, atol=0
)
print(json.dumps({"loaded": sorted(loaded), "output_sum": output.sum().item()}))

# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Register an explicit-file loader in every vLLM worker through its plugin API."""

import copy
import os
from urllib.parse import urlsplit


def register():
    from vllm.model_executor.model_loader import register_model_loader
    from vllm.model_executor.model_loader.runai_streamer_loader import (
        RunaiModelStreamerLoader,
    )

    @register_model_loader("racer_object")
    class RacerObjectLoader(RunaiModelStreamerLoader):
        def __init__(self, load_config):
            config = copy.deepcopy(load_config)
            extra = dict(config.model_loader_extra_config or {})
            self.files = extra.pop("files", None)
            if not isinstance(self.files, list) or not self.files:
                raise ValueError("racer_object requires a nonempty explicit files list")
            for path in self.files:
                if not isinstance(path, str):
                    raise ValueError("files must contain S3 URI strings")
                uri = urlsplit(path)
                if (uri.scheme != "s3" or not uri.netloc or uri.path in ("", "/")
                        or uri.query or uri.fragment or uri.username):
                    raise ValueError(f"expected an explicit s3://bucket/key URI: {path!r}")
            if len(set(self.files)) != len(self.files):
                raise ValueError("duplicate weight files")
            config.model_loader_extra_config = extra
            # The pinned base loader only copies this inside its extra-config block.
            if "AWS_ENDPOINT_URL" in os.environ:
                os.environ.setdefault("RUNAI_STREAMER_S3_ENDPOINT", os.environ["AWS_ENDPOINT_URL"])
            super().__init__(config)

        def _prepare_weights(self, model_name_or_path, revision):
            # Local config and tokenizer are required. Never discover remote files.
            if not os.path.isdir(model_name_or_path):
                raise ValueError("racer_object requires a local model/config directory")
            return list(self.files)

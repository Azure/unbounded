# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Checkpoint tensor filters applied before read planning.

Only explicit rules live here: vLLM's unused-weight hooks, expert locality,
and named draft-model rules. Nothing is skipped by guessing.
"""

from __future__ import annotations

import re

from . import compat

_LAYER = re.compile(r"^model\.layers\.(\d+)\.")

# vLLM classes of the DeepSeek-style MTP draft model (used by GLM MoE drafts).
DEEPSEEK_MTP_CLASSES = frozenset({"DeepSeekMTP", "DeepseekMTP"})


def _num_hidden_layers(model):
    cfg = getattr(model, "config", None)
    for c in (cfg, getattr(cfg, "text_config", None)):
        n = getattr(c, "num_hidden_layers", None)
        if isinstance(n, int):
            return n
    return None


def is_deepseek_mtp(model) -> bool:
    cls = type(model)
    return cls.__name__ in DEEPSEEK_MTP_CLASSES or cls.__module__.endswith(".deepseek_mtp")


def draft_rule(model):
    """A name -> keep predicate for known draft classes, or None."""
    if not is_deepseek_mtp(model):
        return None
    n = _num_hidden_layers(model)
    if n is None:
        return None

    def keep(name: str) -> bool:
        m = _LAYER.match(name)
        return m is None or int(m.group(1)) >= n

    return keep


def keep_predicate(model, role: str, source_is_unused=None, local_expert_ids=None):
    """Return (name, dtype, shape) -> bool for checkpoint tensor names."""
    rules = []
    if source_is_unused is not None:
        rules.append(lambda name: not source_is_unused(name))
    elif compat.model_has_is_unused_checkpoint_weight(model):
        rules.append(lambda name: not model.is_unused_checkpoint_weight(name))
    if role == "draft":
        rule = draft_rule(model)
        if rule is not None:
            rules.append(rule)
    ep = compat.expert_filter(local_expert_ids)
    if ep is not None:
        rules.append(ep)

    def keep(name: str, dtype: str = "", shape: tuple = ()) -> bool:
        return all(r(name) for r in rules)

    return keep

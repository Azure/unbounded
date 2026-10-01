#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

kubectl label nodes --all unbounded-cloud.io/storage-ring=ring1 --overwrite

#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

kubectl label nodes --all unbounded-cloud.io/storage-ring=ring1 --overwrite

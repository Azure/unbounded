// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import (
	"log"
)

func main() { log.Fatal("racer-object requires Linux splice(2)") }

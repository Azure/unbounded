// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racersdk defines validated Racer client/origin protocol values.
//
// Keys use canonical lowercase hexadecimal. Opaque fetch credentials are immutable
// and redacted in diagnostic formatting; explicitly extracting their contents is
// intended only for forwarding to an origin. Validation errors never echo input.
//
// The package currently supplies types and protocol foundations. Streaming client
// and origin serving entry points are implemented in the next design step.
package racersdk

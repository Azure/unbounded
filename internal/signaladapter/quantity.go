// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"
)

func resourceQuantity(value int64) *resource.Quantity {
	quantity := resource.MustParse(strconv.FormatInt(value, 10))

	return &quantity
}

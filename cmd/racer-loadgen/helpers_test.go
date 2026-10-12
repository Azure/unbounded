// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import "net/http"

func (img *syntheticImage) handler() http.Handler {
	return catalogFromImages([]*syntheticImage{img}).handler()
}

func (t *catalogTraversal) nextImage(images []*syntheticImage) *syntheticImage {
	return images[t.nextIndex(len(images))]
}

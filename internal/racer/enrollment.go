// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func (s *Server) enroll(ctx context.Context, r *http.Request, request wire.BootstrapRequest) ([]byte, error) {
	response, hint, err := s.authority.EnrollWithHint(ctx, r, request)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithDeadline(ctx, hint.Expires)
	defer cancel()

	if err := annotateEnrollment(ctx, s.writer, hint); err != nil {
		return nil, err
	}

	return response, nil
}

func annotateEnrollment(ctx context.Context, writer client.Writer, hint authority.EnrollmentHint) error {
	node := hint.Node.DeepCopy()
	value := strconv.FormatUint(uint64(hint.Shares), 10)

	nics, err := json.Marshal(hint.RDMANICs)
	if err != nil {
		return err
	}

	nicValue := string(nics)
	if len(hint.RDMANICs) == 0 {
		nicValue = ""
	}

	_, nicPresent := node.Annotations[enrolledRDMANICsAnnotation]
	if node.Annotations[enrolledSharesAnnotation] == value && node.Annotations[enrolledRDMANICsAnnotation] == nicValue && (nicValue != "" || !nicPresent) {
		return nil
	}

	before := node.DeepCopy()
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}

	node.Annotations[enrolledSharesAnnotation] = value
	if nicValue == "" {
		delete(node.Annotations, enrolledRDMANICsAnnotation)
	} else {
		node.Annotations[enrolledRDMANICsAnnotation] = nicValue
	}

	return writer.Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

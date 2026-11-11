// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnode

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/phases"
)

func TestFuncIsATaskWithItsNameAndBody(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")

	var ran int

	task := Func("do-things", func(context.Context) error { ran++; return boom })

	assert.Equal(t, "do-things", task.Name())
	require.ErrorIs(t, task.Do(t.Context()), boom)
	assert.Equal(t, 1, ran)
}

func TestFuncRunsInAGroupAndNamesTheFailure(t *testing.T) {
	t.Parallel()

	err := phases.Serial(quiet(),
		Func("first", func(context.Context) error { return nil }),
		Func("second", func(context.Context) error { return errors.New("nope") }),
	).Do(t.Context())

	require.ErrorContains(t, err, "second")
}

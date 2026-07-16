// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNormalizeInventoryAnnotationValue(t *testing.T) {
	got, err := normalizeInventoryAnnotationValue([]byte(" /dev/sdb?name=sdb&size_bytes=4096\n"))
	require.NoError(t, err)
	assert.Equal(t, "/dev/sdb?name=sdb&size_bytes=4096", got)
}

func TestNormalizeInventoryAnnotationValueRejectsInvalidFormat(t *testing.T) {
	for _, raw := range []string{"?name=sdb", "/dev/sdb?=sdb", "/dev/sdb?name=", "/dev/sdb?name=%zz"} {
		t.Run(raw, func(t *testing.T) {
			_, err := normalizeInventoryAnnotationValue([]byte(raw))
			require.Error(t, err)
		})
	}
}

func TestPatchNodeAnnotationIfChanged(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(node("self", "red", "10.0.0.1"))
	value := "/dev/sdb?name=sdb&size_bytes=4096"

	changed, err := patchNodeAnnotationIfChanged(ctx, cs, "self", storageBlockDevicesAnnotation, value)
	require.NoError(t, err)
	assert.True(t, changed)

	n, err := cs.CoreV1().Nodes().Get(ctx, "self", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, value, n.Annotations[storageBlockDevicesAnnotation])

	changed, err = patchNodeAnnotationIfChanged(ctx, cs, "self", storageBlockDevicesAnnotation, value)
	require.NoError(t, err)
	assert.False(t, changed)
}

func TestBlockInventoryPublisherPublishesAnnotation(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(node("self", "red", "10.0.0.1"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("/dev/sdb?name=sdb&size_bytes=4096\n"))
	}))
	defer server.Close()

	p := &blockInventoryPublisher{
		clientset:  cs,
		nodeName:   "self",
		url:        server.URL + "/inventory/block",
		httpClient: server.Client(),
	}

	changed, err := p.publishOnce(ctx)
	require.NoError(t, err)
	assert.True(t, changed)

	n, err := cs.CoreV1().Nodes().Get(ctx, "self", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "/dev/sdb?name=sdb&size_bytes=4096", n.Annotations[storageBlockDevicesAnnotation])

	changed, err = p.publishOnce(ctx)
	require.NoError(t, err)
	assert.False(t, changed)
}

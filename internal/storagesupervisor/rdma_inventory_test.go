// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
	got, err := normalizeInventoryAnnotationValue([]byte(" mlx5_0?addr=hex%3A01,/dev/sdb?name=sdb&size_bytes=4096\n"))
	require.NoError(t, err)

	assert.Equal(t, "mlx5_0?addr=hex%3A01,/dev/sdb?name=sdb&size_bytes=4096", got)
}

func TestNormalizeInventoryAnnotationValueRejectsInvalidFormat(t *testing.T) {
	for _, raw := range []string{
		"?addr=hex%3A01",
		"mlx5_0?=hex%3A01",
		"mlx5_0?addr=",
		"mlx5_0?addr=%zz",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := normalizeInventoryAnnotationValue([]byte(raw))
			require.Error(t, err)
		})
	}
}

func TestFirstRdmaInventoryAddr(t *testing.T) {
	addr, err := firstRdmaInventoryAddr("mlx5_0,mlx5_1?addr=hex%3A02&addr=hex%3A03")
	require.NoError(t, err)
	assert.Equal(t, "hex:02", addr)

	addr, err = firstRdmaInventoryAddr("mlx5_0")
	require.NoError(t, err)
	assert.Empty(t, addr)
}

func TestPatchNodeAnnotationIfChanged(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(node("self", "red", "10.0.0.1"))
	value := "mlx5_0?addr=hex%3A01"

	changed, err := patchNodeAnnotationIfChanged(ctx, cs, "self", storageRdmaHcasAnnotation, value)
	require.NoError(t, err)
	assert.True(t, changed)

	n, err := cs.CoreV1().Nodes().Get(ctx, "self", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, value, n.Annotations[storageRdmaHcasAnnotation])

	changed, err = patchNodeAnnotationIfChanged(ctx, cs, "self", storageRdmaHcasAnnotation, value)
	require.NoError(t, err)
	assert.False(t, changed)
}

func TestDeviceInventoryPublisherPublishesAnnotations(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(node("self", "red", "10.0.0.1"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/inventory/rdma":
			_, _ = w.Write([]byte("mlx5_0?addr=hex%3A01\n"))
		case "/inventory/block":
			_, _ = w.Write([]byte("/dev/sdb?name=sdb&size_bytes=4096\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p := &deviceInventoryPublisher{
		clientset: cs,
		nodeName:  "self",
		baseURL:   server.URL + "/inventory",
		endpoints: []inventoryEndpoint{
			{annotation: storageRdmaHcasAnnotation, url: server.URL + "/inventory/rdma"},
			{annotation: storageBlockDevicesAnnotation, url: server.URL + "/inventory/block"},
		},
		httpClient: server.Client(),
	}

	changed, err := p.publishOnce(ctx)
	require.NoError(t, err)
	assert.True(t, changed)

	n, err := cs.CoreV1().Nodes().Get(ctx, "self", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "mlx5_0?addr=hex%3A01", n.Annotations[storageRdmaHcasAnnotation])
	assert.Equal(t, "/dev/sdb?name=sdb&size_bytes=4096", n.Annotations[storageBlockDevicesAnnotation])

	changed, err = p.publishOnce(ctx)
	require.NoError(t, err)
	assert.False(t, changed)
}

func TestInventoryURLForPath(t *testing.T) {
	got, err := inventoryURLForPath("http://127.0.0.1:9100/inventory?ignored=1", "/rdma")
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9100/inventory/rdma", got)
}

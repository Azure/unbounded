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

func TestNormalizeRdmaInventoryJSON(t *testing.T) {
	got, inv, err := normalizeRdmaInventoryJSON([]byte(`{
		"schemaVersion": 1,
		"hcas": [{"name": "mlx5_0", "addrs": ["hex:01"]}]
	}`))
	require.NoError(t, err)

	assert.Equal(t, `{"schemaVersion":1,"hcas":[{"name":"mlx5_0","addrs":["hex:01"]}]}`, got)
	require.Len(t, inv.HCAs, 1)
	assert.Equal(t, "mlx5_0", inv.HCAs[0].Name)
}

func TestNormalizeRdmaInventoryJSONRejectsInvalidSchema(t *testing.T) {
	_, _, err := normalizeRdmaInventoryJSON([]byte(`{"schemaVersion":2,"hcas":[]}`))
	require.Error(t, err)

	_, _, err = normalizeRdmaInventoryJSON([]byte(`{"schemaVersion":1}`))
	require.Error(t, err)
}

func TestFirstRdmaInventoryAddr(t *testing.T) {
	addr, err := firstRdmaInventoryAddr(`{"schemaVersion":1,"hcas":[{"name":"mlx5_0","addrs":[]},{"name":"mlx5_1","addrs":["hex:02","hex:03"]}]}`)
	require.NoError(t, err)
	assert.Equal(t, "hex:02", addr)

	addr, err = firstRdmaInventoryAddr(`{"schemaVersion":1,"hcas":[]}`)
	require.NoError(t, err)
	assert.Empty(t, addr)
}

func TestPatchNodeAnnotationIfChanged(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(node("self", "red", "10.0.0.1"))

	changed, err := patchNodeAnnotationIfChanged(ctx, cs, "self", storageRdmaHcasAnnotation, `{"schemaVersion":1,"hcas":[]}`)
	require.NoError(t, err)
	assert.True(t, changed)

	n, err := cs.CoreV1().Nodes().Get(ctx, "self", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, `{"schemaVersion":1,"hcas":[]}`, n.Annotations[storageRdmaHcasAnnotation])

	changed, err = patchNodeAnnotationIfChanged(ctx, cs, "self", storageRdmaHcasAnnotation, `{"schemaVersion":1,"hcas":[]}`)
	require.NoError(t, err)
	assert.False(t, changed)
}

func TestRdmaInventoryPublisherPublishesMinifiedJSON(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(node("self", "red", "10.0.0.1"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/inventory/rdma", r.URL.Path)

		_, _ = w.Write([]byte(`{"schemaVersion": 1, "hcas": []}`))
	}))
	defer server.Close()

	p := &rdmaInventoryPublisher{
		clientset:  cs,
		nodeName:   "self",
		url:        server.URL + "/inventory/rdma",
		httpClient: server.Client(),
	}

	changed, err := p.publishOnce(ctx)
	require.NoError(t, err)
	assert.True(t, changed)

	n, err := cs.CoreV1().Nodes().Get(ctx, "self", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, `{"schemaVersion":1,"hcas":[]}`, n.Annotations[storageRdmaHcasAnnotation])
}

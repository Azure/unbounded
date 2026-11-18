// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodeinfo

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const annotation = "centerpivot.unbounded-cloud.io/agent-info"

// hostFiles writes a host's machine-id and os-release, and returns a
// publisher reading them.
func hostFiles(t *testing.T, client *fake.Clientset, version string) *Publisher {
	t.Helper()

	dir := t.TempDir()
	machineID, osRelease := filepath.Join(dir, "machine-id"), filepath.Join(dir, "os-release")
	require.NoError(t, os.WriteFile(machineID, []byte("34a267d412544d6a9e97b5ba6762f6e7\n"), 0o600))
	require.NoError(t, os.WriteFile(osRelease,
		[]byte("NAME=\"Microsoft Azure Container Linux\"\nVERSION_ID="+version+"\nBUILD_ID=1247604\n"), 0o600))

	return &Publisher{
		Client: client, NodeName: "acl-1", MachineIDPath: machineID, OSReleasePath: osRelease,
		VersionKey: "VERSION_ID", Annotation: annotation, Log: logr.Discard(),
	}
}

func TestPublish(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "acl-1"}})
	publisher := hostFiles(t, client, "3.0.20261104")

	require.NoError(t, publisher.Publish(t.Context()))

	node, err := client.CoreV1().Nodes().Get(t.Context(), "acl-1", metav1.GetOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"schemaVersion": "1.0",
		"machineId": "ebb9a2bb-9365-d34e-133f-8da0664e7366",
		"currentVersion": "3.0.20261104"
	}`, node.Annotations[annotation])

	client.ClearActions()
	require.NoError(t, publisher.Publish(t.Context()))

	for _, action := range client.Actions() {
		_, patched := action.(k8stesting.PatchAction)
		assert.False(t, patched, "an unchanged agent-info isn't written again")
	}

	publisher = hostFiles(t, client, "3.0.20261106")
	require.NoError(t, publisher.Publish(t.Context()))

	node, err = client.CoreV1().Nodes().Get(t.Context(), "acl-1", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, node.Annotations[annotation], `"currentVersion":"3.0.20261106"`, "a new version after a restart")
}

func TestPublishNeedsTheHostsFiles(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "acl-1"}})
	publisher := hostFiles(t, client, "3.0.20261104")
	publisher.MachineIDPath = filepath.Join(t.TempDir(), "machine-id")

	assert.Error(t, publisher.Publish(t.Context()), "the host agent hasn't bound the machine ID")
}

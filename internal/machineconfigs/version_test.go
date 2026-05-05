// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineconfigs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestResolveVersionFromRef_PinnedVersion(t *testing.T) {
	t.Parallel()

	c := fakeMachineConfigClient(
		machineConfigurationVersion("config-a", 1),
		machineConfigurationVersion("config-a", 2),
	)

	mcv, err := ResolveVersionFromRef(
		context.Background(),
		c,
		&v1alpha3.MachineConfigurationRef{Name: "config-a", Version: ptr.To(int32(1))},
	)
	require.NoError(t, err)
	require.NotNil(t, mcv)
	assert.Equal(t, "config-a-v1", mcv.Name)
	assert.Equal(t, int32(1), mcv.Spec.Version)
}

func TestResolveVersionFromRef_OmittedVersionUsesLatest(t *testing.T) {
	t.Parallel()

	c := fakeMachineConfigClient(
		machineConfigurationVersion("config-a", 1),
		machineConfigurationVersion("config-a", 3),
		machineConfigurationVersion("config-a", 2),
		machineConfigurationVersion("config-b", 9),
	)

	mcv, err := ResolveVersionFromRef(
		context.Background(),
		c,
		&v1alpha3.MachineConfigurationRef{Name: "config-a"},
	)
	require.NoError(t, err)
	require.NotNil(t, mcv)
	assert.Equal(t, "config-a-v3", mcv.Name)
	assert.Equal(t, int32(3), mcv.Spec.Version)
}

func TestResolveLatestVersion(t *testing.T) {
	t.Parallel()

	c := fakeMachineConfigClient(
		machineConfigurationVersion("config-a", 1),
		machineConfigurationVersion("config-a", 5),
		machineConfigurationVersion("config-a", 4),
		machineConfigurationVersion("config-b", 9),
	)

	mcv, err := ResolveLatestVersion(context.Background(), c, "config-a")
	require.NoError(t, err)
	require.NotNil(t, mcv)
	assert.Equal(t, "config-a-v5", mcv.Name)
	assert.Equal(t, int32(5), mcv.Spec.Version)
}

func TestResolveLatestVersion_NotFound(t *testing.T) {
	t.Parallel()

	c := fakeMachineConfigClient(machineConfigurationVersion("config-b", 1))

	mcv, err := ResolveLatestVersion(context.Background(), c, "config-a")
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))
	assert.Nil(t, mcv)
}

func fakeMachineConfigClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(scheme); err != nil {
		panic(err)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}

func machineConfigurationVersion(
	configurationName string,
	version int32,
) *v1alpha3.MachineConfigurationVersion {
	return &v1alpha3.MachineConfigurationVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: v1alpha3.MachineConfigurationVersionName(configurationName, version),
			Labels: map[string]string{
				v1alpha3.MCVConfigurationLabelKey: configurationName,
			},
		},
		Spec: v1alpha3.MachineConfigurationVersionSpec{
			Version: version,
		},
	}
}

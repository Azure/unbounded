// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRDMANICStrictFieldsAndBounds(t *testing.T) {
	valid := `{"device":"mlx5_0","port":1,"rail":0}`
	for _, bad := range []string{
		`{"port":1,"rail":0}`, `{"device":"a","rail":0}`, `{"device":"a","port":1}`,
		strings.Replace(valid, `"port":1`, `"port":0`, 1),
		strings.Replace(valid, `"port":1`, `"port":256`, 1),
		strings.Replace(valid, `"port":1`, `"port":1.0`, 1),
		strings.Replace(valid, `"port":1`, `"port":"1"`, 1),
		strings.Replace(valid, `"port":1`, `"port":-1`, 1),
		strings.Replace(valid, `"port":1`, `"port":1,"port":2`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":65536`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":0,"gid":""`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":0,"gid":null`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":0,"gid":"ABCDEF0123456789abcdef0123456789"`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":0,"gid":"0123"`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":0,"gid":"gggggggggggggggggggggggggggggggg"`, 1),
		strings.Replace(valid, `"rail":0`, `"rail":0,"fabric":"old"`, 1),
	} {
		t.Run(bad, func(t *testing.T) {
			_, err := DecodeRDMANICs(strings.NewReader("[" + bad + "]"))
			require.ErrorIs(t, err, InvalidRequest)
		})
	}

	nics, err := DecodeRDMANICs(strings.NewReader(`[{"device":"mlx5_0","port":255,"rail":65535,"gid":"abcdef0123456789abcdef0123456789","numa_node":4294967295}]`))
	require.NoError(t, err)
	require.Equal(t, uint8(255), nics[0].Port)

	for _, count := range []int{64, 65} {
		nics := make([]RDMANIC, count)
		for i := range nics {
			nics[i] = RDMANIC{Device: fmt.Sprintf("mlx5_%d", i), Port: 1}
		}

		raw, err := json.Marshal(nics)
		require.NoError(t, err)

		_, err = DecodeRDMANICs(bytes.NewReader(raw))
		if count == 64 {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, TooLarge)
		}
	}
}

func TestBootstrapRDMANICRequiredCanonicalAndBounded(t *testing.T) {
	request, err := DecodeBootstrap(bytes.NewReader(fixture(t, "bootstrap-request.json")))
	require.NoError(t, err)

	request.RDMANICs = []RDMANIC{{Device: "b", Port: 2, Rail: 1}, {Device: "a", Port: 1, Rail: 1}}
	raw, err := EncodeBootstrapRequest(request)
	require.NoError(t, err)
	decoded, err := DecodeBootstrap(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, "a", decoded.RDMANICs[0].Device)
	require.Equal(t, "b", request.RDMANICs[0].Device)

	for _, replacement := range []string{`"rails":`, `"Rdma_nics":`} {
		_, err := DecodeBootstrap(bytes.NewReader(bytes.Replace(raw, []byte(`"rdma_nics":`), []byte(replacement), 1)))
		require.ErrorIs(t, err, InvalidRequest)
	}

	request.RDMANICs = append(request.RDMANICs, request.RDMANICs[0])
	_, err = EncodeBootstrapRequest(request)
	require.ErrorIs(t, err, InvalidRequest)

	request.RDMANICs = []RDMANIC{{Device: strings.Repeat("x", MaxBootstrapBytes), Port: 1}}
	require.ErrorIs(t, ValidateBootstrapRequest(request), TooLarge)
	request.RDMANICs = make([]RDMANIC, 65)
	require.ErrorIs(t, ValidateBootstrapRequest(request), TooLarge)
	request.RDMANICs = nil
	raw, err = EncodeBootstrapRequest(request)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"rdma_nics":[]`)
	_, err = DecodeBootstrap(bytes.NewReader(bytes.Replace(raw, []byte(`"rdma_nics":[],`), nil, 1)))
	require.ErrorIs(t, err, InvalidRequest)
}

func TestAdmittedMemberMigrationAndHardBreak(t *testing.T) {
	legacy := `{"node":"22222222-2222-4222-8222-222222222222","shares":4,"peer_endpoint":"192.0.2.1:7443","rails":[{"rail":0,"fabric":"old"}],"alignment_enabled":false,"site":""}`
	member, err := DecodeAdmittedMember(strings.NewReader(legacy))
	require.NoError(t, err)
	require.Empty(t, member.RDMANICs)
	member.RDMANICs = []RDMANIC{{Device: "a", Port: 1}}
	raw, err := json.Marshal(member)
	require.NoError(t, err)
	_, err = DecodeAdmittedMember(bytes.NewReader(raw))
	require.NoError(t, err)

	for _, bad := range []string{
		strings.Replace(string(raw), `"port":1`, `"port":0`, 1),
		strings.Replace(string(raw), `"port":1`, `"port":1,"port":2`, 1),
		strings.Replace(string(raw), `"rdma_nics":[{"device":"a","port":1,"rail":0}]`, `"rdma_nics":null`, 1),
		strings.Replace(string(raw), `"device":"a"`, `"device":"a","unknown":1`, 1),
	} {
		_, err = DecodeAdmittedMember(strings.NewReader(bad))
		require.ErrorIs(t, err, InvalidRequest)
	}

	_, err = DecodePublication(strings.NewReader(`{"schema_version":1,"cluster":"11111111-1111-4111-8111-111111111111","sequence":"1","membership_version":"1","members":[` + legacy + `],"caches":[]}`))
	require.ErrorIs(t, err, InvalidRequest)
}

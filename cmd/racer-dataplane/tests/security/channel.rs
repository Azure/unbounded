// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

impl super::TlsChannel {
    pub(crate) fn assert_offload_for_test(&self) {
        crate::tls::tests::assert_offload(&self.session);
    }
}

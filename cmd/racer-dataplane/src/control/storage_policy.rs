// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Independent, process-local storage request mailbox. No slab mutation occurs
//! here. The runtime consumes the latest request and reports its actual outcome.
use super::{Updates, proto};

const ALIGNMENT: u64 = 4 << 20;
const MIN_BYTES: u64 = 32 << 20;
const MAX_BYTES: u64 = (i64::MAX as u64 / ALIGNMENT) * ALIGNMENT;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct StorageRequest {
    pub identity: [u8; 32],
    pub version: u64,
    pub desired_bytes: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum StorageResult {
    Pending,
    Applied,
    Failed(String),
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct StoragePolicyStatus {
    pub desired: Option<StorageRequest>,
    pub result: Option<StorageResult>,
    pub applied_bytes: u64,
    pub validation_error: Option<String>,
}

#[derive(Default)]
pub(super) struct State {
    status: StoragePolicyStatus,
}

fn valid_bytes(bytes: u64) -> bool {
    (MIN_BYTES..=MAX_BYTES).contains(&bytes) && bytes.is_multiple_of(ALIGNMENT)
}

impl Updates {
    /// Coalesced desired capacity. Equal versions are idempotent; runtime work
    /// should compare the whole request and report against that exact request.
    pub fn desired_storage(&self) -> Option<StorageRequest> {
        self.storage.lock().unwrap().status.desired.clone()
    }

    /// Internal integration data for the runtime and future status surfaces.
    pub fn storage_policy_status(&self) -> StoragePolicyStatus {
        self.storage.lock().unwrap().status.clone()
    }

    /// Returns false for a superseded request or invalid outcome. Applied is
    /// accepted only for the exact target; pending/failure may retain the prior
    /// actual capacity (zero means not yet known). Receipt alone is never applied.
    pub fn report_storage(
        &self,
        request: &StorageRequest,
        result: StorageResult,
        applied_bytes: u64,
    ) -> bool {
        let mut storage = self.storage.lock().unwrap();
        if storage.status.desired.as_ref() != Some(request)
            || (applied_bytes != 0 && !valid_bytes(applied_bytes))
            || (result == StorageResult::Applied && applied_bytes != request.desired_bytes)
        {
            return false;
        }
        let result = match result {
            StorageResult::Failed(message) => {
                StorageResult::Failed(message.chars().take(1024).collect())
            }
            result => result,
        };
        storage.status.result = Some(result);
        storage.status.applied_bytes = applied_bytes;
        true
    }

    // Only the subscriber calls this, after signature, bootstrap, boot, profile
    // and Pod verification. It deliberately does not return a topology error.
    pub(super) fn receive_storage_policy(&self, command: &proto::ControlCommand) {
        let Some(policy) = &command.storage_policy else {
            return;
        };
        let mut storage = self.storage.lock().unwrap();
        let status = &mut storage.status;
        let error = if command.pod_uid.is_empty() {
            Some("storage policy missing Pod identity")
        } else if policy.identity.len() != 32
            || policy.version == 0
            || !valid_bytes(policy.desired_bytes)
        {
            Some("invalid storage policy")
        } else if let Some(old) = &status.desired {
            if policy.identity.as_slice() != old.identity {
                Some("storage policy identity changed within process")
            } else if policy.version < old.version {
                Some("stale storage policy version")
            } else if policy.version == old.version && policy.desired_bytes != old.desired_bytes {
                Some("storage policy version reused with different bytes")
            } else {
                None
            }
        } else {
            None
        };
        if let Some(error) = error {
            status.validation_error = Some(error.into());
            return;
        }
        status.validation_error = None;
        if status
            .desired
            .as_ref()
            .is_some_and(|old| old.version == policy.version)
        {
            return;
        }
        status.desired = Some(StorageRequest {
            identity: policy.identity.as_slice().try_into().unwrap(),
            version: policy.version,
            desired_bytes: policy.desired_bytes,
        });
        status.result = Some(StorageResult::Pending);
        drop(storage);
        self.wake_all();
    }

    pub(super) fn storage_headers(&self) -> Vec<(&'static str, String)> {
        let status = self.storage_policy_status();
        let mut headers = vec![("X-Racer-Storage-Policy", "1".into())];
        if let Some(request) = status.desired {
            let state = match status.result {
                Some(StorageResult::Applied) => "applied",
                Some(StorageResult::Failed(_)) => "failed",
                _ => "pending",
            };
            headers.extend([
                (
                    "X-Racer-Storage-Identity",
                    request
                        .identity
                        .iter()
                        .map(|b| format!("{b:02x}"))
                        .collect(),
                ),
                ("X-Racer-Storage-Version", request.version.to_string()),
                ("X-Racer-Storage-State", state.into()),
                (
                    "X-Racer-Storage-Applied-Bytes",
                    status.applied_bytes.to_string(),
                ),
            ]);
        }
        headers
    }
}

#[cfg(test)]
#[path = "../../tests/control/storage_policy.rs"]
mod tests;

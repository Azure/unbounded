// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Typed harness observations and named failures, separate from diagnostic strings.
use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Serialize, Deserialize)]
pub(crate) enum Transition {
    Invoke {
        request: u64,
        target: String,
        head: bool,
    },
    Response {
        request: u64,
        status: u16,
    },
    Cancel {
        request: u64,
    },
    ProcessLost {
        request: u64,
    },
    Publish {
        revision: u64,
    },
    FaultArmed {
        fault: usize,
        target: String,
    },
    FaultEffective {
        fault: usize,
    },
    FaultReleased {
        fault: usize,
    },
    MutantActivated {
        mutant: Mutant,
    },
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub(crate) enum Mutant {
    SuccessfulGetStatus,
}

#[derive(Debug, Serialize)]
pub(crate) struct Failure {
    pub oracle: &'static str,
    pub detail: String,
}

pub(crate) fn require(ok: bool, oracle: &'static str, detail: impl Into<String>) {
    if !ok {
        std::panic::panic_any(Failure {
            oracle,
            detail: detail.into(),
        });
    }
}

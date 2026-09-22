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
    JoinedFlight {
        target: String,
    },
    FlightProducerCanceled {
        consumers: usize,
    },
    LocalFailureSubmitted {
        cause: String,
        initiated: bool,
    },
    RemoteFailureSubmitted {
        cause: String,
    },
    UnconfirmedRequestAttempt {
        confirmed: bool,
    },
    ConfirmedRequestAdmitted {
        confirmed: bool,
    },
    NamespaceActivated {
        generation: u64,
    },
    NamespaceColdFetch {
        target: String,
    },
    DurabilityWitness {
        target: String,
    },
    DirtyCheckpointCrash {
        dirty: usize,
        persisted: Vec<u64>,
    },
    DurableRecovery {
        target: String,
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
    StaleNamespaceSelection,
    SkipCheckpointDataSync,
    SkipCanceledFlightAccounting,
    LocalFailureAsRemote,
    UnconfirmedSessionAdmission,
}

#[derive(Debug, Serialize)]
pub(crate) struct Failure {
    pub oracle: &'static str,
    pub detail: String,
}

pub(crate) fn require(ok: bool, oracle: &'static str, detail: impl Into<String>) {
    if !ok {
        let failure = Failure {
            oracle,
            detail: detail.into(),
        };
        eprintln!("oracle failure: {}: {}", failure.oracle, failure.detail);
        std::panic::panic_any(failure);
    }
}

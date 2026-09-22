// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Routing capabilities only. Byte, dependency, deadline, and resource checks
//! cannot be disabled through this declaration.
#[derive(Clone, Copy, Debug, serde::Serialize)]
pub(super) enum Oracle {
    HealthyRecovery,
    HttpGraph,
    HttpRank,
    RdmaRank,
    OriginPlacement,
}

const ALL: [Oracle; 5] = [
    Oracle::HealthyRecovery,
    Oracle::HttpGraph,
    Oracle::HttpRank,
    Oracle::RdmaRank,
    Oracle::OriginPlacement,
];

#[derive(Clone, Copy, serde::Serialize)]
pub(in crate::runtime) enum Check {
    IndependentCanonical,
    FixtureReplacement {
        reason: &'static str,
        check: &'static str,
        scope: &'static str,
        independence: &'static str,
    },
}

#[derive(Clone, Copy, serde::Serialize)]
pub(in crate::runtime) struct Capabilities(pub(in crate::runtime) [Check; 5]);

impl Capabilities {
    pub(super) const CANONICAL: Self = Self([Check::IndependentCanonical; 5]);

    pub(super) fn validate(self) {
        for check in self.0 {
            if let Check::FixtureReplacement {
                reason,
                check,
                scope,
                independence,
            } = check
            {
                assert!(
                    [reason, check, scope, independence]
                        .iter()
                        .all(|field| !field.trim().is_empty()),
                    "oracle replacement requires reason, check, scope, and independence"
                );
            }
        }
    }

    pub(super) fn canonical(self, oracle: Oracle) -> bool {
        matches!(self.0[oracle as usize], Check::IndependentCanonical)
    }

    pub(super) fn declare(self, world: &crate::simulation::World) {
        self.validate();
        for (oracle, check) in ALL.into_iter().zip(self.0) {
            world.observation(crate::simulation::history::Transition::OracleCapability {
                oracle: format!("{oracle:?}"),
                declaration: serde_json::to_value(check).unwrap(),
            });
        }
    }
}

#[test]
fn replacements_are_scoped_per_oracle_and_require_explanations() {
    let mut capabilities = Capabilities::CANONICAL;
    capabilities.0[Oracle::HttpRank as usize] = Check::FixtureReplacement {
        reason: "colocated logical slots",
        check: "logical cursor transition assertions",
        scope: "B03 routed requests",
        independence: "fixture expectations plus production cursor validation",
    };
    capabilities.validate();
    assert!(!capabilities.canonical(Oracle::HttpRank));
    for oracle in [
        Oracle::HealthyRecovery,
        Oracle::HttpGraph,
        Oracle::RdmaRank,
        Oracle::OriginPlacement,
    ] {
        assert!(capabilities.canonical(oracle));
    }
    capabilities.0[0] = Check::FixtureReplacement {
        reason: "",
        check: "",
        scope: "",
        independence: "",
    };
    assert!(std::panic::catch_unwind(|| capabilities.validate()).is_err());
}

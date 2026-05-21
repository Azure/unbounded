// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Transport configuration types.
//!
//! A `NicConfig` carries everything `Nic::new` needs to bring up a Mercury
//! class plus its progress contexts. `PeerEntry` is a static seed for the
//! peer table; full dynamic peer routing lives in `router.rs`.

use super::error::{HgError, Result};
use super::peer::PeerId;

#[derive(Debug, Clone)]
pub struct PeerEntry {
    pub id: PeerId,
    /// NA address string, e.g. `"ofi+verbs;ofi_rxm://10.0.0.1:1234"` or
    /// `"na+sm://12345/0"` for shared-memory loopback.
    pub na_addr: String,
}

#[derive(Debug, Clone)]
pub struct NicConfig {
    /// NA info string passed to `HG_Init_opt2`,
    /// e.g. `"ofi+verbs;ofi_rxm"` or `"na+sm"`.
    pub na_info_string: String,
    /// Whether this Nic accepts incoming RPCs.
    pub listen: bool,
    /// Number of `HG_Context` instances per `Nic`. Each gets its own
    /// progress thread. Must be >= 1.
    pub contexts_per_nic: u16,
    /// Bound on outstanding forwards per context. Backpressure kicks in
    /// when reached. Must be >= 1.
    pub max_in_flight_per_ctx: u32,
    /// Timeout passed to `HG_Progress` per iteration. 0 is busy-poll.
    pub progress_timeout_ms: u32,
    /// Max trigger callbacks drained per loop iteration. Must be >= 1.
    pub trigger_max_count: u32,
    pub peers: Vec<PeerEntry>,
}

impl Default for NicConfig {
    fn default() -> Self {
        Self {
            na_info_string: String::new(),
            listen: false,
            contexts_per_nic: 2,
            max_in_flight_per_ctx: 1024,
            progress_timeout_ms: 1,
            trigger_max_count: 64,
            peers: Vec::new(),
        }
    }
}

impl NicConfig {
    /// Reject obviously-broken values.
    pub fn validate(&self) -> Result<()> {
        if self.na_info_string.is_empty() {
            return Err(HgError::BadConfig("na_info_string is empty"));
        }
        if self.contexts_per_nic == 0 {
            return Err(HgError::BadConfig("contexts_per_nic must be >= 1"));
        }
        if self.max_in_flight_per_ctx == 0 {
            return Err(HgError::BadConfig("max_in_flight_per_ctx must be >= 1"));
        }
        if self.trigger_max_count == 0 {
            return Err(HgError::BadConfig("trigger_max_count must be >= 1"));
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn valid() -> NicConfig {
        NicConfig {
            na_info_string: "na+sm".to_string(),
            ..NicConfig::default()
        }
    }

    #[test]
    fn default_values_match_spec() {
        let c = NicConfig::default();
        assert_eq!(c.na_info_string, "");
        assert!(!c.listen);
        assert_eq!(c.contexts_per_nic, 2);
        assert_eq!(c.max_in_flight_per_ctx, 1024);
        assert_eq!(c.progress_timeout_ms, 1);
        assert_eq!(c.trigger_max_count, 64);
        assert!(c.peers.is_empty());
    }

    #[test]
    fn validate_rejects_empty_na_info_string() {
        let c = NicConfig::default();
        assert!(matches!(c.validate(), Err(HgError::BadConfig(_))));
    }

    #[test]
    fn validate_rejects_zero_contexts_per_nic() {
        let mut c = valid();
        c.contexts_per_nic = 0;
        assert!(matches!(c.validate(), Err(HgError::BadConfig(_))));
    }

    #[test]
    fn validate_rejects_zero_max_in_flight() {
        let mut c = valid();
        c.max_in_flight_per_ctx = 0;
        assert!(matches!(c.validate(), Err(HgError::BadConfig(_))));
    }

    #[test]
    fn validate_rejects_zero_trigger_max_count() {
        let mut c = valid();
        c.trigger_max_count = 0;
        assert!(matches!(c.validate(), Err(HgError::BadConfig(_))));
    }

    #[test]
    fn validate_accepts_populated_default() {
        let c = valid();
        assert!(c.validate().is_ok());
    }
}

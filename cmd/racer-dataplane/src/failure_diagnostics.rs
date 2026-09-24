// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Bounded diagnostic projection; never carries targets, credentials, or raw errors.
use std::{
    sync::{
        Mutex, OnceLock,
        mpsc::{SyncSender, sync_channel},
    },
    time::{Duration, Instant},
};

#[derive(Clone, Debug, serde::Serialize)]
pub(crate) struct Failure {
    pub reason: String,
    pub cause: Option<String>,
    pub phase: Option<String>,
    pub endpoint: Option<String>,
    pub initiated: Option<bool>,
    pub relayed: bool,
}
impl Failure {
    pub(crate) fn from_error(error: &crate::cache::Error) -> Self {
        let e = error.evidence();
        Self {
            reason: format!("{:?}", e.reason()),
            cause: e.cause().map(|v| format!("{v:?}")),
            phase: e
                .semantic
                .and_then(|s| s.evidence.map(|v| v.phase))
                .or_else(|| e.attempt.map(|v| v.phase))
                .map(|v| format!("{v:?}")),
            endpoint: e
                .semantic
                .and_then(|s| s.evidence.map(|v| v.endpoint.to_string()))
                .or_else(|| {
                    e.attempt
                        .and_then(|v| v.endpoint.tcp().map(|v| v.to_string()))
                }),
            initiated: e
                .semantic
                .and_then(|s| s.evidence.map(|v| v.initiated))
                .or_else(|| e.attempt.map(|v| v.initiated)),
            relayed: e.semantic.is_some() || e.routed.is_some_and(|r| r.reported),
        }
    }
}

#[derive(serde::Serialize)]
pub struct CacheFailure {
    pub key: String,
    pub checksum: Option<String>,
    pub offset: Option<u64>,
    pub state: &'static str,
    pub polled_state: Option<&'static str>,
    pub site: &'static str,
    pub caller_remaining_ms: u128,
    pub candidate_remaining_ms: u128,
    pub buffer_wait: bool,
    pub network_flight: bool,
}

#[derive(Default)]
pub(crate) struct Samples {
    next: Option<Instant>,
    suppressed: u64,
}
impl Samples {
    pub(crate) fn take(&mut self, now: Instant) -> Option<u64> {
        if self.next.is_some_and(|next| now < next) {
            self.suppressed = self.suppressed.saturating_add(1);
            return None;
        }
        self.next = Some(now + Duration::from_secs(30));
        Some(std::mem::take(&mut self.suppressed))
    }
}

struct Diagnostics {
    sender: Option<SyncSender<serde_json::Value>>,
    key: Option<String>,
    rate: Mutex<ProcessRate>,
}
static DIAGNOSTICS: OnceLock<Diagnostics> = OnceLock::new();

const QUEUE_SIZE: usize = 16;
const PROCESS_SAMPLES: usize = 16;
const INTERVAL: Duration = Duration::from_secs(30);

#[derive(Default)]
struct ProcessRate {
    until: Option<Instant>,
    count: usize,
}
impl ProcessRate {
    fn take(&mut self, now: Instant) -> bool {
        if self.until.is_none_or(|end| now >= end) {
            self.until = Some(now + INTERVAL);
            self.count = 0;
        }
        if self.count == PROCESS_SAMPLES {
            return false;
        }
        self.count += 1;
        true
    }
}

fn parse_key(value: Option<String>) -> std::io::Result<Option<String>> {
    if value.as_ref().is_some_and(|v| {
        v.len() != 64
            || !v
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    }) {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidInput,
            "RACER_DIAGNOSTIC_PAGE_KEY must be 64 lowercase hexadecimal characters",
        ));
    }
    Ok(value)
}

/// Initialize bounded diagnostics before starting reactor workers. Unset filter
/// enables automatic sampled warnings; invalid filters fail startup without
/// printing their value. Failure to start the log thread disables diagnostics.
pub fn initialize() -> std::io::Result<()> {
    let key = match std::env::var("RACER_DIAGNOSTIC_PAGE_KEY") {
        Ok(value) => Some(value),
        Err(std::env::VarError::NotPresent) => None,
        Err(_) => {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidInput,
                "invalid RACER_DIAGNOSTIC_PAGE_KEY encoding",
            ));
        }
    };
    let key = parse_key(key)?;
    DIAGNOSTICS.get_or_init(|| {
        let (tx, rx) = sync_channel(QUEUE_SIZE);
        let sender = std::thread::Builder::new()
            .name("racer-diagnostics".into())
            .spawn(move || {
                use std::io::Write;
                for record in rx {
                    // Broken/blocked stderr affects only this dedicated thread.
                    let _ = writeln!(std::io::stderr().lock(), "{record}");
                }
            })
            .ok()
            .map(|_| tx);
        Diagnostics {
            sender,
            key,
            rate: Mutex::new(ProcessRate::default()),
        }
    });
    Ok(())
}

pub(crate) fn emit(mut record: serde_json::Value) {
    let Some(diagnostics) = DIAGNOSTICS.get() else {
        return;
    };
    let Some(sender) = &diagnostics.sender else {
        return;
    };
    let Ok(mut rate) = diagnostics.rate.try_lock() else {
        return;
    };
    if !rate.take(Instant::now()) {
        return;
    }
    drop(rate);
    record["level"] = serde_json::json!("WARN");
    record["unix_ms"] = serde_json::json!(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis()
    );
    let _ = sender.try_send(record);
}

pub(crate) fn selected(key: &str) -> bool {
    DIAGNOSTICS.get().is_some_and(|d| {
        d.sender.is_some() && d.key.as_ref().is_none_or(|selected| selected == key)
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn production_page_key_uses_hashed_catalog_universe() {
        use crate::cache::{Namespace, PeerDescriptor, PeerPage};
        use sha2::{Digest, Sha256};
        let catalog = "961c8537760126f11d60ac6d6700128a13a0455a5b627ac0ac8d44672d5e2448";
        // Matches controlplane model::identity_bytes and topology::snapshot.
        let universe = Sha256::digest(format!("racer/universe/v1\0{catalog}").as_bytes());
        let volume = "2547e40d-ea84-41b0-bd2a-d2150a8e7eed";
        let namespace = Namespace::volume(&universe, volume, 1, Namespace::new(volume).unwrap());
        let checksum = crate::metadata::Checksum::from_etag(
            "\"be4ead90e8027ff03c1f01a35389d0312ff5e0e348c22652314f85ddb4895a6d\"",
        )
        .unwrap();
        let target = "/gantry/v1/aW1hZ2UtZml4dHVyZS0yMDI2MDkyMy50ZXN0/bG9hZGdlbi9pbWFnZXM/blobs/sha256:be4ead90e8027ff03c1f01a35389d0312ff5e0e348c22652314f85ddb4895a6d";
        let key = PeerDescriptor::page(target, PeerPage::new(805306368, 1000000000, checksum))
            .key(namespace)
            .unwrap();
        assert_eq!(
            crate::cache::peer_wire::hex(&key),
            "c9821ff871b06ff48f815dcdcd649437f29baa92ad90976b24bb7c942795ed10"
        );
        assert_eq!(
            u64::from_le_bytes(key[..8].try_into().unwrap()) % 262144,
            230089
        );
    }
    #[test]
    fn bounded_sampling_and_redaction() {
        let mut samples = Samples::default();
        let now = Instant::now();
        assert_eq!(samples.take(now), Some(0));
        for _ in 0..100 {
            assert_eq!(samples.take(now), None);
        }
        assert_eq!(samples.take(now + Duration::from_secs(30)), Some(100));
        let e = crate::cache::Error::InvalidData("secret-target?token=secret");
        let text = serde_json::to_string(&Failure::from_error(&e)).unwrap();
        assert!(!text.contains("secret"));
        assert!(text.contains("Protocol"));
        let error = crate::outcome::PeerFailure {
            identity: [0; 32],
            candidate: 0,
            reason: crate::outcome::PeerReason::Deadline,
            evidence: None,
            response: crate::outcome::ResponseMetadata {
                challenge: crate::header_value::HeaderValue::default(),
                retry_after: crate::header_value::HeaderValue::default(),
            },
        };
        let projected = Failure::from_error(&std::io::Error::other(error).into());
        assert!(projected.relayed);
        assert!(!Failure::from_error(&crate::cache::Error::Timeout).relayed);
    }

    #[test]
    fn global_budget_queue_and_filter_are_bounded() {
        let now = Instant::now();
        let mut rate = ProcessRate::default();
        for _ in 0..PROCESS_SAMPLES {
            assert!(rate.take(now));
        }
        for _ in 0..100 {
            assert!(!rate.take(now));
        }
        assert!(rate.take(now + INTERVAL));
        let (tx, _rx) = sync_channel(QUEUE_SIZE);
        for i in 0..QUEUE_SIZE {
            tx.try_send(i).unwrap();
        }
        assert!(matches!(
            tx.try_send(0),
            Err(std::sync::mpsc::TrySendError::Full(_))
        ));
        assert!(parse_key(None).unwrap().is_none());
        assert!(parse_key(Some("a".repeat(64))).is_ok());
        for key in ["A".repeat(64), "b".repeat(65), "target?token=secret".into()] {
            let err = parse_key(Some(key)).unwrap_err().to_string();
            assert!(!err.contains("secret"));
        }
    }

    #[test]
    fn breaker_evidence_follows_generation_and_success() {
        use crate::breaker::CircuitBreaker;
        let clock = std::rc::Rc::new(std::cell::Cell::new(Instant::now()));
        let now = clock.clone();
        let breaker = CircuitBreaker::with_clock(Duration::from_secs(1), move || now.get());
        let first = breaker.try_acquire().unwrap();
        let stale = breaker.try_acquire().unwrap();
        first.failure_with_evidence(&crate::cache::Error::Timeout);
        assert_eq!(breaker.last_failure().unwrap().1.reason, "Deadline");
        stale.failure_with_evidence(&crate::cache::Error::InvalidData("secret"));
        assert_eq!(breaker.last_failure().unwrap().1.reason, "Deadline");
        assert!(breaker.try_acquire().is_err());
        clock.set(clock.get() + Duration::from_secs(1));
        assert_eq!(breaker.last_failure().unwrap().0, 1000);
        breaker.try_acquire().unwrap().success();
        assert!(breaker.last_failure().is_none());
        breaker.try_acquire().unwrap().failure();
        assert!(breaker.last_failure().is_none());
        clock.set(clock.get() + Duration::from_secs(1));
        breaker
            .try_acquire()
            .unwrap()
            .failure_with_evidence(&crate::cache::Error::Timeout);
        clock.set(clock.get() + Duration::from_secs(1));
        drop(breaker.try_acquire().unwrap());
        assert!(
            breaker.last_failure().is_none(),
            "canceled probe must not inherit old failure evidence"
        );
    }
}

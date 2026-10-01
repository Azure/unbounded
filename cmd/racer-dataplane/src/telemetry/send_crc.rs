//! Opt-in materialized HTTP relay sampling. No payload scan runs on I/O, and
//! delivery never awaits diagnostics. CRC describes the immutable send allocation,
//! possibly observed after send completion, not an on-wire authentication proof.
//!
//! RACER_SEND_CRC_PAIR=sender-node-uid,receiver-node-uid selects exactly one
//! authenticated pair. Unset disables sampling. Admission is shared by workers:
//! burst one, two samples/second, one retained ciphertext owner, 240 samples and
//! 120 seconds from the first eligible send. Restart resets these bounds.
//! Each uncached sample scans at most 16 MiB + 16 bytes on crypto (about 32 MiB/s
//! at the cap); cached samples do not rescan. The existing reservation stays held.
//! Only materialized HTTP responses with acquisition provenance are eligible;
//! opaque splice and native sends bypass this hook. Missing page keys skip CRC.
//! Small identity hashes reuse the AEAD record definition, only on sampled work.
//! Poll /debug/send-crc within 32 seconds at the maximum rate; joins use acquisition,
//! attempt, page, key, nonce, lengths, and AAD, with receiver.remote == sender UID.
//! Pending/unavailable samples may lack fingerprints; absence is not agreement.
use crate::{
    error::{Error, Result},
    model::NodeId,
    runtime::environment,
    telemetry::failures::AeadFailure,
};
use std::{
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

pub const CAPACITY: usize = 64;

#[derive(Clone, Debug)]
pub struct Pair {
    pub sender: NodeId,
    pub receiver: NodeId,
}
impl Pair {
    pub fn parse(value: &str) -> Result<Self> {
        let (sender, receiver) = value.split_once(',').ok_or(Error::InvalidConfiguration)?;
        let pair = Self {
            sender: NodeId(sender.into()),
            receiver: NodeId(receiver.into()),
        };
        pair.validate()?;
        Ok(pair)
    }
    pub fn validate(&self) -> Result<()> {
        if self.sender == self.receiver
            || !crate::security::identity::canonical_uuid(&self.sender.0)
            || !crate::security::identity::canonical_uuid(&self.receiver.0)
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}

#[derive(Clone, Default)]
pub struct Samples(Arc<Mutex<State>>);
struct State {
    first: Option<Instant>,
    last: Option<Instant>,
    busy: bool,
    eligible: u64,
    sampled: u64,
    skipped: u64,
    entries: [Option<Arc<Sample>>; CAPACITY],
}
impl Default for State {
    fn default() -> Self {
        Self {
            first: None,
            last: None,
            busy: false,
            eligible: 0,
            sampled: 0,
            skipped: 0,
            entries: std::array::from_fn(|_| None),
        }
    }
}
struct Sample {
    sequence: u64,
    sender: NodeId,
    receiver: NodeId,
    data: Mutex<Data>,
}
struct Data {
    crypto: Option<crate::runtime::crypto::CryptoId>,
    send: &'static str,
    status: &'static str,
    error: Option<Error>,
    facts: Option<AeadFailure>,
}
pub(crate) struct Ticket(Arc<Sample>);
pub(crate) struct Work {
    sample: Arc<Sample>,
    owner: Samples,
    pub facts: Option<AeadFailure>,
    pub cached: bool,
}
impl Samples {
    #[cfg(test)]
    pub(crate) fn fill_test_ring(&self) {
        let pair = Pair::parse(
            "8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111",
        )
        .unwrap();
        for _ in 0..CAPACITY {
            self.0.lock().unwrap().last = None;
            let (ticket, mut work) = self.begin(&pair, &pair.sender, &pair.receiver).unwrap();
            work.facts = Some(crate::telemetry::failures::test_aead_failure());
            work.identify(crate::runtime::crypto::CryptoId {
                worker: crate::model::WorkerId(u16::MAX),
                generation: u64::MAX,
                sequence: u64::MAX,
            });
            work.finish(None);
            ticket.finish(true);
        }
    }
    pub(crate) fn begin(
        &self,
        pair: &Pair,
        sender: &NodeId,
        receiver: &NodeId,
    ) -> Option<(Ticket, Work)> {
        if sender != &pair.sender || receiver != &pair.receiver {
            return None;
        }
        let now = environment::now();
        let mut state = self.0.lock().unwrap_or_else(|e| e.into_inner());
        state.eligible = state.eligible.saturating_add(1);
        let first = *state.first.get_or_insert(now);
        if state.busy
            || state.sampled >= 240
            || now.saturating_duration_since(first) >= Duration::from_secs(120)
            || state.last.is_some_and(|last| {
                now.saturating_duration_since(last) < Duration::from_millis(500)
            })
        {
            state.skipped = state.skipped.saturating_add(1);
            return None;
        }
        state.busy = true;
        state.last = Some(now);
        state.sampled += 1;
        let sample = Arc::new(Sample {
            sequence: state.sampled,
            sender: sender.clone(),
            receiver: receiver.clone(),
            data: Mutex::new(Data {
                crypto: None,
                send: "pending",
                status: "pending",
                error: None,
                facts: None,
            }),
        });
        let index = (state.sampled as usize - 1) % CAPACITY;
        state.entries[index] = Some(sample.clone());
        Some((
            Ticket(sample.clone()),
            Work {
                sample,
                owner: self.clone(),
                facts: None,
                cached: false,
            },
        ))
    }
    pub fn write(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let (entries, eligible, sampled, skipped, busy) = {
            let state = self.0.lock().unwrap_or_else(|e| e.into_inner());
            (
                state.entries.clone(),
                state.eligible,
                state.sampled,
                state.skipped,
                state.busy,
            )
        };
        writeln!(
            out,
            "eligible={eligible} sampled={sampled} skipped={skipped} busy={} retained={} overwritten={} capacity={CAPACITY}",
            u8::from(busy),
            sampled.min(CAPACITY as u64),
            sampled.saturating_sub(CAPACITY as u64)
        )?;
        for sequence in sampled.saturating_sub(CAPACITY as u64) + 1..=sampled {
            if let Some(sample) = &entries[(sequence as usize - 1) % CAPACITY] {
                let data = sample.data.lock().unwrap_or_else(|e| e.into_inner());
                write!(
                    out,
                    "seq={} sender={} receiver={} send={} status={} error={:?}",
                    sample.sequence,
                    sample.sender.0,
                    sample.receiver.0,
                    data.send,
                    data.status,
                    data.error
                )?;
                if let Some(id) = data.crypto {
                    write!(
                        out,
                        " w={} crypto={}:{}",
                        id.worker.0, id.generation, id.sequence
                    )?;
                }
                if let Some(f) = data.facts {
                    f.write_fields(out)?;
                }
                writeln!(out)?;
            }
        }
        Ok(())
    }
}
impl Ticket {
    pub(crate) fn finish(&self, success: bool) {
        self.0.data.lock().unwrap_or_else(|e| e.into_inner()).send =
            if success { "completed" } else { "failed" };
    }
}
impl Drop for Ticket {
    fn drop(&mut self) {
        let mut data = self.0.data.lock().unwrap_or_else(|e| e.into_inner());
        if data.send == "pending" {
            data.send = "abandoned";
        }
    }
}
impl Work {
    pub(crate) fn identify(&self, id: crate::runtime::crypto::CryptoId) {
        self.sample
            .data
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .crypto = Some(id);
    }
    pub(crate) fn finish(&self, error: Option<Error>) {
        let mut data = self.sample.data.lock().unwrap_or_else(|e| e.into_inner());
        data.facts = self.facts;
        data.error = error;
        data.status = if error.is_some() {
            "unavailable"
        } else if self.cached {
            "cached"
        } else {
            "computed"
        };
    }
}
impl Drop for Work {
    fn drop(&mut self) {
        {
            let mut data = self.sample.data.lock().unwrap_or_else(|e| e.into_inner());
            if data.status == "pending" {
                data.status = "unavailable";
            }
        }
        self.owner.0.lock().unwrap_or_else(|e| e.into_inner()).busy = false;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn pair() -> Pair {
        Pair::parse("8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111")
            .unwrap()
    }
    #[test]
    fn send_crc_pair_and_shared_owner_bounds() {
        for value in [
            "",
            "x,y",
            "11111111-1111-4111-8111-111111111111,11111111-1111-4111-8111-111111111111",
            "x,y,z",
        ] {
            assert!(Pair::parse(value).is_err());
        }
        let p = pair();
        let samples = Samples::default();
        assert!(samples.begin(&p, &p.receiver, &p.sender).is_none());
        let (ticket, work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
        drop(ticket);
        let other = samples.clone();
        let p2 = p.clone();
        assert!(
            std::thread::spawn(move || other.begin(&p2, &p2.sender, &p2.receiver).is_none())
                .join()
                .unwrap()
        );
        assert!(samples.0.lock().unwrap().busy);
        drop(work);
        assert!(!samples.0.lock().unwrap().busy);
        assert!(
            samples.begin(&p, &p.sender, &p.receiver).is_none(),
            "burst is one"
        );
        let mut text = String::new();
        samples.write(&mut text).unwrap();
        assert!(text.contains("send=abandoned status=unavailable"));
        samples.0.lock().unwrap().last = None;
        samples.0.lock().unwrap().first = Some(environment::now() - Duration::from_secs(120));
        assert!(samples.begin(&p, &p.sender, &p.receiver).is_none());
    }
    #[test]
    fn send_crc_ring_is_bounded_and_preserves_send_outcomes() {
        let p = pair();
        let samples = Samples::default();
        for i in 0..240 {
            samples.0.lock().unwrap().last = None;
            let (ticket, mut work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
            ticket.finish(i % 2 == 0);
            work.facts = Some(crate::telemetry::failures::test_aead_failure());
            work.finish(None);
            drop(work);
            drop(ticket);
        }
        samples.0.lock().unwrap().last = None;
        assert!(samples.begin(&p, &p.sender, &p.receiver).is_none());
        let mut text = String::new();
        samples.write(&mut text).unwrap();
        assert_eq!(text.lines().count(), CAPACITY + 1);
        assert!(text.contains("overwritten=176 capacity=64\nseq=177 "));
        assert!(text.contains("send=completed"));
        assert!(text.contains("send=failed"));
        assert!(text.len() < crate::telemetry::MAX_RESPONSE_BYTES - 256);
    }
}

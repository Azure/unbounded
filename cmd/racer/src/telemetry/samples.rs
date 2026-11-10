use super::*;

pub const SEND_TAG_CAPACITY: usize = 64;
const SEND_TAG_LIMITS: SampleLimits = SampleLimits {
    total: 240,
    duration: Duration::from_secs(120),
    interval: Duration::from_millis(500),
};
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
            || !racer_crypto::identity::canonical_uuid(&self.sender.0)
            || !racer_crypto::identity::canonical_uuid(&self.receiver.0)
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}
/// Racer's selected sender/receiver sampling policy and diagnostic records.
#[derive(Clone)]
pub struct Samples(crate::telemetry::primitives::Sampler<Sample, SEND_TAG_CAPACITY>);

impl Default for Samples {
    /// Apply Racer's fixed send-checksum sample limits.
    fn default() -> Self {
        Self(crate::telemetry::primitives::Sampler::new(SEND_TAG_LIMITS))
    }
}
struct Sample {
    sequence: u64,
    sender: NodeId,
    receiver: NodeId,
    data: Mutex<Data>,
}
struct Data {
    crypto: Option<crate::security::CryptoId>,
    send: &'static str,
    status: &'static str,
    error: Option<Error>,
    facts: Option<AeadFailure>,
}
pub(crate) struct Ticket(Arc<Sample>);
pub(crate) struct Work {
    sample: Arc<Sample>,

    _lease: crate::telemetry::primitives::SampleLease<Sample, SEND_TAG_CAPACITY>,

    pub facts: Option<AeadFailure>,
}
impl Samples {
    #[cfg(test)]
    pub(crate) fn fill_test_ring(&self, env: &racer_io::Env) {
        let pair = Pair::parse(
            "8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111",
        )
        .unwrap();
        let now = env.ctx.clock.clone().now();
        for i in 0..SEND_TAG_CAPACITY {
            let (ticket, mut work) = self
                .begin_at(&pair, &pair.sender, &pair.receiver, || {
                    now + SEND_TAG_LIMITS.interval * i as u32
                })
                .unwrap();
            work.facts = Some(crate::telemetry::test_aead_failure());
            work.identify(crate::security::CryptoId {
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
        now: Instant,
    ) -> Option<(Ticket, Work)> {
        self.begin_at(pair, sender, receiver, || now)
    }
    fn begin_at(
        &self,
        pair: &Pair,
        sender: &NodeId,
        receiver: &NodeId,
        now: impl FnOnce() -> Instant,
    ) -> Option<(Ticket, Work)> {
        if sender != &pair.sender || receiver != &pair.receiver {
            return None;
        }
        let now = now();
        let (sample, lease) = self.0.acquire(now, |sequence| Sample {
            sequence,
            sender: sender.clone(),
            receiver: receiver.clone(),
            data: Mutex::new(Data {
                crypto: None,
                send: "pending",
                status: "pending",
                error: None,
                facts: None,
            }),
        })?;
        Some((
            Ticket(sample.clone()),
            Work {
                sample,
                _lease: lease,
                facts: None,
            },
        ))
    }
    pub fn write(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let (entries, counts) = self.0.snapshot();
        let crate::telemetry::primitives::SampleCounts {
            eligible,
            sampled,
            skipped,
            busy,
        } = counts;
        writeln!(
            out,
            "eligible={eligible} sampled={sampled} skipped={skipped} busy={} retained={} overwritten={} capacity={SEND_TAG_CAPACITY}",
            u8::from(busy),
            sampled.min(SEND_TAG_CAPACITY as u64),
            sampled.saturating_sub(SEND_TAG_CAPACITY as u64)
        )?;
        for (_, sample) in entries.iter_refs() {
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
    pub(crate) fn identify(&self, id: crate::security::CryptoId) {
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
        data.status = match error {
            None => "verified",
            Some(Error::CorruptRecord) => "mismatch",
            Some(_) => "unavailable",
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
    }
}

// Bounded internal failures, exported separately from low-cardinality metrics.
// Only typed errors, correlation IDs, and numeric progress/resource facts enter
// this ring. Object keys, ETags, headers, credentials, and payloads never enter it.
// A separate 64-entry AEAD ring survives transient admission floods. Its stored
// tag is a receiver-side fingerprint, not proof of equality with sender bytes. Page/AAD
// hashes are pseudonyms (known inputs can be guessed), never authentication.
// Supplier is the original response signer; remote is the last reverse signer,
// not a TCP address. Acquisition IDs can differ from the decrypt request after
// retention or cross-worker handoff. Disk reconstruction has no peer provenance.

#[cfg(test)]
mod tests {
    use super::*;

    pub(crate) mod send_tag_tests {
        use super::*;
        fn pair() -> Pair {
            Pair::parse("8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111")
                .unwrap()
        }
        #[test]
        fn send_tag_pair_and_shared_owner_bounds() {
            let clock = racer_io::SimClock::new(72);
            let env = racer_io::Env::new(racer_io::Driver::Uring, clock.ctx(0));
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
            assert!(
                samples
                    .begin(&p, &p.receiver, &p.sender, env.ctx.clock.clone().now())
                    .is_none()
            );
            let (ticket, work) = samples
                .begin(&p, &p.sender, &p.receiver, env.ctx.clock.clone().now())
                .unwrap();
            drop(ticket);
            let other = samples.clone();
            let p2 = p.clone();
            let thread_clock = env.ctx.clock.clone();
            assert!(
                std::thread::spawn(move || other
                    .begin(&p2, &p2.sender, &p2.receiver, thread_clock.now())
                    .is_none())
                .join()
                .unwrap()
            );
            assert!(samples.0.counts().busy);
            drop(work);
            assert!(!samples.0.counts().busy);
            assert!(
                samples
                    .begin(&p, &p.sender, &p.receiver, env.ctx.clock.clone().now())
                    .is_none(),
                "burst is one"
            );
            let mut text = String::new();
            samples.write(&mut text).unwrap();
            assert!(text.contains("send=abandoned status=unavailable"));
            clock.advance(Duration::from_secs(120));
            assert!(
                samples
                    .begin(&p, &p.sender, &p.receiver, env.ctx.clock.clone().now())
                    .is_none()
            );
        }
        #[test]
        fn send_tag_ineligible_pairs_do_not_start_the_sampling_window() {
            let clock = racer_io::SimClock::new(76);
            let env = racer_io::Env::new(racer_io::Driver::Uring, clock.ctx(0));
            let p = pair();
            let samples = Samples::default();
            assert!(
                samples
                    .begin(&p, &p.receiver, &p.sender, env.ctx.clock.clone().now())
                    .is_none()
            );
            clock.advance(Duration::from_secs(120));
            let (ticket, work) = samples
                .begin(&p, &p.sender, &p.receiver, env.ctx.clock.clone().now())
                .unwrap();
            assert_eq!(ticket.0.sequence, 1);
            assert_eq!(samples.0.counts().eligible, 1);
            work.finish(Some(Error::Io));
            drop(work);
            assert!(!samples.0.counts().busy);
            let mut text = String::new();
            samples.write(&mut text).unwrap();
            assert!(text.contains("status=unavailable error=Some(Io)"));
        }
        #[test]
        fn send_tag_interval_accepts_exactly_500ms() {
            let clock = racer_io::SimClock::new(73);
            let env = racer_io::Env::new(racer_io::Driver::Uring, clock.ctx(0));
            let p = pair();
            let samples = Samples::default();
            let (ticket, work) = samples
                .begin(&p, &p.sender, &p.receiver, env.ctx.clock.clone().now())
                .unwrap();
            drop(work);
            drop(ticket);
            clock.advance(Duration::from_millis(500) - Duration::from_nanos(1));
            assert!(
                samples
                    .begin(&p, &p.sender, &p.receiver, env.ctx.clock.clone().now())
                    .is_none()
            );
            clock.advance(Duration::from_nanos(1));
            let (ticket, work) = samples
                .begin(&p, &p.sender, &p.receiver, env.ctx.clock.clone().now())
                .unwrap();
            assert_eq!(ticket.0.sequence, 2);
            drop(work);
            drop(ticket);
            let state = samples.0.counts();
            assert_eq!((state.eligible, state.sampled, state.skipped), (3, 2, 1));
            assert!(!state.busy);
        }

        #[test]
        fn send_tag_ticket_and_snapshot_survive_retention_overwrite() {
            let clock = racer_io::SimClock::new(74);
            let env = racer_io::Env::new(racer_io::Driver::Uring, clock.ctx(0));
            let p = pair();
            let samples = Samples::default();
            let (ticket, work) = samples
                .begin(&p, &p.sender, &p.receiver, env.ctx.clock.clone().now())
                .unwrap();
            let weak = Arc::downgrade(&ticket.0);
            work.finish(None);
            drop(work);
            let snapshot = samples.0.snapshot().0;
            for _ in 0..SEND_TAG_CAPACITY {
                clock.advance(Duration::from_millis(500));
                let (next, work) = samples
                    .begin(&p, &p.sender, &p.receiver, env.ctx.clock.clone().now())
                    .unwrap();
                next.finish(true);
                work.finish(None);
            }
            let mut before = String::new();
            samples.write(&mut before).unwrap();
            assert!(before.starts_with(
                "eligible=65 sampled=65 skipped=0 busy=0 retained=64 overwritten=1 capacity=64\nseq=2 "
            ));
            assert_eq!(before.lines().count(), SEND_TAG_CAPACITY + 1);
            ticket.finish(false);
            let mut after = String::new();
            samples.write(&mut after).unwrap();
            assert_eq!(
                before, after,
                "evicted Ticket cannot change retained records"
            );
            let (_, retained) = snapshot.iter_refs().next().unwrap();
            assert!(Arc::ptr_eq(retained, &ticket.0));
            assert_eq!(retained.data.lock().unwrap().send, "failed");
            drop(ticket);
            assert!(
                weak.upgrade().is_some(),
                "snapshot retains the evicted sample"
            );
            drop(snapshot);
            assert!(weak.upgrade().is_none(), "last handle releases the sample");
        }

        #[test]
        fn send_tag_ring_is_bounded_and_preserves_send_outcomes() {
            let clock = racer_io::SimClock::new(75);
            let env = racer_io::Env::new(racer_io::Driver::Uring, clock.ctx(0));
            let p = pair();
            let samples = Samples::default();
            for i in 0..240 {
                let (ticket, mut work) = samples
                    .begin(&p, &p.sender, &p.receiver, env.ctx.clock.clone().now())
                    .unwrap();
                ticket.finish(i % 2 == 0);
                work.facts = Some(crate::telemetry::test_aead_failure());
                work.finish(None);
                drop(work);
                drop(ticket);
                clock.advance(Duration::from_millis(500));
            }
            assert!(
                samples
                    .begin(&p, &p.sender, &p.receiver, env.ctx.clock.clone().now())
                    .is_none()
            );
            let mut text = String::new();
            samples.write(&mut text).unwrap();
            assert_eq!(text.lines().count(), SEND_TAG_CAPACITY + 1);
            assert!(text.contains("overwritten=176 capacity=64\nseq=177 "));
            assert!(text.contains("send=completed"));
            assert!(text.contains("send=failed"));
            assert!(text.len() < crate::telemetry::MAX_RESPONSE_BYTES - 256);
        }
    }
}

//! Per-owner sharing of GPU-resident sealed ciphertext.
//!
//! A page's owning worker runs every sealed fetch of that page, so this
//! worker-local table sees them all. When one request has read a page's
//! ciphertext into GPU staging with cuFile, or had a peer land it in GPU
//! memory, a concurrent sealed fetch for the same GPU, or for a GPU that can
//! copy from it over a peer path, reuses that ciphertext instead of fetching
//! it again. Each GPU still checks the CRC and the AEAD tag and decrypts by
//! itself: only ciphertext is shared.
//!
//! The table never keeps GPU memory alive. It holds weak references, so a
//! copy is reusable only while some request still holds it, and dead entries
//! are pruned on lookup and on insert. While the first request fetches, later
//! requests for a reachable GPU wait for it instead of starting their own
//! fetch.

use super::sealed::SealedPage;
use super::sealed::SealedSource;
use crate::error::Result;
use crate::gpu::GpuHelper;
use crate::gpu::LandedCiphertext;
use crate::gpu::StagedCiphertext;
use crate::model::ObjectMetadata;
use crate::model::PageEnvelope;
use crate::model::PageId;
use crate::security::PageCrypto;
use crate::store::ReadToken;
use futures::FutureExt;
use futures::channel::oneshot;
use futures::future::LocalBoxFuture;
use futures::future::Shared;
use std::cell::Cell;
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Weak;

/// Pages indexed at once. A new fetch past this bound runs privately rather
/// than grow the table, after dead entries are pruned.
pub(super) const MAX_PAGES: usize = 4096;

/// Everything but the ciphertext and the key that a request needs to build a
/// sealed page around shared ciphertext. The key is derived again from the
/// envelope for each request, because a derived key cannot be copied.
struct Facts {
    metadata: ObjectMetadata,
    envelope: PageEnvelope,
    aad: Vec<u8>,
    expected_crc: Option<u64>,
}

#[derive(Clone)]
enum Held {
    Staged {
        staged: Arc<StagedCiphertext>,
        token: ReadToken,
    },
    Landed(Arc<LandedCiphertext>),
}

enum Retained {
    Staged {
        staged: Weak<StagedCiphertext>,
        token: ReadToken,
    },
    Landed(Weak<LandedCiphertext>),
}

/// A live GPU copy of a page's ciphertext. Holding it keeps the GPU memory.
#[derive(Clone)]
pub(super) struct SharedCopy {
    facts: Rc<Facts>,
    held: Held,
}

impl SharedCopy {
    /// The GPU-resident part of a sealed page, or None for host ciphertext.
    pub(super) fn from_sealed(sealed: &SealedPage) -> Option<Self> {
        let held = match &sealed.source {
            SealedSource::Staged { staged, token } => Held::Staged {
                staged: staged.clone(),
                token: token.clone(),
            },
            SealedSource::Landed(landed) => Held::Landed(landed.clone()),
            SealedSource::Host(_) => return None,
        };
        Some(Self {
            facts: Rc::new(Facts {
                metadata: sealed.metadata.clone(),
                envelope: sealed.envelope.clone(),
                aad: sealed.aad.clone(),
                expected_crc: sealed.expected_crc,
            }),
            held,
        })
    }

    /// The GPU that holds the ciphertext.
    pub(super) fn ordinal(&self) -> i32 {
        match &self.held {
            Held::Staged { staged, .. } => staged.ordinal(),
            Held::Landed(landed) => landed.ordinal(),
        }
    }

    /// A sealed page for another request around the same ciphertext.
    pub(super) fn sealed(&self, crypto: &PageCrypto, page: &PageId) -> Result<Box<SealedPage>> {
        let facts = &self.facts;
        let sealed = Box::new(SealedPage {
            metadata: facts.metadata.clone(),
            envelope: facts.envelope.clone(),
            aad: facts.aad.clone(),
            key: crypto.derive_page_key(&facts.envelope)?,
            expected_crc: facts.expected_crc,
            source: match &self.held {
                Held::Staged { staged, token } => SealedSource::Staged {
                    staged: staged.clone(),
                    token: token.clone(),
                },
                Held::Landed(landed) => SealedSource::Landed(landed.clone()),
            },
        });
        sealed.validate_for(page)?;
        Ok(sealed)
    }

    fn retain(&self) -> RetainedCopy {
        RetainedCopy {
            facts: self.facts.clone(),
            ordinal: self.ordinal(),
            retained: match &self.held {
                Held::Staged { staged, token } => Retained::Staged {
                    staged: Arc::downgrade(staged),
                    token: token.clone(),
                },
                Held::Landed(landed) => Retained::Landed(Arc::downgrade(landed)),
            },
        }
    }
}

struct RetainedCopy {
    facts: Rc<Facts>,
    ordinal: i32,
    retained: Retained,
}

impl RetainedCopy {
    fn upgrade(&self) -> Option<SharedCopy> {
        let held = match &self.retained {
            Retained::Staged { staged, token } => Held::Staged {
                staged: staged.upgrade()?,
                token: token.clone(),
            },
            Retained::Landed(landed) => Held::Landed(landed.upgrade()?),
        };
        Some(SharedCopy {
            facts: self.facts.clone(),
            held,
        })
    }

    fn alive(&self) -> bool {
        match &self.retained {
            Retained::Staged { staged, .. } => staged.strong_count() > 0,
            Retained::Landed(landed) => landed.strong_count() > 0,
        }
    }
}

/// How a shared fetch ended, as its waiters see it.
#[derive(Clone)]
pub(super) enum Outcome {
    /// The ciphertext is on the fetching request's GPU.
    Copy(SharedCopy),

    /// The fetch produced no GPU copy: nothing was found, the copy came
    /// through host memory, or the fetch failed. Waiters fetch for themselves.
    Nothing,

    /// The fetching request ended first (cancelled or out of time). Waiters
    /// may look again, and one of them fetches.
    Abandoned,
}

pub(super) type Receiver = Shared<LocalBoxFuture<'static, Outcome>>;

struct InFlight {
    id: u64,
    ordinal: i32,
    receive: Receiver,
}

#[derive(Default)]
struct Entry {
    copies: Vec<RetainedCopy>,
    flight: Option<InFlight>,
}

impl Entry {
    fn prune(&mut self) {
        self.copies.retain(RetainedCopy::alive);
    }

    fn is_empty(&self) -> bool {
        self.copies.is_empty() && self.flight.is_none()
    }
}

/// What a sealed fetch should do next.
pub(super) enum Lookup {
    /// Reuse this live copy.
    Live(SharedCopy),

    /// Wait for another request's fetch.
    Join(Receiver),

    /// Fetch, and share the result through this handle.
    Lead(Lead),

    /// Fetch without sharing: the in-flight fetch is on a GPU this one
    /// cannot copy from, or the table is full.
    Private,
}

/// The table of one worker. Single-threaded.
#[derive(Default)]
pub(crate) struct GpuCopies {
    entries: RefCell<BTreeMap<PageId, Entry>>,
    next: Cell<u64>,
}

impl GpuCopies {
    /// Pages indexed, live or not yet pruned.
    #[cfg(test)]
    pub(crate) fn len(&self) -> usize {
        self.entries.borrow().len()
    }

    /// Find a reusable copy of `page` for GPU `ordinal`: one on the same
    /// GPU first, then one on a GPU it can copy from. Otherwise join a
    /// reachable in-flight fetch, or lead a new one.
    pub(super) fn lookup(
        self: &Rc<Self>,
        page: &PageId,
        helper: &GpuHelper,
        ordinal: i32,
    ) -> Lookup {
        let mut entries = self.entries.borrow_mut();
        if let Some(entry) = entries.get_mut(page) {
            entry.prune();
            let live =
                entry
                    .copies
                    .iter()
                    .filter(|copy| copy.ordinal == ordinal)
                    .chain(entry.copies.iter().filter(|copy| {
                        copy.ordinal != ordinal && helper.peers(ordinal, copy.ordinal)
                    }))
                    .find_map(RetainedCopy::upgrade);
            if let Some(live) = live {
                return Lookup::Live(live);
            }
            if let Some(flight) = &entry.flight {
                return if helper.peers(ordinal, flight.ordinal) {
                    Lookup::Join(flight.receive.clone())
                } else {
                    Lookup::Private
                };
            }
        } else if entries.len() >= MAX_PAGES {
            entries.retain(|_, entry| {
                entry.prune();
                !entry.is_empty()
            });
            if entries.len() >= MAX_PAGES {
                return Lookup::Private;
            }
        }
        let id = self.next.get();
        self.next.set(id.wrapping_add(1));
        let (send, receive) = oneshot::channel();
        let receive = async move { receive.await.unwrap_or(Outcome::Abandoned) }
            .boxed_local()
            .shared();
        entries.entry(page.clone()).or_default().flight = Some(InFlight {
            id,
            ordinal,
            receive,
        });
        Lookup::Lead(Lead {
            table: self.clone(),
            page: page.clone(),
            id,
            send: Some(send),
        })
    }

    /// Forget every copy of `page`, after a GPU rejected one, so later
    /// readers fetch it again. An in-flight fetch still completes.
    pub(crate) fn forget(&self, page: &PageId) {
        let mut entries = self.entries.borrow_mut();
        if let Some(entry) = entries.get_mut(page) {
            entry.copies.clear();
            if entry.is_empty() {
                entries.remove(page);
            }
        }
    }

    /// Close fetch `id` of `page`, recording `copy` when there is one.
    fn close(&self, page: &PageId, id: u64, copy: Option<&SharedCopy>) {
        let mut entries = self.entries.borrow_mut();
        let Some(entry) = entries.get_mut(page) else {
            return;
        };
        if entry.flight.as_ref().is_some_and(|flight| flight.id == id) {
            entry.flight = None;
        }
        entry.prune();
        if let Some(copy) = copy {
            entry.copies.push(copy.retain());
        }
        if entry.is_empty() {
            entries.remove(page);
        }
    }
}

/// The right and duty to finish one shared fetch. Dropping it, as when the
/// fetching request is cancelled, tells waiters the fetch was abandoned.
pub(super) struct Lead {
    table: Rc<GpuCopies>,
    page: PageId,
    id: u64,
    send: Option<oneshot::Sender<Outcome>>,
}

impl Lead {
    /// Publish the fetch's outcome to the table and to its waiters.
    pub(super) fn finish(mut self, outcome: Outcome) {
        let copy = match &outcome {
            Outcome::Copy(copy) => Some(copy),
            Outcome::Nothing | Outcome::Abandoned => None,
        };
        self.table.close(&self.page, self.id, copy);
        if let Some(send) = self.send.take() {
            let _ = send.send(outcome);
        }
    }
}

impl Drop for Lead {
    fn drop(&mut self) {
        if self.send.is_some() {
            self.table.close(&self.page, self.id, None);
            // Dropping the sender wakes waiters with Abandoned.
            self.send = None;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::*;

    fn page(number: u64) -> PageId {
        PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: racer_control_wire::CacheId(
                        "00000000-0000-4000-8000-000000000001".into(),
                    ),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            number: PageNumber(number),
        }
    }

    fn lead(lookup: Lookup) -> Lead {
        match lookup {
            Lookup::Lead(lead) => lead,
            _ => panic!("expected to lead"),
        }
    }

    #[test]
    /// Waiters on a dropped lead see Abandoned, and the page can be led
    /// again; a finished lead with nothing to share leaves no entry.
    fn dropped_lead_abandons_its_waiters() {
        let helper = crate::gpu::testing::helper(&[None, None]);
        let table = Rc::new(GpuCopies::default());
        let first = lead(table.lookup(&page(0), &helper, 0));
        let Lookup::Join(receive) = table.lookup(&page(0), &helper, 1) else {
            panic!("expected to join");
        };
        drop(first);
        assert!(matches!(
            futures::executor::block_on(receive),
            Outcome::Abandoned
        ));
        assert_eq!(table.len(), 0);
        let second = lead(table.lookup(&page(0), &helper, 1));
        let Lookup::Join(receive) = table.lookup(&page(0), &helper, 0) else {
            panic!("expected to join");
        };
        second.finish(Outcome::Nothing);
        assert!(matches!(
            futures::executor::block_on(receive),
            Outcome::Nothing
        ));
        assert_eq!(table.len(), 0);
    }

    #[test]
    /// A fetch on a GPU that cannot copy from the leader's runs privately.
    fn unreachable_lead_is_not_joined() {
        let helper = crate::gpu::testing::helper_with(&[None, None], |simulation| {
            simulation.set_peer(1, 0, false)
        });
        let table = Rc::new(GpuCopies::default());
        let _lead = lead(table.lookup(&page(0), &helper, 0));
        assert!(matches!(
            table.lookup(&page(0), &helper, 1),
            Lookup::Private
        ));
        assert!(matches!(
            table.lookup(&page(0), &helper, 0),
            Lookup::Join(_)
        ));
    }

    #[test]
    /// The table stops at MAX_PAGES live entries, and makes room again
    /// once their fetches end.
    fn table_is_bounded() {
        let helper = crate::gpu::testing::helper(&[None]);
        let table = Rc::new(GpuCopies::default());
        let leads: Vec<Lead> = (0..MAX_PAGES as u64)
            .map(|number| lead(table.lookup(&page(number), &helper, 0)))
            .collect();
        assert_eq!(table.len(), MAX_PAGES);
        let extra = page(MAX_PAGES as u64);
        assert!(matches!(table.lookup(&extra, &helper, 0), Lookup::Private));
        assert_eq!(table.len(), MAX_PAGES);
        drop(leads);
        assert_eq!(table.len(), 0);
        let _lead = lead(table.lookup(&extra, &helper, 0));
        assert_eq!(table.len(), 1);
    }
}

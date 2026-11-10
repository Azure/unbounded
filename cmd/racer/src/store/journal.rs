//! Per-worker write journal: file IO, group commit, compaction, and replay.
//!
//! The journal is the source of truth for written (write-back) pages and
//! write sessions on one storage worker. It lives at
//! `<directory>/writes/worker-<id>.journal` and uses the
//! [`racer_core::write::journal`] codec: a 64-byte header binding the file to
//! the worker and the storage layout digest, then self-checking records.
//!
//! [`WriteJournal::append`] resolves only after a group commit made the record
//! durable: every data descriptor registered with the batch is `fdatasync`ed
//! first, then the batch is written and the journal itself is `fdatasync`ed.
//! One commit covers every record queued while the previous commit was in
//! flight, up to [`GROUP_RECORDS`] records. A failed or abandoned commit
//! poisons the journal: the outcome of its writes is unknown, so every later
//! append fails until the worker restarts and replays.
//!
//! [`WriteJournal::open`] reads and folds the existing file without writing
//! it; [`WriteJournal::rewrite`] then publishes the folded live state as a
//! fresh file (a compaction), which also drops a torn tail and rebinds the
//! file to the current layout.
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use racer_core::write::JournalRecord;
use racer_core::write::Manifest;
use racer_core::write::SessionId;
use racer_core::write::WriteSpec;
use racer_core::write::journal as codec;
use racer_io::ring::Descriptor;
use racer_io::scoped::SubmissionKind;
use racer_io::scoped::fs::prelude::*;
use std::cell::Cell;
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::ffi::CString;
use std::path::Path;
use std::rc::Rc;
use std::task::Poll;
use std::task::Waker;

/// Most records one group commit writes.
pub const GROUP_RECORDS: usize = 64;

/// Directory under the slab directory that holds the journals.
pub const DIRECTORY: &str = "writes";

/// Deadline of one internal journal operation (open, commit, compaction).
const IO_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(60);

/// Bytes written per SQE when publishing a compacted journal.
const WRITE_CHUNK: usize = 1024 * 1024;

/// Largest journal file [`WriteJournal::open`] reads.
const MAX_FILE_BYTES: u64 = 4 * 1024 * 1024 * 1024;

/// Identity of one written page in the journal: cache, key, ETag, page.
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct PageKey {
    pub cache: String,
    pub key: [u8; 32],
    pub etag: String,
    pub page: u64,
}

/// Identity of one written object version: cache, key, ETag.
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct VersionKey {
    pub cache: String,
    pub key: [u8; 32],
    pub etag: String,
}

/// A live written page as journaled by its last [`JournalRecord::Page`].
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct JournalPage {
    pub segment: u64,
    pub generation: u64,
    pub offset: u64,
    pub length: u32,
    pub session: SessionId,
    pub hold_until_millis: u64,
}

/// The live state of one write session.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum JournalSession {
    /// Opened but not committed.
    Open {
        session: SessionId,
        spec: WriteSpec,
        lease_until_millis: u64,
    },
    /// Committed, with the latest upload state folded in.
    Committed(Manifest),
}

/// The journal folded to its live records.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct JournalState {
    /// Sequence of the last compaction point.
    pub sequence: u64,
    /// Live written pages.
    pub pages: BTreeMap<PageKey, JournalPage>,
    /// Live sessions.
    pub sessions: BTreeMap<VersionKey, JournalSession>,
}

impl JournalState {
    /// Apply one record. `Abort` and `Delete` also drop the version's pages,
    /// so a crash between them and the page releases loses no release.
    pub fn apply(&mut self, record: &JournalRecord) {
        match record.clone() {
            JournalRecord::Page {
                cache,
                key,
                etag,
                page,
                segment,
                generation,
                offset,
                length,
                session,
                hold_until_millis,
            } => {
                self.pages.insert(
                    PageKey {
                        cache,
                        key,
                        etag,
                        page,
                    },
                    JournalPage {
                        segment,
                        generation,
                        offset,
                        length,
                        session,
                        hold_until_millis,
                    },
                );
            }
            JournalRecord::PageRelease {
                cache,
                key,
                etag,
                page,
            } => {
                self.pages.remove(&PageKey {
                    cache,
                    key,
                    etag,
                    page,
                });
            }
            JournalRecord::Open {
                cache,
                key,
                etag,
                session,
                spec,
                lease_until_millis,
            } => {
                let version = VersionKey { cache, key, etag };
                if !matches!(
                    self.sessions.get(&version),
                    Some(JournalSession::Committed(_))
                ) {
                    self.sessions.insert(
                        version,
                        JournalSession::Open {
                            session,
                            spec,
                            lease_until_millis,
                        },
                    );
                }
            }
            JournalRecord::Commit {
                cache,
                key,
                manifest,
            } => {
                let version = VersionKey {
                    cache,
                    key,
                    etag: manifest.etag.clone(),
                };
                self.sessions
                    .insert(version, JournalSession::Committed(manifest));
            }
            JournalRecord::State {
                cache,
                key,
                etag,
                state,
                blob_etag,
            } => {
                if let Some(JournalSession::Committed(manifest)) =
                    self.sessions.get_mut(&VersionKey { cache, key, etag })
                {
                    manifest.state = state;
                    manifest.blob_etag = blob_etag;
                }
            }
            JournalRecord::Abort { cache, key, etag }
            | JournalRecord::Delete { cache, key, etag } => {
                let version = VersionKey { cache, key, etag };
                self.sessions.remove(&version);
                self.pages.retain(|page, _| {
                    page.cache != version.cache
                        || page.key != version.key
                        || page.etag != version.etag
                });
            }
            JournalRecord::Checkpoint { sequence } => self.sequence = sequence,
        }
    }

    /// Fold `records` in order.
    pub fn from_records<'a>(records: impl IntoIterator<Item = &'a JournalRecord>) -> Self {
        let mut state = Self::default();
        for record in records {
            state.apply(record);
        }
        state
    }

    /// The records that rebuild this state, after a compaction point at
    /// `sequence`: the checkpoint, then sessions, then pages.
    pub fn records(&self, sequence: u64) -> Vec<JournalRecord> {
        let mut out = Vec::with_capacity(1 + self.sessions.len() + self.pages.len());
        out.push(JournalRecord::Checkpoint { sequence });
        for (version, session) in &self.sessions {
            out.push(match session {
                JournalSession::Open {
                    session,
                    spec,
                    lease_until_millis,
                } => JournalRecord::Open {
                    cache: version.cache.clone(),
                    key: version.key,
                    etag: version.etag.clone(),
                    session: *session,
                    spec: spec.clone(),
                    lease_until_millis: *lease_until_millis,
                },
                JournalSession::Committed(manifest) => JournalRecord::Commit {
                    cache: version.cache.clone(),
                    key: version.key,
                    manifest: manifest.clone(),
                },
            });
        }
        for (page, entry) in &self.pages {
            out.push(page_record(page, entry));
        }
        out
    }
}

/// The [`JournalRecord::Page`] for `page` at `entry`.
pub fn page_record(page: &PageKey, entry: &JournalPage) -> JournalRecord {
    JournalRecord::Page {
        cache: page.cache.clone(),
        key: page.key,
        etag: page.etag.clone(),
        page: page.page,
        segment: entry.segment,
        generation: entry.generation,
        offset: entry.offset,
        length: entry.length,
        session: entry.session,
        hold_until_millis: entry.hold_until_millis,
    }
}

/// What [`WriteJournal::open`] found.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Opened {
    /// The folded live state. Pages are empty when `layout_changed`.
    pub state: JournalState,
    /// The file was bound to another storage layout or worker: every page it
    /// listed is in `unbound_pages` and must be reported lost.
    pub layout_changed: bool,
    /// Pages of a journal bound to another layout.
    pub unbound_pages: Vec<PageKey>,
    /// Bytes after the last valid record (a torn or corrupt tail).
    pub torn_bytes: u64,
}

/// One queued record.
struct Queued {
    sequence: u64,
    bytes: Vec<u8>,
    record: JournalRecord,
}

/// Commit queue and outcome, shared by appenders.
#[derive(Default)]
struct Commits {
    queue: Vec<Queued>,
    /// Data descriptors to `fdatasync` before the next batch is written.
    syncs: Vec<Rc<Descriptor>>,
    next: u64,
    committed: u64,
    committing: bool,
    failed: Option<Error>,
    waiters: Vec<Waker>,
}

/// Per-worker durable write journal. See the module documentation.
pub struct WriteJournal {
    reactor: Rc<Reactor>,
    directory: Rc<Descriptor>,
    name: CString,
    worker: u16,
    layout: [u8; 32],
    compact_bytes: u64,
    file: RefCell<Option<Rc<Descriptor>>>,
    end: Cell<u64>,
    state: RefCell<JournalState>,
    commits: RefCell<Commits>,
    /// Group commits completed: each is one data and one journal barrier.
    barriers: Cell<u64>,
}

/// Journal file name of `worker`.
pub fn file_name(worker: u16) -> String {
    format!("worker-{worker}.journal")
}

fn internal_scope(reactor: &Reactor) -> Result<RequestScope> {
    let clock = reactor.ctx().clock.clone();
    let deadline = clock.now() + IO_TIMEOUT;
    RequestScope::with_clock(crate::model::RequestId([0; 16]), deadline, clock)
}

/// `fdatasync` one descriptor.
pub(crate) async fn datasync(
    reactor: &Reactor,
    fd: Rc<Descriptor>,
    scope: &RequestScope,
) -> Result<()> {
    reactor
        .run(
            scope,
            None,
            SubmissionKind::OtherIo,
            move |io, ctx| io.fsync(fd, true, ctx),
            |f| f.id(),
        )
        .await
}

/// Write all of `bytes` at `offset`.
async fn write_all(
    reactor: &Reactor,
    fd: &Rc<Descriptor>,
    mut offset: u64,
    bytes: &[u8],
    scope: &RequestScope,
) -> Result<()> {
    for chunk in bytes.chunks(WRITE_CHUNK) {
        let mut buffer = reactor.file_bytes(chunk)?;
        while buffer.remaining() != 0 {
            let done = reactor
                .write_at(fd.clone(), offset, buffer, (), scope)
                .await?;
            buffer = done.buffer;
            if done.bytes == 0 {
                return Err(Error::Io);
            }
            buffer.advance(done.bytes)?;
            offset += done.bytes as u64;
        }
    }
    Ok(())
}

impl WriteJournal {
    /// Open (creating if needed) the journal directory under `slab_directory`
    /// and read the worker's journal, if any, without writing it. Call
    /// [`Self::rewrite`] before the first append.
    pub async fn open(
        reactor: Rc<Reactor>,
        slab_directory: &Path,
        worker: u16,
        layout: [u8; 32],
        compact_bytes: u64,
    ) -> Result<(Self, Opened)> {
        let scope = internal_scope(&reactor)?;
        let path = slab_directory.join(DIRECTORY);
        let directory = reactor.file_directory(&path, true, 4096, &scope).await?;
        let name = CString::new(file_name(worker)).map_err(|_| Error::InvalidConfiguration)?;
        let journal = Self {
            reactor,
            directory,
            name,
            worker,
            layout,
            compact_bytes,
            file: RefCell::new(None),
            end: Cell::new(0),
            state: RefCell::new(JournalState::default()),
            commits: RefCell::new(Commits {
                next: 1,
                ..Commits::default()
            }),
            barriers: Cell::new(0),
        };
        let opened = journal.read_existing(&scope).await?;
        *journal.state.borrow_mut() = opened.state.clone();
        Ok((journal, opened))
    }

    async fn read_existing(&self, scope: &RequestScope) -> Result<Opened> {
        let fd = match self
            .reactor
            .file_open(
                Some(self.directory.clone()),
                self.name.clone(),
                libc::O_RDONLY,
                racer_io::fs::BENEATH | racer_io::fs::NO_SYMLINKS,
                scope,
            )
            .await
        {
            Ok(fd) => fd,
            Err(Error::MissingKey | Error::Os(libc::ENOENT)) => return Ok(Opened::default()),
            Err(error) => return Err(error),
        };
        let stat = self.reactor.file_stat(fd.clone(), scope).await?;
        if stat.stx_size > MAX_FILE_BYTES {
            return Err(Error::Overloaded);
        }
        if stat.stx_size == 0 {
            return Ok(Opened::default());
        }
        let bytes = self
            .reactor
            .file_read_bounded(
                fd,
                stat.stx_size as usize,
                std::num::NonZeroUsize::new(WRITE_CHUNK).expect("nonzero"),
                scope,
            )
            .await?;
        Ok(self.fold_file(&bytes))
    }

    /// Fold a whole journal file.
    pub(crate) fn fold_file(&self, bytes: &[u8]) -> Opened {
        fold_file(bytes, self.worker, &self.layout)
    }

    /// Publish the live state as a fresh journal file: write it to a stage,
    /// `fsync` it, rename it over the journal, and `fsync` the directory.
    /// Waits for no commit; callers serialize it with appends (startup, or
    /// the commit leader).
    pub fn rewrite(&self) -> Operation<'_, ()> {
        Box::pin(async move {
            let scope = internal_scope(&self.reactor)?;
            let result = self.rewrite_in(&scope).await;
            if let Err(error) = result {
                self.commits.borrow_mut().failed.get_or_insert(error);
            }
            result
        })
    }

    async fn rewrite_in(&self, scope: &RequestScope) -> Result<()> {
        let sequence = self.state.borrow().sequence.saturating_add(1);
        self.state.borrow_mut().sequence = sequence;
        let mut bytes = codec::encode_header(self.worker, &self.layout).to_vec();
        for record in self.state.borrow().records(sequence) {
            codec::encode(&record, &mut bytes).map_err(|_| Error::InvalidConfiguration)?;
        }
        let temporary = format!("{}.tmp", file_name(self.worker));
        let staged = self
            .reactor
            .file_stage(
                self.directory.clone(),
                std::ffi::OsStr::new(&temporary),
                255,
                scope,
            )
            .await?;
        write_all(&self.reactor, &staged, 0, &bytes, scope).await?;
        datasync(&self.reactor, staged.clone(), scope).await?;
        let temporary = CString::new(temporary).map_err(|_| Error::InvalidConfiguration)?;
        self.reactor
            .file_rename(self.directory.clone(), temporary, self.name.clone(), scope)
            .await?;
        self.reactor
            .file_sync(self.directory.clone(), scope)
            .await?;
        *self.file.borrow_mut() = Some(staged);
        self.end.set(bytes.len() as u64);
        Ok(())
    }

    /// Queue `record` for the next group commit and resolve once it is
    /// durable. `data`, if any, is `fdatasync`ed before the record is
    /// written, so a durable record never names data that is not. Fails with
    /// the poisoning error once any commit failed. Dropping the future does
    /// not withdraw a queued record.
    pub fn append(&self, record: JournalRecord, data: Option<Rc<Descriptor>>) -> Operation<'_, ()> {
        Box::pin(async move {
            let sequence = self.enqueue(record, data)?;
            loop {
                let lead = std::future::poll_fn(|cx| {
                    let mut commits = self.commits.borrow_mut();
                    if commits.committed >= sequence {
                        return Poll::Ready(Ok(false));
                    }
                    if let Some(error) = commits.failed {
                        return Poll::Ready(Err(error));
                    }
                    if !commits.committing {
                        commits.committing = true;
                        return Poll::Ready(Ok(true));
                    }
                    commits.waiters.push(cx.waker().clone());
                    Poll::Pending
                })
                .await?;
                if !lead {
                    return Ok(());
                }
                self.commit().await;
            }
        })
    }

    fn enqueue(&self, record: JournalRecord, data: Option<Rc<Descriptor>>) -> Result<u64> {
        let mut bytes = Vec::new();
        codec::encode(&record, &mut bytes).map_err(|_| Error::InvalidRequest)?;
        let mut commits = self.commits.borrow_mut();
        if let Some(error) = commits.failed {
            return Err(error);
        }
        if self.file.borrow().is_none() {
            return Err(Error::Unavailable);
        }
        let sequence = commits.next;
        commits.next += 1;
        commits.queue.push(Queued {
            sequence,
            bytes,
            record,
        });
        if let Some(fd) = data
            && !commits.syncs.iter().any(|known| Rc::ptr_eq(known, &fd))
        {
            commits.syncs.push(fd);
        }
        Ok(sequence)
    }

    /// Run one group commit as the leader. Any failure poisons the journal.
    async fn commit(&self) {
        let mut guard = Leader {
            journal: self,
            poison: false,
        };
        // Let appenders that are ready on this turn join the batch.
        racer_io::exec::yield_now().await;
        let (batch, syncs) = {
            let mut commits = self.commits.borrow_mut();
            let take = commits.queue.len().min(GROUP_RECORDS);
            let batch: Vec<Queued> = commits.queue.drain(..take).collect();
            (batch, std::mem::take(&mut commits.syncs))
        };
        if batch.is_empty() {
            return;
        }
        // From here an abandoned commit leaves the file in an unknown state.
        guard.poison = true;
        let result = self.commit_batch(batch, syncs).await;
        if let Err(error) = result {
            self.commits.borrow_mut().failed.get_or_insert(error);
        }
        guard.poison = false;
    }

    async fn commit_batch(&self, batch: Vec<Queued>, syncs: Vec<Rc<Descriptor>>) -> Result<()> {
        let scope = internal_scope(&self.reactor)?;
        for fd in syncs {
            datasync(&self.reactor, fd, &scope).await?;
        }
        let file = self.file.borrow().clone().ok_or(Error::Unavailable)?;
        let bytes: Vec<u8> = batch.iter().flat_map(|q| q.bytes.iter().copied()).collect();
        write_all(&self.reactor, &file, self.end.get(), &bytes, &scope).await?;
        datasync(&self.reactor, file, &scope).await?;
        self.end.set(self.end.get() + bytes.len() as u64);
        self.barriers.set(self.barriers.get() + 1);
        {
            let mut state = self.state.borrow_mut();
            for queued in &batch {
                state.apply(&queued.record);
            }
        }
        let last = batch.last().map_or(0, |q| q.sequence);
        self.commits.borrow_mut().committed = last;
        self.wake();
        if self.end.get() > self.compact_bytes {
            self.rewrite_in(&scope).await?;
        }
        Ok(())
    }

    fn wake(&self) {
        let waiters = std::mem::take(&mut self.commits.borrow_mut().waiters);
        for waiter in waiters {
            waiter.wake();
        }
    }

    /// The folded live state of every durable record.
    pub fn state(&self) -> JournalState {
        self.state.borrow().clone()
    }

    /// Drop pages from the live state without journaling, before
    /// [`Self::rewrite`] publishes it (startup recovery of lost pages).
    pub fn forget_pages(&self, pages: &[PageKey]) {
        let mut state = self.state.borrow_mut();
        for page in pages {
            state.pages.remove(page);
        }
    }

    /// Group commits completed, each one data and one journal barrier.
    pub fn barriers(&self) -> u64 {
        self.barriers.get()
    }

    /// Current journal file length.
    pub fn len(&self) -> u64 {
        self.end.get()
    }

    /// Whether the journal file holds only its header.
    pub fn is_empty(&self) -> bool {
        self.end.get() <= codec::HEADER_BYTES as u64
    }

    /// The poisoning error, if a commit failed.
    pub fn failure(&self) -> Option<Error> {
        self.commits.borrow().failed
    }
}

/// Clears the leader role; an abandoned commit poisons the journal because
/// its writes may or may not have reached the file.
struct Leader<'a> {
    journal: &'a WriteJournal,
    poison: bool,
}

impl Drop for Leader<'_> {
    fn drop(&mut self) {
        {
            let mut commits = self.journal.commits.borrow_mut();
            commits.committing = false;
            if self.poison {
                commits.failed.get_or_insert(Error::Io);
            }
        }
        self.journal.wake();
    }
}

/// Fold a whole journal file of `worker` bound to `layout`.
pub fn fold_file(bytes: &[u8], worker: u16, layout: &[u8; 32]) -> Opened {
    let Ok((bound_worker, bound_layout)) = codec::decode_header(bytes) else {
        return Opened {
            torn_bytes: bytes.len() as u64,
            ..Opened::default()
        };
    };
    let body = &bytes[codec::HEADER_BYTES..];
    let (records, used) = codec::replay(body);
    let mut state = JournalState::from_records(&records);
    let torn_bytes = (body.len() - used) as u64;
    if bound_worker != worker || bound_layout != *layout {
        let unbound_pages = std::mem::take(&mut state.pages).into_keys().collect();
        return Opened {
            state,
            layout_changed: true,
            unbound_pages,
            torn_bytes,
        };
    }
    Opened {
        state,
        layout_changed: false,
        unbound_pages: Vec::new(),
        torn_bytes,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use racer_core::write::Durability;
    use racer_core::write::Holders;
    use racer_core::write::UploadMode;
    use racer_core::write::UploadState;

    fn page(n: u64) -> JournalRecord {
        JournalRecord::Page {
            cache: "c".into(),
            key: [1; 32],
            etag: "\"v\"".into(),
            page: n,
            segment: 2,
            generation: 3,
            offset: 4096 * n,
            length: 4096,
            session: SessionId([9; 16]),
            hold_until_millis: 10,
        }
    }

    fn spec() -> WriteSpec {
        WriteSpec {
            length: 10,
            page_size: 4096,
            content_type: None,
            replicas: 2,
            durability: Durability::Replicas(2),
            upload: UploadMode::Auto,
            hold_until_millis: 0,
        }
    }

    fn manifest(etag: &str) -> Manifest {
        Manifest {
            session: SessionId([9; 16]),
            etag: etag.into(),
            spec: spec(),
            holders: Holders::default(),
            state: UploadState::Pending,
            blob_etag: None,
        }
    }

    #[test]
    fn fold_tracks_pages_sessions_and_round_trips_through_records() {
        let version = |etag: &str| VersionKey {
            cache: "c".into(),
            key: [1; 32],
            etag: etag.into(),
        };
        let records = vec![
            JournalRecord::Open {
                cache: "c".into(),
                key: [1; 32],
                etag: "\"v\"".into(),
                session: SessionId([9; 16]),
                spec: spec(),
                lease_until_millis: 5,
            },
            page(0),
            page(1),
            page(2),
            JournalRecord::PageRelease {
                cache: "c".into(),
                key: [1; 32],
                etag: "\"v\"".into(),
                page: 1,
            },
            JournalRecord::Commit {
                cache: "c".into(),
                key: [1; 32],
                manifest: manifest("\"v\""),
            },
            // An Open after a commit does not reopen it.
            JournalRecord::Open {
                cache: "c".into(),
                key: [1; 32],
                etag: "\"v\"".into(),
                session: SessionId([8; 16]),
                spec: spec(),
                lease_until_millis: 6,
            },
            JournalRecord::State {
                cache: "c".into(),
                key: [1; 32],
                etag: "\"v\"".into(),
                state: UploadState::Committed,
                blob_etag: Some("b".into()),
            },
            // State for an unknown version is ignored.
            JournalRecord::State {
                cache: "c".into(),
                key: [1; 32],
                etag: "\"x\"".into(),
                state: UploadState::Failed,
                blob_etag: None,
            },
            JournalRecord::Open {
                cache: "c".into(),
                key: [1; 32],
                etag: "\"w\"".into(),
                session: SessionId([7; 16]),
                spec: spec(),
                lease_until_millis: 7,
            },
            JournalRecord::Checkpoint { sequence: 4 },
        ];
        let state = JournalState::from_records(&records);
        assert_eq!(state.sequence, 4);
        assert_eq!(
            state.pages.keys().map(|k| k.page).collect::<Vec<_>>(),
            [0, 2]
        );
        let JournalSession::Committed(committed) = &state.sessions[&version("\"v\"")] else {
            panic!("committed session");
        };
        assert_eq!(committed.state, UploadState::Committed);
        assert_eq!(committed.blob_etag.as_deref(), Some("b"));
        assert!(matches!(
            state.sessions[&version("\"w\"")],
            JournalSession::Open { .. }
        ));
        let rebuilt = JournalState::from_records(&state.records(5));
        assert_eq!(rebuilt.pages, state.pages);
        assert_eq!(rebuilt.sessions, state.sessions);
        assert_eq!(rebuilt.sequence, 5);

        // Abort and Delete drop the version's session and pages only.
        let mut state = state;
        state.apply(&JournalRecord::Delete {
            cache: "c".into(),
            key: [1; 32],
            etag: "\"v\"".into(),
        });
        assert!(state.pages.is_empty());
        assert!(!state.sessions.contains_key(&version("\"v\"")));
        state.apply(&JournalRecord::Abort {
            cache: "c".into(),
            key: [1; 32],
            etag: "\"w\"".into(),
        });
        assert!(state.sessions.is_empty());
    }

    #[test]
    fn file_fold_truncates_torn_tails_and_rejects_foreign_bindings() {
        let layout = [5; 32];
        let mut bytes = codec::encode_header(3, &layout).to_vec();
        codec::encode(&page(0), &mut bytes).unwrap();
        codec::encode(&page(1), &mut bytes).unwrap();
        let whole = bytes.len();
        codec::encode(&page(2), &mut bytes).unwrap();
        // Tear the last record.
        bytes.truncate(bytes.len() - 3);
        let opened = fold_file(&bytes, 3, &layout);
        assert!(!opened.layout_changed);
        assert_eq!(opened.state.pages.len(), 2);
        assert_eq!(opened.torn_bytes, (bytes.len() - whole) as u64);
        // A corrupt record ends replay even when later bytes are valid.
        let mut corrupt = bytes[..whole].to_vec();
        let last = corrupt.len() - 1;
        corrupt[last] ^= 1;
        codec::encode(&page(3), &mut corrupt).unwrap();
        let opened = fold_file(&corrupt, 3, &layout);
        assert_eq!(opened.state.pages.len(), 1);
        // Another layout or worker: every page is unbound and reported.
        for (worker, layout) in [(3, [6; 32]), (4, layout)] {
            let opened = fold_file(&bytes, worker, &layout);
            assert!(opened.layout_changed);
            assert!(opened.state.pages.is_empty());
            assert_eq!(opened.unbound_pages.len(), 2);
        }
        // A missing or damaged header is an empty journal.
        for damaged in [&b""[..], &b"RCRWJNL1"[..], &bytes[1..]] {
            let opened = fold_file(damaged, 3, &layout);
            assert_eq!(opened.state, JournalState::default());
            assert_eq!(opened.torn_bytes, damaged.len() as u64);
        }
    }
}

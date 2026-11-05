//! Staged replacement and publication, with file policy supplied by the caller.
use super::*;
use std::ffi::OsStr;

/// The directory must be pinned, trusted, and exclusively controlled throughout
/// this operation. Both names must be distinct single components. The stage must
/// be an empty, singly linked regular file. A verified, nonappend description is
/// reopened for writes, so an O_APPEND description supplied here is never used.
/// Callers serialize access to the stage inode and directory, including via aliases.
pub struct Replacement {
    /// Trusted parent directory pinned throughout validation and publication.
    pub directory: Rc<Descriptor>,

    /// Fresh, empty, single-link stage used to verify the reopened inode.
    pub staged: Rc<Descriptor>,

    /// Stage name as one component within the pinned directory.
    pub temporary: CString,

    /// Distinct destination component replaced atomically by rename.
    pub target: CString,

    /// Requested file and namespace durability fences.
    pub durability: Durability,
}

/// Where publication stopped. An accepted rename may execute even if cancellation
/// wins its completion race. Dropping the future likewise provides no publication
/// outcome; callers must fence outstanding I/O and reconcile the namespace.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ReplacementError<E> {
    /// Validation, writing, or staged-file sync failed before attempting rename.
    BeforeRename(E),
    /// Rename failed or was canceled; reconcile the namespace after fencing.
    RenameUncertain(E),
    /// Rename completed successfully; only the parent durability fence failed.
    Published(E),
}

impl<E: Copy> ReplacementError<E> {
    /// Return the underlying failure without discarding the publication phase.
    pub fn cause(&self) -> E {
        match *self {
            Self::BeforeRename(e) | Self::RenameUncertain(e) | Self::Published(e) => e,
        }
    }
}

/// Explicit fences after writing and after renaming. Stage cleanup/creation
/// fences, when required, must precede this operation and remain caller-owned.
#[derive(Clone, Copy, Debug)]
pub enum Durability {
    /// Namespace publication only; no crash durability is promised.
    Publish,
    /// Sync the staged file before rename, then sync its parent directory.
    FileAndDirectory,
}

/// Input ownership for the shared replacement pipeline.
enum ReplacementInput<'a> {
    /// Consume an already admitted buffer and release it once writing finishes.
    Owned(Buffer),
    /// Copy borrowed input through scratch retained until publication completes.
    Chunked(&'a [u8], NonZeroUsize),
}

/// A validated replacement paired with its verified, nonappend write descriptor.
/// Keeping these together prevents publication with another stage's descriptor.
struct PreparedReplacement {
    replacement: Replacement,

    staged: Rc<Descriptor>,
}

/// A prepared replacement whose entire input has reached its write completion fence.
/// Only this state can enter publication. Scratch remains charged for the same
/// interval as the chunked API's original reusable allocation.
struct WrittenReplacement {
    prepared: PreparedReplacement,

    _scratch: Option<Buffer>,
}

/// Staged replacement sharing the reactor's completion fences.
pub trait ReplaceFiles<S: Scope> {
    /// Write a fresh empty stage, then atomically rename it. Reused/nonempty stages
    /// are rejected, including for empty input. Failure cleanup remains caller-owned.
    fn file_replace<'a>(
        &'a self,
        replacement: Replacement,
        buffer: Buffer,
        scope: &'a S,
    ) -> Operation<'a, (), ReplacementError<S::Error>>;

    /// Write a fresh stage with reusable, bounded scratch, then publish it.
    /// The input may exceed the scratch limit. Stage creation and failure cleanup
    /// remain caller-owned, just as for [`Self::file_replace`].
    fn file_replace_chunked<'a>(
        &'a self,
        replacement: Replacement,
        bytes: &'a [u8],
        chunk: NonZeroUsize,
        scope: &'a S,
    ) -> Operation<'a, (), ReplacementError<S::Error>>;
}

impl<S: Scope> ReplaceFiles<S> for Reactor<S> {
    /// Write a fresh empty stage, then atomically rename it. Reused/nonempty stages
    /// are rejected, including for empty input. Failure cleanup remains caller-owned.
    fn file_replace<'a>(
        &'a self,
        replacement: Replacement,
        buffer: Buffer,
        scope: &'a S,
    ) -> Operation<'a, (), ReplacementError<S::Error>> {
        self.replace_input(replacement, ReplacementInput::Owned(buffer), scope)
    }

    /// Write a fresh stage with reusable, bounded scratch, then publish it.
    /// The input may exceed the scratch limit. Stage creation and failure cleanup
    /// remain caller-owned, just as for [`Self::file_replace`].
    fn file_replace_chunked<'a>(
        &'a self,
        replacement: Replacement,
        bytes: &'a [u8],
        chunk: NonZeroUsize,
        scope: &'a S,
    ) -> Operation<'a, (), ReplacementError<S::Error>> {
        self.replace_input(replacement, ReplacementInput::Chunked(bytes, chunk), scope)
    }
}

/// Replacement steps private to this module.
#[allow(async_fn_in_trait)]
trait ReplaceSteps<S: Scope> {
    /// Share validation, full-write completion, and publication across input forms.
    fn replace_input<'a>(
        &'a self,
        replacement: Replacement,
        input: ReplacementInput<'a>,
        scope: &'a S,
    ) -> Operation<'a, (), ReplacementError<S::Error>>;

    /// Verify a fresh single-link stage and reopen its exact inode without append.
    async fn prepare_replacement(
        &self,
        replacement: Replacement,
        scope: &S,
    ) -> Result<PreparedReplacement, S::Error>;

    /// Finish every input chunk before granting the capability to publish the stage.
    /// Empty input still passes stage validation and preserves allocation policy.
    async fn write_replacement(
        &self,
        prepared: PreparedReplacement,
        input: ReplacementInput<'_>,
        scope: &S,
    ) -> Result<WrittenReplacement, S::Error>;

    /// Consume short writes while retaining the same scratch allocation.
    async fn write_complete(
        &self,
        staged: Rc<Descriptor>,
        buffer: Buffer,
        offset: &mut u64,
        scope: &S,
    ) -> Result<Buffer, S::Error>;

    /// Apply the selected durability fences and preserve the failed publication phase.
    async fn publish_replacement(
        &self,
        written: WrittenReplacement,
        scope: &S,
    ) -> Result<(), ReplacementError<S::Error>>;
}

impl<S: Scope> ReplaceSteps<S> for Reactor<S> {
    /// Share validation, full-write completion, and publication across input forms.
    fn replace_input<'a>(
        &'a self,
        replacement: Replacement,
        input: ReplacementInput<'a>,
        scope: &'a S,
    ) -> Operation<'a, (), ReplacementError<S::Error>> {
        Box::pin(async move {
            let prepared = self
                .prepare_replacement(replacement, scope)
                .await
                .map_err(ReplacementError::BeforeRename)?;
            let written = self
                .write_replacement(prepared, input, scope)
                .await
                .map_err(ReplacementError::BeforeRename)?;
            self.publish_replacement(written, scope).await
        })
    }

    /// Verify a fresh single-link stage and reopen its exact inode without append.
    async fn prepare_replacement(
        &self,
        replacement: Replacement,
        scope: &S,
    ) -> Result<PreparedReplacement, S::Error> {
        for name in [&replacement.temporary, &replacement.target] {
            secure::validate_component(OsStr::from_bytes(name.as_bytes()), PATH_BYTES)?;
        }
        if replacement.temporary == replacement.target {
            return Err(Error::InvalidInput.into());
        }
        let original = self.file_stat(replacement.staged.clone(), scope).await?;
        secure::check_regular_size(&original, 0)?;
        let required = libc::STATX_INO | libc::STATX_NLINK;
        if original.stx_mask & required != required || original.stx_nlink != 1 {
            return Err(Error::InvalidInput.into());
        }
        let staged = self
            .file_open(
                Some(replacement.directory.clone()),
                replacement.temporary.clone(),
                libc::O_WRONLY,
                secure::BENEATH | secure::NO_SYMLINKS,
                scope,
            )
            .await?;
        let current = self.file_stat(staged.clone(), scope).await?;
        secure::check_regular_size(&current, 0)?;
        if current.stx_mask & required != required
            || current.stx_nlink != 1
            || current.stx_ino != original.stx_ino
            || current.stx_dev_major != original.stx_dev_major
            || current.stx_dev_minor != original.stx_dev_minor
        {
            return Err(Error::InvalidInput.into());
        }
        Ok(PreparedReplacement {
            replacement,
            staged,
        })
    }

    /// Finish every input chunk before granting the capability to publish the stage.
    /// Empty input still passes stage validation and preserves allocation policy.
    async fn write_replacement(
        &self,
        prepared: PreparedReplacement,
        input: ReplacementInput<'_>,
        scope: &S,
    ) -> Result<WrittenReplacement, S::Error> {
        let mut offset = 0;
        let scratch = match input {
            ReplacementInput::Owned(buffer) => {
                self.write_complete(prepared.staged.clone(), buffer, &mut offset, scope)
                    .await?;
                None
            }
            ReplacementInput::Chunked(bytes, chunk) => {
                let mut buffer = self.file_buffer(bytes.len().min(chunk.get()))?;
                for bytes in bytes.chunks(chunk.get()) {
                    buffer.start = 0;
                    buffer.end = bytes.len();
                    buffer.data[..bytes.len()].copy_from_slice(bytes);
                    buffer = self
                        .write_complete(prepared.staged.clone(), buffer, &mut offset, scope)
                        .await?;
                }
                Some(buffer)
            }
        };
        Ok(WrittenReplacement {
            prepared,
            _scratch: scratch,
        })
    }

    /// Consume short writes while retaining the same scratch allocation.
    async fn write_complete(
        &self,
        staged: Rc<Descriptor>,
        mut buffer: Buffer,
        offset: &mut u64,
        scope: &S,
    ) -> Result<Buffer, S::Error> {
        while buffer.remaining() != 0 {
            let completion = self
                .write_at(staged.clone(), *offset, buffer, (), scope)
                .await?;
            buffer = completion.buffer;
            buffer.advance(completion.bytes)?;
            *offset += completion.bytes as u64;
        }
        Ok(buffer)
    }

    /// Apply the selected durability fences and preserve the failed publication phase.
    async fn publish_replacement(
        &self,
        written: WrittenReplacement,
        scope: &S,
    ) -> Result<(), ReplacementError<S::Error>> {
        let WrittenReplacement { prepared, _scratch } = written;
        let PreparedReplacement {
            replacement,
            staged,
        } = prepared;
        if matches!(replacement.durability, Durability::FileAndDirectory) {
            self.file_sync(staged.clone(), scope)
                .await
                .map_err(ReplacementError::BeforeRename)?;
        }
        self.file_rename(
            replacement.directory.clone(),
            replacement.temporary,
            replacement.target,
            scope,
        )
        .await
        .map_err(ReplacementError::RenameUncertain)?;
        if matches!(replacement.durability, Durability::FileAndDirectory) {
            self.file_sync(replacement.directory, scope)
                .await
                .map_err(ReplacementError::Published)?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod chunked_tests {
    //! Reused scratch and publication faults exercised through simulated completions.
    use super::*;
    use crate::rt::reactor::{
        filesystem::fixtures::{Admission, Limits, Reactor, scope},
        filesystem::test_support::{drive, poll},
        simulation::{Fault, Simulation},
    };
    use std::path::Path;

    /// Prepare a fresh private stage with namespace-only durability.
    fn replacement(r: &Reactor) -> Replacement {
        let open = |path, flags| {
            drive(
                r,
                r.file_open(None, CString::new(path).unwrap(), flags, 0, &scope()),
            )
            .unwrap()
        };
        Replacement {
            directory: open("/", libc::O_RDONLY | libc::O_DIRECTORY),
            staged: open("/stage", libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL),
            temporary: CString::new("stage").unwrap(),
            target: CString::new("target").unwrap(),
            durability: Durability::Publish,
        }
    }

    /// Publish every chunk despite short writes without promising crash durability.
    #[test]
    fn chunked_publication_handles_short_writes_empty_and_non_durable_crash() {
        for bytes in [vec![], vec![7; 1024 * 1024 + 3]] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = Reactor::new(Rc::new(Admission::new(Limits {
                queue_entries: NonZeroUsize::new(8).unwrap(),
            })));
            sim.write_file(Path::new("/target"), b"old").unwrap();
            sim.disk().sync_all().unwrap();
            sim.set_max_chunk(8191).unwrap();
            drive(
                &r,
                r.file_replace_chunked(
                    replacement(&r),
                    &bytes,
                    NonZeroUsize::new(16384).unwrap(),
                    &scope(),
                ),
            )
            .unwrap();
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), bytes);
            if !bytes.is_empty() {
                let writes: Vec<_> = sim
                    .trace()
                    .into_iter()
                    .filter(|e| e.operation == "complete:write")
                    .map(|e| e.result)
                    .collect();
                assert!(writes.contains(&8191), "no actual short completion");
                assert!(writes.iter().all(|&n| n > 0 && n <= 8191));
            }
            sim.disk().crash().unwrap();
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"old");
        }
    }

    /// Preserve the target and fence accepted owners after failures or abandonment.
    #[test]
    fn chunked_failures_and_abandonment_leave_target_and_fence_stage_owners() {
        for fault in ["write", "zero", "rename", "cancel", "abandon"] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = Reactor::new(Rc::new(Admission::new(Limits {
                queue_entries: NonZeroUsize::new(8).unwrap(),
            })));
            let request = scope();
            sim.write_file(Path::new("/target"), b"old").unwrap();
            let replacement = replacement(&r);
            let weak = Rc::downgrade(&replacement.staged);
            let mut future = r.file_replace_chunked(
                replacement,
                b"new bytes",
                NonZeroUsize::new(3).unwrap(),
                &request,
            );
            match fault {
                "abandon" => {
                    sim.inject("write", Fault::Delay(10)).unwrap();
                    for _ in 0..100 {
                        assert!(poll(&mut future).is_pending());
                        if sim.trace().iter().any(|e| e.operation == "submit:write") {
                            break;
                        }
                        r.poll_budgeted(8).unwrap();
                        r.wait(Duration::from_millis(1)).unwrap();
                    }
                    assert!(sim.trace().iter().any(|e| e.operation == "submit:write"));
                    drop(future);
                    // The securely reopened FD and scratch remain in the SQE,
                    // not the original stage description.
                    assert!(weak.upgrade().is_none());
                    assert_eq!(r.in_flight(), 1);
                    drive(&r, r.file_fence(())).unwrap();
                    assert!(weak.upgrade().is_none());
                    assert_eq!(r.in_flight(), 0);
                }
                "cancel" => {
                    request.cancel().unwrap();
                    assert_eq!(
                        drive(&r, future).map_err(|e| e.cause()),
                        Err(Error::Cancelled)
                    );
                }
                fault => {
                    sim.inject(
                        if fault == "zero" { "write" } else { fault },
                        if fault == "zero" {
                            Fault::Short(0)
                        } else {
                            Fault::Errno(libc::EIO)
                        },
                    )
                    .unwrap();
                    assert_eq!(
                        drive(&r, future).map_err(|e| e.cause()),
                        Err(if fault == "zero" {
                            Error::Io
                        } else {
                            Error::Os(libc::EIO)
                        })
                    );
                }
            }
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"old");
        }
    }
}

#[cfg(test)]
mod tests {
    //! Bounded output ownership and staged publication phase regressions.
    use super::*;
    use crate::rt::reactor::{
        filesystem::fixtures::{Admission, Limits, Reactor, scope},
        filesystem::test_support::{drive, poll},
        simulation::{Fault, Simulation},
    };
    use std::path::Path;

    /// Construct an eight-entry counting reactor in the current environment.
    fn reactor() -> Reactor {
        Reactor::new(Rc::new(Admission::new(Limits {
            queue_entries: NonZeroUsize::new(8).unwrap(),
        })))
    }

    /// Open one test pathname through the same completion path as production.
    fn open(r: &Reactor, path: &str, flags: i32) -> Rc<Descriptor> {
        drive(
            r,
            r.file_open(None, CString::new(path).unwrap(), flags, 0, &scope()),
        )
        .unwrap()
    }

    /// Probe beyond the requested bound while respecting short reads and cancellation.
    #[test]
    fn bounded_reads_cover_empty_exact_short_growth_and_errors() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let request = scope();
        sim.write_file(Path::new("/data"), b"abcdef").unwrap();
        let fd = open(&r, "/data", libc::O_RDONLY);
        sim.set_max_chunk(2).unwrap();
        let read =
            |limit| r.file_read_bounded(fd.clone(), limit, NonZeroUsize::new(3).unwrap(), &request);
        assert_eq!(&*drive(&r, read(6)).unwrap(), b"abcdef");
        let reads: Vec<_> = sim
            .trace()
            .into_iter()
            .filter(|e| e.operation == "complete:read")
            .map(|e| e.result)
            .collect();
        assert!(reads.contains(&2), "missing short read CQE: {reads:?}");
        assert!(
            reads.iter().all(|&n| n <= 2),
            "oversized read CQE: {reads:?}"
        );
        assert!(matches!(drive(&r, read(5)), Err(Error::Overloaded)));
        assert!(matches!(drive(&r, read(0)), Err(Error::Overloaded)));
        assert!(matches!(
            drive(&r, read(usize::MAX)),
            Err(Error::InvalidInput)
        ));
        sim.inject("read", Fault::Errno(libc::EIO)).unwrap();
        assert!(matches!(drive(&r, read(6)), Err(Error::Os(libc::EIO))));
        sim.write_file(Path::new("/empty"), b"").unwrap();
        assert!(
            drive(
                &r,
                r.file_read_bounded(
                    open(&r, "/empty", libc::O_RDONLY),
                    0,
                    NonZeroUsize::new(1).unwrap(),
                    &request
                )
            )
            .unwrap()
            .is_empty()
        );
        request.cancel().unwrap();
        assert!(matches!(drive(&r, read(6)), Err(Error::Cancelled)));
    }

    /// Create a fresh stage and pinned parent using the selected durability policy.
    fn replacement(r: &Reactor, durability: Durability) -> Replacement {
        Replacement {
            directory: open(r, "/", libc::O_RDONLY | libc::O_DIRECTORY),
            staged: open(r, "/stage", libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL),
            temporary: CString::new("stage").unwrap(),
            target: CString::new("target").unwrap(),
            durability,
        }
    }

    /// Preserve each input form's scratch lifetime and fence a shared held rename.
    #[test]
    fn shared_pipeline_preserves_scratch_through_publication() {
        for chunked in [false, true] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let request = scope();
            let replacement = replacement(&r, Durability::Publish);
            let directory = Rc::downgrade(&replacement.directory);
            let staged = Rc::downgrade(&replacement.staged);
            sim.inject("rename", Fault::HoldCompletion(20)).unwrap();
            let mut future = if chunked {
                r.file_replace_chunked(
                    replacement,
                    b"new bytes",
                    NonZeroUsize::new(3).unwrap(),
                    &request,
                )
            } else {
                r.file_replace(replacement, r.file_bytes(b"new bytes").unwrap(), &request)
            };
            for _ in 0..100 {
                assert!(poll(&mut future).is_pending());
                r.poll_budgeted(8).unwrap();
                r.wait(Duration::from_millis(1)).unwrap();
                if sim.trace().iter().any(|e| e.operation == "complete:rename") {
                    break;
                }
            }
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"new bytes");
            assert!(staged.upgrade().is_some());
            drop(future);
            assert!(staged.upgrade().is_none());
            assert!(directory.upgrade().is_some());
            assert_eq!(r.in_flight(), 1);
            drive(&r, r.file_fence(())).unwrap();
            assert!(directory.upgrade().is_none());
            assert_eq!(r.in_flight(), 0);
        }
    }

    /// Honor the selected durability fences after consuming all short writes.
    #[test]
    fn replacement_durability_is_explicit_and_short_writes_are_complete() {
        for durability in [Durability::Publish, Durability::FileAndDirectory] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let request = scope();
            sim.write_file(Path::new("/target"), b"old").unwrap();
            sim.disk().sync_all().unwrap();
            sim.set_max_chunk(2).unwrap();
            let replacement = replacement(&r, durability);
            drive(
                &r,
                r.file_replace(replacement, r.file_bytes(b"replacement").unwrap(), &request),
            )
            .unwrap();
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"replacement");
            let writes: Vec<_> = sim
                .trace()
                .into_iter()
                .filter(|e| e.operation == "complete:write")
                .map(|e| e.result)
                .collect();
            assert!(
                writes.len() > 1 && writes.iter().all(|&n| n > 0 && n <= 2),
                "expected short CQEs: {writes:?}"
            );
            sim.disk().crash().unwrap();
            assert_eq!(
                sim.read_file(Path::new("/target")).unwrap(),
                match durability {
                    Durability::Publish => b"old".as_slice(),
                    Durability::FileAndDirectory => b"replacement".as_slice(),
                }
            );
        }
    }

    /// Reject invalid names and stale stages before any write or publication attempt.
    #[test]
    fn replacement_rejects_nested_names_and_nonempty_or_mismatched_stages() {
        for case in [
            "nested-target",
            "nested-stage",
            "same",
            "reused",
            "empty-reused",
            "mismatched",
        ] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let mut replacement = replacement(&r, Durability::FileAndDirectory);
            sim.write_file(Path::new("/target"), b"old").unwrap();
            match case {
                "nested-target" => replacement.target = CString::new("child/target").unwrap(),
                "nested-stage" => replacement.temporary = CString::new("child/stage").unwrap(),
                "same" => replacement.target = replacement.temporary.clone(),
                "mismatched" => {
                    replacement.staged =
                        open(&r, "/other", libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL)
                }
                _ => {
                    sim.write_file(Path::new("/stage"), b"stale suffix")
                        .unwrap();
                }
            }
            let n = sim.trace().len();
            let result = drive(
                &r,
                r.file_replace(
                    replacement,
                    r.file_bytes(if case == "empty-reused" { b"" } else { b"new" })
                        .unwrap(),
                    &scope(),
                ),
            );
            assert_eq!(
                result,
                Err(ReplacementError::BeforeRename(Error::InvalidInput)),
                "{case}"
            );
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"old");
            assert!(!sim.trace()[n..].iter().any(|e| matches!(
                e.operation.as_str(),
                "submit:write" | "submit:rename" | "submit:fsync"
            )));
        }
    }

    /// Interpret prefixes at the current cursor.
    #[test]
    fn read_output_prefix_uses_current_cursor() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let mut buffer = r.file_bytes(b"abcdef").unwrap();
        buffer.advance(2).unwrap();
        assert_eq!(buffer.prefix(2).unwrap(), b"cd");
        assert_eq!(buffer.prefix(5), Err(Error::Io));
        drop(buffer);
        sim.write_file(Path::new("/data"), b"abc").unwrap();
        r.init().unwrap();
        let out = drive(
            &r,
            r.file_read_bounded(
                open(&r, "/data", libc::O_RDONLY),
                100,
                NonZeroUsize::new(2).unwrap(),
                &scope(),
            ),
        )
        .unwrap();
        assert_eq!(&*out, b"abc");
        drop(out);
    }

    /// Distinguish uncertain rename completion from failed post-publication sync.
    #[test]
    fn post_rename_sync_failure_and_held_cancellation_report_publication_phase() {
        for cancel in [false, true] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let request = scope();
            sim.write_file(Path::new("/target"), b"old").unwrap();
            let replacement = replacement(&r, Durability::FileAndDirectory);
            let weak = Rc::downgrade(&replacement.directory);
            sim.inject("rename", Fault::HoldCompletion(20)).unwrap();
            let mut future = r.file_replace(replacement, r.file_bytes(b"new").unwrap(), &request);
            for _ in 0..100 {
                assert!(poll(&mut future).is_pending());
                r.poll_budgeted(8).unwrap();
                r.wait(Duration::from_millis(1)).unwrap();
                if sim.trace().iter().any(|e| e.operation == "complete:rename") {
                    break;
                }
            }
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"new");
            assert!(weak.upgrade().is_some());
            if cancel {
                request.cancel().unwrap();
            } else {
                sim.inject("fsync", Fault::Errno(libc::EIO)).unwrap();
            }
            let result = drive(&r, future);
            if cancel {
                assert_eq!(
                    result,
                    Err(ReplacementError::RenameUncertain(Error::Cancelled))
                );
            } else {
                assert_eq!(
                    result,
                    Err(ReplacementError::Published(Error::Os(libc::EIO)))
                );
            }
            drive(&r, r.file_fence(())).unwrap();
            assert!(weak.upgrade().is_none());
            assert_eq!(r.in_flight(), 0);
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"new");
        }
    }

    /// Maintain positioned offsets when a completion consumes only part of scratch.
    #[test]
    fn explicitly_injected_short_completions_preserve_offsets_and_reused_scratch() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let request = scope();
        sim.write_file(Path::new("/data"), b"abcdef").unwrap();
        let fd = open(&r, "/data", libc::O_RDONLY);
        sim.inject("read", Fault::Short(1)).unwrap();
        let output = drive(
            &r,
            r.file_read_bounded(fd, 6, NonZeroUsize::new(3).unwrap(), &request),
        )
        .unwrap();
        assert_eq!(&*output, b"abcdef");
        let reads: Vec<_> = sim
            .trace()
            .into_iter()
            .filter(|e| e.operation == "complete:read")
            .map(|e| e.result)
            .collect();
        assert_eq!(reads, [1, 3, 2, 0]);
        let replacement = replacement(&r, Durability::Publish);
        sim.inject("write", Fault::Short(1)).unwrap();
        drive(
            &r,
            r.file_replace_chunked(
                replacement,
                &output,
                NonZeroUsize::new(3).unwrap(),
                &request,
            ),
        )
        .unwrap();
        let writes: Vec<_> = sim
            .trace()
            .into_iter()
            .filter(|e| e.operation == "complete:write")
            .map(|e| e.result)
            .collect();
        assert_eq!(writes, [1, 2, 3]);
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"abcdef");
    }

    /// Leave the old target intact after failed or abandoned prepublication work.
    #[test]
    fn failed_or_abandoned_stage_never_publishes_partial_data() {
        for operation in ["write", "fsync", "rename", "abandon"] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let request = scope();
            sim.write_file(Path::new("/target"), b"old").unwrap();
            let replacement = replacement(&r, Durability::FileAndDirectory);
            let mut future = r.file_replace(replacement, r.file_bytes(b"new").unwrap(), &request);
            if operation == "abandon" {
                sim.inject("write", Fault::Delay(10)).unwrap();
                for _ in 0..100 {
                    assert!(poll(&mut future).is_pending());
                    if sim.trace().iter().any(|e| e.operation == "submit:write") {
                        break;
                    }
                    r.poll_budgeted(8).unwrap();
                    r.wait(Duration::from_millis(1)).unwrap();
                }
                assert!(sim.trace().iter().any(|e| e.operation == "submit:write"));
                assert_eq!(r.in_flight(), 1);
                drop(future);
                drive(&r, r.file_fence(())).unwrap();
                assert_eq!(r.in_flight(), 0);
            } else {
                sim.inject(operation, Fault::Errno(libc::EIO)).unwrap();
                assert_eq!(
                    drive(&r, future).map_err(|e| e.cause()),
                    Err(Error::Os(libc::EIO))
                );
            }
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"old");
        }
    }

    /// Retain a held rename's directory but do not invent a later durability sync.
    #[test]
    fn abandoned_held_rename_keeps_directory_owned_until_fenced() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let request = scope();
        let synced_directories = sim
            .trace()
            .iter()
            .filter(|e| e.operation == "sync:directory")
            .count();
        let replacement = replacement(&r, Durability::FileAndDirectory);
        let weak = Rc::downgrade(&replacement.directory);
        sim.inject("rename", Fault::HoldCompletion(20)).unwrap();
        let mut future = r.file_replace(replacement, r.file_bytes(b"published").unwrap(), &request);
        for _ in 0..100 {
            assert!(poll(&mut future).is_pending());
            r.poll_budgeted(8).unwrap();
            r.wait(Duration::from_millis(1)).unwrap();
            if sim.trace().iter().any(|e| e.operation == "complete:rename") {
                break;
            }
        }
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"published");
        drop(future);
        assert!(weak.upgrade().is_some());
        drive(&r, r.file_fence(())).unwrap();
        assert!(weak.upgrade().is_none());
        assert_eq!(
            sim.trace()
                .iter()
                .filter(|e| e.operation == "sync:directory")
                .count(),
            synced_directories
        );
        assert_eq!(r.in_flight(), 0);
    }

    /// Publish empty input but reject a zero-byte completion for nonempty input.
    #[test]
    fn empty_replacement_and_zero_write_completion_are_distinct() {
        for empty in [true, false] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let request = scope();
            let replacement = replacement(&r, Durability::Publish);
            if !empty {
                sim.inject("write", Fault::Short(0)).unwrap();
            }
            let result = drive(
                &r,
                r.file_replace(
                    replacement,
                    r.file_bytes(if empty { b"" } else { b"x" }).unwrap(),
                    &request,
                ),
            );
            if empty {
                result.unwrap();
                assert!(sim.read_file(Path::new("/target")).unwrap().is_empty());
            } else {
                assert_eq!(result.map_err(|e| e.cause()), Err(Error::Io));
                assert!(sim.read_file(Path::new("/target")).is_err());
            }
        }
    }
}

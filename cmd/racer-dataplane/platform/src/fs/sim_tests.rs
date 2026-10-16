//! Filesystem tests against the simulated world: fault sequences, crash
//! durability and fences that the host tests cannot force.
//!
//! Ported from the legacy runtime's `filesystem::secure_tests`,
//! `operations::chunked_tests` and `operations::tests`. Admission counters
//! of the old fixtures are replaced by a byte-counting [`Meter`] budget.

use super::*;
use crate::reactor::tests::{RequestScope, drive, poll, scope};
use crate::simulation::{EventKind, Fault, Simulation};
use operations::{Durability, Replacement, ReplacementError, publish_new};
use std::{path::Path, task::Poll, time::Duration};
use uring::backend::OpKind;

/// Counts charged bytes so tests can observe retained bookkeeping.
#[derive(Clone, Default)]
struct Meter(Rc<Cell<usize>>);

struct MeterCharge(Rc<Cell<usize>>, usize);

impl Drop for MeterCharge {
    fn drop(&mut self) {
        self.0.set(self.0.get() - self.1);
    }
}

impl Budget for Meter {
    type Charge = MeterCharge;

    fn charge(&self, bytes: usize) -> Result<MeterCharge> {
        self.0.set(self.0.get() + bytes);
        Ok(MeterCharge(self.0.clone(), bytes))
    }
}

impl Meter {
    fn used(&self) -> usize {
        self.0.get()
    }
}

type SimReactor = Reactor<RequestScope, Meter>;

/// An eight-entry counting reactor on the current simulation.
fn reactor() -> (SimReactor, Meter) {
    let meter = Meter::default();
    (Reactor::new(8, meter.clone()), meter)
}

fn fence(r: &SimReactor) {
    drive(r, r.fence_matching(|_| true)).unwrap();
}

/// Open one test path through the same completion path as production.
fn open(r: &SimReactor, path: &str, flags: i32) -> Rc<Descriptor> {
    drive(
        r,
        r.file_open(None, CString::new(path).unwrap(), flags, 0, &scope()),
    )
    .unwrap()
}

fn results(sim: &Simulation, kind: EventKind) -> Vec<i64> {
    sim.trace()
        .into_iter()
        .filter(|e| e.kind == kind)
        .map(|e| e.result)
        .collect()
}

fn traced(sim: &Simulation, kind: EventKind) -> bool {
    sim.trace().iter().any(|e| e.kind == kind)
}

/// Poll `future` while turning the reactor until `kind` appears in the trace.
fn turn_until<T, E>(
    sim: &Simulation,
    r: &SimReactor,
    future: &mut Operation<'_, T, E>,
    kind: EventKind,
) {
    for _ in 0..100 {
        assert!(poll(future).is_pending());
        if traced(sim, kind) {
            return;
        }
        r.poll_budgeted(8).unwrap();
        r.wait(Duration::from_millis(1)).unwrap();
    }
    assert!(traced(sim, kind), "{kind:?} never happened");
}

/// Prepare a fresh stage and pinned parent with the selected durability.
fn replacement(r: &SimReactor, durability: Durability) -> Replacement {
    Replacement {
        directory: open(r, "/", libc::O_RDONLY | libc::O_DIRECTORY),
        staged: open(r, "/stage", libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL),
        temporary: CString::new("stage").unwrap(),
        target: CString::new("target").unwrap(),
        durability,
    }
}

// --- secure_tests ---

/// Keep namespace backing and charges until abandoned operations are fenced.
#[test]
fn abandoned_namespace_changes_retain_descriptor_and_admission() {
    for change in [
        NamespaceChange::EnsureDirectory,
        NamespaceChange::Unlink,
        NamespaceChange::RemoveIfPresent,
    ] {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (r, meter) = reactor();
        let request = scope();
        let directory = drive(&r, r.file_directory(Path::new("/"), false, 4096, &request)).unwrap();
        let weak = Rc::downgrade(&directory);
        let operation = match change {
            NamespaceChange::EnsureDirectory => "mkdir",
            NamespaceChange::Unlink | NamespaceChange::RemoveIfPresent => {
                sim.write_file(Path::new("/entry"), b"old").unwrap();
                "unlink"
            }
        };
        let baseline = meter.used();
        sim.inject(operation, Fault::Delay(10)).unwrap();
        let mut future = r.file_change(directory, CString::new("entry").unwrap(), change, &request);
        assert!(poll(&mut future).is_pending());
        drop(future);
        assert_eq!(r.in_flight(), 1);
        assert!(weak.upgrade().is_some());
        assert!(meter.used() > baseline);
        fence(&r);
        assert_eq!(r.in_flight(), 0);
        assert!(weak.upgrade().is_none());
        assert_eq!(meter.used(), baseline);
    }
}

/// Validate paths before mutation and reject symlinks during pinned traversal.
#[test]
fn traversal_pins_private_directories_and_rejects_parents_symlinks_and_bad_names() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let (r, _) = reactor();
    let request = scope();
    for path in [
        "/must-not-create/../child",
        "/also-absent/child/../../target",
    ] {
        let before = sim.trace().len();
        assert!(matches!(
            drive(&r, r.file_directory(Path::new(path), true, 4096, &request)),
            Err(Error::InvalidConfiguration)
        ));
        assert_eq!(sim.trace().len(), before);
    }
    assert!(sim.metadata(Path::new("/must-not-create")).is_err());
    assert!(sim.metadata(Path::new("/also-absent")).is_err());
    for path in ["/private/child", "private/./child"] {
        let dir = drive(&r, r.file_directory(Path::new(path), true, 4096, &request)).unwrap();
        let stat = drive(&r, r.file_stat(dir.clone(), &request)).unwrap();
        assert_eq!(u32::from(stat.stx_mode), libc::S_IFDIR | 0o700);
        // Simulated worlds report the effective UID, so the private check
        // passes exactly as on the host.
        secure::check_private(&stat, false).unwrap();
    }
    sim.disk().crash().unwrap();
    assert!(sim.metadata(Path::new("/private/child")).is_ok());
    sim.symlink(Path::new("private"), Path::new("/link"))
        .unwrap();
    for (path, limit, error) in [
        ("/link/child", 4096, Error::Os(libc::ELOOP)),
        ("/private/../child", 4096, Error::InvalidConfiguration),
        ("/absent", 4096, Error::NotFound),
        ("/private", 2, Error::InvalidInput),
        ("/bad\0name", 4096, Error::InvalidInput),
    ] {
        let result = drive(
            &r,
            r.file_directory(Path::new(path), false, limit, &request),
        );
        assert!(matches!(result, Err(actual) if actual == error), "{path:?}");
    }
    for path in ["/", ".", ""] {
        assert!(drive(&r, r.file_directory(Path::new(path), false, 4096, &request)).is_ok());
    }
    request.cancel();
    assert!(matches!(
        drive(&r, r.file_directory(Path::new("/"), false, 4096, &request)),
        Err(Error::Canceled)
    ));
}

/// A world that reports another owner fails the private-access check.
#[test]
fn foreign_owner_fails_private_directory_check() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let (r, _) = reactor();
    // SAFETY: geteuid has no preconditions.
    let other = unsafe { libc::geteuid() }.wrapping_add(1);
    sim.world().set_owner(other, 0);
    let request = scope();
    let result = drive(
        &r,
        Box::pin(secure::directory(
            &r,
            Path::new("/private"),
            true,
            true,
            &request,
        )),
    );
    assert!(matches!(result, Err(Error::Os(libc::EACCES))), "{result:?}");
    // SAFETY: as above.
    sim.world().set_owner(unsafe { libc::geteuid() }, 0);
    drive(
        &r,
        Box::pin(secure::directory(
            &r,
            Path::new("/private"),
            true,
            true,
            &request,
        )),
    )
    .unwrap();
}

/// Sync even existing directories and propagate both mkdir and fsync failures.
#[test]
fn existing_directory_creation_reestablishes_fsync_and_propagates_failure() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let (r, _) = reactor();
    sim.create_dir_all(Path::new("/existing")).unwrap();
    sim.inject("fsync", Fault::Errno(libc::EIO)).unwrap();
    assert!(matches!(
        drive(
            &r,
            r.file_directory(Path::new("/existing"), true, 4096, &scope())
        ),
        Err(Error::Os(libc::EIO))
    ));
    drive(
        &r,
        r.file_directory(Path::new("/existing"), true, 4096, &scope()),
    )
    .unwrap();
    sim.disk().crash().unwrap();
    assert!(sim.metadata(Path::new("/existing")).is_ok());
    sim.inject("mkdir", Fault::Errno(libc::EIO)).unwrap();
    assert!(matches!(
        drive(
            &r,
            r.file_directory(Path::new("/new"), true, 4096, &scope())
        ),
        Err(Error::Os(libc::EIO))
    ));
}

/// Fence stage cleanup without following links or publishing abandoned work.
#[test]
fn stage_removes_only_the_link_and_requires_cleanup_fence_before_exclusive_open() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let (r, _) = reactor();
    let request = scope();
    let dir = drive(&r, r.file_directory(Path::new("/"), false, 4096, &request)).unwrap();
    sim.write_file(Path::new("/target"), b"keep").unwrap();
    sim.symlink(Path::new("target"), Path::new("/stage"))
        .unwrap();
    let stage = drive(
        &r,
        r.file_stage(dir.clone(), "stage".as_ref(), 4096, &request),
    )
    .unwrap();
    let stat = drive(&r, r.file_stat(stage, &request)).unwrap();
    assert_eq!(u32::from(stat.stx_mode), libc::S_IFREG | 0o600);
    assert_eq!(stat.stx_size, 0);
    assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"keep");
    assert!(matches!(
        drive(
            &r,
            r.file_stage(dir.clone(), "../target".as_ref(), 4096, &request)
        ),
        Err(Error::InvalidInput)
    ));
    for operation in ["unlink", "fsync", "open"] {
        sim.inject(operation, Fault::Errno(libc::EIO)).unwrap();
        assert!(matches!(
            drive(
                &r,
                r.file_stage(dir.clone(), "stage".as_ref(), 4096, &request)
            ),
            Err(Error::Os(libc::EIO))
        ));
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"keep");
    }
    sim.inject("open", Fault::Errno(libc::EEXIST)).unwrap();
    assert!(matches!(
        drive(
            &r,
            r.file_stage(dir.clone(), "stage".as_ref(), 4096, &request)
        ),
        Err(Error::AlreadyExists)
    ));
    sim.inject("fsync", Fault::Errno(libc::EIO)).unwrap();
    assert_eq!(
        drive(
            &r,
            r.file_remove_synced(dir.clone(), CString::new("absent").unwrap(), &request)
        ),
        Err(Error::Os(libc::EIO))
    );
    let mut abandoned = r.file_stage(dir, "stage".as_ref(), 4096, &request);
    sim.inject("unlink", Fault::Delay(10)).unwrap();
    assert!(poll(&mut abandoned).is_pending());
    drop(abandoned);
    fence(&r);
    assert_eq!(r.in_flight(), 0);
    assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"keep");
}

// --- operations::chunked_tests ---

/// Publish every chunk despite short writes without promising crash durability.
#[test]
fn chunked_publication_handles_short_writes_empty_and_non_durable_crash() {
    for bytes in [vec![], vec![7; 1024 * 1024 + 3]] {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (r, _) = reactor();
        sim.write_file(Path::new("/target"), b"old").unwrap();
        sim.disk().sync_all().unwrap();
        sim.set_max_chunk(8191).unwrap();
        drive(
            &r,
            r.file_replace_chunked(
                replacement(&r, Durability::Publish),
                &bytes,
                NonZeroUsize::new(16384).unwrap(),
                &scope(),
            ),
        )
        .unwrap();
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), bytes);
        if !bytes.is_empty() {
            let writes = results(&sim, EventKind::Complete(OpKind::Write));
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
        let (r, _) = reactor();
        let request = scope();
        sim.write_file(Path::new("/target"), b"old").unwrap();
        let replacement = replacement(&r, Durability::Publish);
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
                turn_until(&sim, &r, &mut future, EventKind::Submit(OpKind::Write));
                drop(future);
                // The securely reopened descriptor and scratch remain with the
                // operation, not the original stage description.
                assert!(weak.upgrade().is_none());
                assert_eq!(r.in_flight(), 1);
                fence(&r);
                assert_eq!(r.in_flight(), 0);
            }
            "cancel" => {
                request.cancel();
                assert_eq!(
                    drive(&r, future).map_err(|e| e.cause()),
                    Err(Error::Canceled)
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

/// Skip occupied candidates and clean failed stages without issuing fsync.
#[test]
fn synchronous_publication_skips_collisions_cleans_failure_and_has_no_fsync() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let directory = Path::new("/");
    let target = Path::new("/target");
    sim.write_file(target, b"old").unwrap();
    sim.write_file(Path::new("/stale"), b"keep").unwrap();
    sim.disk().sync_all().unwrap();
    assert!(publish_new(directory, target, b"new", [PathBuf::from("/stale")]).is_err());
    sim.inject("write", Fault::Short(0)).unwrap();
    assert!(publish_new(directory, target, b"new", [PathBuf::from("/failed")]).is_err());
    assert!(sim.metadata(Path::new("/failed")).is_err());
    sim.inject("rename", Fault::Errno(libc::EIO)).unwrap();
    assert!(publish_new(directory, target, b"new", [PathBuf::from("/failed")]).is_err());
    assert!(sim.metadata(Path::new("/failed")).is_err());
    assert_eq!(sim.read_file(target).unwrap(), b"old");
    sim.set_max_chunk(2).unwrap();
    publish_new(
        directory,
        target,
        b"replacement",
        [PathBuf::from("/stale"), PathBuf::from("/stage")],
    )
    .unwrap();
    assert_eq!(sim.read_file(Path::new("/stale")).unwrap(), b"keep");
    assert_eq!(sim.read_file(target).unwrap(), b"replacement");
    assert!(!traced(
        &sim,
        EventKind::Call(crate::simulation::SyncOp::Fsync)
    ));
    sim.disk().crash().unwrap();
    assert_eq!(sim.read_file(target).unwrap(), b"old");
}

/// Validate containment before mutation and create only private files/directories.
#[test]
fn synchronous_simulation_uses_private_modes_and_validates_before_creation() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let directory = Path::new("/private/child");
    let target = directory.join("target");
    assert!(
        publish_new(
            directory,
            &target,
            b"secret",
            [directory.join("../outside")]
        )
        .is_err()
    );
    assert!(sim.metadata(Path::new("/private")).is_err());
    publish_new(directory, &target, b"secret", [directory.join("stage")]).unwrap();
    let (r, _) = reactor();
    for (path, mode) in [
        (Path::new("/private"), 0o700),
        (directory, 0o700),
        (target.as_path(), 0o600),
    ] {
        let fd = Rc::new(sim.open(None, path, libc::O_RDONLY).unwrap());
        let stat = drive(&r, r.file_stat(fd, &scope())).unwrap();
        assert_eq!(stat.stx_mode & 0o777, mode, "{path:?}");
    }
    sim.symlink(directory, Path::new("/link")).unwrap();
    assert!(
        publish_new(
            Path::new("/link"),
            Path::new("/link/target"),
            b"bad",
            [PathBuf::from("/link/stage")]
        )
        .is_err()
    );
    assert_eq!(sim.read_file(&target).unwrap(), b"secret");
}

// --- operations::tests ---

/// Probe beyond the requested bound while respecting short reads and cancellation.
#[test]
fn bounded_reads_cover_empty_exact_short_growth_and_errors() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let (r, _) = reactor();
    let request = scope();
    sim.write_file(Path::new("/data"), b"abcdef").unwrap();
    let fd = open(&r, "/data", libc::O_RDONLY);
    sim.set_max_chunk(2).unwrap();
    let read =
        |limit| r.file_read_bounded(fd.clone(), limit, NonZeroUsize::new(3).unwrap(), &request);
    assert_eq!(&*drive(&r, read(6)).unwrap(), b"abcdef");
    let reads = results(&sim, EventKind::Complete(OpKind::Read));
    assert!(reads.contains(&2), "missing short read: {reads:?}");
    assert!(reads.iter().all(|&n| n <= 2), "oversized read: {reads:?}");
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
    request.cancel();
    assert!(matches!(drive(&r, read(6)), Err(Error::Canceled)));
}

/// Preserve each input form's scratch lifetime and fence a held rename.
#[test]
fn shared_pipeline_preserves_scratch_accounting_through_publication() {
    let mut retained = Vec::new();
    for chunked in [false, true] {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (r, meter) = reactor();
        let request = scope();
        let replacement = replacement(&r, Durability::Publish);
        let directory = Rc::downgrade(&replacement.directory);
        let staged = Rc::downgrade(&replacement.staged);
        let baseline = meter.used();
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
        turn_until(&sim, &r, &mut future, EventKind::Complete(OpKind::RenameAt));
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"new bytes");
        assert!(staged.upgrade().is_some());
        retained.push(meter.used() - baseline);
        drop(future);
        assert!(staged.upgrade().is_none());
        assert!(directory.upgrade().is_some());
        assert_eq!(r.in_flight(), 1);
        fence(&r);
        assert!(directory.upgrade().is_none());
        assert_eq!(r.in_flight(), 0);
        assert_eq!(meter.used(), baseline);
    }
    // Both paths retain identical rename bookkeeping. Only chunked input keeps
    // its three-byte scratch allocation after all writes have finished.
    assert_eq!(retained[1], retained[0] + 3);
}

/// Honor the selected durability fences after consuming all short writes.
#[test]
fn replacement_durability_is_explicit_and_short_writes_are_complete() {
    for durability in [Durability::Publish, Durability::FileAndDirectory] {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (r, _) = reactor();
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
        let writes = results(&sim, EventKind::Complete(OpKind::Write));
        assert!(
            writes.len() > 1 && writes.iter().all(|&n| n > 0 && n <= 2),
            "expected short completions: {writes:?}"
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

/// Reject invalid names and stale stages before any write or publication.
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
        let (r, _) = reactor();
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
            _ => sim
                .write_file(Path::new("/stage"), b"stale suffix")
                .unwrap(),
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
            e.kind,
            EventKind::Submit(OpKind::Write | OpKind::RenameAt | OpKind::Fsync)
        )));
    }
}

/// Keep output charged until drop and interpret prefixes at the current cursor.
#[test]
fn read_output_owns_budget_and_prefix_uses_current_cursor() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let (r, meter) = reactor();
    let mut buffer = r.file_bytes(b"abcdef").unwrap();
    buffer.advance(2).unwrap();
    assert_eq!(buffer.prefix(2).unwrap(), b"cd");
    assert_eq!(buffer.prefix(5), Err(Error::Io));
    drop(buffer);
    sim.write_file(Path::new("/data"), b"abc").unwrap();
    r.init().unwrap();
    let fd = open(&r, "/data", libc::O_RDONLY);
    let baseline = meter.used();
    let out = drive(
        &r,
        r.file_read_bounded(fd, 100, NonZeroUsize::new(2).unwrap(), &scope()),
    )
    .unwrap();
    assert_eq!(&*out, b"abc");
    assert_eq!(meter.used(), baseline + 100);
    drop(out);
    assert_eq!(meter.used(), baseline);
}

/// Distinguish uncertain rename completion from failed post-publication sync.
///
/// The library lets a successful completion win over cancellation, so a
/// canceled scope observed after a held rename completes reports the
/// rename as uncertain only if the scope check runs before the fsync; here
/// it is asserted as either the legacy `RenameUncertain(Canceled)` or the
/// post-rename phase `Published(Canceled)`.
#[test]
fn post_rename_sync_failure_and_held_cancellation_report_publication_phase() {
    for cancel in [false, true] {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (r, _) = reactor();
        let request = scope();
        sim.write_file(Path::new("/target"), b"old").unwrap();
        let replacement = replacement(&r, Durability::FileAndDirectory);
        let weak = Rc::downgrade(&replacement.directory);
        sim.inject("rename", Fault::HoldCompletion(20)).unwrap();
        let mut future = r.file_replace(replacement, r.file_bytes(b"new").unwrap(), &request);
        turn_until(&sim, &r, &mut future, EventKind::Complete(OpKind::RenameAt));
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"new");
        assert!(weak.upgrade().is_some());
        if cancel {
            request.cancel();
        } else {
            sim.inject("fsync", Fault::Errno(libc::EIO)).unwrap();
        }
        let result = drive(&r, future);
        if cancel {
            assert!(
                matches!(
                    result,
                    Err(ReplacementError::RenameUncertain(Error::Canceled)
                        | ReplacementError::Published(Error::Canceled))
                ),
                "{result:?}"
            );
        } else {
            assert_eq!(
                result,
                Err(ReplacementError::Published(Error::Os(libc::EIO)))
            );
        }
        fence(&r);
        assert!(weak.upgrade().is_none());
        assert_eq!(r.in_flight(), 0);
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"new");
    }
}

/// Reject output allocation before submitting reads when admission is exhausted.
#[test]
fn output_budget_failure_precedes_any_read() {
    /// Budget that rejects every allocation.
    struct Reject;

    impl Budget for Reject {
        type Charge = ();

        fn charge(&self, _: usize) -> Result<()> {
            Err(Error::Overloaded)
        }
    }
    let sim = Simulation::new();
    let _environment = sim.enter();
    sim.write_file(Path::new("/data"), b"abc").unwrap();
    let fd = Rc::new(sim.open(None, Path::new("/data"), libc::O_RDONLY).unwrap());
    let r = Reactor::<RequestScope, Reject>::new(8, Reject);
    let request = scope();
    let mut future = r.file_read_bounded(fd, 3, NonZeroUsize::new(2).unwrap(), &request);
    assert!(matches!(
        poll(&mut future),
        Poll::Ready(Err(Error::Overloaded))
    ));
    assert!(!traced(&sim, EventKind::Submit(OpKind::Read)));
}

/// Maintain positioned offsets when a completion consumes only part of scratch.
#[test]
fn explicitly_injected_short_completions_preserve_offsets_and_reused_scratch() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let (r, _) = reactor();
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
    assert_eq!(
        results(&sim, EventKind::Complete(OpKind::Read)),
        [1, 3, 2, 0]
    );
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
    assert_eq!(results(&sim, EventKind::Complete(OpKind::Write)), [1, 2, 3]);
    assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"abcdef");
}

/// Leave the old target intact after failed or abandoned prepublication work.
#[test]
fn failed_or_abandoned_stage_never_publishes_partial_data() {
    for operation in ["write", "fsync", "rename", "abandon"] {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (r, _) = reactor();
        let request = scope();
        sim.write_file(Path::new("/target"), b"old").unwrap();
        let replacement = replacement(&r, Durability::FileAndDirectory);
        let mut future = r.file_replace(replacement, r.file_bytes(b"new").unwrap(), &request);
        if operation == "abandon" {
            sim.inject("write", Fault::Delay(10)).unwrap();
            turn_until(&sim, &r, &mut future, EventKind::Submit(OpKind::Write));
            assert_eq!(r.in_flight(), 1);
            drop(future);
            fence(&r);
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
    let (r, _) = reactor();
    let request = scope();
    let replacement = replacement(&r, Durability::FileAndDirectory);
    let synced_directories = results(&sim, EventKind::SyncDirectory).len();
    let weak = Rc::downgrade(&replacement.directory);
    sim.inject("rename", Fault::HoldCompletion(20)).unwrap();
    let mut future = r.file_replace(replacement, r.file_bytes(b"published").unwrap(), &request);
    turn_until(&sim, &r, &mut future, EventKind::Complete(OpKind::RenameAt));
    assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"published");
    drop(future);
    assert!(weak.upgrade().is_some());
    fence(&r);
    assert!(weak.upgrade().is_none());
    assert_eq!(
        results(&sim, EventKind::SyncDirectory).len(),
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
        let (r, _) = reactor();
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

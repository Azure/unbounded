use super::test_support::{drive, poll};
use super::*;
use crate::reactor::{
    simulation::{Fault, Simulation},
    tests::{
        fixtures::{Admission, Limits, Reactor},
        scope,
    },
};
use std::{num::NonZeroUsize, path::Path};

fn reactor() -> Reactor {
    Reactor::new(Rc::new(Admission::new(Limits {
        queue_entries: NonZeroUsize::new(8).unwrap(),
    })))
}

fn open(r: &Reactor, path: &str, flags: i32) -> Rc<Descriptor> {
    drive(
        r,
        r.file_open(None, CString::new(path).unwrap(), flags, 0, &scope()),
    )
    .unwrap()
}

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
    // Assert actual CQE sizes, not just the final reconstructed bytes.
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

fn replacement(r: &Reactor, durability: Durability) -> Replacement {
    Replacement {
        directory: open(r, "/", libc::O_RDONLY | libc::O_DIRECTORY),
        staged: open(r, "/stage", libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL),
        temporary: CString::new("stage").unwrap(),
        target: CString::new("target").unwrap(),
        durability,
    }
}

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

#[test]
fn read_output_owns_budget_and_prefix_uses_current_cursor() {
    use crate::reactor::tests::fixtures::ResourceClass;
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
    let baseline = r.admission.used(ResourceClass::RequestContext);
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
    assert_eq!(
        r.admission.used(ResourceClass::RequestContext),
        baseline + 100
    );
    drop(out);
    assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
}

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

#[test]
fn output_budget_failure_precedes_any_read() {
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
    let r = super::super::Reactor::new(8, Reject);
    let request = scope();
    let mut future = r.file_read_bounded(fd, 3, NonZeroUsize::new(2).unwrap(), &request);
    assert!(matches!(
        poll(&mut future),
        Poll::Ready(Err(Error::Overloaded))
    ));
    assert!(!sim.trace().iter().any(|e| e.operation == "submit:read"));
}

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

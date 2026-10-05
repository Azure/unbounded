use super::*;
use crate::reactor::{
    simulation::{Fault, Simulation},
    tests::{
        drive,
        fixtures::{Admission, Limits, Reactor},
        poll, scope,
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
    sim.set_max_chunk(2);
    let read =
        |limit| r.file_read_bounded(fd.clone(), limit, NonZeroUsize::new(3).unwrap(), &request);
    assert_eq!(&*drive(&r, read(6)).unwrap(), b"abcdef");
    assert!(matches!(drive(&r, read(5)), Err(Error::Overloaded)));
    assert!(matches!(drive(&r, read(0)), Err(Error::Overloaded)));
    assert!(matches!(
        drive(&r, read(usize::MAX)),
        Err(Error::InvalidInput)
    ));
    sim.inject("read", Fault::Errno(libc::EIO));
    assert!(matches!(drive(&r, read(6)), Err(Error::Io)));
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
        sim.set_max_chunk(2);
        let replacement = replacement(&r, durability);
        drive(
            &r,
            r.file_replace(replacement, r.file_bytes(b"replacement").unwrap(), &request),
        )
        .unwrap();
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"replacement");
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
            sim.inject("write", Fault::Delay(10));
            assert!(poll(&mut future).is_pending());
            assert_eq!(r.in_flight(), 1);
            drop(future);
            drive(&r, r.file_fence(())).unwrap();
            assert_eq!(r.in_flight(), 0);
        } else {
            sim.inject(operation, Fault::Errno(libc::EIO));
            assert_eq!(drive(&r, future), Err(Error::Io));
        }
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"old");
    }
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
            sim.inject("write", Fault::Short(0));
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
            assert_eq!(result, Err(Error::Io));
            assert!(sim.read_file(Path::new("/target")).is_err());
        }
    }
}

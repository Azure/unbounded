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

#[test]
fn traversal_pins_private_directories_and_rejects_parents_symlinks_and_bad_names() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let r = reactor();
    let request = scope();
    for path in ["/private/child", "private/./child"] {
        let dir = drive(&r, r.file_directory(Path::new(path), true, 4096, &request)).unwrap();
        let stat = drive(&r, r.file_stat(dir, &request)).unwrap();
        assert_eq!(stat.stx_mode as u32, libc::S_IFDIR | 0o700);
    }
    sim.disk().crash().unwrap();
    assert!(sim.metadata(Path::new("/private/child")).is_ok());
    sim.symlink(Path::new("private"), Path::new("/link"))
        .unwrap();
    for (path, limit, error) in [
        ("/link/child", 4096, Error::Io),
        ("/private/../child", 4096, Error::InvalidConfiguration),
        ("/absent", 4096, Error::NotFound),
        ("/private", 2, Error::InvalidInput),
        ("/bad\0name", 4096, Error::InvalidInput),
    ] {
        assert!(
            matches!(drive(&r, r.file_directory(Path::new(path), false, limit, &request)), Err(actual) if actual == error),
            "{path:?}"
        );
    }
    for path in ["/", ".", ""] {
        assert!(drive(&r, r.file_directory(Path::new(path), false, 4096, &request)).is_ok());
    }
    request.cancel().unwrap();
    assert!(matches!(
        drive(&r, r.file_directory(Path::new("/"), false, 4096, &request)),
        Err(Error::Cancelled)
    ));
}

#[test]
fn existing_directory_creation_reestablishes_fsync_and_propagates_failure() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let r = reactor();
    sim.create_dir_all(Path::new("/existing")).unwrap();
    sim.inject("fsync", Fault::Errno(libc::EIO));
    assert!(matches!(
        drive(
            &r,
            r.file_directory(Path::new("/existing"), true, 4096, &scope())
        ),
        Err(Error::Io)
    ));
    drive(
        &r,
        r.file_directory(Path::new("/existing"), true, 4096, &scope()),
    )
    .unwrap();
    sim.disk().crash().unwrap();
    assert!(sim.metadata(Path::new("/existing")).is_ok());
    sim.inject("mkdir", Fault::Errno(libc::EIO));
    assert!(matches!(
        drive(
            &r,
            r.file_directory(Path::new("/new"), true, 4096, &scope())
        ),
        Err(Error::Io)
    ));
}

#[test]
fn stage_removes_only_the_link_and_requires_cleanup_fence_before_exclusive_open() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let r = reactor();
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
    assert_eq!(stat.stx_mode as u32, libc::S_IFREG | 0o600);
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
        sim.inject(operation, Fault::Errno(libc::EIO));
        assert!(matches!(
            drive(
                &r,
                r.file_stage(dir.clone(), "stage".as_ref(), 4096, &request)
            ),
            Err(Error::Io)
        ));
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"keep");
    }
    sim.inject("open", Fault::Errno(libc::EEXIST));
    assert!(matches!(
        drive(
            &r,
            r.file_stage(dir.clone(), "stage".as_ref(), 4096, &request)
        ),
        Err(Error::AlreadyExists)
    ));
    sim.inject("fsync", Fault::Errno(libc::EIO));
    assert_eq!(
        drive(
            &r,
            r.file_remove_synced(dir.clone(), CString::new("absent").unwrap(), &request)
        ),
        Err(Error::Io)
    );
    let mut abandoned = r.file_stage(dir, "stage".as_ref(), 4096, &request);
    sim.inject("unlink", Fault::Delay(10));
    assert!(poll(&mut abandoned).is_pending());
    drop(abandoned);
    drive(&r, r.file_fence(())).unwrap();
    assert_eq!(r.in_flight(), 0);
    assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"keep");
}

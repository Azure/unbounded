//! Bounded scratch staged writes and synchronous namespace-only publication.
use super::*;
use std::num::NonZeroUsize;

impl<S: Scope, B: Budget> Reactor<S, B> {
    /// Write a caller-prepared stage using bounded submission scratch, then
    /// rename it. Stage creation, stale-stage removal, and failure cleanup remain
    /// caller-owned. Unlike a single Buffer, `bytes` may exceed the scratch limit.
    pub fn file_replace_chunked<'a>(
        &'a self,
        replacement: Replacement,
        bytes: &'a [u8],
        chunk: NonZeroUsize,
        scope: &'a S,
    ) -> Operation<'a, (), ReplacementError<S::Error>> {
        Box::pin(async move {
            let staged = self
                .prepare_replacement(&replacement, scope)
                .await
                .map_err(ReplacementError::BeforeRename)?;
            let mut offset = 0u64;
            let mut buffer = self
                .file_buffer(bytes.len().min(chunk.get()))
                .map_err(|e| ReplacementError::BeforeRename(e.into()))?;
            for bytes in bytes.chunks(chunk.get()) {
                buffer.start = 0;
                buffer.end = bytes.len();
                buffer.data[..bytes.len()].copy_from_slice(bytes);
                buffer = self
                    .write_complete(staged.clone(), buffer, &mut offset, scope)
                    .await
                    .map_err(ReplacementError::BeforeRename)?;
            }
            self.publish_replacement(replacement, staged, scope).await
        })
    }
}

#[cfg(all(test, feature = "simulation"))]
mod tests {
    use super::super::test_support::{drive, poll};
    use super::*;
    use crate::reactor::{
        simulation::{Fault, Simulation},
        tests::{
            fixtures::{Admission, Limits, Reactor},
            scope,
        },
    };
    use std::path::{Path, PathBuf};

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
                    // The original stage is no longer the write description;
                    // the securely reopened FD and scratch remain in the SQE.
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
        sim.disk().crash().unwrap();
        assert_eq!(sim.read_file(target).unwrap(), b"old");
    }

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
        let r = Reactor::new(Rc::new(Admission::new(Limits {
            queue_entries: NonZeroUsize::new(8).unwrap(),
        })));
        for (path, mode) in [
            (Path::new("/private"), 0o700),
            (directory, 0o700),
            (target.as_path(), 0o600),
        ] {
            let fd = Rc::new(sim.open(None, path, libc::O_RDONLY).unwrap());
            let stat = drive(&r, r.file_stat(fd, &scope())).unwrap();
            assert_eq!(stat.stx_mode & 0o777, mode);
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
}

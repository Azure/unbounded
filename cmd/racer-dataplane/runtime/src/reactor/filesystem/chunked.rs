//! Bounded scratch staged writes and synchronous namespace-only publication.
use super::*;
use std::{fs, io::Write, num::NonZeroUsize, path::Path, path::PathBuf};

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
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let Replacement {
                directory,
                staged,
                temporary,
                target,
                durability,
            } = replacement;
            let mut offset = 0u64;
            for bytes in bytes.chunks(chunk.get()) {
                let mut buffer = self.file_bytes(bytes)?;
                while buffer.remaining() != 0 {
                    let completion = self
                        .write_at(staged.clone(), offset, buffer, (), scope)
                        .await?;
                    buffer = completion.buffer;
                    buffer.advance(completion.bytes)?;
                    offset += completion.bytes as u64;
                }
            }
            if matches!(durability, Durability::FileAndDirectory) {
                self.file_sync(staged.clone(), scope).await?;
            }
            // Keep the stage owner through rename, as with the original chunked
            // publication. Every submitted write additionally owns its FD/buffer.
            self.file_rename(directory.clone(), temporary, target, scope)
                .await?;
            if matches!(durability, Durability::FileAndDirectory) {
                self.file_sync(directory, scope).await?;
            }
            drop(staged);
            Ok(())
        })
    }
}

#[cfg(all(test, feature = "simulation"))]
mod tests {
    use super::*;
    use crate::reactor::{
        simulation::{Fault, Simulation},
        tests::{
            drive,
            fixtures::{Admission, Limits, Reactor},
            poll, scope,
        },
    };

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
            sim.set_max_chunk(8191);
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
                    sim.inject("write", Fault::Delay(10));
                    assert!(poll(&mut future).is_pending());
                    drop(future);
                    assert!(weak.upgrade().is_some());
                    drive(&r, r.file_fence(())).unwrap();
                    assert!(weak.upgrade().is_none());
                    assert_eq!(r.in_flight(), 0);
                }
                "cancel" => {
                    request.cancel().unwrap();
                    assert_eq!(drive(&r, future), Err(Error::Cancelled));
                }
                fault => {
                    sim.inject(
                        if fault == "zero" { "write" } else { fault },
                        if fault == "zero" {
                            Fault::Short(0)
                        } else {
                            Fault::Errno(libc::EIO)
                        },
                    );
                    assert_eq!(drive(&r, future), Err(Error::Io));
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
        sim.inject("write", Fault::Short(0));
        assert!(publish_new(directory, target, b"new", [PathBuf::from("/failed")]).is_err());
        assert!(sim.metadata(Path::new("/failed")).is_err());
        sim.inject("rename", Fault::Errno(libc::EIO));
        assert!(publish_new(directory, target, b"new", [PathBuf::from("/failed")]).is_err());
        assert!(sim.metadata(Path::new("/failed")).is_err());
        assert_eq!(sim.read_file(target).unwrap(), b"old");
        sim.set_max_chunk(2);
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
}

/// Synchronous, namespace-only replacement. No fsync is issued. The caller
/// supplies bounded, same-directory stage candidates and serializes publication.
/// Existing stage files are skipped, never truncated or removed. Failed writes
/// and renames remove only the stage created by this invocation, best effort.
pub fn publish_new(
    directory: &Path,
    target: &Path,
    bytes: &[u8],
    candidates: impl IntoIterator<Item = PathBuf>,
) -> std::io::Result<()> {
    #[cfg(feature = "simulation")]
    if let Some(sim) = simulation::Simulation::current() {
        sim.create_dir_all(directory)?;
        for temporary in candidates {
            let fd = match sim.open(
                None,
                &temporary,
                libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL,
            ) {
                Ok(fd) => fd,
                Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => continue,
                Err(error) => return Err(error),
            };
            let result = (|| {
                let handle = fd.as_sim().expect("simulated stage");
                let mut offset = 0;
                while offset < bytes.len() {
                    let written = handle.file_write(offset as u64, &bytes[offset..])?;
                    if written == 0 {
                        return Err(std::io::Error::other("zero stage write"));
                    }
                    offset += written;
                }
                drop(fd);
                sim.rename(&temporary, target, 0)
            })();
            if result.is_err() {
                let _ = sim.unlink(&temporary);
            }
            return result;
        }
        return Err(std::io::Error::other("stage candidates exhausted"));
    }
    fs::create_dir_all(directory)?;
    for temporary in candidates {
        let mut file = match fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)
        {
            Ok(file) => file,
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(error) => return Err(error),
        };
        let result = (|| {
            file.write_all(bytes)?;
            drop(file);
            fs::rename(&temporary, target)
        })();
        if result.is_err() {
            let _ = fs::remove_file(&temporary);
        }
        return result;
    }
    Err(std::io::Error::other("stage candidates exhausted"))
}

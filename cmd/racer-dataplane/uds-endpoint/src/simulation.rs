//! Simulated Unix socket ownership with inode-checked cleanup and publication.
//! Layout and access policy are supplied by the application, just as for host sockets.

use crate::publication::{Endpoint, Rename};
use std::{
    cell::RefCell,
    io,
    path::{Path, PathBuf},
};
use uring_runtime::reactor::{SocketAddress, descriptor::Descriptor, simulation::Simulation};

/// A simulated listener and the exact directory entry it owns.
pub struct BoundSocket {
    sim: Simulation,

    listener: Descriptor,

    directory: PathBuf,

    directory_handle: Descriptor,

    inode: u64,

    basename: RefCell<String>,
}

impl BoundSocket {
    /// Create the directory and bind one socket without choosing its permission mode.
    /// Reject invalid basename components before creating any directory.
    pub fn bind(sim: Simulation, directory: PathBuf, basename: &str) -> io::Result<Self> {
        crate::directory::component(basename.as_bytes())?;
        sim.create_dir_all(&directory)?;
        let directory_handle = sim.open(None, &directory, libc::O_PATH | libc::O_DIRECTORY)?;
        let path = directory.join(basename);
        let listener = sim.listen(SocketAddress::Unix(path.clone()))?;
        let (inode, _) = sim.metadata(&path)?;
        Ok(Self {
            sim,
            listener,
            directory,
            directory_handle,
            inode,
            basename: RefCell::new(basename.into()),
        })
    }

    /// Accept one queued connection without performing host syscalls.
    pub fn accept(&self) -> io::Result<Descriptor> {
        self.listener.as_sim().expect("simulated listener").accept()
    }

    /// Return the original bind directory, not its current location after a rename.
    pub fn directory(&self) -> &Path {
        &self.directory
    }

    fn entry_path(&self, basename: &str) -> io::Result<PathBuf> {
        crate::directory::component(basename.as_bytes())?;
        Ok(self
            .sim
            .directory_path(&self.directory_handle)?
            .join(basename))
    }

    fn metadata(&self, basename: &str) -> io::Result<(u64, u16)> {
        self.sim.metadata(&self.entry_path(basename)?)
    }

    /// Set the owned socket's permissions, rejecting a missing or replaced entry.
    /// Reject bits outside 0777 with `InvalidMode` before checking ownership.
    pub fn set_mode(&self, mode: u32) -> io::Result<()> {
        if mode & !0o777 != 0 {
            return Err(crate::error(crate::Error::InvalidMode));
        }
        let basename = self.basename.borrow();
        if !self.owns(&basename) {
            return Err(Self::ownership_error());
        }
        // These synchronous calls cannot yield to another worker-local simulation task.
        self.sim.chmod(&self.entry_path(&basename)?, mode)
    }
}

impl Endpoint for BoundSocket {
    type Error = io::Error;

    /// Match the host adapter's ownership failure category.
    fn ownership_error() -> io::Error {
        super::rejected()
    }

    /// Only the originally bound socket inode is owned, never a replacement entry.
    fn owns(&self, basename: &str) -> bool {
        self.metadata(basename).is_ok_and(|(actual, mode)| {
            actual == self.inode && mode as u32 & libc::S_IFMT == libc::S_IFSOCK
        })
    }

    /// Absence requires a genuine not-found response, not an arbitrary failure.
    fn absent(&self, basename: &str) -> bool {
        self.metadata(basename)
            .is_err_and(|e| e.kind() == io::ErrorKind::NotFound)
    }

    /// Require the same simulation and retained directory, even after a rename.
    fn same_directory(&self, other: &Self) -> io::Result<bool> {
        // Relative open checks descriptor provenance before resolving its inode.
        // Numeric inode and handle IDs alone can collide across simulations.
        let probe = match self.sim.open(
            Some(&other.directory_handle),
            Path::new("."),
            libc::O_PATH | libc::O_DIRECTORY,
        ) {
            Ok(probe) => probe,
            Err(error) if error.raw_os_error() == Some(libc::EXDEV) => return Ok(false),
            Err(error) => return Err(error),
        };
        let retained = self
            .directory_handle
            .as_sim()
            .expect("simulated directory")
            .stat()?
            .stx_ino;
        let other_retained = probe.as_sim().expect("simulated directory").stat()?.stx_ino;
        Ok(retained == other_retained)
    }

    /// Perform the publication's requested atomic rename operation.
    fn rename(&self, from: &str, to: &str, mode: Rename) -> io::Result<()> {
        self.sim
            .rename(&self.entry_path(from)?, &self.entry_path(to)?, mode.flags())
    }

    /// Follow the owned inode after a successful publication or rollback rename.
    fn set_basename(&self, basename: String) {
        *self.basename.borrow_mut() = basename;
    }
}

impl Drop for BoundSocket {
    /// Never unlink another owner's replacement, even after publication changes.
    fn drop(&mut self) {
        let basename = self.basename.borrow();
        if self.owns(&basename)
            && let Ok(path) = self.entry_path(&basename)
        {
            let _ = self.sim.unlink(&path);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::publication::{Publication, Replacement};
    use std::rc::Rc;
    use uring_runtime::reactor::simulation::Fault;

    #[test]
    fn bind_rejects_invalid_names_without_mutation() {
        for basename in [
            "".into(),
            ".".into(),
            "..".into(),
            "/".into(),
            "/escaped".into(),
            "/foreign/file".into(),
            "/foreign/socket".into(),
            "../escaped".into(),
            "../foreign/file".into(),
            "../foreign/socket".into(),
            "child/socket".into(),
            "./socket".into(),
            "socket/".into(),
            "a\0b".into(),
            "x".repeat(256),
            "é".repeat(128),
        ] {
            for directory in ["/new/nested", "/sockets"] {
                let sim = Simulation::new();
                let foreign =
                    BoundSocket::bind(sim.clone(), PathBuf::from("/foreign"), "socket").unwrap();
                let file = Path::new("/foreign/file");
                sim.write_file(file, b"foreign").unwrap();
                sim.create_dir_all(Path::new("/sockets")).unwrap();
                let paths = [
                    "/",
                    "/foreign",
                    "/foreign/socket",
                    "/foreign/file",
                    "/sockets",
                ];
                let before = paths.map(|path| sim.metadata(Path::new(path)).unwrap());

                let result = BoundSocket::bind(sim.clone(), PathBuf::from(directory), &basename);
                let error = result.err().expect("invalid basename must fail");
                assert_eq!(error.kind(), io::ErrorKind::InvalidInput, "{basename:?}");
                assert_eq!(
                    error.get_ref().unwrap().downcast_ref(),
                    Some(&crate::Error::InvalidName),
                    "{basename:?}"
                );
                assert_eq!(
                    paths.map(|path| sim.metadata(Path::new(path)).unwrap()),
                    before
                );
                assert_eq!(sim.read_file(file).unwrap(), b"foreign");
                assert!(foreign.owns("socket"));
                for absent in ["/new", "/escaped", "/sockets/child", "/sockets/socket"] {
                    assert_eq!(
                        sim.metadata(Path::new(absent)).unwrap_err().kind(),
                        io::ErrorKind::NotFound,
                        "{basename:?}: {absent}"
                    );
                }
            }
        }
    }

    #[test]
    fn bind_accepts_normal_components_and_cleans_up() {
        for basename in ["socket", ".hidden", "..suffix", "a\\b", "with space", "é"] {
            let sim = Simulation::new();
            let directory = PathBuf::from("/new/nested");
            let socket = BoundSocket::bind(sim.clone(), directory.clone(), basename).unwrap();
            assert!(socket.owns(basename));
            let connection = sim
                .connect(SocketAddress::Unix(directory.join(basename)))
                .unwrap();
            let accepted = socket.accept().unwrap();
            drop((connection, accepted, socket));
            assert_eq!(
                sim.metadata(&directory.join(basename)).unwrap_err().kind(),
                io::ErrorKind::NotFound
            );
            assert!(sim.metadata(&directory).is_ok());
        }
    }

    #[test]
    fn bind_accepts_component_byte_limit_before_filesystem_errors() {
        for basename in ["x".repeat(255), format!("{}x", "é".repeat(127))] {
            let sim = Simulation::new();
            sim.inject("open", Fault::Errno(libc::EIO)).unwrap();
            let error = BoundSocket::bind(sim, PathBuf::from("/sockets"), &basename)
                .err()
                .expect("directory open must fail");
            assert_eq!(error.raw_os_error(), Some(libc::EIO));
        }
    }

    #[test]
    fn bind_preserves_occupied_foreign_paths() {
        let sim = Simulation::new();
        let directory = PathBuf::from("/sockets");
        let foreign = BoundSocket::bind(sim.clone(), directory.clone(), "socket").unwrap();
        sim.write_file(&directory.join("file"), b"foreign").unwrap();
        for basename in ["socket", "file"] {
            let before = sim.metadata(&directory.join(basename)).unwrap();
            let error = BoundSocket::bind(sim.clone(), directory.clone(), basename)
                .err()
                .expect("occupied basename must fail");
            assert_eq!(error.raw_os_error(), Some(libc::EADDRINUSE));
            assert_eq!(sim.metadata(&directory.join(basename)).unwrap(), before);
        }
        assert_eq!(sim.read_file(&directory.join("file")).unwrap(), b"foreign");
        assert!(foreign.owns("socket"));
    }

    #[test]
    fn publication_rejects_independent_simulations() {
        let first_sim = Simulation::new();
        let second_sim = Simulation::new();
        let directory = PathBuf::from("/sockets");
        let previous =
            Rc::new(BoundSocket::bind(first_sim.clone(), directory.clone(), "canonical").unwrap());
        let foreign =
            BoundSocket::bind(second_sim.clone(), directory.clone(), "canonical").unwrap();
        let next =
            Rc::new(BoundSocket::bind(second_sim.clone(), directory.clone(), "temporary").unwrap());
        foreign.set_mode(0o640).unwrap();
        let canonical = directory.join("canonical");
        let temporary = directory.join("temporary");
        let previous_before = first_sim.metadata(&canonical).unwrap();
        let foreign_before = second_sim.metadata(&canonical).unwrap();
        let next_before = second_sim.metadata(&temporary).unwrap();
        // Equal inode numbers in separate worlds must not establish ownership.
        assert_eq!(previous_before.0, foreign_before.0);
        assert_eq!(
            first_sim.metadata(&directory).unwrap().0,
            second_sim.metadata(&directory).unwrap().0
        );

        let mut publication = Publication::default();
        let result = Replacement::prepare(
            next.clone(),
            Some(previous.clone()),
            "temporary".into(),
            "canonical".into(),
        )
        .and_then(|replacement| publication.publish(replacement));

        assert_eq!(second_sim.metadata(&canonical).unwrap(), foreign_before);
        assert_eq!(second_sim.metadata(&temporary).unwrap(), next_before);
        assert_eq!(first_sim.metadata(&canonical).unwrap(), previous_before);
        assert_eq!(
            result.unwrap_err().get_ref().unwrap().downcast_ref(),
            Some(&crate::Error::OwnershipChanged)
        );
        assert!(!previous.same_directory(&next).unwrap());
        assert!(!next.same_directory(&previous).unwrap());
        drop(publication);
        drop(next);
        drop(previous);
        assert_eq!(second_sim.metadata(&canonical).unwrap(), foreign_before);
        assert!(foreign.owns("canonical"));
    }

    #[test]
    fn publication_succeeds_with_cloned_simulation() {
        let sim = Simulation::new();
        let directory = PathBuf::from("/sockets");
        let previous =
            Rc::new(BoundSocket::bind(sim.clone(), directory.clone(), "canonical").unwrap());
        let next = Rc::new(BoundSocket::bind(sim.clone(), directory.clone(), "temporary").unwrap());
        let replacement = Replacement::prepare(
            next.clone(),
            Some(previous.clone()),
            "temporary".into(),
            "canonical".into(),
        )
        .unwrap();
        let mut publication = Publication::default();
        publication.publish(replacement).unwrap();
        assert!(next.owns("canonical"));
        assert!(previous.owns("temporary"));
        publication.rollback();
        assert!(next.owns("temporary"));
        assert!(previous.owns("canonical"));
    }

    #[test]
    fn same_directory_rejects_replaced_directory() {
        let sim = Simulation::new();
        let directory = PathBuf::from("/sockets");
        let first = BoundSocket::bind(sim.clone(), directory.clone(), "first").unwrap();
        let second = BoundSocket::bind(sim.clone(), directory.clone(), "second").unwrap();
        sim.rename(&directory, Path::new("/retired"), libc::RENAME_NOREPLACE)
            .unwrap();
        let replacement = BoundSocket::bind(sim.clone(), directory, "replacement").unwrap();
        assert!(!first.same_directory(&replacement).unwrap());
        assert!(!replacement.same_directory(&first).unwrap());
        assert!(first.same_directory(&second).unwrap());
        assert!(replacement.owns("replacement"));
    }

    #[test]
    fn publication_and_cleanup_follow_directory_moves() {
        for ancestor in [false, true] {
            for exchange in [false, true] {
                for commit in [false, true] {
                    let sim = Simulation::new();
                    let directory = PathBuf::from("/parent/sockets");
                    let previous = exchange.then(|| {
                        Rc::new(
                            BoundSocket::bind(sim.clone(), directory.clone(), "canonical").unwrap(),
                        )
                    });
                    let next = Rc::new(
                        BoundSocket::bind(sim.clone(), directory.clone(), "temporary").unwrap(),
                    );
                    let source = if ancestor {
                        Path::new("/parent")
                    } else {
                        &directory
                    };
                    sim.rename(source, Path::new("/moved"), libc::RENAME_NOREPLACE)
                        .unwrap();
                    let moved = if ancestor {
                        Path::new("/moved/sockets")
                    } else {
                        Path::new("/moved")
                    };
                    sim.write_file(&directory.join("temporary"), b"foreign temporary")
                        .unwrap();
                    sim.write_file(&directory.join("canonical"), b"foreign canonical")
                        .unwrap();
                    next.set_mode(0o640).unwrap();
                    assert_eq!(
                        sim.metadata(&moved.join("temporary")).unwrap(),
                        (next.inode, (libc::S_IFSOCK | 0o640) as u16)
                    );
                    let replacement = Replacement::prepare(
                        next.clone(),
                        previous.clone(),
                        "temporary".into(),
                        "canonical".into(),
                    )
                    .unwrap();
                    let mut publication = Publication::default();
                    publication.publish(replacement).unwrap();
                    assert!(next.owns("canonical"));
                    let connection = sim
                        .connect(SocketAddress::Unix(moved.join("canonical")))
                        .unwrap();
                    let accepted = next.accept().unwrap();
                    drop((connection, accepted));
                    // Move again between publication and rollback or final cleanup.
                    sim.rename(
                        Path::new("/moved"),
                        Path::new("/retired"),
                        libc::RENAME_NOREPLACE,
                    )
                    .unwrap();
                    let retired = if ancestor {
                        Path::new("/retired/sockets")
                    } else {
                        Path::new("/retired")
                    };
                    if commit {
                        publication.commit();
                    } else {
                        publication.rollback();
                        assert!(next.owns("temporary"));
                        if let Some(previous) = &previous {
                            assert!(previous.owns("canonical"));
                        } else {
                            assert!(next.absent("canonical"));
                        }
                    }
                    drop((publication, next, previous));
                    for basename in ["temporary", "canonical"] {
                        assert_eq!(
                            sim.metadata(&retired.join(basename)).unwrap_err().kind(),
                            io::ErrorKind::NotFound
                        );
                    }
                    assert_eq!(
                        sim.read_file(&directory.join("temporary")).unwrap(),
                        b"foreign temporary"
                    );
                    assert_eq!(
                        sim.read_file(&directory.join("canonical")).unwrap(),
                        b"foreign canonical"
                    );
                    assert_eq!(sim.live_handles(), 0);
                }
            }
        }
    }

    #[test]
    fn moved_directory_preserves_mutation_errors_and_cleanup_names() {
        let sim = Simulation::new();
        let next = Rc::new(
            BoundSocket::bind(sim.clone(), PathBuf::from("/sockets"), "temporary").unwrap(),
        );
        sim.rename(
            Path::new("/sockets"),
            Path::new("/moved"),
            libc::RENAME_NOREPLACE,
        )
        .unwrap();
        let before = sim.metadata(Path::new("/moved/temporary")).unwrap();
        sim.inject("chmod", Fault::Errno(libc::EACCES)).unwrap();
        assert_eq!(
            next.set_mode(0o640).unwrap_err().raw_os_error(),
            Some(libc::EACCES)
        );
        assert_eq!(sim.metadata(Path::new("/moved/temporary")).unwrap(), before);
        let prepare = || {
            Replacement::prepare(next.clone(), None, "temporary".into(), "canonical".into())
                .unwrap()
        };
        let mut publication = Publication::default();
        sim.inject("rename", Fault::Errno(libc::EIO)).unwrap();
        assert_eq!(
            publication.publish(prepare()).unwrap_err().raw_os_error(),
            Some(libc::EIO)
        );
        assert!(next.owns("temporary"));
        assert!(next.absent("canonical"));
        publication.publish(prepare()).unwrap();
        sim.inject("rename", Fault::Errno(libc::EIO)).unwrap();
        publication.rollback();
        assert!(next.owns("canonical"));
        assert!(next.absent("temporary"));
        drop((publication, next));
        assert_eq!(
            sim.metadata(Path::new("/moved/canonical"))
                .unwrap_err()
                .kind(),
            io::ErrorKind::NotFound
        );
        assert_eq!(sim.live_handles(), 0);
    }

    #[test]
    fn moved_directory_preserves_foreign_entries_and_rejects_missing_socket() {
        for replacement in ["missing", "file", "socket", "symlink"] {
            let sim = Simulation::new();
            let original =
                BoundSocket::bind(sim.clone(), PathBuf::from("/sockets"), "socket").unwrap();
            sim.rename(
                Path::new("/sockets"),
                Path::new("/moved"),
                libc::RENAME_NOREPLACE,
            )
            .unwrap();
            let path = Path::new("/moved/socket");
            sim.unlink(path).unwrap();
            let mut foreign = None;
            match replacement {
                "file" => sim.write_file(path, b"foreign").unwrap(),
                "socket" => {
                    foreign = Some(
                        BoundSocket::bind(sim.clone(), PathBuf::from("/moved"), "socket").unwrap(),
                    )
                }
                "symlink" => sim.symlink(Path::new("/target"), path).unwrap(),
                _ => {}
            }
            let before = sim.metadata(path).ok();
            assert!(!original.owns("socket"));
            assert_eq!(original.absent("socket"), replacement == "missing");
            assert_eq!(
                original
                    .set_mode(0o666)
                    .unwrap_err()
                    .get_ref()
                    .unwrap()
                    .downcast_ref(),
                Some(&crate::Error::OwnershipChanged)
            );
            drop(original);
            assert_eq!(sim.metadata(path).ok(), before);
            if replacement == "file" {
                assert_eq!(sim.read_file(path).unwrap(), b"foreign");
            }
            drop(foreign);
        }
    }

    #[test]
    fn same_directory_preserves_probe_errors() {
        let sim = Simulation::new();
        let first = BoundSocket::bind(sim.clone(), PathBuf::from("/first"), "socket").unwrap();
        let second = BoundSocket::bind(sim.clone(), PathBuf::from("/second"), "socket").unwrap();
        assert!(!first.same_directory(&second).unwrap());
        sim.inject("open", Fault::Errno(libc::EIO)).unwrap();
        assert_eq!(
            first.same_directory(&second).unwrap_err().raw_os_error(),
            Some(libc::EIO)
        );
    }

    #[test]
    fn set_mode_accepts_all_permission_bits() {
        let sim = Simulation::new();
        let directory = PathBuf::from("/sockets");
        let socket = BoundSocket::bind(sim.clone(), directory.clone(), "socket").unwrap();
        let path = directory.join("socket");
        let before = sim.metadata(&path).unwrap();

        for mode in 0..=0o777 {
            socket.set_mode(mode).unwrap();
            assert_eq!(
                sim.metadata(&path).unwrap(),
                (before.0, (libc::S_IFSOCK | mode) as u16)
            );
        }
    }

    #[test]
    fn set_mode_rejects_every_non_permission_bit_without_mutation() {
        let sim = Simulation::new();
        let directory = PathBuf::from("/sockets");
        let socket = BoundSocket::bind(sim.clone(), directory.clone(), "socket").unwrap();
        let path = directory.join("socket");
        socket.set_mode(0o640).unwrap();
        let before = sim.metadata(&path).unwrap();

        for mode in (9..u32::BITS)
            .flat_map(|bit| [1u32 << bit, (1u32 << bit) | 0o600])
            .chain([libc::S_IFSOCK | 0o600, libc::S_IFREG | 0o777, u32::MAX])
        {
            let error = socket.set_mode(mode).unwrap_err();
            assert_eq!(error.kind(), io::ErrorKind::InvalidInput, "{mode:#o}");
            assert_eq!(
                error.get_ref().unwrap().downcast_ref(),
                Some(&crate::Error::InvalidMode),
                "{mode:#o}"
            );
            assert_eq!(sim.metadata(&path).unwrap(), before, "{mode:#o}");
        }
    }

    #[test]
    fn set_mode_validates_mode_before_ownership() {
        let sim = Simulation::new();
        let directory = PathBuf::from("/sockets");
        let path = directory.join("socket");
        let socket = BoundSocket::bind(sim.clone(), directory, "socket").unwrap();
        sim.unlink(&path).unwrap();
        sim.write_file(&path, b"foreign").unwrap();
        sim.chmod(&path, 0o640).unwrap();
        let before = sim.metadata(&path).unwrap();

        for mode in [0o1000, 0o2600, 0o4600, libc::S_IFSOCK | 0o600, u32::MAX] {
            let error = socket.set_mode(mode).unwrap_err();
            assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
            assert_eq!(
                error.get_ref().unwrap().downcast_ref(),
                Some(&crate::Error::InvalidMode)
            );
            assert_eq!(sim.metadata(&path).unwrap(), before);
            assert_eq!(sim.read_file(&path).unwrap(), b"foreign");
        }

        sim.unlink(&path).unwrap();
        let error = socket.set_mode(u32::MAX).unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
        assert_eq!(
            error.get_ref().unwrap().downcast_ref(),
            Some(&crate::Error::InvalidMode)
        );
        assert!(socket.absent("socket"));
    }

    #[test]
    fn set_mode_preserves_foreign_socket() {
        let sim = Simulation::new();
        let directory = PathBuf::from("/sockets");
        let path = directory.join("socket");
        let original = BoundSocket::bind(sim.clone(), directory.clone(), "socket").unwrap();
        sim.unlink(&path).unwrap();
        let replacement = BoundSocket::bind(sim.clone(), directory, "socket").unwrap();
        replacement.set_mode(0o640).unwrap();
        let before = sim.metadata(&path).unwrap();
        assert_ne!(before.0, original.inode);

        let result = original.set_mode(0o666);
        assert_eq!(sim.metadata(&path).unwrap(), before);
        assert_eq!(
            result.unwrap_err().get_ref().unwrap().downcast_ref(),
            Some(&crate::Error::OwnershipChanged)
        );
    }

    #[test]
    fn set_mode_preserves_replacement_file() {
        let sim = Simulation::new();
        let directory = PathBuf::from("/sockets");
        let path = directory.join("socket");
        let original = BoundSocket::bind(sim.clone(), directory, "socket").unwrap();
        sim.unlink(&path).unwrap();
        sim.write_file(&path, b"foreign").unwrap();
        sim.chmod(&path, 0o640).unwrap();
        let before = sim.metadata(&path).unwrap();
        assert_ne!(before.0, original.inode);
        assert_eq!(before.1 as u32 & libc::S_IFMT, libc::S_IFREG);

        let result = original.set_mode(0o666);
        assert_eq!(sim.metadata(&path).unwrap(), before);
        assert_eq!(sim.read_file(&path).unwrap(), b"foreign");
        assert_eq!(
            result.unwrap_err().get_ref().unwrap().downcast_ref(),
            Some(&crate::Error::OwnershipChanged)
        );
    }

    #[test]
    fn set_mode_rejects_missing_entry() {
        let sim = Simulation::new();
        let directory = PathBuf::from("/sockets");
        let original = BoundSocket::bind(sim.clone(), directory.clone(), "socket").unwrap();
        sim.unlink(&directory.join("socket")).unwrap();

        assert_eq!(
            original
                .set_mode(0o666)
                .unwrap_err()
                .get_ref()
                .unwrap()
                .downcast_ref(),
            Some(&crate::Error::OwnershipChanged)
        );
        assert!(original.absent("socket"));
    }

    /// Publication bookkeeping follows renames and cleanup preserves replacement inodes.
    #[test]
    fn rename_and_drop_preserve_exact_inode_ownership() {
        let sim = Simulation::new();
        let directory = PathBuf::from("/sockets");
        let first = BoundSocket::bind(sim.clone(), directory.clone(), "first").unwrap();
        first.set_mode(0o666).unwrap();
        assert!(first.owns("first"));
        assert!(first.absent("second"));
        first.rename("first", "second", Rename::NoReplace).unwrap();
        first.set_basename("second".into());
        assert!(first.owns("second"));
        first.set_mode(0o640).unwrap();
        assert_eq!(
            sim.metadata(&directory.join("second")).unwrap().1 as u32 & 0o777,
            0o640
        );
        sim.unlink(&directory.join("second")).unwrap();
        let second = BoundSocket::bind(sim.clone(), directory.clone(), "second").unwrap();
        assert!(!first.owns("second"));
        assert!(first.same_directory(&second).unwrap());
        drop(first);
        assert!(second.owns("second"));
        drop(second);
        assert!(sim.metadata(&directory.join("second")).is_err());
    }
}

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
    pub fn bind(sim: Simulation, directory: PathBuf, basename: &str) -> io::Result<Self> {
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

    /// Return the logical directory used by the simulated filesystem.
    pub fn directory(&self) -> &Path {
        &self.directory
    }

    /// Set the owned socket's permissions, rejecting a missing or replaced entry.
    pub fn set_mode(&self, mode: u32) -> io::Result<()> {
        let basename = self.basename.borrow();
        if !self.owns(&basename) {
            return Err(Self::ownership_error());
        }
        // These synchronous calls cannot yield to another worker-local simulation task.
        self.sim
            .chmod(&self.directory.join(basename.as_str()), mode as _)
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
        self.sim
            .metadata(&self.directory.join(basename))
            .is_ok_and(|(actual, mode)| {
                actual == self.inode && mode as u32 & libc::S_IFMT == libc::S_IFSOCK
            })
    }

    /// Absence requires a genuine not-found response, not an arbitrary failure.
    fn absent(&self, basename: &str) -> bool {
        self.sim
            .metadata(&self.directory.join(basename))
            .is_err_and(|e| e.kind() == io::ErrorKind::NotFound)
    }

    /// Require the same simulation and retained directory, with both paths still valid.
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
        Ok(retained == other_retained
            && self.sim.metadata(&self.directory)?.0 == retained
            && other.sim.metadata(&other.directory)?.0 == other_retained)
    }

    /// Perform the publication's requested atomic rename operation.
    fn rename(&self, from: &str, to: &str, mode: Rename) -> io::Result<()> {
        self.sim.rename(
            &self.directory.join(from),
            &self.directory.join(to),
            mode.flags(),
        )
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
        if self.owns(&basename) {
            let _ = self.sim.unlink(&self.directory.join(basename.as_str()));
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
        assert!(!first.same_directory(&second).unwrap());
        assert!(replacement.owns("replacement"));
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

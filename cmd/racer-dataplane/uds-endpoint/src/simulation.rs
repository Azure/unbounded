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

    inode: u64,

    basename: RefCell<String>,
}

impl BoundSocket {
    /// Create the directory and bind one socket without choosing its permission mode.
    pub fn bind(sim: Simulation, directory: PathBuf, basename: &str) -> io::Result<Self> {
        sim.create_dir_all(&directory)?;
        let path = directory.join(basename);
        let listener = sim.listen(SocketAddress::Unix(path.clone()))?;
        let (inode, _) = sim.metadata(&path)?;
        Ok(Self {
            sim,
            listener,
            directory,
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

    /// Set the current entry's permissions using the simulated filesystem.
    pub fn set_mode(&self, mode: u32) -> io::Result<()> {
        self.sim.chmod(
            &self.directory.join(self.basename.borrow().as_str()),
            mode as _,
        )
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

    /// Compare directory identities through the simulated filesystem.
    fn same_directory(&self, other: &Self) -> io::Result<bool> {
        Ok(self.sim.metadata(&self.directory)?.0 == self.sim.metadata(&other.directory)?.0)
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
        assert_eq!(
            sim.metadata(&directory.join("second")).unwrap().1 as u32 & 0o777,
            0o666
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

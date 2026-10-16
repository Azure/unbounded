//! Durability and crash simulation.
//!
//! The world keeps two images of the filesystem: the volatile image every
//! operation reads and writes, and the durable image that survives a
//! [`Disk::crash`]. Nothing becomes durable implicitly:
//!
//! - syncing a regular file (`fsync`, [`SimFd::sync`](crate::SimFd::sync),
//!   [`Disk::sync`]) makes its data and length durable;
//! - syncing a directory makes its immediate name-to-inode entries durable
//!   (new children are recorded with empty contents until they are synced
//!   themselves), independently of file data and of its ancestors.
//!
//! This is one conservative outcome the POSIX durability contract allows.
//! [`Disk::crash_under`] limits power loss to one subtree, so a multi-node
//! simulation can crash one node's disk (for example `/node-2`) while the
//! others keep running. Open file descriptions under a crashed subtree are
//! closed (later use fails with `EBADF`) and queued asynchronous operations
//! that touch it complete with `EIO`.
//!
//! ```
//! use uring_runtime_sim::{DiskState, World};
//!
//! let world = World::new();
//! world.write_file("/data/log", b"hello").unwrap();
//! world.disk().sync("/data/log").unwrap(); // file data
//! world.disk().sync("/data").unwrap(); // directory entry
//! world.disk().sync("/").unwrap(); // the /data entry itself
//! world.write_file("/data/log", b"lost").unwrap();
//! world.disk().crash().unwrap();
//! assert_eq!(world.read_file("/data/log").unwrap(), b"hello");
//! assert_eq!(
//!     world.disk().read("/data/log", 0, 64, DiskState::Durable).unwrap(),
//!     b"hello"
//! );
//! ```

use std::cell::{Cell, RefCell};
use std::collections::{BTreeMap, BTreeSet};
use std::ffi::OsString;
use std::path::{Path, PathBuf};
use std::rc::{Rc, Weak};

use uring_runtime::Result;

use crate::fs::{
    Node, NodeRef, Pages, check_page_budget, file_type, normalize, overwrite, read_pages,
};
use crate::trace::EventKind;
use crate::world::{Resource, State, World, bounded_bytes, errno};

/// Which image of the disk to read or corrupt.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum DiskState {
    /// The live image that operations see.
    Volatile,
    /// The image that survives a crash.
    Durable,
    /// Both images; only valid for [`Disk::corrupt`].
    Both,
}

/// A durable inode snapshot sharing pages with the live inode.
#[derive(Clone, Debug)]
struct Image {
    mode: u32,
    length: u64,
    pages: Pages,
    symlink: Option<PathBuf>,
}

impl Image {
    fn capture(node: &Node) -> Self {
        Self {
            mode: node.mode,
            length: node.length,
            pages: node.pages.clone(),
            symlink: node.symlink.clone(),
        }
    }

    /// A newly bound inode whose data was never synced.
    fn initial(node: &Node) -> Self {
        Self {
            mode: node.mode,
            length: 0,
            pages: Pages::new(),
            symlink: node.symlink.clone(),
        }
    }

    fn restore(&self, inode: u64) -> Node {
        Node {
            inode,
            mode: self.mode,
            length: self.length,
            pages: self.pages.clone(),
            locked: false,
            symlink: self.symlink.clone(),
        }
    }
}

/// Watches whether a crash hit any path a queued operation touches.
#[derive(Debug)]
pub(crate) struct CrashWatch {
    paths: Vec<PathBuf>,
    crashed: Cell<bool>,
}

impl CrashWatch {
    pub(crate) fn crashed(&self) -> bool {
        self.crashed.get()
    }
}

/// The durable image.
#[derive(Debug, Default)]
pub(crate) struct Durable {
    inodes: BTreeMap<u64, Image>,
    directories: BTreeMap<u64, BTreeMap<OsString, u64>>,
    root: Option<u64>,
    generation: u64,
    watches: Vec<Weak<CrashWatch>>,
}

impl Durable {
    /// Watch `paths` for crashes while the returned handle is alive.
    pub(crate) fn watch(&mut self, paths: Vec<PathBuf>) -> Rc<CrashWatch> {
        self.watches.retain(|watch| watch.strong_count() != 0);
        let watch = Rc::new(CrashWatch {
            paths,
            crashed: Cell::new(false),
        });
        if !watch.paths.is_empty() {
            self.watches.push(Rc::downgrade(&watch));
        }
        watch
    }

    fn invalidate(&mut self, root: &Path) {
        self.watches.retain(|watch| {
            let Some(watch) = watch.upgrade() else {
                return false;
            };
            if watch.paths.iter().any(|path| path.starts_with(root)) {
                watch.crashed.set(true);
            }
            true
        });
    }

    /// Reachable durable names, ignoring directory cycles.
    fn paths(&self) -> BTreeMap<PathBuf, u64> {
        let mut paths = BTreeMap::new();
        let Some(root) = self.root else {
            return paths;
        };
        let mut pending = vec![(PathBuf::from("/"), root, BTreeSet::new())];
        while let Some((path, inode, mut ancestors)) = pending.pop() {
            if !ancestors.insert(inode) {
                continue;
            }
            paths.insert(path.clone(), inode);
            if let Some(entries) = self.directories.get(&inode) {
                for (name, child) in entries {
                    pending.push((path.join(name), *child, ancestors.clone()));
                }
            }
        }
        paths
    }
}

impl State {
    /// Make inode contents and, for a directory, its entries durable.
    fn sync_inode(&mut self, node: &NodeRef) -> Result<()> {
        let n = node.borrow();
        let inode = n.inode;
        let kind = n.kind();
        if kind != libc::S_IFREG && kind != libc::S_IFDIR {
            return Err(errno(libc::EINVAL));
        }
        self.fs.durable.inodes.insert(inode, Image::capture(&n));
        if kind == libc::S_IFDIR
            && let Some(path) = self.fs.path_of(node)
        {
            // An unlinked directory still persists its inode, but cannot
            // recreate its removed parent entry.
            let mut entries = BTreeMap::new();
            for (child, child_node) in &self.fs.paths {
                if child != &path && child.parent() == Some(path.as_path()) {
                    let c = child_node.borrow();
                    if c.kind() == libc::S_IFSOCK {
                        continue;
                    }
                    entries.insert(child.file_name().expect("non-root").to_owned(), c.inode);
                    self.fs
                        .durable
                        .inodes
                        .entry(c.inode)
                        .or_insert_with(|| Image::initial(&c));
                }
            }
            self.fs.durable.directories.insert(inode, entries);
            if path == Path::new("/") {
                self.fs.durable.root = Some(inode);
            }
        }
        drop(n);
        let kind = if kind == libc::S_IFDIR {
            EventKind::SyncDirectory
        } else {
            EventKind::SyncFile
        };
        self.record(kind, inode, 0);
        Ok(())
    }

    /// `fsync` an open description.
    pub(crate) fn sync_file(&mut self, id: u64) -> Result<()> {
        let file = self.file(id)?;
        if file.flags & libc::O_PATH != 0 {
            return Err(errno(libc::EBADF));
        }
        let node = file.node.clone();
        self.sync_inode(&node)
    }

    /// Paths whose crash should fail an operation on description `id`.
    pub(crate) fn watched_path(&self, id: u64) -> Option<PathBuf> {
        self.file(id).ok().map(|file| file.opened_path.clone())
    }
}

/// Persistence, power loss and corruption controls for a [`World`].
///
/// Obtained with [`World::disk`].
#[derive(Clone, Debug)]
pub struct Disk(World);

impl World {
    /// The world's disk controls.
    pub fn disk(&self) -> Disk {
        Disk(self.clone())
    }
}

impl Disk {
    /// Sync one existing file or directory (not its parent or children).
    pub fn sync(&self, path: impl AsRef<Path>) -> Result<()> {
        let path = normalize(path.as_ref())?;
        let mut state = self.0.state();
        let node = state
            .fs
            .paths
            .get(&path)
            .ok_or_else(|| errno(libc::ENOENT))?
            .clone();
        state.sync_inode(&node)
    }

    /// Make every current file and directory durable: a provisioning
    /// barrier for fixtures.
    pub fn sync_all(&self) -> Result<()> {
        let mut state = self.0.state();
        let nodes: Vec<NodeRef> = state
            .fs
            .paths
            .values()
            .filter(|node| matches!(node.borrow().kind(), libc::S_IFREG | libc::S_IFDIR))
            .cloned()
            .collect();
        for node in nodes {
            state.sync_inode(&node)?;
        }
        Ok(())
    }

    /// Lose power for the whole disk; equivalent to `crash_under("/")`.
    pub fn crash(&self) -> Result<()> {
        self.crash_under("/")
    }

    /// Lose power for the subtree at absolute path `root`: its volatile
    /// image is replaced by its durable image, open file descriptions in it
    /// are closed, and queued operations touching it will fail with `EIO`.
    /// Sockets, pipes and other subtrees are unaffected.
    ///
    /// Fails with `EINVAL` for a relative path.
    pub fn crash_under(&self, root: impl AsRef<Path>) -> Result<()> {
        let root = root.as_ref();
        if !root.is_absolute() {
            return Err(errno(libc::EINVAL));
        }
        let root = normalize(root)?;
        let mut state = self.0.state();
        let state = &mut *state;
        state.fs.durable.generation += 1;
        let generation = state.fs.durable.generation;
        state.fs.durable.invalidate(&root);
        let lost: BTreeSet<u64> = state
            .fs
            .paths
            .iter()
            .filter(|(path, _)| path.starts_with(&root))
            .map(|(_, node)| node.borrow().inode)
            .collect();
        let closed: Vec<u64> = state
            .resources
            .iter()
            .filter(|(_, resource)| {
                matches!(resource, Resource::File(file)
                    if file.opened_path.starts_with(&root) || lost.contains(&file.node.borrow().inode))
            })
            .map(|(id, _)| *id)
            .collect();
        for id in closed {
            state.resources.remove(&id);
            state.unowned.remove(&id);
        }
        state.fs.paths.retain(|path, _| !path.starts_with(&root));
        let mut restored = BTreeMap::<u64, NodeRef>::new();
        for (path, inode) in state.fs.durable.paths() {
            if !path.starts_with(&root) {
                continue;
            }
            let image = &state.fs.durable.inodes[&inode];
            let node = restored
                .entry(inode)
                .or_insert_with(|| Rc::new(RefCell::new(image.restore(inode))))
                .clone();
            state.fs.paths.insert(path, node);
        }
        let mut reachable: BTreeSet<u64> = state.fs.durable.paths().values().copied().collect();
        reachable.extend(state.fs.paths.values().map(|node| node.borrow().inode));
        reachable.extend(
            state
                .resources
                .values()
                .filter_map(|resource| match resource {
                    Resource::File(file) => Some(file.node.borrow().inode),
                    _ => None,
                }),
        );
        let durable = &mut state.fs.durable;
        durable.inodes.retain(|inode, _| reachable.contains(inode));
        durable
            .directories
            .retain(|inode, _| reachable.contains(inode));
        state.record(EventKind::Crash, generation, lost.len() as i64);
        Ok(())
    }

    /// Read up to `length` bytes at `offset` of a regular file from one
    /// image, without materializing sparse holes beyond the range.
    ///
    /// Fails with `EINVAL` for [`DiskState::Both`], `ENOENT` if the path does
    /// not exist in that image and `EISDIR` if it is not a regular file.
    pub fn read(
        &self,
        path: impl AsRef<Path>,
        offset: u64,
        length: usize,
        state: DiskState,
    ) -> Result<Vec<u8>> {
        let path = normalize(path.as_ref())?;
        let world = self.0.state();
        let image = match state {
            DiskState::Volatile => Image::capture(
                &world
                    .fs
                    .paths
                    .get(&path)
                    .ok_or_else(|| errno(libc::ENOENT))?
                    .borrow(),
            ),
            DiskState::Durable => {
                let inode = *world
                    .fs
                    .durable
                    .paths()
                    .get(&path)
                    .ok_or_else(|| errno(libc::ENOENT))?;
                world.fs.durable.inodes[&inode].clone()
            }
            DiskState::Both => return Err(errno(libc::EINVAL)),
        };
        if file_type(image.mode) != libc::S_IFREG {
            return Err(errno(libc::EISDIR));
        }
        let available = usize::try_from(image.length.saturating_sub(offset)).unwrap_or(usize::MAX);
        let mut bytes = bounded_bytes(length.min(available))?;
        read_pages(&image.pages, offset, &mut bytes);
        Ok(bytes)
    }

    /// Overwrite existing bytes of a regular file in one or both images,
    /// without changing its length.
    ///
    /// Durable-only corruption becomes visible after a crash. With
    /// [`DiskState::Both`] the volatile and durable names must refer to the
    /// same inode (`ESTALE` otherwise), so a replaced file is never corrupted
    /// by accident. The range must lie within the file (`EINVAL`).
    /// Validation happens before any change.
    pub fn corrupt(
        &self,
        path: impl AsRef<Path>,
        offset: u64,
        bytes: &[u8],
        state: DiskState,
    ) -> Result<()> {
        let path = normalize(path.as_ref())?;
        let end = offset
            .checked_add(bytes.len() as u64)
            .ok_or_else(|| errno(libc::EFBIG))?;
        let mut world = self.0.state();
        let volatile = match state {
            DiskState::Durable => None,
            _ => Some(
                world
                    .fs
                    .paths
                    .get(&path)
                    .ok_or_else(|| errno(libc::ENOENT))?
                    .clone(),
            ),
        };
        let durable = match state {
            DiskState::Volatile => None,
            _ => Some(
                *world
                    .fs
                    .durable
                    .paths()
                    .get(&path)
                    .ok_or_else(|| errno(libc::ENOENT))?,
            ),
        };
        if let Some(node) = &volatile {
            let n = node.borrow();
            if n.kind() != libc::S_IFREG || end > n.length {
                return Err(errno(libc::EINVAL));
            }
            if durable.is_some_and(|inode| inode != n.inode) {
                return Err(errno(libc::ESTALE));
            }
            check_page_budget(&n.pages, offset, bytes.len())?;
        }
        if let Some(inode) = durable {
            let image = &world.fs.durable.inodes[&inode];
            if file_type(image.mode) != libc::S_IFREG || end > image.length {
                return Err(errno(libc::EINVAL));
            }
            check_page_budget(&image.pages, offset, bytes.len())?;
        }
        if let Some(node) = &volatile {
            overwrite(&mut node.borrow_mut().pages, offset, bytes);
        }
        if let Some(inode) = durable {
            let image = world.fs.durable.inodes.get_mut(&inode).expect("validated");
            overwrite(&mut image.pages, offset, bytes);
        }
        let inode = durable.unwrap_or_else(|| volatile.as_ref().expect("one image").borrow().inode);
        world.record(EventKind::Corrupt(state), inode, bytes.len() as i64);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::fault::{Fault, FaultTarget, SyncOp};

    fn read(world: &World, path: &str) -> Vec<u8> {
        world.disk().read(path, 0, 64, DiskState::Volatile).unwrap()
    }

    #[test]
    fn file_sync_and_namespace_sync_are_independent() {
        for (file_sync, dir_sync) in [(false, false), (true, false), (false, true), (true, true)] {
            let world = World::new();
            world.create_dir_all("/disk").unwrap();
            world.disk().sync_all().unwrap();
            world.write_file("/disk/new", b"contents").unwrap();
            if file_sync {
                world
                    .open("/disk/new", libc::O_RDWR)
                    .unwrap()
                    .sync()
                    .unwrap();
            }
            if dir_sync {
                world
                    .open("/disk", libc::O_DIRECTORY)
                    .unwrap()
                    .sync()
                    .unwrap();
            }
            world.disk().crash().unwrap();
            if dir_sync {
                let expected: &[u8] = if file_sync { b"contents" } else { b"" };
                assert_eq!(read(&world, "/disk/new"), expected);
            } else {
                let error = world.read_file("/disk/new").unwrap_err();
                assert_eq!(error.raw_os_error(), Some(libc::ENOENT));
            }
        }
    }

    #[test]
    fn replacement_unlink_and_directory_ancestors_require_namespace_fences() {
        let world = World::new();
        let disk = world.disk();
        world.write_file("/disk/current", b"old").unwrap();
        disk.sync_all().unwrap();
        world.write_file("/disk/stage", b"new").unwrap();
        disk.sync("/disk/stage").unwrap();
        world.rename("/disk/stage", "/disk/current", 0).unwrap();
        disk.crash().unwrap();
        assert_eq!(read(&world, "/disk/current"), b"old");
        world.write_file("/disk/stage", b"new").unwrap();
        disk.sync("/disk/stage").unwrap();
        world.rename("/disk/stage", "/disk/current", 0).unwrap();
        disk.sync("/disk").unwrap();
        disk.crash().unwrap();
        assert_eq!(read(&world, "/disk/current"), b"new");
        world.unlink("/disk/current").unwrap();
        disk.crash().unwrap();
        assert_eq!(read(&world, "/disk/current"), b"new");
        world.unlink("/disk/current").unwrap();
        disk.sync("/disk").unwrap();
        disk.crash().unwrap();
        assert!(world.read_file("/disk/current").is_err());
        world.write_file("/disk/child/file", b"hidden").unwrap();
        disk.sync("/disk/child/file").unwrap();
        disk.sync("/disk/child").unwrap();
        disk.crash().unwrap();
        assert!(world.metadata("/disk/child").is_err());
    }

    #[test]
    fn corruption_and_truncate_do_not_mutate_durable_shared_pages() {
        let world = World::new();
        world.write_file("/file", b"abcdef").unwrap();
        world.disk().sync_all().unwrap();
        let fd = world.open("/file", libc::O_RDWR).unwrap();
        fd.set_len(2).unwrap();
        fd.set_len(6).unwrap();
        assert_eq!(read(&world, "/file"), b"ab\0\0\0\0");
        assert_eq!(
            world
                .disk()
                .read("/file", 0, 8, DiskState::Durable)
                .unwrap(),
            b"abcdef"
        );
        world
            .disk()
            .corrupt("/file", 1, b"X", DiskState::Volatile)
            .unwrap();
        world
            .disk()
            .corrupt("/file", 4, b"Y", DiskState::Durable)
            .unwrap();
        world.disk().crash().unwrap();
        assert_eq!(read(&world, "/file"), b"abcdYf");
        assert_eq!(
            fd.stat().unwrap_err().raw_os_error(),
            Some(libc::EBADF),
            "crash closes open descriptions"
        );
        let fd = world.open("/file", libc::O_RDWR).unwrap();
        fd.write_at(1 << 40, b"sparse").unwrap();
        fd.sync().unwrap();
        assert_eq!(
            world
                .disk()
                .read("/file", (1 << 40) - 2, 10, DiskState::Durable)
                .unwrap(),
            b"\0\0sparse"
        );
        assert!(
            world
                .disk()
                .corrupt("/file", u64::MAX, b"bad", DiskState::Both)
                .is_err()
        );
        assert_eq!(
            world
                .disk()
                .read("/file", 0, 1, DiskState::Both)
                .unwrap_err()
                .raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(
            world
                .disk()
                .crash_under("relative")
                .unwrap_err()
                .raw_os_error(),
            Some(libc::EINVAL)
        );
    }

    #[test]
    fn renamed_directory_fsync_uses_inode_and_corruption_targets_exact_generation() {
        let world = World::new();
        world.write_file("/old/file", b"before").unwrap();
        world.disk().sync_all().unwrap();
        let directory = world.open("/old", libc::O_DIRECTORY).unwrap();
        world.rename("/old", "/new", 0).unwrap();
        world.write_file("/new/extra", b"extra").unwrap();
        world.disk().sync("/new/extra").unwrap();
        directory.sync().unwrap();
        world.disk().sync("/").unwrap();
        world.disk().crash().unwrap();
        assert_eq!(read(&world, "/new/file"), b"before");
        assert_eq!(read(&world, "/new/extra"), b"extra");
        assert!(world.metadata("/old").is_err());
        world.write_file("/stage", b"replace").unwrap();
        world.rename("/stage", "/new/file", 0).unwrap();
        assert_eq!(
            world
                .disk()
                .corrupt("/new/file", 0, b"X", DiskState::Both)
                .unwrap_err()
                .raw_os_error(),
            Some(libc::ESTALE)
        );
        assert_eq!(read(&world, "/new/file"), b"replace");
        assert_eq!(
            world
                .disk()
                .read("/new/file", 0, 64, DiskState::Durable)
                .unwrap(),
            b"before"
        );
    }

    #[test]
    fn crash_watches_are_bounded_and_orphan_images_are_reclaimed() {
        let world = World::new();
        let (affected, unrelated, network) = {
            let mut state = world.state();
            (
                state.fs.durable.watch(vec!["/unused/file".into()]),
                state.fs.durable.watch(vec!["/file".into()]),
                state.fs.durable.watch(vec![]),
            )
        };
        for _ in 0..4100 {
            world.disk().crash_under("/unused").unwrap();
        }
        assert!(affected.crashed());
        assert!(!unrelated.crashed());
        assert!(!network.crashed());
        assert!(world.state().fs.durable.watches.len() <= 2);
        world.write_file("/file", b"x").unwrap();
        world.disk().sync_all().unwrap();
        world.unlink("/file").unwrap();
        world.disk().sync("/").unwrap();
        world.disk().crash().unwrap();
        assert!(world.read_file("/file").is_err());
    }

    #[test]
    fn failed_sync_leaves_durable_image_untouched() {
        let world = World::new();
        world.write_file("/f", b"old").unwrap();
        world.disk().sync_all().unwrap();
        let fd = world.open("/f", libc::O_RDWR).unwrap();
        fd.write_at(0, b"new").unwrap();
        world
            .inject(FaultTarget::Sync(SyncOp::Fsync), Fault::Errno(libc::EIO))
            .unwrap();
        assert_eq!(fd.sync().unwrap_err().raw_os_error(), Some(libc::EIO));
        world.disk().crash().unwrap();
        assert_eq!(read(&world, "/f"), b"old");
    }
}

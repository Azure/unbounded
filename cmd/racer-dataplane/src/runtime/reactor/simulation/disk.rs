//! Sparse crash disk used by the simulated OS, not a parallel storage service.
//!
//! File fsync commits inode data/length/metadata. Directory fsync commits that
//! directory's immediate name-to-inode bindings, independently of file data and
//! ancestor bindings. Unsynced state never persists implicitly. This is one
//! deterministic, conservative outcome allowed by the durability contract.
use super::*;
use std::ffi::OsString;

#[derive(Clone, Debug)]
pub struct CrashDisk(pub(super) Simulation);

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DiskState {
    Volatile,
    Durable,
    Both,
}

#[derive(Clone, Debug)]
struct Image {
    mode: u16,
    length: u64,
    pages: BTreeMap<u64, Rc<[u8; 4096]>>,
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
    fn initial(node: &Node) -> Self {
        Self {
            mode: node.mode,
            length: 0,
            pages: BTreeMap::new(),
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

#[derive(Default, Debug)]
pub(super) struct State {
    inodes: BTreeMap<u64, Image>,
    directories: BTreeMap<u64, BTreeMap<OsString, u64>>,
    root: Option<u64>,
    pub(super) generation: u64,
    crashes: Vec<(u64, PathBuf)>,
}
impl State {
    pub(super) fn crashed_since(&self, generation: u64, paths: &[PathBuf]) -> bool {
        self.crashes
            .iter()
            .any(|(at, root)| *at > generation && paths.iter().any(|path| path.starts_with(root)))
    }
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

impl World {
    fn sync_inode(&mut self, node: &Rc<RefCell<Node>>) -> io::Result<()> {
        let n = node.borrow();
        let inode = n.inode;
        let kind = n.mode as u32 & libc::S_IFMT;
        if kind != libc::S_IFREG && kind != libc::S_IFDIR {
            return Err(errno(libc::EINVAL));
        }
        self.disk.inodes.insert(inode, Image::capture(&n));
        if kind == libc::S_IFDIR {
            // Fsync an unlinked directory still persists its inode, but it cannot
            // recreate its removed parent binding.
            if let Some(path) = self
                .paths
                .iter()
                .find(|(_, candidate)| Rc::ptr_eq(candidate, node))
                .map(|(p, _)| p.clone())
            {
                let mut entries = BTreeMap::new();
                for (child, node) in &self.paths {
                    if child != &path && child.parent() == Some(path.as_path()) {
                        let n = node.borrow();
                        if n.mode as u32 & libc::S_IFMT == libc::S_IFSOCK {
                            continue;
                        }
                        entries.insert(child.file_name().unwrap().to_owned(), n.inode);
                        self.disk
                            .inodes
                            .entry(n.inode)
                            .or_insert_with(|| Image::initial(&n));
                    }
                }
                self.disk.directories.insert(inode, entries);
                if path == Path::new("/") {
                    self.disk.root = Some(inode);
                }
            }
        }
        self.record(
            if kind == libc::S_IFDIR {
                "sync:directory"
            } else {
                "sync:file"
            },
            inode,
            0,
        );
        Ok(())
    }
}

impl Handle {
    /// Same durable transition as the reactor Fsync operation. Injection failures
    /// leave the durable image untouched; completion ownership stays in Reactor.
    pub fn sync(&self) -> io::Result<()> {
        let (node, _) = self.node()?;
        let mut w = self.sim.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("fsync") {
            return Err(errno(n));
        }
        w.sync_inode(&node)
    }
}

impl CrashDisk {
    /// Fsync one existing file or directory. Does not sync its parent or children.
    pub fn sync(&self, path: &Path) -> io::Result<()> {
        let fd = self.0.open(None, path, libc::O_RDONLY)?;
        let Descriptor::Sim(handle) = fd else {
            unreachable!()
        };
        handle.sync()
    }

    /// Explicit fixture provisioning/barrier: persist every current inode and
    /// directory binding. Ordinary write_file/create_dir_all remain volatile.
    pub fn sync_all(&self) -> io::Result<()> {
        let paths: Vec<_> = self
            .0
            .0
            .borrow()
            .paths
            .iter()
            .filter(|(_, node)| {
                matches!(
                    node.borrow().mode as u32 & libc::S_IFMT,
                    libc::S_IFREG | libc::S_IFDIR
                )
            })
            .map(|(path, _)| path.clone())
            .collect();
        for path in paths {
            self.sync(&path)?;
        }
        Ok(())
    }

    /// Power loss for all storage. Socket/pipe resources are independent: the app
    /// harness tears down process/network owners separately. Pending disk SQEs
    /// complete EIO via the production reactor; crash never drops Entry owners.
    pub fn crash(&self) -> io::Result<()> {
        self.crash_under(Path::new("/"))
    }

    /// Crash one node's disk subtree in a multi-node Simulation. Other subtrees,
    /// their open file descriptions, and their queued disk operations survive.
    /// The root must be absolute and normal; use /dst/node-N for app DST.
    pub fn crash_under(&self, root: &Path) -> io::Result<()> {
        if !root.is_absolute() {
            return Err(errno(libc::EINVAL));
        }
        let root = normalize(root)?;
        let mut w = self.0.0.borrow_mut();
        w.disk.generation += 1;
        let generation = w.disk.generation;
        w.disk.crashes.push((generation, root.clone()));
        let lost: BTreeSet<_> = w
            .paths
            .iter()
            .filter(|(path, _)| path.starts_with(&root))
            .map(|(_, n)| n.borrow().inode)
            .collect();
        w.resources.retain(|_, resource| {
            !matches!(resource, Resource::File { node, opened_path, .. }
            if opened_path.starts_with(&root) || lost.contains(&node.borrow().inode))
        });
        w.paths.retain(|path, _| !path.starts_with(&root));
        let durable = w.disk.paths();
        let mut restored = BTreeMap::<u64, Rc<RefCell<Node>>>::new();
        for (path, inode) in durable {
            if !path.starts_with(&root) {
                continue;
            }
            let image = w
                .disk
                .inodes
                .get(&inode)
                .expect("durable binding owns inode");
            let node = restored
                .entry(inode)
                .or_insert_with(|| Rc::new(RefCell::new(image.restore(inode))))
                .clone();
            w.paths.insert(path, node);
        }
        w.record("disk:crash", generation, lost.len() as i64);
        Ok(())
    }

    /// Read a bounded range from either image without allocating a sparse file's
    /// logical size. Both is a mutation selector, not a read selector.
    pub fn read(
        &self,
        path: &Path,
        offset: u64,
        length: usize,
        state: DiskState,
    ) -> io::Result<Vec<u8>> {
        let path = normalize(path)?;
        let w = self.0.0.borrow();
        let image = match state {
            DiskState::Volatile => Image::capture(
                &w.paths
                    .get(&path)
                    .ok_or_else(|| errno(libc::ENOENT))?
                    .borrow(),
            ),
            DiskState::Durable => {
                let inode = *w
                    .disk
                    .paths()
                    .get(&path)
                    .ok_or_else(|| errno(libc::ENOENT))?;
                w.disk.inodes[&inode].clone()
            }
            DiskState::Both => return Err(errno(libc::EINVAL)),
        };
        if image.mode as u32 & libc::S_IFMT != libc::S_IFREG {
            return Err(errno(libc::EISDIR));
        }
        let count =
            length.min(usize::try_from(image.length.saturating_sub(offset)).unwrap_or(usize::MAX));
        let mut bytes = vec![0; count];
        for (index, byte) in bytes.iter_mut().enumerate() {
            let pos = offset + index as u64;
            *byte = image
                .pages
                .get(&(pos / 4096))
                .map_or(0, |page| page[(pos % 4096) as usize]);
        }
        Ok(bytes)
    }

    /// Overwrite existing bytes without changing length or checksum. Durable-only
    /// corruption becomes visible to production reads after crash. Both requires
    /// the same inode at the volatile and durable pathname, avoiding accidental
    /// corruption of two different replacement generations. Validation is atomic.
    pub fn corrupt(
        &self,
        path: &Path,
        offset: u64,
        bytes: &[u8],
        state: DiskState,
    ) -> io::Result<()> {
        let path = normalize(path)?;
        let end = offset
            .checked_add(bytes.len() as u64)
            .ok_or_else(|| errno(libc::EFBIG))?;
        let mut w = self.0.0.borrow_mut();
        let volatile = if state != DiskState::Durable {
            Some(
                w.paths
                    .get(&path)
                    .ok_or_else(|| errno(libc::ENOENT))?
                    .clone(),
            )
        } else {
            None
        };
        let durable = if state != DiskState::Volatile {
            Some(
                *w.disk
                    .paths()
                    .get(&path)
                    .ok_or_else(|| errno(libc::ENOENT))?,
            )
        } else {
            None
        };
        if let Some(node) = &volatile {
            let n = node.borrow();
            if n.mode as u32 & libc::S_IFMT != libc::S_IFREG || end > n.length {
                return Err(errno(libc::EINVAL));
            }
            if durable.is_some_and(|id| id != n.inode) {
                return Err(errno(libc::ESTALE));
            }
        }
        if let Some(inode) = durable {
            let image = &w.disk.inodes[&inode];
            if image.mode as u32 & libc::S_IFMT != libc::S_IFREG || end > image.length {
                return Err(errno(libc::EINVAL));
            }
        }
        if let Some(node) = &volatile {
            overwrite(&mut node.borrow_mut().pages, offset, bytes);
        }
        if let Some(inode) = durable {
            overwrite(
                &mut w.disk.inodes.get_mut(&inode).unwrap().pages,
                offset,
                bytes,
            );
        }
        let inode = durable.unwrap_or_else(|| volatile.as_ref().unwrap().borrow().inode);
        w.record(
            match state {
                DiskState::Volatile => "corrupt:volatile",
                DiskState::Durable => "corrupt:durable",
                DiskState::Both => "corrupt:both",
            },
            inode,
            bytes.len() as i64,
        );
        Ok(())
    }
}

fn overwrite(pages: &mut BTreeMap<u64, Rc<[u8; 4096]>>, offset: u64, bytes: &[u8]) {
    for (index, byte) in bytes.iter().enumerate() {
        let pos = offset + index as u64;
        Rc::make_mut(
            pages
                .entry(pos / 4096)
                .or_insert_with(|| Rc::new([0; 4096])),
        )[(pos % 4096) as usize] = *byte;
    }
}

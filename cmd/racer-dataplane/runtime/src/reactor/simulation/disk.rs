//! Sparse crash disk used by the simulated OS, not a parallel storage service.
//!
//! File fsync commits inode data/length/metadata. Directory fsync commits that
//! directory's immediate name-to-inode bindings, independently of file data and
//! ancestor bindings. Unsynced state never persists implicitly. This is one
//! deterministic, conservative outcome allowed by the durability contract.
use super::*;
use std::ffi::OsString;

/// Controls persistence, power loss, and byte corruption in one simulated world.
#[derive(Clone, Debug)]
pub struct CrashDisk(pub(super) Simulation);

/// Selects the live image, the last synced image, or both mutation targets.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DiskState {
    Volatile,
    Durable,
    Both,
}

/// An inode snapshot sharing immutable sparse pages until a write occurs.
#[derive(Clone, Debug)]
struct Image {
    mode: u16,
    length: u64,
    pages: BTreeMap<u64, Rc<[u8; 4096]>>,
    symlink: Option<PathBuf>,
}
impl Image {
    /// Snapshots current inode contents without copying allocated pages.
    fn capture(node: &Node) -> Self {
        Self {
            mode: node.mode,
            length: node.length,
            pages: node.pages.clone(),
            symlink: node.symlink.clone(),
        }
    }
    /// Persists a new directory binding without implicitly syncing file data.
    fn initial(node: &Node) -> Self {
        Self {
            mode: node.mode,
            length: 0,
            pages: BTreeMap::new(),
            symlink: node.symlink.clone(),
        }
    }
    /// Recreates an unlocked live inode after power loss.
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

/// Durable inodes, directory bindings, and weak watches for pending disk work.
#[derive(Default, Debug)]
pub(super) struct State {
    inodes: BTreeMap<u64, Image>,
    directories: BTreeMap<u64, BTreeMap<OsString, u64>>,
    root: Option<u64>,
    pub(super) generation: u64,
    pending: Vec<std::rc::Weak<PendingCrash>>,
}
/// Records whether a crash affected any path owned by a pending submission.
#[derive(Debug)]
pub(super) struct PendingCrash {
    paths: Vec<PathBuf>,
    pub(super) crashed: std::cell::Cell<bool>,
}
impl State {
    /// Watches exact submission paths without retaining finished operations.
    pub(super) fn watch(&mut self, paths: Vec<PathBuf>) -> Rc<PendingCrash> {
        // Retain only live submissions, never a crash log or a global watermark.
        // A crash marks exact subtree owners before their descriptors disappear.
        self.pending.retain(|pending| pending.strong_count() != 0);
        let pending = Rc::new(PendingCrash {
            paths,
            crashed: std::cell::Cell::new(false),
        });
        if !pending.paths.is_empty() {
            self.pending.push(Rc::downgrade(&pending));
        }
        pending
    }
    /// Marks live submissions touching the crashed subtree.
    fn invalidate(&mut self, root: &Path) {
        self.pending.retain(|pending| {
            let Some(pending) = pending.upgrade() else {
                return false;
            };
            if pending.paths.iter().any(|path| path.starts_with(root)) {
                pending.crashed.set(true);
            }
            true
        });
    }
    /// Reconstructs reachable durable names while avoiding directory cycles.
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
    /// Persists inode contents and, for directories, immediate child bindings.
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
        let (node, flags) = self.node()?;
        if flags & libc::O_PATH != 0 {
            return Err(errno(libc::EBADF));
        }
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
        let Some(handle) = fd.as_sim() else {
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
        w.disk.invalidate(&root);
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
        let mut reachable: BTreeSet<_> = w.disk.paths().values().copied().collect();
        reachable.extend(w.paths.values().map(|node| node.borrow().inode));
        reachable.extend(w.resources.values().filter_map(|resource| match resource {
            Resource::File { node, .. } => Some(node.borrow().inode),
            _ => None,
        }));
        w.disk.inodes.retain(|inode, _| reachable.contains(inode));
        w.disk
            .directories
            .retain(|inode, _| reachable.contains(inode));
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
        let mut bytes = bounded_bytes(count)?;
        read_pages(&image.pages, offset, &mut bytes);
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
            check_page_budget(&n.pages, offset, bytes.len())?;
        }
        if let Some(inode) = durable {
            let image = &w.disk.inodes[&inode];
            if image.mode as u32 & libc::S_IFMT != libc::S_IFREG || end > image.length {
                return Err(errno(libc::EINVAL));
            }
            check_page_budget(&image.pages, offset, bytes.len())?;
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

/// Copies allocated pages and zero-fills holes, including unaligned boundaries.
fn read_pages(pages: &BTreeMap<u64, Rc<[u8; 4096]>>, offset: u64, bytes: &mut [u8]) {
    let mut index = 0;
    while index < bytes.len() {
        let pos = offset + index as u64;
        let start = (pos % 4096) as usize;
        let count = (4096 - start).min(bytes.len() - index);
        let output = &mut bytes[index..index + count];
        if let Some(page) = pages.get(&(pos / 4096)) {
            output.copy_from_slice(&page[start..start + count]);
        } else {
            output.fill(0);
        }
        index += count;
    }
}

/// Writes page-sized runs, preserving shared snapshots through copy-on-write.
fn overwrite(pages: &mut BTreeMap<u64, Rc<[u8; 4096]>>, offset: u64, bytes: &[u8]) {
    let mut index = 0;
    while index < bytes.len() {
        let pos = offset + index as u64;
        let start = (pos % 4096) as usize;
        let count = (4096 - start).min(bytes.len() - index);
        let page = Rc::make_mut(
            pages
                .entry(pos / 4096)
                .or_insert_with(|| Rc::new([0; 4096])),
        );
        page[start..start + count].copy_from_slice(&bytes[index..index + count]);
        index += count;
    }
}

/// Validates a sparse write's allocation budget before changing any page.
fn check_page_budget(
    pages: &BTreeMap<u64, Rc<[u8; 4096]>>,
    offset: u64,
    length: usize,
) -> io::Result<()> {
    if length == 0 {
        return Ok(());
    }
    if length > MAX_ALLOCATION {
        return Err(errno(libc::ENOSPC));
    }
    let end = offset
        .checked_add(length as u64)
        .ok_or_else(|| errno(libc::EFBIG))?;
    let new_pages = (offset / 4096..=(end - 1) / 4096)
        .filter(|page| !pages.contains_key(page))
        .count();
    if pages.len().saturating_add(new_pages) > MAX_ALLOCATION / 4096 {
        return Err(errno(libc::ENOSPC));
    }
    Ok(())
}

impl World {
    /// Resolves a relative name against an open directory's current inode path.
    pub(super) fn path(&self, dir: Option<&Descriptor>, name: &Path) -> io::Result<PathBuf> {
        if name.is_absolute() {
            return normalize(name);
        }
        let base = match dir {
            None => PathBuf::from("/"),
            Some(fd) if fd.as_sim().is_some() => {
                let h = fd.as_sim().unwrap();
                let Some(Resource::File { node, .. }) = self.resources.get(&h.id) else {
                    return Err(errno(libc::ENOTDIR));
                };
                if node.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
                    return Err(errno(libc::ENOTDIR));
                }
                self.paths
                    .iter()
                    .find(|(_, n)| Rc::ptr_eq(n, node))
                    .map(|(p, _)| p.clone())
                    .ok_or_else(|| errno(libc::ENOENT))?
            }
            _ => return Err(errno(libc::EBADF)),
        };
        normalize(&base.join(name))
    }
    /// Allocates an empty inode with a deterministic identity.
    pub(super) fn node(&mut self, mode: u16) -> Rc<RefCell<Node>> {
        Rc::new(RefCell::new(Node {
            inode: self.id(),
            mode,
            length: 0,
            pages: BTreeMap::new(),
            locked: false,
            symlink: None,
        }))
    }
    /// Traverses symlinks while enforcing the requested openat2 boundary.
    fn resolve(
        &self,
        path: PathBuf,
        boundary: Option<&Path>,
        no_symlinks: bool,
        in_root: bool,
        follow_final: bool,
    ) -> io::Result<PathBuf> {
        let parts = |path: &Path| {
            path.components()
                .filter(|c| {
                    !matches!(
                        c,
                        std::path::Component::RootDir | std::path::Component::CurDir
                    )
                })
                .map(|c| c.as_os_str().to_owned())
                .collect::<VecDeque<_>>()
        };
        let mut pending = parts(&path);
        let mut prefix = PathBuf::from("/");
        let mut links = 0;
        while let Some(part) = pending.pop_front() {
            if part == ".." {
                if boundary == Some(prefix.as_path()) {
                    if !in_root {
                        return Err(errno(libc::EXDEV));
                    }
                } else {
                    prefix.pop();
                }
                continue;
            }
            prefix.push(part);
            if let Some(node) = self.paths.get(&prefix) {
                let node = node.borrow();
                if let Some(target) = &node.symlink {
                    // Parent components always follow the traversal policy. The
                    // final component instead obeys open's NOFOLLOW/EXCL policy.
                    if pending.is_empty() && !follow_final {
                        return Ok(prefix);
                    }
                    links += 1;
                    if no_symlinks || links > 40 {
                        return Err(errno(libc::ELOOP));
                    }
                    if target.is_absolute() {
                        if boundary.is_some() && !in_root {
                            return Err(errno(libc::EXDEV));
                        }
                        prefix = boundary
                            .filter(|_| in_root)
                            .unwrap_or(Path::new("/"))
                            .to_owned();
                    } else {
                        prefix.pop();
                    }
                    let mut next = parts(target);
                    next.append(&mut pending);
                    pending = next;
                } else if !pending.is_empty() && node.mode as u32 & libc::S_IFMT != libc::S_IFDIR {
                    return Err(errno(libc::ENOTDIR));
                }
            } else if !pending.is_empty() {
                return Err(errno(libc::ENOENT));
            }
        }
        Ok(prefix)
    }
    /// Validates supported openat2 policies and resolves the final pathname.
    pub(super) fn open_path(
        &self,
        dir: Option<&Descriptor>,
        path: &Path,
        policy: u64,
        flags: i32,
    ) -> io::Result<PathBuf> {
        // No mounts or magic links exist in this world. NO_MAGICLINKS is therefore
        // enforced by construction; mount/cache policies are explicitly unsupported.
        if policy & !(0x02 | 0x04 | 0x08 | 0x10) != 0 || policy & 0x18 == 0x18 {
            return Err(errno(libc::EINVAL));
        }
        if path.as_os_str().is_empty() {
            return Err(errno(libc::ENOENT));
        }
        if policy & 0x08 != 0 && path.is_absolute() {
            return Err(errno(libc::EXDEV));
        }
        let base = self.path(dir, Path::new("."))?;
        let boundary = (policy & 0x18 != 0).then_some(base.as_path());
        let path = if policy & 0x10 != 0 && path.is_absolute() {
            base.join(path.strip_prefix("/").unwrap())
        } else {
            base.join(path)
        };
        let follow_final = flags & libc::O_NOFOLLOW == 0
            && flags & (libc::O_CREAT | libc::O_EXCL) != (libc::O_CREAT | libc::O_EXCL);
        self.resolve(
            path,
            boundary,
            policy & 0x04 != 0,
            policy & 0x10 != 0,
            follow_final,
        )
    }
}
/// Normalizes fixture names without allowing parent traversal.
fn normalize(path: &Path) -> io::Result<PathBuf> {
    let mut out = PathBuf::from("/");
    for component in path.components() {
        match component {
            std::path::Component::RootDir | std::path::Component::CurDir => (),
            std::path::Component::Normal(name) => out.push(name),
            _ => return Err(errno(libc::EXDEV)),
        }
    }
    Ok(out)
}

impl Simulation {
    /// Creates missing ancestors as volatile directories for a fixture.
    pub fn create_dir_all(&self, path: &Path) -> io::Result<()> {
        let path = normalize(path)?;
        let mut w = self.0.borrow_mut();
        let mut paths: Vec<_> = path.ancestors().collect();
        paths.reverse();
        for path in paths {
            if let Some(node) = w.paths.get(path) {
                if node.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
                    return Err(errno(libc::ENOTDIR));
                }
            } else {
                let node = w.node(libc::S_IFDIR as u16 | 0o755);
                w.paths.insert(path.into(), node);
            }
        }
        Ok(())
    }
    /// Create exactly one directory, unlike the recursive fixture helper.
    pub fn mkdir(&self, path: &Path) -> io::Result<()> {
        let mut w = self.0.borrow_mut();
        let path = normalize(path)?;
        if w.paths.contains_key(&path) {
            return Err(errno(libc::EEXIST));
        }
        let parent = w
            .paths
            .get(path.parent().ok_or_else(|| errno(libc::ENOENT))?)
            .ok_or_else(|| errno(libc::ENOENT))?;
        if parent.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
            return Err(errno(libc::ENOTDIR));
        }
        let node = w.node(libc::S_IFDIR as u16 | 0o700);
        w.paths.insert(path, node);
        Ok(())
    }
    /// Opens a simulated file using ordinary pathname resolution.
    pub fn open(
        &self,
        dir: Option<&Descriptor>,
        path: &Path,
        flags: i32,
    ) -> io::Result<Descriptor> {
        self.open_resolved(dir, path, flags, 0)
    }
    /// Creates a volatile symbolic link without resolving its target.
    pub fn symlink(&self, target: &Path, path: &Path) -> io::Result<()> {
        let path = normalize(path)?;
        let mut w = self.0.borrow_mut();
        if w.paths.contains_key(&path) {
            return Err(errno(libc::EEXIST));
        }
        if !w
            .paths
            .contains_key(path.parent().unwrap_or(Path::new("/")))
        {
            return Err(errno(libc::ENOENT));
        }
        let node = w.node(libc::S_IFLNK as u16 | 0o777);
        node.borrow_mut().symlink = Some(target.into());
        w.paths.insert(path, node);
        w.record("symlink", 0, 0);
        Ok(())
    }
    /// Opens a file under explicit openat2 resolution and creation policies.
    pub(super) fn open_resolved(
        &self,
        dir: Option<&Descriptor>,
        path: &Path,
        flags: i32,
        resolve: u64,
    ) -> io::Result<Descriptor> {
        if let Some(dir) = dir {
            self.handle(dir)?;
        }
        let (node, opened_path) = {
            let mut w = self.0.borrow_mut();
            if let Some(Fault::Errno(n)) = w.fault("open") {
                return Err(errno(n));
            }
            let path = w.open_path(dir, path, resolve, flags)?;
            if w.paths.contains_key(&path)
                && flags & (libc::O_CREAT | libc::O_EXCL) == (libc::O_CREAT | libc::O_EXCL)
            {
                return Err(errno(libc::EEXIST));
            }
            if !w.paths.contains_key(&path) {
                if flags & libc::O_CREAT == 0 {
                    return Err(errno(libc::ENOENT));
                }
                let parent = w
                    .paths
                    .get(path.parent().ok_or_else(|| errno(libc::ENOENT))?)
                    .ok_or_else(|| errno(libc::ENOENT))?;
                if parent.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
                    return Err(errno(libc::ENOTDIR));
                }
                let node = w.node(libc::S_IFREG as u16 | 0o600);
                w.paths.insert(path.clone(), node);
            }
            let node = w.paths[&path].clone();
            if node.borrow().symlink.is_some() && flags & libc::O_PATH == 0 {
                return Err(errno(libc::ELOOP));
            }
            if flags & libc::O_DIRECTORY != 0
                && node.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR
            {
                return Err(errno(libc::ENOTDIR));
            }
            if flags & libc::O_TRUNC != 0 {
                let mut n = node.borrow_mut();
                if n.mode as u32 & libc::S_IFMT != libc::S_IFREG {
                    return Err(errno(libc::EISDIR));
                }
                if flags & libc::O_ACCMODE == libc::O_RDONLY {
                    return Err(errno(libc::EINVAL));
                }
                n.pages.clear();
                n.length = 0;
            }
            (node, path)
        };
        Ok(self.insert(Resource::File {
            node,
            flags,
            lock_owner: false,
            opened_path,
        }))
    }
    /// Replaces a fixture file, retrying short writes without syncing it.
    pub fn write_file(&self, path: &Path, bytes: &[u8]) -> io::Result<()> {
        if let Some(parent) = path.parent() {
            self.create_dir_all(parent)?;
        }
        let fd = self.open(None, path, libc::O_CREAT | libc::O_RDWR | libc::O_TRUNC)?;
        let Some(h) = fd.as_sim() else { unreachable!() };
        let mut offset = 0;
        while offset < bytes.len() {
            let written = h.file_write(offset as u64, &bytes[offset..])?;
            if written == 0 {
                return Err(errno(libc::EIO));
            }
            offset += written;
        }
        Ok(())
    }
    /// Materializes a bounded file while honoring short reads and injected faults.
    pub fn read_file(&self, path: &Path) -> io::Result<Vec<u8>> {
        let fd = self.open(None, path, libc::O_RDONLY)?;
        let Some(h) = fd.as_sim() else { unreachable!() };
        let mut bytes =
            bounded_bytes(usize::try_from(h.stat()?.stx_size).map_err(|_| errno(libc::EFBIG))?)?;
        let mut offset = 0;
        while offset < bytes.len() {
            let count = h.file_read(offset as u64, &mut bytes[offset..])?;
            if count == 0 {
                bytes.truncate(offset);
                break;
            }
            offset += count;
        }
        Ok(bytes)
    }
    /// Returns a named inode's identity and mode without following symlinks.
    pub fn metadata(&self, path: &Path) -> io::Result<(u64, u16)> {
        let w = self.0.borrow();
        let node = w
            .paths
            .get(&normalize(path)?)
            .ok_or_else(|| errno(libc::ENOENT))?
            .borrow();
        Ok((node.inode, node.mode))
    }
    /// Changes permission bits while preserving the inode's file type.
    pub fn chmod(&self, path: &Path, mode: u32) -> io::Result<()> {
        let mut w = self.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("chmod") {
            return Err(errno(n));
        }
        let mut node = w
            .paths
            .get(&normalize(path)?)
            .ok_or_else(|| errno(libc::ENOENT))?
            .borrow_mut();
        node.mode = (node.mode & libc::S_IFMT as u16) | (mode as u16 & 0o7777);
        Ok(())
    }
    /// Moves or exchanges names, descendants, and open-description crash paths.
    pub fn rename(&self, from: &Path, to: &Path, flags: u32) -> io::Result<()> {
        let mut w = self.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("rename") {
            return Err(errno(n));
        }
        let from = normalize(from)?;
        let to = normalize(to)?;
        if flags & !(libc::RENAME_NOREPLACE | libc::RENAME_EXCHANGE) != 0
            || flags == (libc::RENAME_NOREPLACE | libc::RENAME_EXCHANGE)
        {
            return Err(errno(libc::EINVAL));
        }
        if !w.paths.contains_key(&from) {
            return Err(errno(libc::ENOENT));
        }
        if from == to {
            return Ok(());
        }
        if from == Path::new("/") || to == Path::new("/") {
            return Err(errno(libc::EBUSY));
        }
        let parent = w
            .paths
            .get(to.parent().unwrap())
            .ok_or_else(|| errno(libc::ENOENT))?;
        if parent.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
            return Err(errno(libc::ENOTDIR));
        }
        if to.starts_with(&from) || from.starts_with(&to) {
            return Err(errno(libc::EINVAL));
        }
        if flags & libc::RENAME_NOREPLACE != 0 && w.paths.contains_key(&to) {
            return Err(errno(libc::EEXIST));
        }
        if flags & libc::RENAME_EXCHANGE != 0 && !w.paths.contains_key(&to) {
            return Err(errno(libc::ENOENT));
        }
        if flags & libc::RENAME_EXCHANGE == 0
            && let Some(target) = w.paths.get(&to)
        {
            let source_dir = w.paths[&from].borrow().mode as u32 & libc::S_IFMT == libc::S_IFDIR;
            let target_dir = target.borrow().mode as u32 & libc::S_IFMT == libc::S_IFDIR;
            if source_dir != target_dir {
                return Err(errno(if source_dir {
                    libc::ENOTDIR
                } else {
                    libc::EISDIR
                }));
            }
            if target_dir && w.paths.keys().any(|p| p != &to && p.starts_with(&to)) {
                return Err(errno(libc::ENOTEMPTY));
            }
        }
        let descendants: Vec<_> = w
            .paths
            .iter()
            .filter_map(|(path, node)| {
                if path != &from && path.starts_with(&from) {
                    Some((
                        path.clone(),
                        to.join(path.strip_prefix(&from).unwrap()),
                        node.clone(),
                    ))
                } else if flags & libc::RENAME_EXCHANGE != 0 && path != &to && path.starts_with(&to)
                {
                    Some((
                        path.clone(),
                        from.join(path.strip_prefix(&to).unwrap()),
                        node.clone(),
                    ))
                } else {
                    None
                }
            })
            .collect();
        for (path, _, _) in &descendants {
            w.paths.remove(path);
        }
        for (_, path, node) in descendants {
            w.paths.insert(path, node);
        }
        let source = w.paths.remove(&from).unwrap();
        let target = w.paths.insert(to.clone(), source);
        if flags & libc::RENAME_EXCHANGE != 0 {
            w.paths.insert(from.clone(), target.unwrap());
        }
        // Keep open-description crash ownership attached to the moved inode.
        let locations: BTreeMap<_, _> = w
            .paths
            .iter()
            .map(|(path, node)| (node.borrow().inode, path.clone()))
            .collect();
        for resource in w.resources.values_mut() {
            if let Resource::File {
                node, opened_path, ..
            } = resource
                && let Some(path) = locations.get(&node.borrow().inode)
            {
                *opened_path = path.clone();
            }
        }
        let listeners = std::mem::take(&mut w.listeners);
        for (address, id) in listeners {
            let address = match address {
                SocketAddress::Unix(path) if path.starts_with(&from) => {
                    SocketAddress::Unix(to.join(path.strip_prefix(&from).unwrap()))
                }
                SocketAddress::Unix(path) if path.starts_with(&to) => {
                    if flags & libc::RENAME_EXCHANGE == 0 {
                        continue;
                    }
                    SocketAddress::Unix(from.join(path.strip_prefix(&to).unwrap()))
                }
                address => address,
            };
            w.listeners.insert(address, id);
        }
        w.record("rename", 0, 0);
        Ok(())
    }
    /// Removes a non-directory name while retaining any open inode owners.
    pub fn unlink(&self, path: &Path) -> io::Result<()> {
        let mut w = self.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("unlink") {
            return Err(errno(n));
        }
        let path = normalize(path)?;
        if w.paths
            .get(&path)
            .is_some_and(|n| n.borrow().mode as u32 & libc::S_IFMT == libc::S_IFDIR)
        {
            return Err(errno(libc::EISDIR));
        }
        w.paths.remove(&path).ok_or_else(|| errno(libc::ENOENT))?;
        w.listeners.remove(&SocketAddress::Unix(path));
        w.record("unlink", 0, 0);
        Ok(())
    }
}

impl Handle {
    /// Borrows a file's inode and open flags, rejecting stale descriptors.
    pub(super) fn node(&self) -> io::Result<(Rc<RefCell<Node>>, i32)> {
        match self.sim.0.borrow().resources.get(&self.id) {
            Some(Resource::File { node, flags, .. }) => Ok((node.clone(), *flags)),
            _ => Err(errno(libc::EBADF)),
        }
    }
    /// Reports inode metadata and the simulation's direct-I/O alignment.
    pub fn stat(&self) -> io::Result<libc::statx> {
        let (node, _) = self.node()?;
        let node = node.borrow();
        let mut stat: libc::statx = unsafe { std::mem::zeroed() };
        stat.stx_mask = libc::STATX_BASIC_STATS | libc::STATX_DIOALIGN;
        stat.stx_mode = node.mode;
        stat.stx_ino = node.inode;
        stat.stx_size = node.length;
        stat.stx_nlink = 1;
        stat.stx_dio_mem_align = 4096;
        stat.stx_dio_offset_align = 4096;
        stat.stx_uid = 0;
        stat.stx_gid = 0;
        Ok(stat)
    }
    /// Resizes a writable file, clearing truncated data without allocating holes.
    pub fn set_len(&self, length: u64) -> io::Result<()> {
        if length > i64::MAX as u64 {
            return Err(errno(libc::EINVAL));
        }
        let (node, flags) = self.node()?;
        if flags & libc::O_PATH != 0 || flags & libc::O_ACCMODE == libc::O_RDONLY {
            return Err(errno(libc::EBADF));
        }
        let mut node = node.borrow_mut();
        if node.mode as u32 & libc::S_IFMT != libc::S_IFREG {
            return Err(errno(libc::EINVAL));
        }
        node.pages.retain(|page, _| *page < length.div_ceil(4096));
        if !length.is_multiple_of(4096)
            && let Some(page) = node.pages.get_mut(&(length / 4096))
        {
            Rc::make_mut(page)[(length % 4096) as usize..].fill(0);
        }
        node.length = length;
        Ok(())
    }
    /// Acquires an exclusive nonblocking inode lock owned by this descriptor.
    pub fn lock(&self) -> io::Result<()> {
        if matches!(
            self.sim.0.borrow().resources.get(&self.id),
            Some(Resource::File {
                lock_owner: true,
                ..
            })
        ) {
            return Ok(());
        }
        let (node, flags) = self.node()?;
        if flags & libc::O_PATH != 0 {
            return Err(errno(libc::EBADF));
        }
        let mut n = node.borrow_mut();
        if n.locked {
            return Err(errno(libc::EWOULDBLOCK));
        }
        n.locked = true;
        if let Some(Resource::File { lock_owner, .. }) =
            self.sim.0.borrow_mut().resources.get_mut(&self.id)
        {
            *lock_owner = true;
        }
        Ok(())
    }
    /// Reads a bounded sparse range after validating flags and direct alignment.
    pub fn file_read(&self, offset: u64, bytes: &mut [u8]) -> io::Result<usize> {
        if offset > i64::MAX as u64 {
            return Err(errno(libc::EINVAL));
        }
        let limit = match self.sim.0.borrow_mut().fault("read") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let (node, flags) = self.node()?;
        if !self.sim.0.borrow().executing {
            check_direct(flags, offset, bytes.as_ptr(), bytes.len())?;
        }
        if flags & libc::O_PATH != 0 || flags & libc::O_ACCMODE == libc::O_WRONLY {
            return Err(errno(libc::EBADF));
        }
        let node = node.borrow();
        if node.mode as u32 & libc::S_IFMT != libc::S_IFREG {
            return Err(errno(libc::EISDIR));
        }
        let len = bytes
            .len()
            .min(node.length.saturating_sub(offset) as usize)
            .min(self.sim.0.borrow().max_chunk)
            .min(limit);
        read_pages(&node.pages, offset, &mut bytes[..len]);
        Ok(len)
    }
    /// Writes a bounded sparse range without modifying durable shared pages.
    pub(in crate::reactor) fn file_write(&self, offset: u64, bytes: &[u8]) -> io::Result<usize> {
        if offset > i64::MAX as u64 {
            return Err(errno(libc::EINVAL));
        }
        let (node, flags) = self.node()?;
        if !self.sim.0.borrow().executing {
            check_direct(flags, offset, bytes.as_ptr(), bytes.len())?;
        }
        let limit = match self.sim.0.borrow_mut().fault("write") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let bytes = &bytes[..bytes.len().min(limit).min(self.sim.0.borrow().max_chunk)];
        if flags & libc::O_PATH != 0 || flags & libc::O_ACCMODE == libc::O_RDONLY {
            return Err(errno(libc::EBADF));
        }
        let offset = if flags & libc::O_APPEND != 0 {
            node.borrow().length
        } else {
            offset
        };
        let end = offset
            .checked_add(bytes.len() as u64)
            .filter(|end| *end <= i64::MAX as u64)
            .ok_or_else(|| errno(libc::EFBIG))?;
        let mut node = node.borrow_mut();
        if node.mode as u32 & libc::S_IFMT != libc::S_IFREG {
            return Err(errno(libc::EISDIR));
        }
        if bytes.is_empty() {
            return Ok(0);
        }
        check_page_budget(&node.pages, offset, bytes.len())?;
        overwrite(&mut node.pages, offset, bytes);
        node.length = node.length.max(end);
        Ok(bytes.len())
    }
}

/// Checks the original request's alignment before a fault shortens its transfer.
pub(super) fn check_direct(
    flags: i32,
    offset: u64,
    ptr: *const u8,
    length: usize,
) -> io::Result<()> {
    if flags & libc::O_DIRECT != 0
        && (!offset.is_multiple_of(4096)
            || !(ptr as usize).is_multiple_of(4096)
            || !length.is_multiple_of(4096))
    {
        return Err(errno(libc::EINVAL));
    }
    Ok(())
}

/// Durability, sparse-page, and reactor crash-ownership contracts.
#[cfg(test)]
mod tests {
    use super::*;
    use crate::Error;
    use crate::reactor::simulation::network::tests::{drive, poll, reactor, scope};
    use crate::reactor::tests::fixtures::{Reactor, RequestScope, ResourceClass};
    use std::time::{Duration, Instant};

    /// Enters a fresh world and creates reactor owners for crash and durability tests.
    fn setup() -> (Simulation, Environment, Reactor, RequestScope) {
        let sim = Simulation::new();
        let environment = sim.enter();
        let r = reactor();
        let scope = RequestScope::new((), Instant::now() + Duration::from_secs(30)).unwrap();
        (sim, environment, r, scope)
    }
    /// Opens an existing fixture for read/write submissions through the reactor.
    fn open(sim: &Simulation, path: &str) -> Rc<Descriptor> {
        Rc::new(sim.open(None, Path::new(path), libc::O_RDWR).unwrap())
    }
    /// Reads a small volatile prefix without allocating a sparse file's logical size.
    fn read(sim: &Simulation, path: &str) -> Vec<u8> {
        sim.disk()
            .read(Path::new(path), 0, 64, DiskState::Volatile)
            .unwrap()
    }

    #[test]
    fn delayed_completion_and_fault_trace_replay_exactly() {
        /// Captures one delayed-open and failed-read scenario for exact trace replay.
        fn run() -> Vec<Event> {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let scope = scope();
            sim.write_file(Path::new("/file"), b"abc").unwrap();
            sim.inject("open", Fault::Delay(2)).unwrap();
            let fd = drive(
                &r,
                r.file_open(
                    None,
                    CString::new("/file").unwrap(),
                    libc::O_RDONLY,
                    0,
                    &scope,
                ),
            )
            .unwrap();
            sim.inject("read", Fault::Errno(libc::EIO)).unwrap();
            assert!(matches!(
                drive(&r, r.read_at(fd, 0, r.file_buffer(3).unwrap(), (), &scope)),
                Err(Error::Os(libc::EIO))
            ));
            sim.trace()
        }
        assert_eq!(run(), run());
    }

    #[test]
    fn sparse_page_copies_preserve_boundaries_holes_and_snapshots() {
        for offset in [0, 1, 4095, 4096, 4097] {
            for length in [0, 1, 4095, 4096, 4097, 8193] {
                let sim = Simulation::new();
                let file = sim
                    .open(None, Path::new("/file"), libc::O_CREAT | libc::O_RDWR)
                    .unwrap()
                    .into_sim()
                    .unwrap();
                // Leave a full sparse page before an unaligned multi-page write.
                let offset = 8192 + offset;
                let original: Vec<_> = (0..length).map(|i| (i % 251) as u8).collect();
                assert_eq!(file.file_write(offset, &original).unwrap(), length);
                let mut expected = vec![0; if length == 0 { 0 } else { offset as usize }];
                expected.extend_from_slice(&original);
                let (node, _) = file.node().unwrap();
                let snapshot = node.borrow().pages.clone();
                let replacement = vec![0xa5; length + 1];
                sim.inject("write", Fault::Short(length / 2)).unwrap();
                assert_eq!(file.file_write(offset, &replacement).unwrap(), length / 2);
                if length != 0 {
                    expected[offset as usize..offset as usize + length / 2].fill(0xa5);
                }
                for (index, byte) in original.iter().enumerate() {
                    let pos = offset as usize + index;
                    assert_eq!(snapshot[&(pos as u64 / 4096)][pos % 4096], *byte);
                }
                let mut output = vec![0xcc; expected.len() + 8];
                assert_eq!(file.file_read(0, &mut output).unwrap(), expected.len());
                assert_eq!(&output[..expected.len()], expected);
                assert_eq!(&output[expected.len()..], &[0xcc; 8]);
                let mut partial = vec![0xcc; length + 8];
                sim.inject("read", Fault::Short(length / 2)).unwrap();
                assert_eq!(file.file_read(offset, &mut partial).unwrap(), length / 2);
                if length != 0 {
                    assert_eq!(
                        &partial[..length / 2],
                        &expected[offset as usize..offset as usize + length / 2]
                    );
                }
                assert!(partial[length / 2..].iter().all(|b| *b == 0xcc));
                sim.inject("write", Fault::Errno(libc::EIO)).unwrap();
                assert_eq!(
                    file.file_write(offset, b"bad").unwrap_err().raw_os_error(),
                    Some(libc::EIO)
                );
                assert_eq!(sim.read_file(Path::new("/file")).unwrap(), expected);
            }
        }
    }

    #[test]
    fn sparse_files_partial_io_faults_rename_unlink_and_open_inode_ownership() {
        let (sim, _environment, r, scope) = setup();
        sim.create_dir_all(Path::new("/data")).unwrap();
        let dir = drive(
            &r,
            r.file_open(
                None,
                CString::new("/data").unwrap(),
                libc::O_RDONLY | libc::O_DIRECTORY,
                0,
                &scope,
            ),
        )
        .unwrap();
        let file = drive(
            &r,
            r.file_open(
                Some(dir.clone()),
                CString::new("a").unwrap(),
                libc::O_CREAT | libc::O_RDWR | libc::O_EXCL,
                0,
                &scope,
            ),
        )
        .unwrap();
        sim.inject("write", Fault::Short(2)).unwrap();
        let write = drive(
            &r,
            r.write_at(
                file.clone(),
                1 << 30,
                r.file_bytes(b"abcdef").unwrap(),
                (),
                &scope,
            ),
        )
        .unwrap();
        assert_eq!(write.bytes, 2);
        drop(write);
        let stat = drive(&r, r.file_stat(file.clone(), &scope)).unwrap();
        assert_eq!(stat.stx_size, (1 << 30) + 2);
        sim.inject("fsync", Fault::Errno(libc::EIO)).unwrap();
        assert_eq!(
            drive(&r, r.file_sync(file.clone(), &scope)),
            Err(Error::Os(libc::EIO))
        );
        drive(&r, r.file_sync(file.clone(), &scope)).unwrap();
        drive(
            &r,
            r.file_rename(
                dir.clone(),
                CString::new("a").unwrap(),
                CString::new("b").unwrap(),
                &scope,
            ),
        )
        .unwrap();
        drive(
            &r,
            r.file_unlink(dir.clone(), CString::new("b").unwrap(), &scope),
        )
        .unwrap();
        let read = drive(
            &r,
            r.read_at(
                file.clone(),
                (1 << 30) - 2,
                r.file_buffer(8).unwrap(),
                (),
                &scope,
            ),
        )
        .unwrap();
        assert_eq!(read.bytes, 4);
        assert_eq!(read.buffer.prefix(4).unwrap(), b"\0\0ab");
        assert!(matches!(
            drive(
                &r,
                r.file_open(
                    Some(dir.clone()),
                    CString::new("b").unwrap(),
                    libc::O_RDONLY,
                    0,
                    &scope
                )
            ),
            Err(Error::NotFound)
        ));
        drop((read, file, dir));
        assert_eq!(sim.live_handles(), 0);
        assert!(
            sim.trace()
                .iter()
                .any(|e| e.operation == "complete:fsync" && e.result == -(libc::EIO as i64))
        );
    }

    #[test]
    fn partition_stalls_established_streams_and_connects_then_heals_without_loss() {
        let (sim, _environment, r, scope) = setup();
        let a = SocketAddress::Inet("127.0.0.1:101".parse().unwrap());
        let b = SocketAddress::Inet("127.0.0.1:102".parse().unwrap());
        let listener = sim.listen(b.clone()).unwrap();
        let _node = sim.enter_endpoint(a.clone());
        let client = Rc::new(sim.connect(b.clone()).unwrap());
        let listener = listener.into_sim().unwrap();
        let server = Rc::new(listener.accept().unwrap());
        drive(
            &r,
            r.send(client.clone(), r.file_bytes(b"before").unwrap(), (), &scope),
        )
        .unwrap();
        sim.partition(a.clone(), b.clone());
        let read = drive(
            &r,
            r.recv(server.clone(), r.file_buffer(32).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(read.buffer.prefix(read.bytes).unwrap(), b"before");
        drop(read);
        let mut send = r.send(client.clone(), r.file_bytes(b"after").unwrap(), (), &scope);
        let mut reverse = r.send(
            server.clone(),
            r.file_bytes(b"reverse").unwrap(),
            (),
            &scope,
        );
        let fresh = Rc::new(Descriptor::socket(libc::AF_INET).unwrap());
        let mut connect = r.connect(fresh.clone(), b.clone(), &scope);
        let mut ready = r.readiness(client.clone(), libc::POLLOUT as u32, &scope);
        for _ in 0..5 {
            assert!(poll(&mut send).is_pending());
            assert!(poll(&mut reverse).is_pending());
            assert!(poll(&mut connect).is_pending());
            assert!(poll(&mut ready).is_pending());
            r.poll_budgeted(8).unwrap();
        }
        let (x, y) = sim.socket_pair();
        x.try_send(b"ok").unwrap();
        let mut bytes = [0; 8];
        assert_eq!(y.try_recv(&mut bytes).unwrap(), 2);
        sim.heal(b.clone(), a.clone());
        assert_eq!(drive(&r, send).unwrap().bytes, 5);
        assert_eq!(drive(&r, reverse).unwrap().bytes, 7);
        drive(&r, connect).unwrap();
        drive(&r, ready).unwrap();
        assert_eq!(server.try_recv(&mut bytes).unwrap(), 5);
        assert_eq!(&bytes[..5], b"after");
        assert_eq!(client.try_recv(&mut bytes).unwrap(), 7);
        assert_eq!(&bytes[..7], b"reverse");
        sim.partition(a, b);
        let weak = Rc::downgrade(&client);
        let mut pending = r.send(client, r.file_bytes(b"blocked").unwrap(), (), &scope);
        assert!(poll(&mut pending).is_pending());
        drop(pending);
        r.poll_budgeted(1).unwrap();
        assert!(weak.upgrade().is_some());
        drive(&r, r.drain()).unwrap();
        assert!(weak.upgrade().is_none());
    }

    #[test]
    fn file_sync_and_namespace_sync_are_independent() {
        for (file_sync, dir_sync) in [(false, false), (true, false), (false, true), (true, true)] {
            let (sim, _environment, r, scope) = setup();
            sim.create_dir_all(Path::new("/disk")).unwrap();
            sim.disk().sync_all().unwrap();
            sim.write_file(Path::new("/disk/new"), b"contents").unwrap();
            if file_sync {
                drive(&r, r.file_sync(open(&sim, "/disk/new"), &scope)).unwrap();
            }
            if dir_sync {
                let dir = Rc::new(
                    sim.open(None, Path::new("/disk"), libc::O_RDONLY | libc::O_DIRECTORY)
                        .unwrap(),
                );
                drive(&r, r.file_sync(dir, &scope)).unwrap();
            }
            sim.disk().crash().unwrap();
            if !dir_sync {
                assert_eq!(
                    sim.read_file(Path::new("/disk/new"))
                        .unwrap_err()
                        .raw_os_error(),
                    Some(libc::ENOENT)
                );
            } else {
                assert_eq!(
                    read(&sim, "/disk/new"),
                    if file_sync {
                        b"contents".as_slice()
                    } else {
                        b""
                    }
                );
            }
        }
    }

    #[test]
    fn replacement_unlink_and_directory_ancestors_require_namespace_fences() {
        let (sim, _environment, _, _) = setup();
        sim.write_file(Path::new("/disk/current"), b"old").unwrap();
        sim.disk().sync_all().unwrap();
        sim.write_file(Path::new("/disk/stage"), b"new").unwrap();
        sim.disk().sync(Path::new("/disk/stage")).unwrap();
        sim.rename(Path::new("/disk/stage"), Path::new("/disk/current"), 0)
            .unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/disk/current"), b"old");
        sim.write_file(Path::new("/disk/stage"), b"new").unwrap();
        sim.disk().sync(Path::new("/disk/stage")).unwrap();
        sim.rename(Path::new("/disk/stage"), Path::new("/disk/current"), 0)
            .unwrap();
        sim.disk().sync(Path::new("/disk")).unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/disk/current"), b"new");
        sim.unlink(Path::new("/disk/current")).unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/disk/current"), b"new");
        sim.unlink(Path::new("/disk/current")).unwrap();
        sim.disk().sync(Path::new("/disk")).unwrap();
        sim.disk().crash().unwrap();
        assert!(sim.read_file(Path::new("/disk/current")).is_err());
        sim.write_file(Path::new("/disk/child/file"), b"hidden")
            .unwrap();
        sim.disk().sync(Path::new("/disk/child/file")).unwrap();
        sim.disk().sync(Path::new("/disk/child")).unwrap();
        sim.disk().crash().unwrap();
        assert!(sim.metadata(Path::new("/disk/child")).is_err());
    }

    #[test]
    fn failed_sync_and_crash_during_pending_write_preserve_fences_and_other_disks() {
        let (sim, _environment, r, scope) = setup();
        for path in ["/a/file", "/b/file"] {
            sim.write_file(Path::new(path), b"old").unwrap();
        }
        sim.disk().sync_all().unwrap();
        let a = open(&sim, "/a/file");
        let b = open(&sim, "/b/file");
        drive(
            &r,
            r.write_at(a.clone(), 0, r.file_bytes(b"bad").unwrap(), (), &scope),
        )
        .unwrap();
        sim.inject("fsync", Fault::Errno(libc::EIO)).unwrap();
        assert_eq!(
            drive(&r, r.file_sync(a.clone(), &scope)),
            Err(Error::Os(libc::EIO))
        );
        sim.inject("write", Fault::Delay(3)).unwrap();
        let weak = Rc::downgrade(&a);
        let lease = r
            .admission
            .reserve(None, ResourceClass::Connection, 1)
            .unwrap();
        let mut write = r.write_at(a, 0, r.file_bytes(b"late").unwrap(), lease, &scope);
        assert!(poll(&mut write).is_pending());
        sim.disk().crash_under(Path::new("/a")).unwrap();
        assert!(weak.upgrade().is_some());
        assert_eq!(r.in_flight(), 1);
        assert_eq!(read(&sim, "/a/file"), b"old");
        drive(
            &r,
            r.write_at(b, 0, r.file_bytes(b"new").unwrap(), (), &scope),
        )
        .unwrap();
        assert!(matches!(drive(&r, write), Err(Error::Os(libc::EIO))));
        assert!(weak.upgrade().is_none());
        assert_eq!(read(&sim, "/a/file"), b"old");
        assert_eq!(read(&sim, "/b/file"), b"new");
        assert_eq!(r.admission.used(ResourceClass::Connection), 0);
        // A path-only open queued before crash cannot recreate lost names afterwards.
        sim.inject("open", Fault::Delay(2)).unwrap();
        let mut op = r.file_open(
            None,
            CString::new("/a/late").unwrap(),
            libc::O_CREAT | libc::O_RDWR,
            0,
            &scope,
        );
        assert!(poll(&mut op).is_pending());
        sim.disk().crash_under(Path::new("/a")).unwrap();
        assert!(matches!(drive(&r, op), Err(Error::Os(libc::EIO))));
        assert!(sim.metadata(Path::new("/a/late")).is_err());
    }

    #[test]
    fn sparse_corruption_and_truncate_do_not_mutate_durable_shared_pages() {
        let (sim, _environment, r, scope) = setup();
        sim.write_file(Path::new("/file"), b"abcdef").unwrap();
        sim.disk().sync_all().unwrap();
        let fd = open(&sim, "/file");
        let Some(h) = fd.as_sim() else { unreachable!() };
        h.set_len(2).unwrap();
        h.set_len(6).unwrap();
        assert_eq!(read(&sim, "/file"), b"ab\0\0\0\0");
        assert_eq!(
            sim.disk()
                .read(Path::new("/file"), 0, 8, DiskState::Durable)
                .unwrap(),
            b"abcdef"
        );
        sim.disk()
            .corrupt(Path::new("/file"), 1, b"X", DiskState::Volatile)
            .unwrap();
        sim.disk()
            .corrupt(Path::new("/file"), 4, b"Y", DiskState::Durable)
            .unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/file"), b"abcdYf");
        let fd = open(&sim, "/file");
        drive(
            &r,
            r.write_at(
                fd.clone(),
                1 << 40,
                r.file_bytes(b"sparse").unwrap(),
                (),
                &scope,
            ),
        )
        .unwrap();
        drive(&r, r.file_sync(fd, &scope)).unwrap();
        assert_eq!(
            sim.disk()
                .read(Path::new("/file"), (1 << 40) - 2, 10, DiskState::Durable)
                .unwrap(),
            b"\0\0sparse"
        );
        assert!(
            sim.disk()
                .corrupt(Path::new("/file"), u64::MAX, b"bad", DiskState::Both)
                .is_err()
        );
        sim.disk().crash().unwrap();
        assert_eq!(
            sim.disk()
                .read(Path::new("/file"), 1 << 40, 6, DiskState::Volatile)
                .unwrap(),
            b"sparse"
        );
    }

    #[test]
    fn crash_after_sync_issue_before_cqe_and_abandoned_sync_keep_real_fences() {
        let (sim, _environment, r, scope) = setup();
        sim.write_file(Path::new("/file"), b"old").unwrap();
        sim.disk().sync_all().unwrap();
        let fd = open(&sim, "/file");
        drive(
            &r,
            r.write_at(fd.clone(), 0, r.file_bytes(b"new").unwrap(), (), &scope),
        )
        .unwrap();
        let mut sync = r.file_sync(fd.clone(), &scope);
        assert!(poll(&mut sync).is_pending());
        // Submit fsync, but leave its successful original CQE unconsumed.
        r.poll_budgeted(1).unwrap();
        assert_eq!(r.in_flight(), 1);
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/file"), b"new");
        assert!(matches!(drive(&r, sync), Ok(())));
        assert!(matches!(
            drive(&r, r.file_stat(fd, &scope)),
            Err(Error::Os(libc::EBADF))
        ));
        let fd = open(&sim, "/file");
        drive(
            &r,
            r.write_at(fd.clone(), 0, r.file_bytes(b"bad").unwrap(), (), &scope),
        )
        .unwrap();
        let weak = Rc::downgrade(&fd);
        let mut sync = r.file_sync(fd, &scope);
        assert!(poll(&mut sync).is_pending());
        drop(sync);
        r.poll_budgeted(1).unwrap();
        assert!(weak.upgrade().is_some());
        r.poll_budgeted(1).unwrap();
        assert!(weak.upgrade().is_some());
        r.poll_budgeted(1).unwrap();
        assert!(weak.upgrade().is_none());
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/file"), b"new");
    }

    #[test]
    fn renamed_directory_fsync_uses_inode_and_durable_corruption_targets_exact_generation() {
        let (sim, _environment, _, _) = setup();
        sim.write_file(Path::new("/old/file"), b"before").unwrap();
        sim.disk().sync_all().unwrap();
        let directory = sim
            .open(None, Path::new("/old"), libc::O_RDONLY | libc::O_DIRECTORY)
            .unwrap();
        sim.rename(Path::new("/old"), Path::new("/new"), 0).unwrap();
        sim.write_file(Path::new("/new/extra"), b"extra").unwrap();
        sim.disk().sync(Path::new("/new/extra")).unwrap();
        let directory = directory.into_sim().unwrap();
        directory.sync().unwrap();
        sim.disk().sync(Path::new("/")).unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/new/file"), b"before");
        assert_eq!(read(&sim, "/new/extra"), b"extra");
        assert!(sim.metadata(Path::new("/old")).is_err());
        sim.write_file(Path::new("/stage"), b"replace").unwrap();
        sim.rename(Path::new("/stage"), Path::new("/new/file"), 0)
            .unwrap();
        assert_eq!(
            sim.disk()
                .corrupt(Path::new("/new/file"), 0, b"X", DiskState::Both)
                .unwrap_err()
                .raw_os_error(),
            Some(libc::ESTALE)
        );
        assert_eq!(read(&sim, "/new/file"), b"replace");
        assert_eq!(
            sim.disk()
                .read(Path::new("/new/file"), 0, 64, DiskState::Durable)
                .unwrap(),
            b"before"
        );
    }

    // Persistence faults mutate the integrated disk; production entries own I/O.
    #[test]
    fn crashprefix_covers_missing_torn_and_reordered_overwrites() {
        let offset = (1 << 40) - 4;
        for (prefix, expected) in [b"base", b"bBBe", b"AABe"].into_iter().enumerate() {
            let (sim, _environment, r, scope) = setup();
            let path = Path::new("/sparse");
            sim.write_file(path, b"").unwrap();
            let fd = open(&sim, "/sparse");
            drive(
                &r,
                r.write_at(
                    fd.clone(),
                    offset,
                    r.file_bytes(b"base").unwrap(),
                    (),
                    &scope,
                ),
            )
            .unwrap();
            sim.disk().sync_all().unwrap();
            for (at, bytes) in [(offset, b"AAAA".as_slice()), (offset + 1, b"BB".as_slice())] {
                drive(
                    &r,
                    r.write_at(fd.clone(), at, r.file_bytes(bytes).unwrap(), (), &scope),
                )
                .unwrap();
            }
            assert_eq!(
                sim.disk()
                    .read(path, offset, 4, DiskState::Volatile)
                    .unwrap(),
                b"ABBA"
            );
            // Explicit durable-only fault prefixes preserve the independent volatile image.
            if prefix >= 1 {
                sim.disk()
                    .corrupt(path, offset + 1, b"BB", DiskState::Durable)
                    .unwrap();
            }
            if prefix >= 2 {
                sim.disk()
                    .corrupt(path, offset, b"AA", DiskState::Durable)
                    .unwrap();
            }
            assert_eq!(
                sim.disk()
                    .read(path, offset, 4, DiskState::Volatile)
                    .unwrap(),
                b"ABBA"
            );
            sim.disk().crash().unwrap();
            assert!(matches!(
                drive(
                    &r,
                    r.write_at(fd, offset, r.file_bytes(b"late").unwrap(), (), &scope)
                ),
                Err(Error::Os(libc::EBADF))
            ));
            let result = drive(
                &r,
                r.read_at(
                    open(&sim, "/sparse"),
                    offset,
                    r.file_buffer(4).unwrap(),
                    (),
                    &scope,
                ),
            )
            .unwrap();
            assert_eq!(result.bytes, 4);
            assert_eq!(result.buffer.prefix(4).unwrap(), expected);
            drop(result);
            assert_eq!(r.in_flight(), 0);
            assert_eq!(sim.live_handles(), 0);
        }
    }

    #[test]
    fn invalid_fault_plans_and_extents_are_atomic_and_holes_are_zero() {
        let (sim, _environment, r, scope) = setup();
        let path = Path::new("/file");
        sim.write_file(path, b"\0\0\0\0data").unwrap();
        sim.disk().sync_all().unwrap();
        for (offset, bytes) in [(4, b"large".as_slice()), (u64::MAX, b"x".as_slice())] {
            assert!(
                sim.disk()
                    .corrupt(path, offset, bytes, DiskState::Both)
                    .is_err()
            );
            for state in [DiskState::Volatile, DiskState::Durable] {
                assert_eq!(sim.disk().read(path, 0, 8, state).unwrap(), b"\0\0\0\0data");
            }
        }
        let old = open(&sim, "/file");
        assert!(matches!(
            drive(
                &r,
                r.write_at(
                    old.clone(),
                    u64::MAX,
                    r.file_bytes(b"x").unwrap(),
                    (),
                    &scope
                )
            ),
            Err(Error::InvalidInput)
        ));
        assert_eq!(read(&sim, "/file"), b"\0\0\0\0data");
        let holes = drive(
            &r,
            r.read_at(old.clone(), 0, r.file_buffer(4).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(holes.buffer.prefix(4).unwrap(), &[0; 4]);
        drop(holes);
        assert_eq!(
            drive(
                &r,
                r.read_at(old.clone(), 15, r.file_buffer(2).unwrap(), (), &scope)
            )
            .unwrap()
            .bytes,
            0
        );
        sim.disk().crash().unwrap();
        let fresh = open(&sim, "/file");
        let (Some(a), Some(b)) = (old.as_sim(), fresh.as_sim()) else {
            unreachable!()
        };
        assert_ne!(a.id(), b.id());
        assert!(matches!(
            drive(&r, r.file_stat(old, &scope)),
            Err(Error::Os(libc::EBADF))
        ));
        drive(
            &r,
            r.write_at(fresh, 0, r.file_bytes(b"new").unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(read(&sim, "/file"), b"new\0data");
        assert_eq!(r.in_flight(), 0);
    }
}

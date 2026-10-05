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
    pending: Vec<std::rc::Weak<PendingCrash>>,
}
#[derive(Debug)]
pub(super) struct PendingCrash {
    paths: Vec<PathBuf>,
    pub(super) crashed: std::cell::Cell<bool>,
}
impl State {
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
pub(super) fn normalize(path: &Path) -> io::Result<PathBuf> {
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
    pub fn open(
        &self,
        dir: Option<&Descriptor>,
        path: &Path,
        flags: i32,
    ) -> io::Result<Descriptor> {
        self.open_resolved(dir, path, flags, 0)
    }
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
    pub(super) fn open_resolved(
        &self,
        dir: Option<&Descriptor>,
        path: &Path,
        flags: i32,
        resolve: u64,
    ) -> io::Result<Descriptor> {
        if let Some(dir) = dir {
            match dir.as_sim() {
                Some(handle) if Rc::ptr_eq(&self.0, &handle.sim.0) => (),
                _ => return Err(errno(libc::EXDEV)),
            }
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
    pub fn metadata(&self, path: &Path) -> io::Result<(u64, u16)> {
        let w = self.0.borrow();
        let node = w
            .paths
            .get(&normalize(path)?)
            .ok_or_else(|| errno(libc::ENOENT))?
            .borrow();
        Ok((node.inode, node.mode))
    }
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
    pub(super) fn node(&self) -> io::Result<(Rc<RefCell<Node>>, i32)> {
        match self.sim.0.borrow().resources.get(&self.id) {
            Some(Resource::File { node, flags, .. }) => Ok((node.clone(), *flags)),
            _ => Err(errno(libc::EBADF)),
        }
    }
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
        // Copy one sparse page at a time, including unaligned first/last pages.
        let mut index = 0;
        while index < len {
            let pos = offset + index as u64;
            let start = (pos % 4096) as usize;
            let count = (4096 - start).min(len - index);
            let output = &mut bytes[index..index + count];
            if let Some(page) = node.pages.get(&(pos / 4096)) {
                output.copy_from_slice(&page[start..start + count]);
            } else {
                output.fill(0);
            }
            index += count;
        }
        Ok(len)
    }
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
        let mut index = 0;
        while index < bytes.len() {
            let pos = offset + index as u64;
            let start = (pos % 4096) as usize;
            let count = (4096 - start).min(bytes.len() - index);
            let page = Rc::make_mut(
                node.pages
                    .entry(pos / 4096)
                    .or_insert_with(|| Rc::new([0; 4096])),
            );
            page[start..start + count].copy_from_slice(&bytes[index..index + count]);
            index += count;
        }
        node.length = node.length.max(end);
        Ok(bytes.len())
    }
}
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

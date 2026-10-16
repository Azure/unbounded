//! The simulated filesystem: namespace, path resolution, inodes and file I/O.
//!
//! The namespace is a map from normalized absolute paths to inodes. Inodes
//! store their contents as sparse 4 KiB pages shared copy-on-write with the
//! durable image (see [`Disk`](crate::Disk)), so syncing and crashing large
//! sparse files is cheap.
//!
//! The simulated process's working directory is always `/`; a directory
//! argument of `None` (`AT_FDCWD`) means `/`.
//!
//! # Linux compatibility
//!
//! `openat2` path resolution follows Linux, including `RESOLVE_BENEATH`,
//! `RESOLVE_IN_ROOT`, `RESOLVE_NO_SYMLINKS`, trailing-slash and final-symlink
//! rules, and `O_PATH`, `O_NOFOLLOW`, `O_EXCL`, `O_DIRECTORY` and `O_TRUNC`.
//! The world has a single mount and no magic links, so `RESOLVE_NO_XDEV` and
//! `RESOLVE_NO_MAGICLINKS` never fail, and every lookup is cached, so
//! `RESOLVE_CACHED` fails with `EAGAIN` exactly when Linux always would
//! (with `O_CREAT`, `O_TRUNC` or `O_TMPFILE`). `O_TMPFILE` is not supported
//! (`EOPNOTSUPP`). `O_DIRECT` transfers require 4096-byte alignment of the
//! offset, buffer address and length. Ownership is reported from
//! [`World::set_owner`]; permission bits are stored but never enforced.

use std::cell::RefCell;
use std::collections::{BTreeMap, VecDeque};
use std::ffi::{OsStr, OsString};
use std::os::unix::ffi::OsStrExt;
use std::path::{Component, Path, PathBuf};
use std::rc::Rc;

use uring_runtime::Result;
use uring_runtime::backend::OpenHow;

use crate::disk::Durable;
use crate::fault::SyncOp;
use crate::trace::EventKind;
use crate::world::{MAX_ALLOCATION, Resource, SimFd, State, World, bounded_bytes, errno};

/// The simulated page size and `O_DIRECT` alignment.
pub const PAGE_SIZE: usize = 4096;

/// `RESOLVE_NO_XDEV`.
pub const RESOLVE_NO_XDEV: u64 = 0x01;
/// `RESOLVE_NO_MAGICLINKS`.
pub const RESOLVE_NO_MAGICLINKS: u64 = 0x02;
/// `RESOLVE_NO_SYMLINKS`.
pub const RESOLVE_NO_SYMLINKS: u64 = 0x04;
/// `RESOLVE_BENEATH`.
pub const RESOLVE_BENEATH: u64 = 0x08;
/// `RESOLVE_IN_ROOT`.
pub const RESOLVE_IN_ROOT: u64 = 0x10;
/// `RESOLVE_CACHED`.
pub const RESOLVE_CACHED: u64 = 0x20;

const RESOLVE_ALL: u64 = 0x3f;
const MAX_SYMLINKS: usize = 40;

pub(crate) type Page = Rc<[u8; PAGE_SIZE]>;
pub(crate) type Pages = BTreeMap<u64, Page>;
pub(crate) type NodeRef = Rc<RefCell<Node>>;

pub(crate) fn file_type(mode: u32) -> u32 {
    mode & libc::S_IFMT
}

/// An inode.
#[derive(Debug)]
pub(crate) struct Node {
    pub(crate) inode: u64,
    /// File type and permission bits (`st_mode`).
    pub(crate) mode: u32,
    pub(crate) length: u64,
    pub(crate) pages: Pages,
    pub(crate) locked: bool,
    pub(crate) symlink: Option<PathBuf>,
}

impl Node {
    pub(crate) fn kind(&self) -> u32 {
        file_type(self.mode)
    }
}

/// An open file description.
#[derive(Debug)]
pub(crate) struct File {
    pub(crate) node: NodeRef,
    pub(crate) flags: i32,
    pub(crate) lock_owner: bool,
    /// Where the inode was when opened (kept current across renames); used
    /// to decide which crashes invalidate the description.
    pub(crate) opened_path: PathBuf,
    pub(crate) position: u64,
}

impl File {
    pub(crate) fn release(self) {
        if self.lock_owner {
            self.node.borrow_mut().locked = false;
        }
    }
}

/// Reported ownership and creation defaults.
#[derive(Debug, Clone, Copy)]
pub(crate) struct Identity {
    pub(crate) uid: u32,
    pub(crate) gid: u32,
    pub(crate) link_count: Option<u32>,
    pub(crate) umask: u32,
}

/// The filesystem part of the world state.
#[derive(Debug)]
pub(crate) struct Namespace {
    pub(crate) paths: BTreeMap<PathBuf, NodeRef>,
    next_inode: u64,
    pub(crate) durable: Durable,
    pub(crate) identity: Identity,
}

/// A summary of a named inode, as returned by [`World::metadata`].
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub struct Metadata {
    /// Inode number.
    pub inode: u64,
    /// File type and permission bits (`st_mode`).
    pub mode: u32,
    /// Length in bytes.
    pub len: u64,
}

impl Namespace {
    pub(crate) fn new() -> Self {
        let mut namespace = Self {
            paths: BTreeMap::new(),
            next_inode: 1,
            durable: Durable::default(),
            identity: Identity {
                uid: 0,
                gid: 0,
                link_count: None,
                umask: 0o022,
            },
        };
        let root = namespace.node(libc::S_IFDIR | 0o755);
        namespace.paths.insert(PathBuf::from("/"), root);
        namespace
    }

    pub(crate) fn len(&self) -> usize {
        self.paths.len()
    }

    /// Allocate an empty inode.
    pub(crate) fn node(&mut self, mode: u32) -> NodeRef {
        let inode = self.next_inode;
        self.next_inode += 1;
        Rc::new(RefCell::new(Node {
            inode,
            mode,
            length: 0,
            pages: BTreeMap::new(),
            locked: false,
            symlink: None,
        }))
    }

    /// The current path of `node`, if it is linked.
    pub(crate) fn path_of(&self, node: &NodeRef) -> Option<PathBuf> {
        self.paths
            .iter()
            .find(|(_, candidate)| Rc::ptr_eq(candidate, node))
            .map(|(path, _)| path.clone())
    }

    fn get(&self, path: &Path) -> Option<&NodeRef> {
        self.paths.get(path)
    }

    fn kind_of(&self, path: &Path) -> Option<u32> {
        self.get(path).map(|node| node.borrow().kind())
    }

    /// Require `path`'s parent to exist and be a directory.
    fn check_parent(&self, path: &Path) -> Result<()> {
        let parent = path.parent().ok_or_else(|| errno(libc::ENOENT))?;
        match self.kind_of(parent) {
            None => Err(errno(libc::ENOENT)),
            Some(libc::S_IFDIR) => Ok(()),
            Some(_) => Err(errno(libc::ENOTDIR)),
        }
    }

    fn creation_mode(&self, kind: u32, mode: u32) -> u32 {
        kind | (mode & 0o7777 & !self.identity.umask)
    }

    /// Follow symlinks in `path` (absolute), enforcing an `openat2`
    /// boundary.
    fn resolve(
        &self,
        path: PathBuf,
        boundary: Option<&Path>,
        no_symlinks: bool,
        in_root: bool,
        follow_final: bool,
    ) -> Result<PathBuf> {
        // `.` and a trailing slash require the preceding inode to be a
        // directory, but `Path::components` drops both, so split bytes.
        let parts = |path: &Path| {
            let bytes = path.as_os_str().as_bytes();
            let mut parts: VecDeque<OsString> = bytes
                .split(|byte| *byte == b'/')
                .filter(|part| !part.is_empty())
                .map(|part| OsStr::from_bytes(part).to_owned())
                .collect();
            if bytes.ends_with(b"/") {
                parts.push_back(OsString::from("."));
            }
            parts
        };
        let mut pending = parts(&path);
        let mut prefix = PathBuf::from("/");
        let mut links = 0;
        while let Some(part) = pending.pop_front() {
            if part == "." {
                continue;
            }
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
            let Some(node) = self.get(&prefix) else {
                if !pending.is_empty() {
                    return Err(errno(libc::ENOENT));
                }
                continue;
            };
            let node = node.borrow();
            if let Some(target) = &node.symlink {
                // Parent components always follow the traversal policy; the
                // final one obeys the open's NOFOLLOW/EXCL policy.
                if pending.is_empty() && !follow_final {
                    return Ok(prefix);
                }
                links += 1;
                if no_symlinks || links > MAX_SYMLINKS {
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
            } else if !pending.is_empty() && node.kind() != libc::S_IFDIR {
                return Err(errno(libc::ENOTDIR));
            }
        }
        Ok(prefix)
    }
}

/// Normalize a fixture path to an absolute path. Relative paths are taken
/// relative to `/`; `..` components are rejected with `EXDEV`.
pub(crate) fn normalize(path: &Path) -> Result<PathBuf> {
    let mut out = PathBuf::from("/");
    for component in path.components() {
        match component {
            Component::RootDir | Component::CurDir => (),
            Component::Normal(name) => out.push(name),
            _ => return Err(errno(libc::EXDEV)),
        }
    }
    Ok(out)
}

fn c_path(path: &Path) -> &[u8] {
    path.as_os_str().as_bytes()
}

/// Copy pages into `bytes`, zero-filling holes.
pub(crate) fn read_pages(pages: &Pages, offset: u64, bytes: &mut [u8]) {
    let mut index = 0;
    while index < bytes.len() {
        let pos = offset + index as u64;
        let start = (pos % PAGE_SIZE as u64) as usize;
        let count = (PAGE_SIZE - start).min(bytes.len() - index);
        let output = &mut bytes[index..index + count];
        match pages.get(&(pos / PAGE_SIZE as u64)) {
            Some(page) => output.copy_from_slice(&page[start..start + count]),
            None => output.fill(0),
        }
        index += count;
    }
}

/// Write `bytes` at `offset`, copying shared pages on write.
pub(crate) fn overwrite(pages: &mut Pages, offset: u64, bytes: &[u8]) {
    let mut index = 0;
    while index < bytes.len() {
        let pos = offset + index as u64;
        let start = (pos % PAGE_SIZE as u64) as usize;
        let count = (PAGE_SIZE - start).min(bytes.len() - index);
        let page = Rc::make_mut(
            pages
                .entry(pos / PAGE_SIZE as u64)
                .or_insert_with(|| Rc::new([0; PAGE_SIZE])),
        );
        page[start..start + count].copy_from_slice(&bytes[index..index + count]);
        index += count;
    }
}

/// Check that writing `length` bytes at `offset` stays within the page
/// budget, before changing anything.
pub(crate) fn check_page_budget(pages: &Pages, offset: u64, length: usize) -> Result<()> {
    if length == 0 {
        return Ok(());
    }
    if length > MAX_ALLOCATION {
        return Err(errno(libc::ENOSPC));
    }
    let end = offset
        .checked_add(length as u64)
        .ok_or_else(|| errno(libc::EFBIG))?;
    let page = PAGE_SIZE as u64;
    let new_pages = (offset / page..=(end - 1) / page)
        .filter(|index| !pages.contains_key(index))
        .count();
    if pages.len().saturating_add(new_pages) > MAX_ALLOCATION / PAGE_SIZE {
        return Err(errno(libc::ENOSPC));
    }
    Ok(())
}

/// `O_DIRECT` alignment, checked against the caller's original request.
pub(crate) fn check_direct(flags: i32, offset: u64, ptr: *const u8, length: usize) -> Result<()> {
    let align = PAGE_SIZE as u64;
    if flags & libc::O_DIRECT != 0
        && (!offset.is_multiple_of(align)
            || !(ptr as usize).is_multiple_of(PAGE_SIZE)
            || !length.is_multiple_of(PAGE_SIZE))
    {
        return Err(errno(libc::EINVAL));
    }
    Ok(())
}

/// A resolved transfer offset: explicit, or the file position.
fn transfer_offset(offset: u64) -> Result<Option<u64>> {
    if offset == u64::MAX {
        Ok(None)
    } else if offset > i64::MAX as u64 {
        Err(errno(libc::EINVAL))
    } else {
        Ok(Some(offset))
    }
}

impl State {
    pub(crate) fn file(&self, id: u64) -> Result<&File> {
        match self.resources.get(&id) {
            Some(Resource::File(file)) => Ok(file),
            Some(_) => Err(errno(libc::EINVAL)),
            None => Err(errno(libc::EBADF)),
        }
    }

    fn file_mut(&mut self, id: u64) -> Result<&mut File> {
        match self.resources.get_mut(&id) {
            Some(Resource::File(file)) => Ok(file),
            Some(_) => Err(errno(libc::EINVAL)),
            None => Err(errno(libc::EBADF)),
        }
    }

    /// The absolute path a directory argument stands for.
    fn base(&self, dir: Option<u64>) -> Result<PathBuf> {
        let Some(id) = dir else {
            return Ok(PathBuf::from("/"));
        };
        let node = match self.resources.get(&id) {
            Some(Resource::File(file)) => &file.node,
            Some(_) => return Err(errno(libc::ENOTDIR)),
            None => return Err(errno(libc::EBADF)),
        };
        if node.borrow().kind() != libc::S_IFDIR {
            return Err(errno(libc::ENOTDIR));
        }
        self.fs.path_of(node).ok_or_else(|| errno(libc::ENOENT))
    }

    /// Resolve `path` relative to `dir` under `openat2` rules.
    pub(crate) fn open_path(
        &self,
        dir: Option<u64>,
        path: &Path,
        resolve: u64,
        flags: i32,
    ) -> Result<PathBuf> {
        if resolve & !RESOLVE_ALL != 0
            || resolve & (RESOLVE_BENEATH | RESOLVE_IN_ROOT) == RESOLVE_BENEATH | RESOLVE_IN_ROOT
        {
            return Err(errno(libc::EINVAL));
        }
        if resolve & RESOLVE_CACHED != 0
            && (flags & (libc::O_CREAT | libc::O_TRUNC) != 0
                || flags & libc::O_TMPFILE == libc::O_TMPFILE)
        {
            return Err(errno(libc::EAGAIN));
        }
        if path.as_os_str().is_empty() {
            return Err(errno(libc::ENOENT));
        }
        let absolute = path.is_absolute();
        if resolve & RESOLVE_BENEATH != 0 && absolute {
            return Err(errno(libc::EXDEV));
        }
        let in_root = resolve & RESOLVE_IN_ROOT != 0;
        let base = if absolute && !in_root {
            PathBuf::from("/")
        } else {
            self.base(dir)?
        };
        let boundary =
            (resolve & (RESOLVE_BENEATH | RESOLVE_IN_ROOT) != 0).then_some(base.as_path());
        let full = if in_root && absolute {
            // Keep `.` and trailing-slash components inside the scoped root.
            let bytes = c_path(path);
            let start = bytes.iter().position(|b| *b != b'/').unwrap_or(bytes.len());
            base.join(OsStr::from_bytes(&bytes[start..]))
        } else {
            base.join(path)
        };
        let follow_final = flags & libc::O_NOFOLLOW == 0
            && flags & (libc::O_CREAT | libc::O_EXCL) != (libc::O_CREAT | libc::O_EXCL);
        self.fs.resolve(
            full,
            boundary,
            resolve & RESOLVE_NO_SYMLINKS != 0,
            in_root,
            follow_final,
        )
    }

    /// Resolve a `*at` path argument, following symlinks in the parent
    /// components and (if `follow`) in the final one.
    pub(crate) fn at_path(&self, dir: Option<u64>, path: &Path, follow: bool) -> Result<PathBuf> {
        let flags = if follow { 0 } else { libc::O_NOFOLLOW };
        self.open_path(dir, path, 0, flags)
    }

    /// `openat2(dir, path, how)`, returning the new resource id.
    pub(crate) fn open(&mut self, dir: Option<u64>, path: &Path, how: &OpenHow) -> Result<u64> {
        let flags = i32::try_from(how.flags).map_err(|_| errno(libc::EINVAL))?;
        if flags & (libc::O_CREAT | libc::O_DIRECTORY) == libc::O_CREAT | libc::O_DIRECTORY
            || flags & libc::O_ACCMODE == libc::O_ACCMODE
        {
            return Err(errno(libc::EINVAL));
        }
        if flags & libc::O_TMPFILE == libc::O_TMPFILE {
            return Err(errno(if flags & libc::O_ACCMODE == libc::O_RDONLY {
                libc::EINVAL
            } else {
                libc::EOPNOTSUPP
            }));
        }
        if how.mode != 0 && flags & libc::O_CREAT == 0 || how.mode & !0o7777 != 0 {
            return Err(errno(libc::EINVAL));
        }
        if let Some(id) = dir
            && (!path.is_absolute() || how.resolve & RESOLVE_IN_ROOT != 0)
            && !self.resources.contains_key(&id)
        {
            return Err(errno(libc::EBADF));
        }
        let path = self.open_path(dir, path, how.resolve, flags)?;
        let exclusive = flags & (libc::O_CREAT | libc::O_EXCL) == libc::O_CREAT | libc::O_EXCL;
        if self.fs.paths.contains_key(&path) && exclusive {
            return Err(errno(libc::EEXIST));
        }
        if !self.fs.paths.contains_key(&path) {
            if flags & libc::O_CREAT == 0 {
                return Err(errno(libc::ENOENT));
            }
            self.fs.check_parent(&path)?;
            let mode = self.fs.creation_mode(libc::S_IFREG, how.mode as u32);
            let node = self.fs.node(mode);
            self.fs.paths.insert(path.clone(), node);
        }
        let node = self.fs.paths[&path].clone();
        {
            let n = node.borrow();
            if n.symlink.is_some() && flags & libc::O_PATH == 0 {
                return Err(errno(libc::ELOOP));
            }
            if flags & libc::O_DIRECTORY != 0 && n.kind() != libc::S_IFDIR {
                return Err(errno(libc::ENOTDIR));
            }
            if n.kind() == libc::S_IFDIR && flags & libc::O_ACCMODE != libc::O_RDONLY {
                return Err(errno(libc::EISDIR));
            }
        }
        if flags & libc::O_TRUNC != 0 && flags & libc::O_PATH == 0 {
            let mut n = node.borrow_mut();
            if n.kind() != libc::S_IFREG {
                return Err(errno(libc::EISDIR));
            }
            n.pages.clear();
            n.length = 0;
        }
        self.insert(Resource::File(File {
            node,
            flags,
            lock_owner: false,
            opened_path: path,
            position: 0,
        }))
    }

    /// `pread`; an offset of `u64::MAX` uses and advances the file position.
    pub(crate) fn file_read(
        &mut self,
        id: u64,
        offset: u64,
        buf: &mut [u8],
        limit: usize,
    ) -> Result<usize> {
        let offset = transfer_offset(offset)?;
        let max_chunk = self.knobs.max_chunk;
        let file = self.file_mut(id)?;
        let at = offset.unwrap_or(file.position);
        check_direct(file.flags, at, buf.as_ptr(), buf.len())?;
        if file.flags & libc::O_PATH != 0 || file.flags & libc::O_ACCMODE == libc::O_WRONLY {
            return Err(errno(libc::EBADF));
        }
        let node = file.node.borrow();
        if node.kind() != libc::S_IFREG {
            return Err(errno(libc::EISDIR));
        }
        let len = buf
            .len()
            .min(usize::try_from(node.length.saturating_sub(at)).unwrap_or(usize::MAX))
            .min(max_chunk)
            .min(limit);
        read_pages(&node.pages, at, &mut buf[..len]);
        drop(node);
        if offset.is_none() {
            file.position = at + len as u64;
        }
        Ok(len)
    }

    /// `pwrite`; an offset of `u64::MAX` uses and advances the file
    /// position, and `O_APPEND` writes at the end.
    pub(crate) fn file_write(
        &mut self,
        id: u64,
        offset: u64,
        buf: &[u8],
        limit: usize,
    ) -> Result<usize> {
        let offset = transfer_offset(offset)?;
        let max_chunk = self.knobs.max_chunk;
        let file = self.file_mut(id)?;
        check_direct(
            file.flags,
            offset.unwrap_or(file.position),
            buf.as_ptr(),
            buf.len(),
        )?;
        let buf = &buf[..buf.len().min(limit).min(max_chunk)];
        if file.flags & libc::O_PATH != 0 || file.flags & libc::O_ACCMODE == libc::O_RDONLY {
            return Err(errno(libc::EBADF));
        }
        let mut node = file.node.borrow_mut();
        let at = if file.flags & libc::O_APPEND != 0 {
            node.length
        } else {
            offset.unwrap_or(file.position)
        };
        let end = at
            .checked_add(buf.len() as u64)
            .filter(|end| *end <= i64::MAX as u64)
            .ok_or_else(|| errno(libc::EFBIG))?;
        if node.kind() != libc::S_IFREG {
            return Err(errno(libc::EISDIR));
        }
        if !buf.is_empty() {
            check_page_budget(&node.pages, at, buf.len())?;
            overwrite(&mut node.pages, at, buf);
            node.length = node.length.max(end);
        }
        drop(node);
        if offset.is_none() || file.flags & libc::O_APPEND != 0 {
            file.position = end;
        }
        Ok(buf.len())
    }

    pub(crate) fn stat_node(&self, node: &Node) -> libc::statx {
        // SAFETY: all-zero bytes are a valid `statx`.
        let mut stat: libc::statx = unsafe { std::mem::zeroed() };
        let identity = self.fs.identity;
        stat.stx_mask = libc::STATX_BASIC_STATS | libc::STATX_DIOALIGN;
        stat.stx_blksize = PAGE_SIZE as u32;
        stat.stx_mode = node.mode as u16;
        stat.stx_ino = node.inode;
        stat.stx_size = node.length;
        stat.stx_blocks = node.pages.len() as u64 * (PAGE_SIZE as u64 / 512);
        stat.stx_nlink = identity
            .link_count
            .unwrap_or(if node.kind() == libc::S_IFDIR { 2 } else { 1 });
        stat.stx_uid = identity.uid;
        stat.stx_gid = identity.gid;
        stat.stx_dio_mem_align = PAGE_SIZE as u32;
        stat.stx_dio_offset_align = PAGE_SIZE as u32;
        stat
    }

    pub(crate) fn fstat(&self, id: u64) -> Result<libc::statx> {
        let file = self.file(id)?;
        Ok(self.stat_node(&file.node.borrow()))
    }

    /// `statx(dir, path, flags)`.
    pub(crate) fn statx(&self, dir: Option<u64>, path: &Path, flags: i32) -> Result<libc::statx> {
        if path.as_os_str().is_empty() {
            if flags & libc::AT_EMPTY_PATH == 0 {
                return Err(errno(libc::ENOENT));
            }
            return match dir {
                Some(id) => self.fstat(id),
                None => self.statx(None, Path::new("/"), 0),
            };
        }
        let path = self.at_path(dir, path, flags & libc::AT_SYMLINK_NOFOLLOW == 0)?;
        let node = self.fs.get(&path).ok_or_else(|| errno(libc::ENOENT))?;
        Ok(self.stat_node(&node.borrow()))
    }

    pub(crate) fn set_len(&mut self, id: u64, length: u64) -> Result<()> {
        if length > i64::MAX as u64 {
            return Err(errno(libc::EINVAL));
        }
        let file = self.file(id)?;
        if file.flags & libc::O_PATH != 0 || file.flags & libc::O_ACCMODE == libc::O_RDONLY {
            return Err(errno(libc::EBADF));
        }
        let mut node = file.node.borrow_mut();
        if node.kind() != libc::S_IFREG {
            return Err(errno(libc::EINVAL));
        }
        let page = PAGE_SIZE as u64;
        node.pages.retain(|index, _| *index < length.div_ceil(page));
        if !length.is_multiple_of(page)
            && let Some(last) = node.pages.get_mut(&(length / page))
        {
            Rc::make_mut(last)[(length % page) as usize..].fill(0);
        }
        node.length = length;
        Ok(())
    }

    pub(crate) fn lock(&mut self, id: u64) -> Result<()> {
        let file = self.file_mut(id)?;
        if file.lock_owner {
            return Ok(());
        }
        if file.flags & libc::O_PATH != 0 {
            return Err(errno(libc::EBADF));
        }
        let mut node = file.node.borrow_mut();
        if node.locked {
            return Err(errno(libc::EWOULDBLOCK));
        }
        node.locked = true;
        drop(node);
        file.lock_owner = true;
        Ok(())
    }

    pub(crate) fn unlock(&mut self, id: u64) -> Result<()> {
        let file = self.file_mut(id)?;
        if file.flags & libc::O_PATH != 0 {
            return Err(errno(libc::EBADF));
        }
        if file.lock_owner {
            file.node.borrow_mut().locked = false;
            file.lock_owner = false;
        }
        Ok(())
    }

    /// Create one directory at an absolute, resolved path.
    pub(crate) fn mkdir(&mut self, path: &Path, mode: u32) -> Result<()> {
        if self.fs.paths.contains_key(path) {
            return Err(errno(libc::EEXIST));
        }
        self.fs.check_parent(path)?;
        let node = self.fs.node(self.fs.creation_mode(libc::S_IFDIR, mode));
        let inode = node.borrow().inode;
        self.fs.paths.insert(path.to_owned(), node);
        self.record(EventKind::Mkdir, inode, 0);
        Ok(())
    }

    /// Remove a non-directory (or, with `directory`, an empty directory).
    pub(crate) fn unlink(&mut self, path: &Path, directory: bool) -> Result<()> {
        let kind = self.fs.kind_of(path).ok_or_else(|| errno(libc::ENOENT))?;
        if directory {
            if kind != libc::S_IFDIR {
                return Err(errno(libc::ENOTDIR));
            }
            if path == Path::new("/") {
                return Err(errno(libc::EBUSY));
            }
            if self
                .fs
                .paths
                .keys()
                .any(|p| p != path && p.starts_with(path))
            {
                return Err(errno(libc::ENOTEMPTY));
            }
        } else if kind == libc::S_IFDIR {
            return Err(errno(libc::EISDIR));
        }
        self.fs.paths.remove(path);
        self.net.forget_unix(path);
        self.record(EventKind::Unlink, 0, 0);
        Ok(())
    }

    /// `renameat2` on absolute, resolved paths.
    pub(crate) fn rename(&mut self, from: &Path, to: &Path, flags: u32) -> Result<()> {
        let exchange = flags & libc::RENAME_EXCHANGE != 0;
        if flags & !(libc::RENAME_NOREPLACE | libc::RENAME_EXCHANGE) != 0
            || flags == libc::RENAME_NOREPLACE | libc::RENAME_EXCHANGE
        {
            return Err(errno(libc::EINVAL));
        }
        let paths = &self.fs.paths;
        if !paths.contains_key(from) {
            return Err(errno(libc::ENOENT));
        }
        if from == to {
            return Ok(());
        }
        if from == Path::new("/") || to == Path::new("/") {
            return Err(errno(libc::EBUSY));
        }
        self.fs.check_parent(to)?;
        if to.starts_with(from) || from.starts_with(to) {
            return Err(errno(libc::EINVAL));
        }
        if flags & libc::RENAME_NOREPLACE != 0 && paths.contains_key(to) {
            return Err(errno(libc::EEXIST));
        }
        if exchange && !paths.contains_key(to) {
            return Err(errno(libc::ENOENT));
        }
        if !exchange && let Some(target) = paths.get(to) {
            let source_dir = paths[from].borrow().kind() == libc::S_IFDIR;
            let target_dir = target.borrow().kind() == libc::S_IFDIR;
            if source_dir != target_dir {
                return Err(errno(if source_dir {
                    libc::ENOTDIR
                } else {
                    libc::EISDIR
                }));
            }
            if target_dir && paths.keys().any(|p| p != to && p.starts_with(to)) {
                return Err(errno(libc::ENOTEMPTY));
            }
        }
        let moved: Vec<_> = paths
            .iter()
            .filter_map(|(path, node)| {
                if path != from && path.starts_with(from) {
                    Some((
                        path.clone(),
                        to.join(path.strip_prefix(from).ok()?),
                        node.clone(),
                    ))
                } else if exchange && path != to && path.starts_with(to) {
                    Some((
                        path.clone(),
                        from.join(path.strip_prefix(to).ok()?),
                        node.clone(),
                    ))
                } else {
                    None
                }
            })
            .collect();
        let paths = &mut self.fs.paths;
        for (old, _, _) in &moved {
            paths.remove(old);
        }
        for (_, new, node) in moved {
            paths.insert(new, node);
        }
        let source = paths.remove(from).expect("checked above");
        let replaced = paths.insert(to.to_owned(), source);
        if exchange {
            paths.insert(from.to_owned(), replaced.expect("checked above"));
        }
        // Keep each open description's crash ownership with its inode.
        let locations: BTreeMap<u64, PathBuf> = self
            .fs
            .paths
            .iter()
            .map(|(path, node)| (node.borrow().inode, path.clone()))
            .collect();
        for resource in self.resources.values_mut() {
            if let Resource::File(file) = resource
                && let Some(path) = locations.get(&file.node.borrow().inode)
            {
                file.opened_path = path.clone();
            }
        }
        self.net.rename_unix(from, to, exchange);
        self.record(EventKind::Rename, 0, 0);
        Ok(())
    }
}

/// Filesystem fixtures and inspection.
impl World {
    /// Report `uid` and `gid` as the owner of every inode (default 0 and 0).
    pub fn set_owner(&self, uid: u32, gid: u32) {
        let mut state = self.state();
        state.fs.identity.uid = uid;
        state.fs.identity.gid = gid;
    }

    /// Report `count` as every inode's link count, or restore the default
    /// (2 for directories, 1 otherwise) with `None`.
    pub fn set_link_count(&self, count: Option<u32>) {
        self.state().fs.identity.link_count = count;
    }

    /// Set the mask applied to the mode of created files and directories
    /// (default `0o022`). Returns the previous mask.
    pub fn set_umask(&self, umask: u32) -> u32 {
        std::mem::replace(&mut self.state().fs.identity.umask, umask & 0o777)
    }

    /// Create `path` and its missing ancestors (mode `0o755`, before the
    /// umask). Like every namespace change, the new entries are volatile
    /// until synced.
    pub fn create_dir_all(&self, path: impl AsRef<Path>) -> Result<()> {
        let path = normalize(path.as_ref())?;
        let mut state = self.state();
        let mut ancestors: Vec<_> = path.ancestors().collect();
        ancestors.reverse();
        for path in ancestors {
            match state.fs.kind_of(path) {
                Some(libc::S_IFDIR) => {}
                Some(_) => return Err(errno(libc::ENOTDIR)),
                None => state.mkdir(path, 0o755)?,
            }
        }
        Ok(())
    }

    /// Create exactly one directory (mode `0o755`, before the umask); the
    /// parent must exist.
    pub fn mkdir(&self, path: impl AsRef<Path>) -> Result<()> {
        let path = normalize(path.as_ref())?;
        let mut state = self.state();
        state.sync_limit(SyncOp::Mkdir)?;
        state.mkdir(&path, 0o755)
    }

    /// Create a symbolic link at `path` pointing at `target` (not resolved).
    pub fn symlink(&self, target: impl AsRef<Path>, path: impl AsRef<Path>) -> Result<()> {
        let path = normalize(path.as_ref())?;
        let mut state = self.state();
        if state.fs.paths.contains_key(&path) {
            return Err(errno(libc::EEXIST));
        }
        state.fs.check_parent(&path)?;
        let node = state.fs.node(libc::S_IFLNK | 0o777);
        node.borrow_mut().symlink = Some(target.as_ref().to_owned());
        state.fs.paths.insert(path, node);
        state.record(EventKind::Symlink, 0, 0);
        Ok(())
    }

    /// Open `path` (`AT_FDCWD`-relative) with `O_*` `flags`. Files created
    /// with `O_CREAT` get mode `0o666` before the umask.
    pub fn open(&self, path: impl AsRef<Path>, flags: i32) -> Result<SimFd> {
        self.open_at(None, path, flags, 0)
    }

    /// `openat2(dir, path, flags, resolve)` as a synchronous call. Files
    /// created with `O_CREAT` get mode `0o666` before the umask.
    pub fn open_at(
        &self,
        dir: Option<&SimFd>,
        path: impl AsRef<Path>,
        flags: i32,
        resolve: u64,
    ) -> Result<SimFd> {
        let dir = dir.map(|fd| self.id_of(fd)).transpose()?;
        let how = OpenHow {
            flags: flags as u32 as u64,
            mode: if flags & libc::O_CREAT != 0 { 0o666 } else { 0 },
            resolve,
        };
        let mut state = self.state();
        state.sync_limit(SyncOp::Open)?;
        let id = state.open(dir, path.as_ref(), &how)?;
        drop(state);
        Ok(self.handle(id))
    }

    /// Replace `path` with `bytes`, creating missing ancestors. Not synced.
    pub fn write_file(&self, path: impl AsRef<Path>, bytes: &[u8]) -> Result<()> {
        let path = path.as_ref();
        if let Some(parent) = path.parent() {
            self.create_dir_all(parent)?;
        }
        let fd = self.open(path, libc::O_CREAT | libc::O_RDWR | libc::O_TRUNC)?;
        let mut offset = 0;
        while offset < bytes.len() {
            let written = fd.write_at(offset as u64, &bytes[offset..])?;
            if written == 0 {
                return Err(errno(libc::EIO));
            }
            offset += written;
        }
        Ok(())
    }

    /// Read all of `path` (at most [`MAX_ALLOCATION`] bytes), honoring short
    /// reads and synchronous faults.
    pub fn read_file(&self, path: impl AsRef<Path>) -> Result<Vec<u8>> {
        let fd = self.open(path, libc::O_RDONLY)?;
        let size = usize::try_from(fd.stat()?.stx_size).map_err(|_| errno(libc::EFBIG))?;
        let mut bytes = bounded_bytes(size)?;
        let mut offset = 0;
        while offset < bytes.len() {
            let count = fd.read_at(offset as u64, &mut bytes[offset..])?;
            if count == 0 {
                bytes.truncate(offset);
                break;
            }
            offset += count;
        }
        Ok(bytes)
    }

    /// Describe the inode named by `path`, without following a final
    /// symlink.
    pub fn metadata(&self, path: impl AsRef<Path>) -> Result<Metadata> {
        let path = normalize(path.as_ref())?;
        let state = self.state();
        let node = state
            .fs
            .get(&path)
            .ok_or_else(|| errno(libc::ENOENT))?
            .borrow();
        Ok(Metadata {
            inode: node.inode,
            mode: node.mode,
            len: node.length,
        })
    }

    /// Change the permission bits of `path`, keeping its file type.
    pub fn chmod(&self, path: impl AsRef<Path>, mode: u32) -> Result<()> {
        let path = normalize(path.as_ref())?;
        let mut state = self.state();
        state.sync_limit(SyncOp::Chmod)?;
        let mut node = state
            .fs
            .get(&path)
            .ok_or_else(|| errno(libc::ENOENT))?
            .borrow_mut();
        node.mode = node.kind() | (mode & 0o7777);
        Ok(())
    }

    /// `renameat2(from, to, flags)` with `RENAME_NOREPLACE` and
    /// `RENAME_EXCHANGE` support. Open descriptions and Unix listeners move
    /// with their names.
    pub fn rename(&self, from: impl AsRef<Path>, to: impl AsRef<Path>, flags: u32) -> Result<()> {
        let from = normalize(from.as_ref())?;
        let to = normalize(to.as_ref())?;
        let mut state = self.state();
        state.sync_limit(SyncOp::Rename)?;
        state.rename(&from, &to, flags)
    }

    /// Remove a non-directory name. Open descriptions keep the inode.
    pub fn unlink(&self, path: impl AsRef<Path>) -> Result<()> {
        let path = normalize(path.as_ref())?;
        let mut state = self.state();
        state.sync_limit(SyncOp::Unlink)?;
        state.unlink(&path, false)
    }

    /// Remove an empty directory.
    pub fn rmdir(&self, path: impl AsRef<Path>) -> Result<()> {
        let path = normalize(path.as_ref())?;
        let mut state = self.state();
        state.sync_limit(SyncOp::Unlink)?;
        state.unlink(&path, true)
    }
}

/// File operations.
impl SimFd {
    /// `pread`: read at `offset` (`u64::MAX`: at and advancing the file
    /// position). Honors [`World::set_max_chunk`] and synchronous
    /// [`SyncOp::Read`] faults.
    pub fn read_at(&self, offset: u64, buf: &mut [u8]) -> Result<usize> {
        let mut state = self.state();
        let limit = state.sync_limit(SyncOp::Read)?;
        state.file_read(self.id(), offset, buf, limit)
    }

    /// `pwrite`: write at `offset` (`u64::MAX`: at and advancing the file
    /// position; ignored with `O_APPEND`). Honors [`World::set_max_chunk`]
    /// and synchronous [`SyncOp::Write`] faults.
    pub fn write_at(&self, offset: u64, buf: &[u8]) -> Result<usize> {
        let mut state = self.state();
        let limit = state.sync_limit(SyncOp::Write)?;
        state.file_write(self.id(), offset, buf, limit)
    }

    /// `fstat` as a `statx` structure.
    pub fn stat(&self) -> Result<libc::statx> {
        self.state().fstat(self.id())
    }

    /// `ftruncate`.
    pub fn set_len(&self, length: u64) -> Result<()> {
        self.state().set_len(self.id(), length)
    }

    /// `flock(LOCK_EX | LOCK_NB)`: fails with `EWOULDBLOCK` if another open
    /// description holds the lock. Released by [`unlock`](Self::unlock) or
    /// when the description is closed.
    pub fn lock(&self) -> Result<()> {
        self.state().lock(self.id())
    }

    /// `flock(LOCK_UN)`.
    pub fn unlock(&self) -> Result<()> {
        self.state().unlock(self.id())
    }

    /// `fsync`: make a file's data and length, or a directory's entries,
    /// durable. Honors synchronous [`SyncOp::Fsync`] faults.
    pub fn sync(&self) -> Result<()> {
        let mut state = self.state();
        state.sync_limit(SyncOp::Fsync)?;
        state.sync_file(self.id())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::disk::DiskState;
    use crate::fault::{Fault, FaultTarget};

    fn code<T: std::fmt::Debug>(result: Result<T>) -> i32 {
        result.unwrap_err().raw_os_error().unwrap()
    }

    fn open_resolved(
        world: &World,
        dir: &SimFd,
        path: &str,
        flags: i32,
        resolve: u64,
    ) -> Result<SimFd> {
        world.open_at(Some(dir), path, flags, resolve)
    }

    #[test]
    fn short_files_zero_writes_append_locks_and_bounds() {
        let world = World::new();
        let file = world.open("/f", libc::O_CREAT | libc::O_RDWR).unwrap();
        world.set_max_chunk(2).unwrap();
        assert_eq!(file.write_at(0, b"abcd").unwrap(), 2);
        assert_eq!(file.write_at(100, b"").unwrap(), 0);
        assert_eq!(file.stat().unwrap().stx_size, 2);
        assert_eq!(file.write_at(2, b"cd").unwrap(), 2);
        let mut bytes = [0xcc; 4];
        assert_eq!(file.read_at(0, &mut bytes).unwrap(), 2);
        assert_eq!(bytes, [b'a', b'b', 0xcc, 0xcc]);
        let append = world.open("/f", libc::O_RDWR | libc::O_APPEND).unwrap();
        append.write_at(0, b"ef").unwrap();
        assert_eq!(world.read_file("/f").unwrap(), b"abcdef");
        file.lock().unwrap();
        file.lock().unwrap();
        assert_eq!(code(append.lock()), libc::EWOULDBLOCK);
        drop(file);
        append.lock().unwrap();
        append.unlock().unwrap();
        append.lock().unwrap();
        let beyond = i64::MAX as u64 + 1;
        assert_eq!(code(append.write_at(beyond, b"x")), libc::EINVAL);
        assert_eq!(code(append.read_at(beyond, &mut bytes)), libc::EINVAL);
        assert_eq!(code(append.set_len(u64::MAX)), libc::EINVAL);
        append.set_len(1 << 40).unwrap();
        assert_eq!(code(world.read_file("/f")), libc::EFBIG);
        assert_eq!(
            code(world.disk().read("/f", 0, usize::MAX, DiskState::Volatile)),
            libc::EFBIG
        );
        assert_eq!(
            world
                .disk()
                .read("/f", 1 << 39, 4, DiskState::Volatile)
                .unwrap(),
            [0; 4]
        );
    }

    #[test]
    fn file_position_reads_and_writes_advance() {
        let world = World::new();
        world.write_file("/p", b"hello world").unwrap();
        let fd = world.open("/p", libc::O_RDWR).unwrap();
        let mut buf = [0; 5];
        assert_eq!(fd.read_at(u64::MAX, &mut buf).unwrap(), 5);
        assert_eq!(&buf, b"hello");
        assert_eq!(fd.read_at(u64::MAX, &mut buf).unwrap(), 5);
        assert_eq!(&buf, b" worl");
        assert_eq!(fd.write_at(u64::MAX, b"D!").unwrap(), 2);
        assert_eq!(world.read_file("/p").unwrap(), b"hello worlD!");
        assert_eq!(fd.read_at(u64::MAX, &mut buf).unwrap(), 0);
    }

    #[test]
    fn identity_and_umask_are_configurable() {
        let world = World::new();
        world.set_owner(1000, 100);
        assert_eq!(world.set_umask(0o077), 0o022);
        world.write_file("/a/f", b"x").unwrap();
        let stat = world.open("/a/f", libc::O_RDONLY).unwrap().stat().unwrap();
        assert_eq!((stat.stx_uid, stat.stx_gid, stat.stx_nlink), (1000, 100, 1));
        assert_eq!(u32::from(stat.stx_mode), libc::S_IFREG | 0o600);
        assert_eq!(world.metadata("/a").unwrap().mode, libc::S_IFDIR | 0o700);
        world.set_link_count(Some(7));
        let dir = world.open("/a", libc::O_DIRECTORY).unwrap();
        assert_eq!(dir.stat().unwrap().stx_nlink, 7);
        world.chmod("/a/f", 0o4755).unwrap();
        assert_eq!(world.metadata("/a/f").unwrap().mode, libc::S_IFREG | 0o4755);
        world
            .inject(FaultTarget::Sync(SyncOp::Chmod), Fault::Errno(libc::EPERM))
            .unwrap();
        assert_eq!(code(world.chmod("/a/f", 0)), libc::EPERM);
    }

    #[test]
    fn resolver_policies_and_single_component_mkdir() {
        let world = World::new();
        world.write_file("/root/child/f", b"x").unwrap();
        world.symlink("/child/f", "/root/absolute").unwrap();
        world.symlink("child/f", "/root/relative").unwrap();
        let dir = world.open("/root", libc::O_DIRECTORY).unwrap();
        for policy in [0x40, RESOLVE_BENEATH | RESOLVE_IN_ROOT, 1 << 40] {
            assert_eq!(
                code(open_resolved(&world, &dir, "child/f", 0, policy)),
                libc::EINVAL
            );
        }
        // A single-mount world never crosses mounts and every lookup is cached.
        for policy in [RESOLVE_NO_XDEV, RESOLVE_CACHED, RESOLVE_NO_MAGICLINKS] {
            assert!(open_resolved(&world, &dir, "child/f", 0, policy).is_ok());
        }
        assert_eq!(
            code(open_resolved(
                &world,
                &dir,
                "child/f",
                libc::O_CREAT | libc::O_RDWR,
                RESOLVE_CACHED
            )),
            libc::EAGAIN
        );
        assert!(
            open_resolved(
                &world,
                &dir,
                "relative",
                0,
                RESOLVE_NO_MAGICLINKS | RESOLVE_BENEATH
            )
            .is_ok()
        );
        assert_eq!(
            code(open_resolved(
                &world,
                &dir,
                "relative",
                0,
                RESOLVE_NO_SYMLINKS
            )),
            libc::ELOOP
        );
        assert_eq!(
            code(open_resolved(&world, &dir, "absolute", 0, RESOLVE_BENEATH)),
            libc::EXDEV
        );
        assert_eq!(
            code(open_resolved(
                &world,
                &dir,
                "../root/child/f",
                0,
                RESOLVE_BENEATH
            )),
            libc::EXDEV
        );
        assert!(open_resolved(&world, &dir, "child/../child/f", 0, RESOLVE_BENEATH).is_ok());
        for name in ["absolute", "/child/f", "../../child/f"] {
            assert!(
                open_resolved(&world, &dir, name, 0, RESOLVE_IN_ROOT).is_ok(),
                "{name}"
            );
        }
        assert_eq!(code(world.mkdir("/missing/child")), libc::ENOENT);
        assert!(world.metadata("/missing").is_err());
        world.mkdir("/one").unwrap();
        assert_eq!(code(world.mkdir("/one")), libc::EEXIST);
        assert_eq!(code(world.mkdir("/root/child/f/dir")), libc::ENOTDIR);
        assert_eq!(
            code(world.open("/x", libc::O_CREAT | libc::O_DIRECTORY)),
            libc::EINVAL
        );
        assert_eq!(code(world.open("/x", libc::O_ACCMODE)), libc::EINVAL);
        assert_eq!(
            code(world.open("/", libc::O_TMPFILE | libc::O_RDWR)),
            libc::EOPNOTSUPP
        );
        assert_eq!(code(world.open("", 0)), libc::ENOENT);
    }

    #[test]
    fn rename_unlink_and_rmdir_semantics() {
        let world = World::new();
        world.write_file("/a/f", b"1").unwrap();
        world.write_file("/b/g", b"2").unwrap();
        world.create_dir_all("/empty").unwrap();
        let held = world.open("/a/f", libc::O_RDONLY).unwrap();
        assert_eq!(code(world.rename("/a", "/b", 0)), libc::ENOTEMPTY);
        assert_eq!(code(world.rename("/a/f", "/b", 0)), libc::EISDIR);
        assert_eq!(code(world.rename("/a", "/b/g", 0)), libc::ENOTDIR);
        assert_eq!(code(world.rename("/a", "/a/sub", 0)), libc::EINVAL);
        assert_eq!(
            code(world.rename("/a/f", "/b/g", libc::RENAME_NOREPLACE)),
            libc::EEXIST
        );
        assert_eq!(
            code(world.rename("/a/f", "/b/none", libc::RENAME_EXCHANGE)),
            libc::ENOENT
        );
        assert_eq!(code(world.rename("/a/f", "/b/g", 0xff)), libc::EINVAL);
        assert_eq!(code(world.rename("/", "/z", 0)), libc::EBUSY);
        world.rename("/a/f", "/b/g", libc::RENAME_EXCHANGE).unwrap();
        assert_eq!(world.read_file("/a/f").unwrap(), b"2");
        assert_eq!(world.read_file("/b/g").unwrap(), b"1");
        world.rename("/a", "/c", 0).unwrap();
        assert_eq!(world.read_file("/c/f").unwrap(), b"2");
        assert_eq!(code(world.unlink("/c")), libc::EISDIR);
        assert_eq!(code(world.rmdir("/c")), libc::ENOTEMPTY);
        assert_eq!(code(world.rmdir("/c/f")), libc::ENOTDIR);
        assert_eq!(code(world.rmdir("/")), libc::EBUSY);
        world.rmdir("/empty").unwrap();
        world.unlink("/b/g").unwrap();
        let mut byte = [0];
        assert_eq!(
            held.read_at(0, &mut byte).unwrap(),
            1,
            "unlinked inode stays readable"
        );
        assert_eq!(code(world.unlink("/b/g")), libc::ENOENT);
    }

    #[test]
    fn sparse_page_copies_preserve_boundaries_holes_and_snapshots() {
        for offset in [0, 1, 4095, 4096, 4097] {
            for length in [0, 1, 4095, 4096, 4097, 8193] {
                let world = World::new();
                let file = world.open("/file", libc::O_CREAT | libc::O_RDWR).unwrap();
                let offset = 8192 + offset;
                let original: Vec<u8> = (0..length).map(|i| (i % 251) as u8).collect();
                assert_eq!(file.write_at(offset, &original).unwrap(), length);
                let mut expected = vec![0; if length == 0 { 0 } else { offset as usize }];
                expected.extend_from_slice(&original);
                let snapshot = world
                    .state()
                    .file(file.id())
                    .unwrap()
                    .node
                    .borrow()
                    .pages
                    .clone();
                world
                    .inject(FaultTarget::Sync(SyncOp::Write), Fault::Short(length / 2))
                    .unwrap();
                assert_eq!(
                    file.write_at(offset, &vec![0xa5; length + 1]).unwrap(),
                    length / 2
                );
                if length != 0 {
                    expected[offset as usize..offset as usize + length / 2].fill(0xa5);
                }
                for (index, byte) in original.iter().enumerate() {
                    let pos = offset as usize + index;
                    assert_eq!(snapshot[&(pos as u64 / 4096)][pos % 4096], *byte);
                }
                let mut output = vec![0xcc; expected.len() + 8];
                assert_eq!(file.read_at(0, &mut output).unwrap(), expected.len());
                assert_eq!(&output[..expected.len()], expected);
                assert_eq!(&output[expected.len()..], &[0xcc; 8]);
                let mut partial = vec![0xcc; length + 8];
                world
                    .inject(FaultTarget::Sync(SyncOp::Read), Fault::Short(length / 2))
                    .unwrap();
                assert_eq!(file.read_at(offset, &mut partial).unwrap(), length / 2);
                assert!(partial[length / 2..].iter().all(|b| *b == 0xcc));
                world
                    .inject(FaultTarget::Sync(SyncOp::Write), Fault::Errno(libc::EIO))
                    .unwrap();
                assert_eq!(code(file.write_at(offset, b"bad")), libc::EIO);
                assert_eq!(world.read_file("/file").unwrap(), expected);
            }
        }
    }

    #[test]
    fn path_descriptors_stat_but_reject_io() {
        let world = World::new();
        world.write_file("/real/file", b"data").unwrap();
        world.symlink("real", "/parent").unwrap();
        world.symlink("file", "/real/link").unwrap();
        for (name, flags, kind) in [
            ("/parent/file", libc::O_PATH, libc::S_IFREG),
            (
                "/parent/link",
                libc::O_PATH | libc::O_NOFOLLOW,
                libc::S_IFLNK,
            ),
        ] {
            let fd = world.open(name, flags).unwrap();
            assert_eq!(u32::from(fd.stat().unwrap().stx_mode) & libc::S_IFMT, kind);
            assert_eq!(code(fd.read_at(0, &mut [0; 4])), libc::EBADF);
            assert_eq!(code(fd.write_at(0, b"bad")), libc::EBADF);
            assert_eq!(code(fd.sync()), libc::EBADF);
            assert_eq!(code(fd.set_len(0)), libc::EBADF);
            assert_eq!(code(fd.lock()), libc::EBADF);
        }
        let direct = world
            .open("/real/file", libc::O_RDWR | libc::O_DIRECT)
            .unwrap();
        assert_eq!(code(direct.write_at(1, &[0; 4096])), libc::EINVAL);
    }
}

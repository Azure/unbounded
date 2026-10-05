//! Linux Unix socket filesystem ownership. Application layout and access modes
//! are supplied by callers; this crate has no runtime or simulation dependency.

pub mod publication;

use std::cell::RefCell;
use std::ffi::CString;
use std::fs::{self, File, OpenOptions};
use std::io::{self, ErrorKind};
use std::os::fd::{AsRawFd, FromRawFd};
use std::os::unix::fs::{FileTypeExt, MetadataExt, OpenOptionsExt};
use std::os::unix::net::{SocketAddr, UnixListener, UnixStream};
use std::path::{Component, Path, PathBuf};
use std::rc::Rc;

fn rejected() -> io::Error {
    io::Error::other("endpoint ownership validation failed")
}

/// Descriptor-backed pathname. The caller must retain the file while using it.
pub fn file_path(file: &File) -> PathBuf {
    PathBuf::from(format!("/proc/self/fd/{}", file.as_raw_fd()))
}

/// Walk from the filesystem root, creating missing components with `mode`.
/// Every component is opened with O_NOFOLLOW. Relative paths are also rooted
/// at `/`, matching the original walk; parent/current components are rejected.
pub fn open_directory(path: &Path, mode: u32) -> io::Result<File> {
    let mut directory = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open("/")?;
    for component in path.components() {
        match component {
            Component::RootDir => {}
            Component::Normal(name) => {
                directory = child_directory(&directory, name.as_encoded_bytes(), mode)?;
            }
            _ => return Err(io::Error::from(ErrorKind::InvalidInput)),
        }
    }
    Ok(directory)
}

/// Open or create a child directory. `name` must be one normal path component.
pub fn child_directory(parent: &File, name: &[u8], mode: u32) -> io::Result<File> {
    let name = CString::new(name).map_err(|_| io::Error::from(ErrorKind::InvalidInput))?;
    // SAFETY: C strings are terminated; the parent descriptor remains owned.
    unsafe {
        let flags = libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC;
        let mut fd = libc::openat(parent.as_raw_fd(), name.as_ptr(), flags);
        if fd < 0 && io::Error::last_os_error().kind() == ErrorKind::NotFound {
            if libc::mkdirat(parent.as_raw_fd(), name.as_ptr(), mode) != 0
                && io::Error::last_os_error().kind() != ErrorKind::AlreadyExists
            {
                return Err(io::Error::last_os_error());
            }
            fd = libc::openat(parent.as_raw_fd(), name.as_ptr(), flags);
        }
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(File::from_raw_fd(fd))
    }
}

pub fn same_inode(first: &fs::Metadata, second: &fs::Metadata) -> bool {
    first.dev() == second.dev() && first.ino() == second.ino()
}

/// Remove only specified permission bits from an effective-user-owned directory.
/// Uses the pinned inode, never changes ancestors, and never broadens access.
pub fn restrict_directory(directory: &File, remove: u32) -> io::Result<()> {
    let before = directory.metadata()?;
    // SAFETY: geteuid has no preconditions.
    if !before.is_dir() || before.uid() != unsafe { libc::geteuid() } {
        return Err(rejected());
    }
    let mode = before.mode() & 0o7777 & !remove;
    if before.mode() & remove != 0 {
        // SAFETY: directory retains its descriptor throughout fchmod.
        if unsafe { libc::fchmod(directory.as_raw_fd(), mode) } != 0 {
            return Err(io::Error::last_os_error());
        }
    }
    let after = directory.metadata()?;
    if !after.is_dir()
        || after.uid() != before.uid()
        || after.gid() != before.gid()
        || !same_inode(&before, &after)
        || after.mode() & 0o7777 != mode
    {
        return Err(rejected());
    }
    Ok(())
}

/// Pin and verify a socket before operations through its descriptor pathname.
/// O_PATH and O_NOFOLLOW prevent a replacement symlink from redirecting chmod.
pub fn pin_socket(directory: &File, basename: &str, device: u64, inode: u64) -> io::Result<File> {
    let socket = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(file_path(directory).join(basename))?;
    let metadata = socket.metadata()?;
    if metadata.dev() != device || metadata.ino() != inode || !metadata.file_type().is_socket() {
        return Err(rejected());
    }
    Ok(socket)
}

/// Caller-defined namespace. Names must be single components. The witness prefix
/// and temporary predicate must describe a disjoint namespace from the lock and
/// canonical name. Keep this configuration stable across process restarts.
#[derive(Clone, Copy)]
pub struct Layout {
    pub lock: &'static str,
    pub canonical: &'static str,
    pub witness_prefix: &'static str,
    pub temporary_name: fn(&str) -> bool,
}

/// Persistent flock ownership. The lock inode is never removed, including on
/// clean shutdown. Hard-linked sockets are crash witnesses, not connect heuristics.
pub struct EndpointOwner {
    lock: File,
    directory: File,
    layout: Layout,
}

impl Drop for EndpointOwner {
    fn drop(&mut self) {
        // Explicit unlock avoids retaining ownership in a concurrent fork before
        // exec closes CLOEXEC descriptors. Socket owners must clean up first.
        // SAFETY: self retains the lock descriptor throughout flock.
        unsafe { libc::flock(self.lock.as_raw_fd(), libc::LOCK_UN) };
    }
}

impl EndpointOwner {
    /// Model an inherited open file description without exposing it in production.
    #[cfg(feature = "test-util")]
    pub fn clone_lock_for_test(&self) -> io::Result<File> {
        self.lock.try_clone()
    }

    /// Acquire an effective-user-owned, non-group/world-writable directory.
    /// Lock files must be private regular files with exactly one hard link.
    /// `lock_mode` is the creation mode (subject to the process umask).
    pub fn acquire(directory: &File, layout: Layout, lock_mode: u32) -> io::Result<Self> {
        let metadata = directory.metadata()?;
        // SAFETY: geteuid has no preconditions.
        if metadata.uid() != unsafe { libc::geteuid() } || metadata.mode() & 0o022 != 0 {
            return Err(rejected());
        }
        let lock = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(lock_mode)
            .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC | libc::O_NONBLOCK)
            .open(file_path(directory).join(layout.lock))?;
        let metadata = lock.metadata()?;
        if !metadata.is_file()
            || metadata.nlink() != 1
            || metadata.uid() != unsafe { libc::geteuid() }
            || metadata.mode() & 0o077 != 0
        {
            return Err(rejected());
        }
        // SAFETY: lock owns a valid descriptor. The kernel releases on process death.
        if unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
            return Err(io::Error::last_os_error());
        }
        let owner = Self {
            lock,
            directory: directory.try_clone()?,
            layout,
        };
        owner.validate(directory)?;
        owner.recover()?;
        Ok(owner)
    }

    /// Reject directory or lock-path replacement while retaining the original lock.
    pub fn validate(&self, directory: &File) -> io::Result<()> {
        let first = self.directory.metadata()?;
        let second = directory.metadata()?;
        let lock = self.lock.metadata()?;
        let current = fs::symlink_metadata(file_path(directory).join(self.layout.lock))?;
        if !same_inode(&first, &second)
            || !current.is_file()
            || !same_inode(&lock, &current)
            || current.nlink() != 1
        {
            return Err(rejected());
        }
        Ok(())
    }

    fn recover(&self) -> io::Result<()> {
        let directory = file_path(&self.directory);
        let mut witnesses = Vec::new();
        let mut temporary_paths = Vec::new();
        for entry in fs::read_dir(&directory)? {
            let entry = entry?;
            let name = entry.file_name();
            if name.to_str().is_some_and(self.layout.temporary_name) {
                temporary_paths.push(entry.path());
                continue;
            }
            let Some(name) = name
                .to_str()
                .filter(|name| name.starts_with(self.layout.witness_prefix))
            else {
                continue;
            };
            if !(self.layout.temporary_name)(&name[self.layout.witness_prefix.len()..]) {
                return Err(rejected());
            }
            let metadata = fs::symlink_metadata(entry.path())?;
            if !metadata.file_type().is_socket() {
                return Err(rejected());
            }
            witnesses.push((name.to_owned(), metadata));
        }
        let socket = directory.join(self.layout.canonical);
        let canonical = match fs::symlink_metadata(&socket) {
            Ok(metadata) => Some(metadata),
            Err(error) if error.kind() == ErrorKind::NotFound => None,
            Err(error) => return Err(error),
        };
        if canonical.as_ref().is_some_and(|canonical| {
            !canonical.file_type().is_socket()
                || !witnesses.iter().any(|(_, m)| same_inode(canonical, m))
        }) {
            return Err(rejected());
        }
        // Validate every witness before touching any path. Noncooperating live
        // listeners (including inherited FDs) remain protected without their lock.
        for (name, _) in &witnesses {
            refused(&directory.join(name))?;
        }
        for path in &temporary_paths {
            let current = fs::symlink_metadata(path)?;
            if !current.file_type().is_socket()
                || !witnesses.iter().any(|(_, m)| same_inode(&current, m))
            {
                return Err(rejected());
            }
        }
        if canonical.is_some() {
            fs::remove_file(socket)?;
        }
        for path in temporary_paths {
            fs::remove_file(path)?;
        }
        for (name, _) in witnesses {
            fs::remove_file(directory.join(name))?;
        }
        Ok(())
    }
}

fn refused(path: &Path) -> io::Result<()> {
    use std::os::unix::ffi::OsStrExt;
    let bytes = path.as_os_str().as_bytes();
    // SAFETY: zero is a valid initial representation for sockaddr_un.
    let mut address: libc::sockaddr_un = unsafe { std::mem::zeroed() };
    if bytes.len() >= address.sun_path.len() {
        return Err(rejected());
    }
    address.sun_family = libc::AF_UNIX as _;
    for (target, source) in address.sun_path.iter_mut().zip(bytes) {
        *target = *source as _;
    }
    // SAFETY: socket has no pointer arguments; File owns the returned descriptor.
    let fd = unsafe {
        libc::socket(
            libc::AF_UNIX,
            libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
            0,
        )
    };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    let socket = unsafe { File::from_raw_fd(fd) };
    // SAFETY: address is initialized and its full size is supplied.
    let result = unsafe {
        libc::connect(
            socket.as_raw_fd(),
            (&address as *const libc::sockaddr_un).cast(),
            std::mem::size_of_val(&address) as _,
        )
    };
    if result == -1 && io::Error::last_os_error().raw_os_error() == Some(libc::ECONNREFUSED) {
        Ok(())
    } else {
        Err(rejected())
    }
}

/// A real nonblocking listener and its inode-checked pathname cleanup. Publication
/// remains caller-owned: update `basename` only after a successful rename. Keep
/// the owner alive until cleanup completes, including failed bind staging.
pub struct BoundSocket {
    listener: UnixListener,
    directory: Rc<File>,
    device: u64,
    inode: u64,
    basename: Rc<RefCell<String>>,
    witness: String,
    _owner: Rc<EndpointOwner>,
}

impl BoundSocket {
    /// Bind the witness first so a crash at any later step leaves recovery proof.
    /// The owner must cover `directory`; names must follow its recovery layout.
    /// Access permissions are deliberately left to the caller after this returns.
    pub fn bind(
        directory: impl Into<Rc<File>>,
        owner: Rc<EndpointOwner>,
        basename: String,
        witness: String,
    ) -> io::Result<Self> {
        let directory = directory.into();
        let path = file_path(&directory).join(&basename);
        let witness_path = file_path(&directory).join(&witness);
        let listener = UnixListener::bind(&witness_path)?;
        let metadata = fs::symlink_metadata(&witness_path)?;
        let bound = Self {
            listener,
            directory,
            device: metadata.dev(),
            inode: metadata.ino(),
            basename: Rc::new(RefCell::new(basename)),
            witness,
            _owner: owner,
        };
        fs::hard_link(&witness_path, &path)?;
        bound.listener.set_nonblocking(true)?;
        Ok(bound)
    }

    pub fn accept(&self) -> io::Result<(UnixStream, SocketAddr)> {
        self.listener.accept()
    }

    pub fn identity(&self) -> (u64, u64) {
        (self.device, self.inode)
    }

    /// Shared with the worker-local publication transaction, not thread-safe.
    pub fn basename(&self) -> Rc<RefCell<String>> {
        self.basename.clone()
    }
}

impl AsRawFd for BoundSocket {
    fn as_raw_fd(&self) -> std::os::fd::RawFd {
        self.listener.as_raw_fd()
    }
}

impl Drop for BoundSocket {
    fn drop(&mut self) {
        let path = file_path(&self.directory).join(self.basename.borrow().as_str());
        if fs::symlink_metadata(&path).is_ok_and(|metadata| {
            metadata.file_type().is_socket()
                && metadata.dev() == self.device
                && metadata.ino() == self.inode
        }) && fs::remove_file(path).is_err()
        {
            // Keep recovery proof if pathname cleanup failed.
            return;
        }
        let path = file_path(&self.directory).join(&self.witness);
        if fs::symlink_metadata(&path).is_ok_and(|metadata| {
            metadata.file_type().is_socket()
                && metadata.dev() == self.device
                && metadata.ino() == self.inode
        }) {
            let _ = fs::remove_file(path);
        }
    }
}

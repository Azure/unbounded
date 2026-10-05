//! Linux Unix socket filesystem ownership. Application layout and access modes
//! are supplied by callers. The optional simulation feature uses the runtime's
//! simulated filesystem; production builds need no runtime dependency.
//! Directory mutation must be serialized by cooperating effective-UID owners.
//! Hostile processes with the same UID (or root) are outside this boundary.

#![cfg(target_os = "linux")]

mod directory;
pub mod publication;
#[cfg(feature = "simulation")]
pub mod simulation;
#[cfg(test)]
mod tests;

use directory::{Dir, chmod_pin, component};
use std::cell::RefCell;
use std::fmt;
use std::fs::{self, File, OpenOptions};
use std::io::{self, ErrorKind};
use std::os::fd::{AsFd, AsRawFd, BorrowedFd, FromRawFd, OwnedFd};
use std::os::unix::fs::{FileTypeExt, MetadataExt, OpenOptionsExt};
use std::os::unix::net::{SocketAddr, UnixListener, UnixStream};
use std::path::{Component, Path, PathBuf};
use std::rc::Rc;

/// Semantic failures carried inside `io::Error`. Syscall failures retain errno.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    InvalidName,
    InvalidLayout,
    InvalidMode,
    UnsafeDirectory,
    UnsafeLock,
    OwnershipChanged,
    UnrecognizedSocket,
    LiveSocket,
    PublicationCompleted,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::InvalidName => "endpoint name must be one normal component of at most 255 bytes",
            Self::InvalidLayout => {
                "endpoint layout namespaces overlap or do not recognize the name"
            }
            Self::InvalidMode => "socket mode must contain only permission bits",
            Self::UnsafeDirectory => {
                "endpoint directory must be effective-user-owned and not group/world writable"
            }
            Self::UnsafeLock => {
                "endpoint lock must be a private effective-user-owned single-link regular file"
            }
            Self::OwnershipChanged => "endpoint inode or ownership changed",
            Self::UnrecognizedSocket => "endpoint socket has no valid ownership witness",
            Self::LiveSocket => "endpoint witness still has a live or busy listener",
            Self::PublicationCompleted => "endpoint publication is already completed",
        })
    }
}

impl std::error::Error for Error {}

fn error(reason: Error) -> io::Error {
    let kind = match reason {
        Error::InvalidName | Error::InvalidLayout | Error::InvalidMode => ErrorKind::InvalidInput,
        Error::UnsafeDirectory | Error::UnsafeLock => ErrorKind::PermissionDenied,
        Error::LiveSocket => ErrorKind::AddrInUse,
        _ => ErrorKind::Other,
    };
    io::Error::new(kind, reason)
}

fn rejected() -> io::Error {
    error(Error::OwnershipChanged)
}

fn effective_uid() -> u32 {
    // SAFETY: geteuid has no preconditions.
    unsafe { libc::geteuid() }
}

/// Descriptor-backed pathname. The caller must retain the file while using it.
#[must_use]
pub fn file_path(file: &File) -> PathBuf {
    PathBuf::from(format!("/proc/self/fd/{}", file.as_raw_fd()))
}

/// Walk an absolute path from `/`, creating missing components with `mode`.
/// Reject relative paths and explicit parent/current components before mutation.
/// Every component is opened with O_NOFOLLOW.
///
/// # Errors
/// Returns `InvalidName` for invalid paths, or the underlying open/mkdir error
/// for inaccessible components, symlinks, non-directories, or filesystem failures.
pub fn open_directory(path: &Path, mode: u32) -> io::Result<File> {
    if !path.is_absolute()
        || path
            .as_os_str()
            .as_encoded_bytes()
            .split(|b| *b == b'/')
            .any(|c| c == b"." || c == b"..")
    {
        return Err(error(Error::InvalidName));
    }
    for c in path.components() {
        if let Component::Normal(name) = c {
            component(name.as_encoded_bytes())?;
        }
    }
    let mut directory = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open("/")?;
    for c in path.components() {
        if let Component::Normal(name) = c {
            directory = child_directory(&directory, name.as_encoded_bytes(), mode)?;
        }
    }
    Ok(directory)
}

/// Open or create a child directory. `name` must be one normal path component.
///
/// # Errors
/// Returns `InvalidName` for malformed names, or the underlying open/mkdir error.
pub fn child_directory(parent: &File, name: &[u8], mode: u32) -> io::Result<File> {
    let dir = Dir(parent);
    let flags = libc::O_RDONLY | libc::O_DIRECTORY;
    match dir.open(name, flags, 0) {
        Err(e) if e.kind() == ErrorKind::NotFound => {
            match dir.mkdir(name, mode) {
                Ok(()) => {}
                Err(e) if e.kind() == ErrorKind::AlreadyExists => {}
                Err(e) => return Err(e),
            }
            dir.open(name, flags, 0)
        }
        result => result,
    }
}

#[must_use]
pub fn same_inode(first: &fs::Metadata, second: &fs::Metadata) -> bool {
    first.dev() == second.dev() && first.ino() == second.ino()
}

/// Remove requested permission bits from a pinned effective-user-owned directory.
/// Non-permission bits in `remove` are ignored. Linux may additionally clear
/// setgid when the caller is not a member of the inode's group; that is narrowing.
///
/// # Errors
/// Returns `UnsafeDirectory` for foreign ownership or non-directories,
/// `OwnershipChanged` if verification fails, or the underlying stat/chmod error.
pub fn restrict_directory(directory: &File, remove: u32) -> io::Result<()> {
    let before = directory.metadata()?;
    if !before.is_dir() || before.uid() != effective_uid() {
        return Err(error(Error::UnsafeDirectory));
    }
    let remove = remove & 0o7777;
    let mode = before.mode() & 0o7777 & !remove;
    if before.mode() & remove != 0 {
        // SAFETY: directory retains its descriptor throughout fchmod.
        if unsafe { libc::fchmod(directory.as_raw_fd(), mode) } != 0 {
            return Err(io::Error::last_os_error());
        }
    }
    let after = directory.metadata()?;
    let actual = after.mode() & 0o7777;
    if !after.is_dir()
        || after.uid() != before.uid()
        || after.gid() != before.gid()
        || !same_inode(&before, &after)
        || (actual != mode && actual != mode & !0o2000)
    {
        return Err(rejected());
    }
    Ok(())
}

/// Pin and verify a socket without following a replacement symlink.
///
/// # Errors
/// Returns `InvalidName` for malformed names, `OwnershipChanged` for a wrong
/// inode, type, or owner, or the underlying open/stat error.
pub fn pin_socket(directory: &File, basename: &str, device: u64, inode: u64) -> io::Result<File> {
    let socket = Dir(directory).pin(basename)?;
    let metadata = socket.metadata()?;
    if metadata.dev() != device
        || metadata.ino() != inode
        || !metadata.file_type().is_socket()
        || metadata.uid() != effective_uid()
    {
        return Err(rejected());
    }
    Ok(socket)
}

/// Stable caller-defined namespace, checked both at construction and for each
/// actual temporary/witness name. Arbitrary predicates cannot be proven disjoint.
#[derive(Clone, Copy, Debug)]
pub struct Layout {
    lock: &'static str,
    canonical: &'static str,
    witness_prefix: &'static str,
    temporary_name: fn(&str) -> bool,
}

impl Layout {
    /// Construct a stable namespace, checking all statically known overlaps.
    ///
    /// # Errors
    /// Returns `InvalidName` for malformed components or `InvalidLayout` for
    /// namespace overlaps. Dynamic names are checked later at bind/recovery.
    pub fn new(
        lock: &'static str,
        canonical: &'static str,
        witness_prefix: &'static str,
        temporary_name: fn(&str) -> bool,
    ) -> io::Result<Self> {
        for name in [lock, canonical, witness_prefix] {
            component(name.as_bytes())?;
        }
        if lock == canonical
            || lock.starts_with(witness_prefix)
            || canonical.starts_with(witness_prefix)
            || temporary_name(lock)
            || temporary_name(canonical)
            || temporary_name(witness_prefix)
        {
            return Err(error(Error::InvalidLayout));
        }
        Ok(Self {
            lock,
            canonical,
            witness_prefix,
            temporary_name,
        })
    }

    #[must_use]
    pub fn lock(&self) -> &'static str {
        self.lock
    }
    #[must_use]
    pub fn canonical(&self) -> &'static str {
        self.canonical
    }
    #[must_use]
    pub fn witness_prefix(&self) -> &'static str {
        self.witness_prefix
    }

    fn witness(&self, temporary: &str) -> io::Result<String> {
        component(temporary.as_bytes())?;
        let witness = format!("{}{temporary}", self.witness_prefix);
        component(witness.as_bytes())?;
        if temporary == self.lock
            || temporary == self.canonical
            || temporary.starts_with(self.witness_prefix)
            || !(self.temporary_name)(temporary)
            || witness == self.lock
            || witness == self.canonical
            || (self.temporary_name)(&witness)
        {
            return Err(error(Error::InvalidLayout));
        }
        Ok(witness)
    }
}

/// Persistent flock ownership. The lock inode is never removed, including on
/// clean shutdown. Hard-linked sockets are crash witnesses, not connect heuristics.
#[derive(Debug)]
pub struct EndpointOwner {
    lock: File,
    directory: File,
    layout: Layout,
}

impl Drop for EndpointOwner {
    fn drop(&mut self) {
        // Explicit unlock is intentional: inherited or test-util cloned open file
        // descriptions must not retain the lock after all socket owners are gone.
        // SAFETY: self retains the lock descriptor throughout flock.
        unsafe { libc::flock(self.lock.as_raw_fd(), libc::LOCK_UN) };
    }
}

fn validate_directory(metadata: &fs::Metadata) -> io::Result<()> {
    if !metadata.is_dir() || metadata.uid() != effective_uid() || metadata.mode() & 0o022 != 0 {
        return Err(error(Error::UnsafeDirectory));
    }
    Ok(())
}

fn validate_lock(metadata: &fs::Metadata) -> io::Result<()> {
    if !metadata.is_file()
        || metadata.nlink() != 1
        || metadata.uid() != effective_uid()
        || metadata.mode() & 0o7177 != 0
    {
        return Err(error(Error::UnsafeLock));
    }
    Ok(())
}

impl EndpointOwner {
    /// Model an inherited open file description without exposing it in production.
    ///
    /// # Errors
    /// Returns the underlying descriptor duplication error.
    #[cfg(feature = "test-util")]
    pub fn clone_lock_for_test(&self) -> io::Result<File> {
        self.lock.try_clone()
    }

    /// Acquire a private persistent lock in an effective-user-owned directory.
    /// Creation uses 0600. Pinning before permission repair also recovers a lock
    /// left with mode 000 by a crash under a restrictive umask.
    /// Recovery retains a constant number of descriptors, independently of the
    /// number of stale witnesses. Probing a live listener may enqueue a connection
    /// which immediately closes; that side effect is inherent in this liveness check.
    ///
    /// # Errors
    /// Returns a typed validation error for unsafe directory/lock metadata,
    /// malformed recovery names, unrecognized sockets, or live listeners. Lock
    /// contention and filesystem/probe failures retain their underlying OS errors.
    /// Permission denial is never accepted as evidence of a dead listener.
    pub fn acquire(directory: &File, layout: Layout) -> io::Result<Self> {
        validate_directory(&directory.metadata()?)?;
        let dir = Dir(directory);
        let pinned = match dir.pin(layout.lock) {
            Ok(file) => file,
            Err(e) if e.kind() == ErrorKind::NotFound => {
                match dir.open(
                    layout.lock.as_bytes(),
                    libc::O_RDONLY | libc::O_CREAT | libc::O_EXCL,
                    0o600,
                ) {
                    Ok(file) => file,
                    Err(e) if e.kind() == ErrorKind::AlreadyExists => dir.pin(layout.lock)?,
                    Err(e) => return Err(e),
                }
            }
            Err(e) => return Err(e),
        };
        let metadata = pinned.metadata()?;
        validate_lock(&metadata)?;
        if metadata.mode() & 0o7777 != 0o600 {
            chmod_pin(&pinned, 0o600)?;
        }
        // This proc link resolves the already validated regular inode, not the
        // directory name. O_NOFOLLOW would reject the intentional proc link.
        let lock = OpenOptions::new()
            .read(true)
            .write(true)
            .custom_flags(libc::O_CLOEXEC | libc::O_NONBLOCK)
            .open(file_path(&pinned))?;
        if !same_inode(&metadata, &lock.metadata()?) {
            return Err(rejected());
        }
        // SAFETY: lock owns a valid descriptor. Kernel releases it on process death.
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

    /// Reject directory/lock replacement or weakened permissions while retaining
    /// the original lock. Ancestor renames do not invalidate the pinned directory.
    ///
    /// # Errors
    /// Returns `UnsafeDirectory`, `UnsafeLock`, or `OwnershipChanged` if validation
    /// fails, or the underlying metadata error when validation cannot complete.
    pub fn validate(&self, directory: &File) -> io::Result<()> {
        let first = self.directory.metadata()?;
        let second = directory.metadata()?;
        validate_directory(&first)?;
        validate_directory(&second)?;
        let lock = self.lock.metadata()?;
        let current = Dir(directory).metadata(self.layout.lock)?;
        validate_lock(&lock)?;
        validate_lock(&current)?;
        if !same_inode(&first, &second) || !same_inode(&lock, &current) {
            return Err(rejected());
        }
        Ok(())
    }

    fn recover(&self) -> io::Result<()> {
        let dir = Dir(&self.directory);
        let mut witnesses = Vec::new();
        let mut temporary = Vec::new();
        for entry in fs::read_dir(file_path(&self.directory))? {
            let entry = entry?;
            let name = entry.file_name();
            let Some(name) = name.to_str() else {
                if name
                    .as_encoded_bytes()
                    .starts_with(self.layout.witness_prefix.as_bytes())
                {
                    return Err(error(Error::InvalidName));
                }
                continue;
            };
            if let Some(suffix) = name.strip_prefix(self.layout.witness_prefix) {
                if self.layout.witness(suffix)? != name {
                    return Err(error(Error::InvalidLayout));
                }
                let metadata = dir.metadata(name)?;
                if !metadata.file_type().is_socket() || metadata.uid() != effective_uid() {
                    return Err(error(Error::UnrecognizedSocket));
                }
                witnesses.push((name.to_owned(), metadata));
            } else if (self.layout.temporary_name)(name) {
                self.layout.witness(name)?;
                temporary.push((name.to_owned(), dir.metadata(name)?));
            }
        }
        let canonical = match dir.metadata(self.layout.canonical) {
            Ok(metadata) => Some(metadata),
            Err(e) if e.kind() == ErrorKind::NotFound => None,
            Err(e) => return Err(e),
        };
        // Validate the entire namespace before either repairing permissions or
        // removing names. Foreign inodes never become recovery evidence.
        for metadata in canonical
            .iter()
            .chain(temporary.iter().map(|(_, metadata)| metadata))
        {
            if !metadata.file_type().is_socket()
                || !witnesses.iter().any(|(_, m)| same_inode(metadata, m))
            {
                return Err(error(Error::UnrecognizedSocket));
            }
        }
        for (name, expected) in &witnesses {
            // Keep descriptor use constant regardless of the number of stale
            // generations. Reopening must prove identity against the fully
            // validated snapshot before any permission repair or connection.
            let pinned = pin_socket(&self.directory, name, expected.dev(), expected.ino())?;
            let metadata = pinned.metadata()?;
            // bind followed by chmod has a crash window. O_PATH pins the socket
            // even if umask disabled owner write. Repair only that validated inode
            // before probing it; EACCES is never classified as a dead listener.
            if metadata.mode() & 0o200 == 0 {
                chmod_pin(&pinned, (metadata.mode() & 0o777) | 0o200)?;
            }
            refused(&file_path(&pinned))?;
        }
        self.validate(&self.directory)?;
        if let Some(metadata) = canonical {
            dir.unlink_owned(self.layout.canonical, &metadata)?;
        }
        for (name, metadata) in temporary {
            dir.unlink_owned(&name, &metadata)?;
        }
        for (name, metadata) in witnesses {
            dir.unlink_owned(&name, &metadata)?;
        }
        Ok(())
    }
}

fn refused(path: &Path) -> io::Result<()> {
    let bytes = path.as_os_str().as_encoded_bytes();
    // SAFETY: zero is a valid initial representation for sockaddr_un.
    let mut address: libc::sockaddr_un = unsafe { std::mem::zeroed() };
    if bytes.len() >= address.sun_path.len() {
        return Err(error(Error::InvalidName));
    }
    address.sun_family = libc::AF_UNIX as _;
    for (target, source) in address.sun_path.iter_mut().zip(bytes) {
        *target = *source as _;
    }
    // SAFETY: socket has no pointer arguments; OwnedFd takes ownership below.
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
    // SAFETY: socket returned a new descriptor, transferred exactly once.
    let socket = unsafe { OwnedFd::from_raw_fd(fd) };
    // SAFETY: address is initialized and its full size is supplied.
    let result = unsafe {
        libc::connect(
            socket.as_raw_fd(),
            (&address as *const libc::sockaddr_un).cast(),
            std::mem::size_of_val(&address) as _,
        )
    };
    if result == 0 {
        return Err(error(Error::LiveSocket));
    }
    let e = io::Error::last_os_error();
    match e.raw_os_error() {
        Some(libc::ECONNREFUSED) => Ok(()),
        Some(libc::EAGAIN | libc::EINPROGRESS | libc::EALREADY) => Err(error(Error::LiveSocket)),
        _ => Err(e),
    }
}

/// Nonblocking listener, pinned inode, and inode-checked pathname cleanup. Its
/// owner survives until cleanup finishes, including failed bind staging.
#[derive(Debug)]
pub struct BoundSocket {
    listener: UnixListener,
    pinned: File,
    device: u64,
    inode: u64,
    basename: RefCell<String>,
    witness: String,
    owner: Rc<EndpointOwner>,
}

impl BoundSocket {
    /// Derive the witness and directory from the owner. Bind the witness first so
    /// a crash before chmod or linking still leaves recoverable evidence.
    ///
    /// # Errors
    /// Returns name/layout/ownership validation errors or the underlying bind,
    /// pin, chmod, nonblocking, or link error. Failed staging preserves recovery
    /// evidence whenever cleanup cannot prove which inode to remove.
    pub fn bind(owner: Rc<EndpointOwner>, temporary: &str) -> io::Result<Self> {
        owner.validate(&owner.directory)?;
        let witness = owner.layout.witness(temporary)?;
        let basename = RefCell::new(temporary.to_owned());
        let listener = UnixListener::bind(file_path(&owner.directory).join(&witness))?;
        // If pin/stat fails, preserve the witness rather than guessing which inode
        // to unlink. The next owner can recover it after the listener closes.
        let pinned = Dir(&owner.directory).pin(&witness)?;
        let metadata = pinned.metadata()?;
        if !metadata.file_type().is_socket() || metadata.uid() != effective_uid() {
            return Err(rejected());
        }
        let bound = Self {
            listener,
            pinned,
            device: metadata.dev(),
            inode: metadata.ino(),
            basename,
            witness,
            owner,
        };
        bound.set_mode(0o600)?;
        bound.listener.set_nonblocking(true)?;
        Dir(&bound.owner.directory).link(&bound.witness, temporary)?;
        Ok(bound)
    }

    /// Accept one queued client without blocking.
    ///
    /// # Errors
    /// Returns `WouldBlock` when no client is queued, or the socket's accept error.
    pub fn accept(&self) -> io::Result<(UnixStream, SocketAddr)> {
        self.listener.accept()
    }
    #[must_use]
    pub fn identity(&self) -> (u64, u64) {
        (self.device, self.inode)
    }
    /// Read-only snapshot; publication owns cleanup-name mutation.
    #[must_use]
    pub fn basename(&self) -> String {
        self.basename.borrow().clone()
    }

    /// Set permission bits on the retained socket inode, never a replacement path.
    ///
    /// # Errors
    /// Returns `InvalidMode` for non-permission bits, owner validation errors,
    /// `OwnershipChanged` on mode verification failure, or the stat/chmod error.
    pub fn set_mode(&self, mode: u32) -> io::Result<()> {
        if mode & !0o777 != 0 {
            return Err(error(Error::InvalidMode));
        }
        self.owner.validate(&self.owner.directory)?;
        chmod_pin(&self.pinned, mode)?;
        if self.pinned.metadata()?.mode() & 0o777 != mode {
            return Err(rejected());
        }
        Ok(())
    }
}

impl AsFd for BoundSocket {
    fn as_fd(&self) -> BorrowedFd<'_> {
        self.listener.as_fd()
    }
}

impl AsRawFd for BoundSocket {
    fn as_raw_fd(&self) -> std::os::fd::RawFd {
        self.listener.as_raw_fd()
    }
}

impl Drop for BoundSocket {
    fn drop(&mut self) {
        let Ok(metadata) = self.pinned.metadata() else {
            return;
        };
        let dir = Dir(&self.owner.directory);
        if dir
            .unlink_owned(self.basename.get_mut(), &metadata)
            .is_err()
        {
            // Includes stat errors, not only failed unlink. Preserve recovery proof.
            return;
        }
        let _ = dir.unlink_owned(&self.witness, &metadata);
    }
}

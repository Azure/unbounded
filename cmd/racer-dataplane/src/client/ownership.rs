//! Persistent per-endpoint ownership. Never remove the lock inode, even on clean
//! shutdown. Socket hard links are crash witnesses, not connect-failure heuristics.
use super::*;

const LOCK: &str = ".racer-client.lock";
const WITNESS: &str = ".racer-owned-";

pub(super) struct EndpointOwner {
    lock: File,
    directory: File,
}

impl EndpointOwner {
    pub(super) fn acquire(directory: &File) -> Result<Self> {
        let metadata = directory.metadata().map_err(|_| Error::Io)?;
        // SAFETY: geteuid has no preconditions.
        if metadata.uid() != unsafe { libc::geteuid() } || metadata.mode() & 0o022 != 0 {
            return Err(Error::Io);
        }
        let lock = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC | libc::O_NONBLOCK)
            .open(anchored(directory).join(LOCK))
            .map_err(|_| Error::Io)?;
        let metadata = lock.metadata().map_err(|_| Error::Io)?;
        if !metadata.is_file()
            || metadata.nlink() != 1
            || metadata.uid() != unsafe { libc::geteuid() }
            || metadata.mode() & 0o077 != 0
        {
            return Err(Error::Io);
        }
        // SAFETY: lock owns a valid descriptor. Nonblocking flock covers the entire
        // listener lifetime and is released by the kernel on process death.
        if unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
            return Err(Error::Io);
        }
        let owner = Self {
            lock,
            directory: directory.try_clone().map_err(|_| Error::Io)?,
        };
        owner.validate(directory)?;
        owner.recover()?;
        Ok(owner)
    }

    pub(super) fn validate(&self, directory: &File) -> Result<()> {
        let first = self.directory.metadata().map_err(|_| Error::Io)?;
        let second = directory.metadata().map_err(|_| Error::Io)?;
        let lock = self.lock.metadata().map_err(|_| Error::Io)?;
        let current =
            fs::symlink_metadata(anchored(directory).join(LOCK)).map_err(|_| Error::Io)?;
        if first.dev() != second.dev()
            || first.ino() != second.ino()
            || !current.is_file()
            || lock.dev() != current.dev()
            || lock.ino() != current.ino()
            || current.nlink() != 1
        {
            return Err(Error::Io);
        }
        Ok(())
    }

    fn recover(&self) -> Result<()> {
        let directory = anchored(&self.directory);
        let mut witnesses = Vec::new();
        let mut temporary_paths = Vec::new();
        for entry in fs::read_dir(&directory).map_err(|_| Error::Io)? {
            let entry = entry.map_err(|_| Error::Io)?;
            let name = entry.file_name();
            if name.to_str().is_some_and(temporary_name) {
                temporary_paths.push(entry.path());
                continue;
            }
            let Some(name) = name.to_str().filter(|name| name.starts_with(WITNESS)) else {
                continue;
            };
            let temporary = &name[WITNESS.len()..];
            if !temporary_name(temporary) {
                return Err(Error::Io);
            }
            let metadata = fs::symlink_metadata(entry.path()).map_err(|_| Error::Io)?;
            if !metadata.file_type().is_socket() {
                return Err(Error::Io);
            }
            witnesses.push((name.to_owned(), metadata));
        }
        let socket = directory.join("socket");
        let canonical = match fs::symlink_metadata(&socket) {
            Ok(metadata) => Some(metadata),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => None,
            Err(_) => return Err(Error::Io),
        };
        if canonical.as_ref().is_some_and(|canonical| {
            !canonical.file_type().is_socket()
                || !witnesses.iter().any(|(_, m)| same_inode(canonical, m))
        }) {
            return Err(Error::Io);
        }
        // Validate every witness before touching any path. A noncooperating live
        // listener (including an inherited FD) remains protected without its lock.
        for (name, _) in &witnesses {
            refused(&directory.join(name))?;
        }
        for path in &temporary_paths {
            let current = fs::symlink_metadata(path).map_err(|_| Error::Io)?;
            if !current.file_type().is_socket()
                || !witnesses.iter().any(|(_, m)| same_inode(&current, m))
            {
                return Err(Error::Io);
            }
        }
        if canonical.is_some() {
            fs::remove_file(socket).map_err(|_| Error::Io)?;
        }
        for path in temporary_paths {
            fs::remove_file(path).map_err(|_| Error::Io)?;
        }
        for (name, _) in witnesses {
            fs::remove_file(directory.join(name)).map_err(|_| Error::Io)?;
        }
        Ok(())
    }
}

fn temporary_name(name: &str) -> bool {
    name.len() == 39
        && name.starts_with(".racer-")
        && name[7..].bytes().all(|b| b.is_ascii_hexdigit())
}

fn same_inode(first: &fs::Metadata, second: &fs::Metadata) -> bool {
    first.dev() == second.dev() && first.ino() == second.ino()
}

fn refused(path: &Path) -> Result<()> {
    use std::os::unix::ffi::OsStrExt;
    let bytes = path.as_os_str().as_bytes();
    // SAFETY: zero is a valid initial representation for sockaddr_un.
    let mut address: libc::sockaddr_un = unsafe { std::mem::zeroed() };
    if bytes.len() >= address.sun_path.len() {
        return Err(Error::Io);
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
        return Err(Error::Io);
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
    if result == -1 && std::io::Error::last_os_error().raw_os_error() == Some(libc::ECONNREFUSED) {
        Ok(())
    } else {
        Err(Error::Io)
    }
}

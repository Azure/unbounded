//! Worker-local listener preparation. Filesystem publication precedes control
//! publication; no new listener is accepted until the infallible memory swap.
use super::*;

struct Replacement {
    next: Rc<BoundListener>,
    previous: Option<Rc<BoundListener>>,
    temporary: String,
}

/// Owns prepared paths and sockets. Keep this on the listener's worker. Drop
/// rolls back; commit performs no filesystem or network operations.
pub struct PreparedListeners {
    definitions: Vec<CacheDefinition>,
    target: Rc<RefCell<BTreeMap<CacheId, Rc<BoundListener>>>>,
    next: BTreeMap<CacheId, Rc<BoundListener>>,
    replacements: Vec<Replacement>,
    preparing: Rc<Cell<bool>>,
    accepting: Rc<Cell<bool>>,
    cleanup: Rc<RefCell<VecDeque<Rc<BoundListener>>>>,
    committed: bool,
}

impl PreparedListeners {
    pub fn definitions(&self) -> &[CacheDefinition] {
        &self.definitions
    }

    /// Infallible activation for CacheLifecycle::stage's prepared-resource handoff.
    /// The application serializes prepare/commit with cache stop/drain operations.
    /// Worker polling performs deferred pathname cleanup after this returns.
    pub fn commit(mut self) {
        let mut target = self.target.borrow_mut();
        let mut cleanup = self.cleanup.borrow_mut();
        if self.accepting.get() {
            let previous = std::mem::replace(&mut *target, std::mem::take(&mut self.next));
            for (id, listener) in previous {
                if !target
                    .get(&id)
                    .is_some_and(|next| Rc::ptr_eq(next, &listener))
                {
                    listener.retired.set(true);
                    cleanup.push_back(listener);
                }
            }
        } else {
            // Shutdown may overtake preparation, but must never revive admission.
            for (_, listener) in std::mem::take(&mut self.next) {
                listener.retired.set(true);
                cleanup.push_back(listener);
            }
        }
        for replacement in &self.replacements {
            if let Some(previous) = &replacement.previous {
                cleanup.push_back(previous.clone());
            }
        }
        self.committed = true;
    }
}

impl crate::control::caches::CacheTransition for PreparedListeners {
    fn commit(self: Box<Self>) {
        (*self).commit();
    }
}

impl Drop for PreparedListeners {
    fn drop(&mut self) {
        if !self.committed {
            for replacement in self.replacements.iter().rev() {
                let next = &replacement.next;
                if let Some(previous) = &replacement.previous {
                    // Never exchange or unlink a foreign replacement inode. Under
                    // exclusive directory ownership these checks also make rollback
                    // independent of the caller's expired/canceled request scope.
                    if owns(next, "socket") && owns(previous, &replacement.temporary) {
                        if rename(
                            &next.directory,
                            "socket",
                            &replacement.temporary,
                            libc::RENAME_EXCHANGE,
                        )
                        .is_ok()
                        {
                            *next.basename.borrow_mut() = replacement.temporary.clone();
                            *previous.basename.borrow_mut() = "socket".into();
                        }
                    } else if absent(&next.directory, "socket")
                        && owns(previous, &replacement.temporary)
                    {
                        if rename(
                            &next.directory,
                            &replacement.temporary,
                            "socket",
                            libc::RENAME_NOREPLACE,
                        )
                        .is_ok()
                        {
                            *previous.basename.borrow_mut() = "socket".into();
                        }
                    }
                }
            }
        }
        self.preparing.set(false);
    }
}

pub(super) fn prepare<'a>(
    owner: &'a ClientListeners,
    definitions: &'a [CacheDefinition],
    scope: &'a RequestScope,
) -> Operation<'a, PreparedListeners> {
    Box::pin(async move {
        scope.check()?;
        crate::control::caches::validate_definitions(definitions)?;
        if !owner.accepting.get() {
            return Err(Error::Unavailable);
        }
        if owner.preparing.replace(true) {
            return Err(Error::Overloaded);
        }
        let mut prepared = PreparedListeners {
            definitions: definitions.to_vec(),
            target: owner.listeners.clone(),
            next: BTreeMap::new(),
            replacements: Vec::new(),
            preparing: owner.preparing.clone(),
            accepting: owner.accepting.clone(),
            cleanup: owner.cleanup.clone(),
            committed: false,
        };
        // Finish older deferred unlinks before reusing a removed cache name.
        owner.cleanup.borrow_mut().clear();
        let old = owner.listeners.borrow().clone();
        // Commit must not allocate while adding deferred owners to the queue.
        owner
            .cleanup
            .borrow_mut()
            .try_reserve(
                old.len()
                    .saturating_mul(2)
                    .saturating_add(definitions.len()),
            )
            .map_err(|_| Error::Overloaded)?;
        let mut pending = Vec::new();
        // Bind and chmod every changed socket before touching any active pathname.
        for definition in definitions {
            scope.check()?;
            if let Some(current) = old
                .get(&definition.id)
                .filter(|current| current.definition == *definition)
            {
                if !owns(current, "socket") {
                    return Err(Error::Io);
                }
                prepared.next.insert(definition.id.clone(), current.clone());
                continue;
            }
            let mut random = [0; 16];
            getrandom::getrandom(&mut random).map_err(|_| Error::Unavailable)?;
            let temporary = format!(".racer-{:032x}", u128::from_ne_bytes(random));
            let previous = old
                .values()
                .find(|current| current.definition.name == definition.name)
                .cloned();
            let next = Rc::new(bind(
                &owner.root,
                definition.clone(),
                &temporary,
                previous.as_deref(),
            )?);
            if let Some(previous) = &previous {
                if !owns(previous, "socket")
                    || !same_directory(&previous.directory, &next.directory)?
                {
                    return Err(Error::Io);
                }
            } else {
                recover_dead_socket(&next.directory)?;
            }
            prepared.next.insert(definition.id.clone(), next.clone());
            pending.push(Replacement {
                next,
                previous,
                temporary,
            });
            let mut yielded = false;
            std::future::poll_fn(|cx| {
                if yielded {
                    Poll::Ready(())
                } else {
                    yielded = true;
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            })
            .await;
        }
        for replacement in pending {
            scope.check()?;
            let next = &replacement.next;
            if let Some(previous) = &replacement.previous {
                if !owns(previous, "socket") || !owns(next, &replacement.temporary) {
                    return Err(Error::Io);
                }
                rename(
                    &next.directory,
                    &replacement.temporary,
                    "socket",
                    libc::RENAME_EXCHANGE,
                )?;
                *previous.basename.borrow_mut() = replacement.temporary.clone();
            } else {
                rename(
                    &next.directory,
                    &replacement.temporary,
                    "socket",
                    libc::RENAME_NOREPLACE,
                )?;
            }
            *next.basename.borrow_mut() = "socket".into();
            prepared.replacements.push(replacement);
        }
        scope.check()?;
        Ok(prepared)
    })
}

fn owns(listener: &BoundListener, basename: &str) -> bool {
    fs::symlink_metadata(anchored(&listener.directory).join(basename)).is_ok_and(|metadata| {
        metadata.file_type().is_socket()
            && metadata.dev() == listener.device
            && metadata.ino() == listener.inode
    })
}

// The caller holds the client directory's exclusive flock. A refused connection
// proves that a same-UID socket has no listener, including older processes that
// predate directory locking. Live, full-backlog, foreign, and non-socket paths
// fail closed. Pin and recheck the inode before unlinking; never follow symlinks.
fn recover_dead_socket(directory: &File) -> Result<()> {
    let path = anchored(directory).join("socket");
    let pinned = match OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(&path)
    {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(_) => return Err(Error::Io),
    };
    let metadata = pinned.metadata().map_err(|_| Error::Io)?;
    if !metadata.file_type().is_socket() || metadata.uid() != unsafe { libc::geteuid() } {
        return Err(Error::Io);
    }
    let raw = unsafe {
        libc::socket(
            libc::AF_UNIX,
            libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
            0,
        )
    };
    if raw < 0 {
        return Err(Error::Io);
    }
    let probe = unsafe { std::os::fd::OwnedFd::from_raw_fd(raw) };
    let mut address: libc::sockaddr_un = unsafe { std::mem::zeroed() };
    address.sun_family = libc::AF_UNIX as _;
    let bytes = path.as_os_str().as_encoded_bytes();
    if bytes.len() >= address.sun_path.len() {
        return Err(Error::Io);
    }
    for (to, from) in address.sun_path.iter_mut().zip(bytes) {
        *to = *from as _;
    }
    let connected = unsafe {
        libc::connect(
            probe.as_raw_fd(),
            (&address as *const libc::sockaddr_un).cast(),
            std::mem::size_of_val(&address) as _,
        )
    };
    if connected == 0 || std::io::Error::last_os_error().raw_os_error() != Some(libc::ECONNREFUSED)
    {
        return Err(Error::Io);
    }
    let current = fs::symlink_metadata(&path).map_err(|_| Error::Io)?;
    if !current.file_type().is_socket()
        || current.dev() != metadata.dev()
        || current.ino() != metadata.ino()
    {
        return Err(Error::Io);
    }
    fs::remove_file(path).map_err(|_| Error::Io)
}

fn absent(directory: &File, basename: &str) -> bool {
    fs::symlink_metadata(anchored(directory).join(basename))
        .is_err_and(|error| error.kind() == std::io::ErrorKind::NotFound)
}

fn same_directory(first: &File, second: &File) -> Result<bool> {
    let first = first.metadata().map_err(|_| Error::Io)?;
    let second = second.metadata().map_err(|_| Error::Io)?;
    Ok(first.dev() == second.dev() && first.ino() == second.ino())
}

fn rename(directory: &File, from: &str, to: &str, flags: u32) -> Result<()> {
    #[cfg(test)]
    if FAIL_RENAME_AFTER.with(|remaining| match remaining.get() {
        Some(0) => {
            remaining.set(None);
            true
        }
        Some(count) => {
            remaining.set(Some(count - 1));
            false
        }
        None => false,
    }) {
        return Err(Error::Io);
    }
    let from = CString::new(from).map_err(|_| Error::InvalidConfiguration)?;
    let to = CString::new(to).map_err(|_| Error::InvalidConfiguration)?;
    // SAFETY: the owned directory pins both names; strings are NUL terminated.
    if unsafe {
        libc::renameat2(
            directory.as_raw_fd(),
            from.as_ptr(),
            directory.as_raw_fd(),
            to.as_ptr(),
            flags,
        )
    } != 0
    {
        return Err(Error::Io);
    }
    Ok(())
}

#[cfg(test)]
thread_local! {
    pub(super) static FAIL_RENAME_AFTER: Cell<Option<usize>> = const { Cell::new(None) };
}

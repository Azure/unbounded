//! Node-local origin endpoint directory: /run/racer/<cache name>/origin.
//!
//! The privileged dataplane creates this directory and assigns it to the
//! unprivileged origin user so origins never need root or `mkdir` rights in
//! /run/racer. The origin binds its socket here (pkg/racersdk ServeOrigin).
use std::ffi::CString;
use std::fs::File;
use std::fs::Metadata;
use std::io;
use std::os::fd::AsRawFd;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::MetadataExt;

/// UID that Racer origins run as. Keep in sync with the nonroot user of
/// images/racer-object/Containerfile and images/racer-loadgen/Containerfile,
/// and with the origin directory checks in cmd/racer-object/main.go
/// (prepareOriginDirectory) and cmd/racer-loadgen/origin_uds.go.
pub const ORIGIN_UID: u32 = 65532;
/// GID that Racer origins run as. See [`ORIGIN_UID`].
pub const ORIGIN_GID: u32 = 65532;
/// Only the origin user may list, create, or remove origin endpoint entries.
pub const ORIGIN_MODE: u32 = 0o700;

/// Target owner of the origin directory.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Owner {
    pub uid: u32,
    pub gid: u32,
}

impl Owner {
    /// Production origin identity.
    pub const ORIGIN: Self = Self {
        uid: ORIGIN_UID,
        gid: ORIGIN_GID,
    };
}

/// What [`prepare`] changed. Exposed for tests and diagnostics.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Outcome {
    /// The directory owner or group was changed.
    pub chowned: bool,
    /// The directory mode was changed.
    pub chmodded: bool,
    /// Ownership could not be assigned because the dataplane lacks privilege.
    pub unprivileged: bool,
    /// Number of migrated top-level entries re-owned to the origin user.
    pub adopted: usize,
}

/// Create or repair `<cache>/origin` so it is a real directory owned by
/// `owner` with mode [`ORIGIN_MODE`].
///
/// The directory is opened with `O_DIRECTORY | O_NOFOLLOW`, so a symlink or
/// non-directory at that name is rejected, and every change is applied
/// through the pinned descriptor.
///
/// # Errors
/// Returns the open/mkdir/stat/chmod error, or a chown error other than an
/// unprivileged `EPERM`.
pub fn prepare(cache: &File, owner: Owner) -> io::Result<Outcome> {
    let directory = uds_endpoint::child_directory(cache, b"origin", ORIGIN_MODE)?;
    let before = directory.metadata()?;
    if !before.is_dir() {
        return Err(io::Error::from_raw_os_error(libc::ENOTDIR));
    }
    let mut outcome = Outcome::default();
    if before.uid() != owner.uid || before.gid() != owner.gid {
        // SAFETY: directory retains its descriptor throughout fchown.
        if unsafe { libc::fchown(directory.as_raw_fd(), owner.uid, owner.gid) } == 0 {
            outcome.chowned = true;
        } else {
            let error = io::Error::last_os_error();
            // The dataplane runs privileged in production. Without CAP_CHOWN
            // (for example local tests), keep the directory usable by an
            // origin running as the dataplane's own user instead of failing.
            if error.raw_os_error() == Some(libc::EPERM) && effective_uid() != 0 {
                eprintln!(
                    "racer-dataplane: stage=origin-directory warning=unprivileged \
                     uid={} gid={} error={error}",
                    owner.uid, owner.gid
                );
                outcome.unprivileged = true;
            } else {
                return Err(error);
            }
        }
    }
    let mode = directory.metadata()?.mode() & 0o7777;
    if mode != ORIGIN_MODE {
        // SAFETY: directory retains its descriptor throughout fchmod.
        if unsafe { libc::fchmod(directory.as_raw_fd(), ORIGIN_MODE) } != 0 {
            return Err(io::Error::last_os_error());
        }
        outcome.chmodded = true;
    }
    if outcome.chowned && trusted_previous_owner(&before, effective_uid()) {
        outcome.adopted = adopt_entries(&directory, owner);
    }
    Ok(outcome)
}

/// Whether entries left in a directory whose owner we just changed came from
/// a trusted writer and may be re-owned.
///
/// Before this directory was dataplane-managed, root origins created it as
/// root with mode 0755. Only the dataplane's own user (root) could create
/// entries in such a directory, so they are leftovers from a previous root
/// origin: `.racer-origin.lock`, the `.racer-origin.socket` witness, `socket`,
/// and `.racer-origin-*` staging directories. If any other user could have
/// written there, we do not touch the entries: re-owning a planted hard link
/// would hand an arbitrary file to the origin user.
fn trusted_previous_owner(before: &Metadata, euid: u32) -> bool {
    before.uid() == euid && before.mode() & 0o022 == 0
}

/// Re-own top-level entries not already owned by `owner`.
///
/// The SDK refuses a lock file it does not own (pkg/racersdk/origin.go,
/// safeOriginFile) and cannot probe or remove a root-only stale socket, so
/// root-origin leftovers would block a non-root origin forever. control.rs
/// requires that the dataplane never unlink an adapter-owned origin socket,
/// so leftovers are re-owned rather than removed; the SDK's own stale-socket
/// recovery then decides what to delete. Symlinks are never followed or
/// changed, and regular files with extra hard links are skipped. Only entries
/// whose owner differs from `owner` are changed. Failures are logged and
/// skipped so a single bad entry cannot block the client endpoint.
fn adopt_entries(directory: &File, owner: Owner) -> usize {
    let path = format!("/proc/self/fd/{}", directory.as_raw_fd());
    let entries = match std::fs::read_dir(&path) {
        Ok(entries) => entries,
        Err(error) => {
            eprintln!("racer-dataplane: stage=origin-directory-migrate error={error}");
            return 0;
        }
    };
    let mut adopted = 0;
    for entry in entries.flatten() {
        let name = entry.file_name();
        let Ok(name) = CString::new(name.as_bytes()) else {
            continue;
        };
        match adopt_entry(directory, &name, owner) {
            Ok(true) => adopted += 1,
            Ok(false) => {}
            Err(error) => eprintln!(
                "racer-dataplane: stage=origin-directory-migrate entry={:?} error={error}",
                name
            ),
        }
    }
    adopted
}

fn adopt_entry(directory: &File, name: &CString, owner: Owner) -> io::Result<bool> {
    // SAFETY: zeroed stat is a valid out-parameter for fstatat.
    let mut stat: libc::stat = unsafe { std::mem::zeroed() };
    // SAFETY: terminated name, live directory descriptor, valid out-pointer.
    if unsafe {
        libc::fstatat(
            directory.as_raw_fd(),
            name.as_ptr(),
            &mut stat,
            libc::AT_SYMLINK_NOFOLLOW,
        )
    } != 0
    {
        return Err(io::Error::last_os_error());
    }
    if !should_adopt(stat.st_mode, stat.st_nlink as u64, stat.st_uid, owner) {
        return Ok(false);
    }
    // SAFETY: terminated name and live directory descriptor; no symlink follow.
    if unsafe {
        libc::fchownat(
            directory.as_raw_fd(),
            name.as_ptr(),
            owner.uid,
            owner.gid,
            libc::AT_SYMLINK_NOFOLLOW,
        )
    } != 0
    {
        return Err(io::Error::last_os_error());
    }
    Ok(true)
}

fn should_adopt(mode: libc::mode_t, nlink: u64, uid: u32, owner: Owner) -> bool {
    if uid == owner.uid {
        return false;
    }
    match mode & libc::S_IFMT {
        libc::S_IFDIR | libc::S_IFSOCK => true,
        libc::S_IFREG => nlink == 1,
        _ => false,
    }
}

fn effective_uid() -> u32 {
    // SAFETY: geteuid has no preconditions.
    unsafe { libc::geteuid() }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::os::unix::fs::PermissionsExt;

    struct Scratch(std::path::PathBuf);
    impl Scratch {
        fn new() -> Self {
            // Stay inside the worktree and keep socket paths under SUN_LEN.
            static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
            let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join(format!(
                ".odt-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
            ));
            fs::create_dir(&path).unwrap();
            Self(path)
        }
        fn open(&self) -> File {
            File::open(&self.0).unwrap()
        }
    }
    impl Drop for Scratch {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn me() -> Owner {
        // SAFETY: no preconditions.
        Owner {
            uid: unsafe { libc::geteuid() },
            gid: unsafe { libc::getegid() },
        }
    }

    #[test]
    fn creates_private_directory() {
        let scratch = Scratch::new();
        let outcome = prepare(&scratch.open(), me()).unwrap();
        let meta = fs::symlink_metadata(scratch.0.join("origin")).unwrap();
        assert!(meta.is_dir());
        assert_eq!(meta.mode() & 0o7777, ORIGIN_MODE);
        assert_eq!((meta.uid(), meta.gid()), (me().uid, me().gid));
        assert!(!outcome.chowned && outcome.adopted == 0);
        // Idempotent: second call changes nothing.
        assert_eq!(prepare(&scratch.open(), me()).unwrap(), Outcome::default());
    }

    #[test]
    fn repairs_mode_and_keeps_entries() {
        for mode in [0o755, 0o777, 0o1777, 0o2700, 0o500] {
            let scratch = Scratch::new();
            let origin = scratch.0.join("origin");
            fs::create_dir(&origin).unwrap();
            fs::write(origin.join("socket"), b"keep").unwrap();
            fs::set_permissions(&origin, fs::Permissions::from_mode(mode)).unwrap();
            let outcome = prepare(&scratch.open(), me()).unwrap();
            assert!(outcome.chmodded);
            assert_eq!(fs::metadata(&origin).unwrap().mode() & 0o7777, ORIGIN_MODE);
            assert_eq!(fs::read(origin.join("socket")).unwrap(), b"keep");
        }
    }

    #[test]
    fn rejects_symlink_and_non_directory() {
        let scratch = Scratch::new();
        let target = scratch.0.join("target");
        fs::create_dir(&target).unwrap();
        fs::set_permissions(&target, fs::Permissions::from_mode(0o777)).unwrap();
        std::os::unix::fs::symlink(&target, scratch.0.join("origin")).unwrap();
        assert!(prepare(&scratch.open(), me()).is_err());
        assert_eq!(fs::metadata(&target).unwrap().mode() & 0o7777, 0o777);

        let scratch = Scratch::new();
        fs::write(scratch.0.join("origin"), b"file").unwrap();
        assert!(prepare(&scratch.open(), me()).is_err());
        assert_eq!(fs::read(scratch.0.join("origin")).unwrap(), b"file");
    }

    #[test]
    fn unprivileged_chown_is_tolerated() {
        if effective_uid() == 0 {
            return;
        }
        let scratch = Scratch::new();
        let outcome = prepare(&scratch.open(), Owner::ORIGIN).unwrap();
        assert!(outcome.unprivileged && !outcome.chowned && outcome.adopted == 0);
        let meta = fs::metadata(scratch.0.join("origin")).unwrap();
        assert_eq!(meta.mode() & 0o7777, ORIGIN_MODE);
        assert_eq!(meta.uid(), effective_uid());
    }

    #[test]
    fn trusted_previous_owner_requires_dataplane_owner_without_shared_write() {
        let scratch = Scratch::new();
        let euid = effective_uid();
        for (mode, trusted) in [(0o755, true), (0o700, true), (0o775, false), (0o757, false)] {
            fs::set_permissions(&scratch.0, fs::Permissions::from_mode(mode)).unwrap();
            let meta = fs::metadata(&scratch.0).unwrap();
            assert_eq!(trusted_previous_owner(&meta, euid), trusted, "{mode:o}");
            assert!(!trusted_previous_owner(&meta, euid.wrapping_add(1)));
        }
        fs::set_permissions(&scratch.0, fs::Permissions::from_mode(0o700)).unwrap();
    }

    #[test]
    fn should_adopt_only_safe_foreign_entries() {
        let owner = Owner { uid: 7, gid: 7 };
        assert!(should_adopt(libc::S_IFREG | 0o600, 1, 0, owner));
        assert!(should_adopt(libc::S_IFSOCK | 0o600, 2, 0, owner));
        assert!(should_adopt(libc::S_IFDIR | 0o700, 2, 0, owner));
        assert!(!should_adopt(libc::S_IFREG | 0o600, 2, 0, owner));
        assert!(!should_adopt(libc::S_IFLNK | 0o777, 1, 0, owner));
        assert!(!should_adopt(libc::S_IFIFO | 0o600, 1, 0, owner));
        assert!(!should_adopt(libc::S_IFREG | 0o600, 1, 7, owner));
    }

    #[test]
    fn migrates_root_origin_leftovers_when_root() {
        if effective_uid() != 0 {
            return;
        }
        let scratch = Scratch::new();
        let origin = scratch.0.join("origin");
        fs::create_dir(&origin).unwrap();
        fs::set_permissions(&origin, fs::Permissions::from_mode(0o755)).unwrap();
        fs::write(origin.join(".racer-origin.lock"), b"").unwrap();
        fs::create_dir(origin.join(".racer-origin-123")).unwrap();
        // Bind through the descriptor so the worktree path cannot exceed SUN_LEN.
        let pinned = File::open(&origin).unwrap();
        let socket = std::os::unix::net::UnixListener::bind(format!(
            "/proc/self/fd/{}/socket",
            pinned.as_raw_fd()
        ))
        .unwrap();
        fs::hard_link(origin.join("socket"), origin.join(".racer-origin.socket")).unwrap();
        fs::write(scratch.0.join("secret"), b"x").unwrap();
        fs::hard_link(scratch.0.join("secret"), origin.join("linked")).unwrap();
        std::os::unix::fs::symlink(scratch.0.join("secret"), origin.join("link")).unwrap();
        let owner = Owner {
            uid: 4242,
            gid: 4242,
        };
        let outcome = prepare(&scratch.open(), owner).unwrap();
        drop(socket);
        assert!(outcome.chowned && outcome.chmodded);
        // socket and its witness share an inode: the second name is already re-owned.
        assert_eq!(outcome.adopted, 3);
        for name in [
            ".racer-origin.lock",
            ".racer-origin-123",
            "socket",
            ".racer-origin.socket",
        ] {
            assert_eq!(
                fs::symlink_metadata(origin.join(name)).unwrap().uid(),
                4242,
                "{name}"
            );
        }
        assert_eq!(fs::metadata(scratch.0.join("secret")).unwrap().uid(), 0);
        assert_eq!(fs::symlink_metadata(origin.join("link")).unwrap().uid(), 0);
        // Second pass: already owned, no migration.
        assert_eq!(prepare(&scratch.open(), owner).unwrap(), Outcome::default());
    }

    #[test]
    fn untrusted_previous_owner_entries_are_not_adopted_when_root() {
        if effective_uid() != 0 {
            return;
        }
        let scratch = Scratch::new();
        let origin = scratch.0.join("origin");
        fs::create_dir(&origin).unwrap();
        fs::set_permissions(&origin, fs::Permissions::from_mode(0o777)).unwrap();
        fs::write(origin.join(".racer-origin.lock"), b"").unwrap();
        let owner = Owner {
            uid: 4242,
            gid: 4242,
        };
        let outcome = prepare(&scratch.open(), owner).unwrap();
        assert!(outcome.chowned && outcome.adopted == 0);
        assert_eq!(
            fs::metadata(origin.join(".racer-origin.lock"))
                .unwrap()
                .uid(),
            0
        );
    }
}

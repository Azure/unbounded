//! Node-local origin endpoint directory: /run/racer/<cache name>/origin.
//!
//! The privileged dataplane creates this directory and assigns it to the
//! unprivileged origin user so origins never need root or `mkdir` rights in
//! /run/racer. The origin binds its socket here (pkg/racersdk ServeOrigin).
use std::ffi::CStr;
use std::ffi::CString;
use std::fs::File;
use std::io;
use std::os::fd::AsRawFd;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::MetadataExt;

/// UID that Racer origins run as. Keep in sync with the nonroot user of
/// images/racer-object/Containerfile and images/racer-loadgen/Containerfile,
/// and with the origin directory checks in cmd/racer-object/main.go
/// (checkOriginDirectory) and cmd/racer-loadgen/origin_uds.go
/// (checkOriginDirectory).
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

/// Changes made by [`prepare`]. A seam so tests can check the order.
trait Ops {
    /// List the names in `directory`. Any read error fails the whole list.
    fn read_entries(&mut self, directory: &File) -> io::Result<Vec<CString>>;
    /// Re-own one entry of `directory` without following symlinks.
    fn chown_entry(&mut self, directory: &File, name: &CStr, owner: Owner) -> io::Result<()>;
    fn chmod(&mut self, directory: &File, mode: u32) -> io::Result<()>;
    fn chown(&mut self, directory: &File, owner: Owner) -> io::Result<()>;
}

struct System;

impl Ops for System {
    fn read_entries(&mut self, directory: &File) -> io::Result<Vec<CString>> {
        let path = format!("/proc/self/fd/{}", directory.as_raw_fd());
        let mut names = Vec::new();
        for entry in std::fs::read_dir(path)? {
            let name = entry?.file_name();
            // Directory entry names never contain NUL.
            names.push(CString::new(name.as_bytes()).map_err(io::Error::other)?);
        }
        Ok(names)
    }

    fn chown_entry(&mut self, directory: &File, name: &CStr, owner: Owner) -> io::Result<()> {
        // SAFETY: terminated name and live directory descriptor; no symlink follow.
        check(unsafe {
            libc::fchownat(
                directory.as_raw_fd(),
                name.as_ptr(),
                owner.uid,
                owner.gid,
                libc::AT_SYMLINK_NOFOLLOW,
            )
        })
    }

    fn chmod(&mut self, directory: &File, mode: u32) -> io::Result<()> {
        // SAFETY: directory retains its descriptor throughout fchmod.
        check(unsafe { libc::fchmod(directory.as_raw_fd(), mode) })
    }

    fn chown(&mut self, directory: &File, owner: Owner) -> io::Result<()> {
        // SAFETY: directory retains its descriptor throughout fchown.
        check(unsafe { libc::fchown(directory.as_raw_fd(), owner.uid, owner.gid) })
    }
}

fn check(result: libc::c_int) -> io::Result<()> {
    if result == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

/// Create or repair `<cache>/origin` so it is a real directory owned by
/// `owner` with mode [`ORIGIN_MODE`].
///
/// The directory is opened with `O_DIRECTORY | O_NOFOLLOW`, so a symlink or
/// non-directory at that name is rejected, and every change is applied
/// through the pinned descriptor.
///
/// Steps run in this order, so the origin user gets no control until the end:
/// 1. Re-own trusted leftover entries while the old owner still holds the
///    directory.
/// 2. Set the mode.
/// 3. Hand the directory to `owner`.
///
/// If step 1 fails, steps 2 and 3 do not run. If a run stops before step 3,
/// the directory is still trusted, so the next run repeats the migration.
///
/// # Errors
/// Returns the open/mkdir/stat/chmod error, a migration error, or a chown
/// error other than an unprivileged `EPERM`.
pub fn prepare(cache: &File, owner: Owner) -> io::Result<Outcome> {
    let euid = effective_uid();
    prepare_with(cache, owner, euid, euid == 0, &mut System)
}

/// `privileged` says whether the dataplane can assign any owner. It is
/// separate from `euid` so tests can run the root path without root.
fn prepare_with(
    cache: &File,
    owner: Owner,
    euid: u32,
    privileged: bool,
    ops: &mut impl Ops,
) -> io::Result<Outcome> {
    let directory = uds_endpoint::child_directory(cache, b"origin", ORIGIN_MODE)?;
    let before = directory.metadata()?;
    if !before.is_dir() {
        return Err(io::Error::from_raw_os_error(libc::ENOTDIR));
    }
    let mut outcome = Outcome::default();
    let reown = before.uid() != owner.uid || before.gid() != owner.gid;
    if reown && trusted_previous_owner(before.uid(), before.mode(), euid, privileged) {
        outcome.adopted = adopt_entries(&directory, owner, ops)?;
    }
    if before.mode() & 0o7777 != ORIGIN_MODE {
        ops.chmod(&directory, ORIGIN_MODE)?;
        outcome.chmodded = true;
    }
    if reown {
        match ops.chown(&directory, owner) {
            Ok(()) => outcome.chowned = true,
            // The dataplane runs as root in production. Without root (for
            // example local tests), keep the directory usable by an origin
            // running as the dataplane's own user instead of failing.
            Err(error) if error.raw_os_error() == Some(libc::EPERM) && !privileged => {
                eprintln!(
                    "racer-dataplane: stage=origin-directory warning=unprivileged \
                     uid={} gid={} error={error}",
                    owner.uid, owner.gid
                );
                outcome.unprivileged = true;
            }
            Err(error) => return Err(error),
        }
    }
    Ok(outcome)
}

/// Whether entries in a directory we are about to re-own came from a trusted
/// writer and may be re-owned too.
///
/// Before this directory was dataplane-managed, root origins created it as
/// root with mode 0755. Only root could create entries in such a directory,
/// so they are leftovers from a previous root origin: `.racer-origin.lock`,
/// the `.racer-origin.socket` witness, `socket`, and `.racer-origin-*`
/// staging directories. If any other user could have written there, we do
/// not touch the entries: re-owning a planted hard link would hand an
/// arbitrary file to the origin user.
///
/// The dataplane must also be privileged. Without privilege the directory
/// handoff fails, and entries must not change when the directory does not.
fn trusted_previous_owner(uid: u32, mode: u32, euid: u32, privileged: bool) -> bool {
    privileged && uid == euid && mode & 0o022 == 0
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
/// whose owner differs from `owner` are changed. These skipped entries are
/// not errors.
///
/// A list or ownership error is logged, the remaining entries are still
/// tried, and the first error is returned. The caller then keeps the
/// directory with its old owner, so the next run retries the migration.
/// Entries that were already moved are skipped on retry.
fn adopt_entries(directory: &File, owner: Owner, ops: &mut impl Ops) -> io::Result<usize> {
    let names = ops.read_entries(directory).inspect_err(|error| {
        eprintln!("racer-dataplane: stage=origin-directory-migrate error={error}");
    })?;
    let mut adopted = 0;
    let mut failure = None;
    for name in names {
        match adopt_entry(directory, &name, owner, ops) {
            Ok(true) => adopted += 1,
            Ok(false) => {}
            Err(error) => {
                eprintln!(
                    "racer-dataplane: stage=origin-directory-migrate entry={:?} error={error}",
                    name
                );
                failure.get_or_insert(error);
            }
        }
    }
    match failure {
        Some(error) => Err(error),
        None => Ok(adopted),
    }
}

fn adopt_entry(
    directory: &File,
    name: &CStr,
    owner: Owner,
    ops: &mut impl Ops,
) -> io::Result<bool> {
    // SAFETY: zeroed stat is a valid out-parameter for fstatat.
    let mut stat: libc::stat = unsafe { std::mem::zeroed() };
    // SAFETY: terminated name, live directory descriptor, valid out-pointer.
    check(unsafe {
        libc::fstatat(
            directory.as_raw_fd(),
            name.as_ptr(),
            &mut stat,
            libc::AT_SYMLINK_NOFOLLOW,
        )
    })?;
    if !should_adopt(stat.st_mode, stat.st_nlink as u64, stat.st_uid, owner) {
        return Ok(false);
    }
    ops.chown_entry(directory, name, owner)?;
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
            let (uid, mode_bits) = (meta.uid(), meta.mode());
            assert_eq!(
                trusted_previous_owner(uid, mode_bits, euid, true),
                trusted,
                "{mode:o}"
            );
            assert!(!trusted_previous_owner(
                uid,
                mode_bits,
                euid.wrapping_add(1),
                true
            ));
            assert!(!trusted_previous_owner(uid, mode_bits, euid, false));
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

    #[derive(Debug, PartialEq, Eq)]
    enum Event {
        /// Entry name, plus directory uid and mode when it was re-owned.
        Entry(String, u32, u32),
        Chmod(u32, u32),
        Chown(u32, u32),
    }

    /// Records each change and the directory state seen at that moment.
    /// Chmod is applied so later steps see the new mode. Chown calls are only
    /// recorded, so these tests do not need root.
    struct Recorder {
        events: Vec<Event>,
        chown_error: Option<i32>,
        /// Fail the next directory listing with this errno.
        list_error: Option<i32>,
        /// Fail the next re-own of this entry with this errno.
        entry_error: Option<(&'static str, i32)>,
    }

    impl Recorder {
        fn new() -> Self {
            Self {
                events: Vec::new(),
                chown_error: None,
                list_error: None,
                entry_error: None,
            }
        }
    }

    fn state(directory: &File) -> (u32, u32) {
        let meta = directory.metadata().unwrap();
        (meta.uid(), meta.mode() & 0o7777)
    }

    impl Ops for Recorder {
        fn read_entries(&mut self, directory: &File) -> io::Result<Vec<CString>> {
            if let Some(code) = self.list_error.take() {
                return Err(io::Error::from_raw_os_error(code));
            }
            // Sort so event order does not depend on the file system.
            let mut names = System.read_entries(directory)?;
            names.sort();
            Ok(names)
        }

        fn chown_entry(&mut self, directory: &File, name: &CStr, _: Owner) -> io::Result<()> {
            let (uid, mode) = state(directory);
            let name = name.to_string_lossy().into_owned();
            if let Some((failing, code)) = self.entry_error
                && failing == name
            {
                self.entry_error = None;
                return Err(io::Error::from_raw_os_error(code));
            }
            self.events.push(Event::Entry(name, uid, mode));
            Ok(())
        }

        fn chmod(&mut self, directory: &File, mode: u32) -> io::Result<()> {
            let (uid, before) = state(directory);
            self.events.push(Event::Chmod(uid, before));
            System.chmod(directory, mode)
        }

        fn chown(&mut self, directory: &File, _: Owner) -> io::Result<()> {
            let (uid, mode) = state(directory);
            self.events.push(Event::Chown(uid, mode));
            match self.chown_error {
                Some(code) => Err(io::Error::from_raw_os_error(code)),
                None => Ok(()),
            }
        }
    }

    fn legacy(scratch: &Scratch, mode: u32) -> std::path::PathBuf {
        let origin = scratch.0.join("origin");
        fs::create_dir(&origin).unwrap();
        fs::write(origin.join(".racer-origin.lock"), b"").unwrap();
        fs::set_permissions(&origin, fs::Permissions::from_mode(mode)).unwrap();
        origin
    }

    const TARGET: Owner = Owner {
        uid: 4242,
        gid: 4242,
    };

    #[test]
    fn migrates_entries_before_mode_and_hands_off_last() {
        let scratch = Scratch::new();
        legacy(&scratch, 0o755);
        let euid = effective_uid();
        let mut ops = Recorder::new();
        let outcome = prepare_with(&scratch.open(), TARGET, euid, true, &mut ops).unwrap();
        assert_eq!(
            ops.events,
            vec![
                Event::Entry(".racer-origin.lock".into(), euid, 0o755),
                Event::Chmod(euid, 0o755),
                Event::Chown(euid, ORIGIN_MODE),
            ]
        );
        assert_eq!(
            outcome,
            Outcome {
                chowned: true,
                chmodded: true,
                unprivileged: false,
                adopted: 1,
            }
        );
    }

    #[test]
    fn failed_handoff_is_retried_with_migration() {
        let scratch = Scratch::new();
        legacy(&scratch, 0o755);
        let euid = effective_uid();
        let mut ops = Recorder::new();
        ops.chown_error = Some(libc::EIO);
        assert!(prepare_with(&scratch.open(), TARGET, euid, true, &mut ops).is_err());
        // The directory is still old-owned and now 0700, so it stays trusted.
        let mut ops = Recorder::new();
        let outcome = prepare_with(&scratch.open(), TARGET, euid, true, &mut ops).unwrap();
        assert_eq!(
            ops.events,
            vec![
                Event::Entry(".racer-origin.lock".into(), euid, ORIGIN_MODE),
                Event::Chown(euid, ORIGIN_MODE),
            ]
        );
        assert!(outcome.chowned && !outcome.chmodded && outcome.adopted == 1);
    }

    #[test]
    fn untrusted_directory_is_narrowed_without_migration() {
        for mode in [0o775, 0o757, 0o777, 0o1777] {
            let scratch = Scratch::new();
            legacy(&scratch, mode);
            let euid = effective_uid();
            let mut ops = Recorder::new();
            let outcome = prepare_with(&scratch.open(), TARGET, euid, true, &mut ops).unwrap();
            assert_eq!(
                ops.events,
                vec![Event::Chmod(euid, mode), Event::Chown(euid, ORIGIN_MODE)],
                "{mode:o}"
            );
            assert_eq!(outcome.adopted, 0);
        }
    }

    #[test]
    fn foreign_previous_owner_is_not_migrated() {
        let scratch = Scratch::new();
        legacy(&scratch, 0o755);
        let euid = effective_uid().wrapping_add(1);
        let mut ops = Recorder::new();
        prepare_with(&scratch.open(), TARGET, euid, true, &mut ops).unwrap();
        assert!(!ops.events.iter().any(|e| matches!(e, Event::Entry(..))));
    }

    #[test]
    fn unprivileged_handoff_changes_no_entries() {
        let scratch = Scratch::new();
        let origin = legacy(&scratch, 0o755);
        let euid = effective_uid();
        let mut ops = Recorder::new();
        ops.chown_error = Some(libc::EPERM);
        let outcome = prepare_with(&scratch.open(), TARGET, euid, false, &mut ops).unwrap();
        assert_eq!(
            ops.events,
            vec![Event::Chmod(euid, 0o755), Event::Chown(euid, ORIGIN_MODE)]
        );
        assert!(outcome.unprivileged && !outcome.chowned && outcome.adopted == 0);
        assert_eq!(fs::metadata(origin).unwrap().mode() & 0o7777, ORIGIN_MODE);
    }

    #[test]
    fn privileged_eperm_is_fatal() {
        let scratch = Scratch::new();
        legacy(&scratch, 0o700);
        let mut ops = Recorder::new();
        ops.chown_error = Some(libc::EPERM);
        let euid = effective_uid();
        assert!(prepare_with(&scratch.open(), TARGET, euid, true, &mut ops).is_err());
    }

    #[test]
    fn owned_directory_is_left_alone() {
        let scratch = Scratch::new();
        legacy(&scratch, ORIGIN_MODE);
        let mut ops = Recorder::new();
        let outcome = prepare_with(&scratch.open(), me(), effective_uid(), true, &mut ops).unwrap();
        assert!(ops.events.is_empty());
        assert_eq!(outcome, Outcome::default());
    }

    #[test]
    fn failed_listing_blocks_handoff_until_retry() {
        let scratch = Scratch::new();
        let origin = legacy(&scratch, 0o755);
        let euid = effective_uid();
        let mut ops = Recorder::new();
        ops.list_error = Some(libc::EIO);
        let error = prepare_with(&scratch.open(), TARGET, euid, true, &mut ops).unwrap_err();
        assert_eq!(error.raw_os_error(), Some(libc::EIO));
        // No chmod or chown ran, so the directory keeps its old owner and mode.
        assert!(ops.events.is_empty());
        assert_eq!(fs::metadata(&origin).unwrap().mode() & 0o7777, 0o755);

        let outcome = prepare_with(&scratch.open(), TARGET, euid, true, &mut ops).unwrap();
        assert_eq!(
            ops.events,
            vec![
                Event::Entry(".racer-origin.lock".into(), euid, 0o755),
                Event::Chmod(euid, 0o755),
                Event::Chown(euid, ORIGIN_MODE),
            ]
        );
        assert!(outcome.chowned && outcome.chmodded && outcome.adopted == 1);
    }

    #[test]
    fn failed_entry_blocks_handoff_until_retry() {
        let scratch = Scratch::new();
        let origin = legacy(&scratch, 0o755);
        fs::write(origin.join("socket"), b"").unwrap();
        let euid = effective_uid();
        let mut ops = Recorder::new();
        ops.entry_error = Some((".racer-origin.lock", libc::EIO));
        let error = prepare_with(&scratch.open(), TARGET, euid, true, &mut ops).unwrap_err();
        assert_eq!(error.raw_os_error(), Some(libc::EIO));
        // Other entries are still tried, but nothing is published.
        assert_eq!(ops.events, vec![Event::Entry("socket".into(), euid, 0o755)]);
        assert_eq!(fs::metadata(&origin).unwrap().mode() & 0o7777, 0o755);

        let mut ops = Recorder::new();
        let outcome = prepare_with(&scratch.open(), TARGET, euid, true, &mut ops).unwrap();
        assert_eq!(ops.events.last(), Some(&Event::Chown(euid, ORIGIN_MODE)));
        assert!(
            ops.events
                .contains(&Event::Entry(".racer-origin.lock".into(), euid, 0o755))
        );
        assert!(outcome.chowned && outcome.chmodded);
    }

    #[test]
    fn skipped_entries_are_not_migration_errors() {
        let scratch = Scratch::new();
        let origin = legacy(&scratch, 0o755);
        fs::write(scratch.0.join("outside"), b"").unwrap();
        fs::hard_link(scratch.0.join("outside"), origin.join("linked")).unwrap();
        std::os::unix::fs::symlink("/nonexistent", origin.join("link")).unwrap();
        // SAFETY: valid NUL-terminated path; creates a FIFO inside the scratch tree.
        let fifo = CString::new(origin.join("fifo").as_os_str().as_bytes()).unwrap();
        assert_eq!(unsafe { libc::mkfifo(fifo.as_ptr(), 0o600) }, 0);
        let euid = effective_uid();
        let mut ops = Recorder::new();
        let outcome = prepare_with(&scratch.open(), TARGET, euid, true, &mut ops).unwrap();
        assert_eq!(
            ops.events,
            vec![
                Event::Entry(".racer-origin.lock".into(), euid, 0o755),
                Event::Chmod(euid, 0o755),
                Event::Chown(euid, ORIGIN_MODE),
            ]
        );
        assert_eq!(outcome.adopted, 1);
    }

    #[test]
    fn real_listing_failure_is_returned() {
        // Without read permission the real listing fails. Root bypasses this.
        if effective_uid() == 0 {
            return;
        }
        let scratch = Scratch::new();
        let path = scratch.0.join("unreadable");
        fs::create_dir(&path).unwrap();
        let directory = File::open(&path).unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o300)).unwrap();
        assert!(System.read_entries(&directory).is_err());
        assert!(adopt_entries(&directory, TARGET, &mut Recorder::new()).is_err());
        fs::set_permissions(&path, fs::Permissions::from_mode(0o700)).unwrap();
    }

    #[test]
    fn root_lock_failure_keeps_root_directory_until_retry_when_root() {
        if effective_uid() != 0 {
            return;
        }
        let scratch = Scratch::new();
        let origin = legacy(&scratch, 0o755);
        let lock = origin.join(".racer-origin.lock");
        fs::set_permissions(&lock, fs::Permissions::from_mode(0o600)).unwrap();
        struct FailOnce(bool);
        impl Ops for FailOnce {
            fn read_entries(&mut self, directory: &File) -> io::Result<Vec<CString>> {
                System.read_entries(directory)
            }
            fn chown_entry(&mut self, d: &File, name: &CStr, owner: Owner) -> io::Result<()> {
                if std::mem::replace(&mut self.0, false) {
                    return Err(io::Error::from_raw_os_error(libc::EIO));
                }
                System.chown_entry(d, name, owner)
            }
            fn chmod(&mut self, d: &File, mode: u32) -> io::Result<()> {
                System.chmod(d, mode)
            }
            fn chown(&mut self, d: &File, owner: Owner) -> io::Result<()> {
                System.chown(d, owner)
            }
        }
        let mut ops = FailOnce(true);
        assert!(prepare_with(&scratch.open(), TARGET, 0, true, &mut ops).is_err());
        let meta = fs::metadata(&origin).unwrap();
        assert_eq!((meta.uid(), meta.mode() & 0o7777), (0, 0o755));
        assert_eq!(fs::metadata(&lock).unwrap().uid(), 0);

        let outcome = prepare_with(&scratch.open(), TARGET, 0, true, &mut ops).unwrap();
        assert!(outcome.chowned && outcome.adopted == 1);
        let meta = fs::metadata(&origin).unwrap();
        assert_eq!((meta.uid(), meta.mode() & 0o7777), (4242, ORIGIN_MODE));
        assert_eq!(fs::metadata(&lock).unwrap().uid(), 4242);
    }
}

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Exclusive inode creation, atomic namespace publication, and lock lifetime.
use super::*;
impl Slab {
    /// Runtime-only fresh inode. The stable path lock owns this private name;
    /// startup removes it after a crash. No worker can access it until synced.
    pub(crate) fn prepare_replacement(
        path: &Path,
        plan: LayoutPlan,
        budget: CheckpointBudget,
    ) -> io::Result<Self> {
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .open(path)?;
        lock(&file)?;
        validate_page_cache_storage(&file)?;
        file.set_len(plan.capacity())?;
        let io = crate::slab_io::Io::current();
        let slab_file = SlabFile::Os(file.try_clone()?, io.clone());
        for id in 0..plan.shard_count() {
            let g = Geometry::new(plan.capacity(), plan.shard_count(), id)?;
            for slot in 0..2 {
                slab_file.write_all_at(&magic(g, slot as u64 + 1, 0, &[]).0, g.offset(slot))?;
            }
        }
        Layout::new(plan.capacity(), plan.shard_count(), plan.worker_count())?.write(&file)?;
        io.blocking(0, || file.sync_all().map(|()| ((), 0)))?;
        let mut slab = Self {
            file: Arc::new(SlabFile::Os(file, io)),
            pressure: Arc::default(),
            size: plan.capacity(),
            shards: vec![false; plan.shard_count()],
        };
        slab.set_checkpoint_budget(budget)?;
        Ok(slab)
    }
    pub(crate) fn retirement(&self) -> std::sync::Weak<SlabFile> {
        Arc::downgrade(&self.file)
    }
    /// Only for an empty, private replacement. Validate and duplicate file
    /// descriptors on the setup thread, never on a reactor.
    pub(crate) fn prepare_empty_shards(&mut self) -> io::Result<Vec<EmptyShard>> {
        (0..self.shard_count())
            .map(|id| {
                let shard = self.take_shard(ShardId::at(id))?;
                for slot in 0..2 {
                    let page = read_page(&shard.file, shard.geometry, slot)?;
                    if page.0 != magic(shard.geometry, slot as u64 + 1, 0, &[]).0 {
                        return Err(invalid("replacement is not a fresh empty slab"));
                    }
                }
                let descriptor = match &*shard.file {
                    SlabFile::Os(file, _) => file.try_clone()?,
                };
                Ok(EmptyShard { shard, descriptor })
            })
            .collect()
    }
    /// Discover an existing inode's recorded layout under its exclusive lock.
    /// Used after restart following a runtime replacement. File length and
    /// execution worker count must match; no inference or legacy adoption.
    pub fn open_existing_layout(path: impl AsRef<Path>, workers: usize) -> io::Result<Self> {
        let file = OpenOptions::new().read(true).write(true).open(path)?;
        lock(&file)?;
        validate_page_cache_storage(&file)?;
        let layout = Layout::read(&file)?;
        Layout::new(file.metadata()?.len(), layout.shards as usize, workers)?.validate(&file)?;
        Self::open_locked(file, layout.shards as usize)
    }
    pub fn size(&self) -> u64 {
        self.size
    }
    pub fn shard_count(&self) -> usize {
        self.shards.len()
    }
    /// Accounting also supports unchanged legacy layouts outside the planner's
    /// tested envelope. Opening does not apply a new memory admission budget.
    pub fn resources(&self) -> ResourceEstimate {
        ResourceEstimate::for_geometry(
            Geometry::new(self.size, self.shards.len(), 0).expect("validated slab"),
            self.shards.len(),
        )
    }
    /// Set before issuing any shard. Reuse one budget across all runtime storage
    /// generations; a slab defaults to its own two-checkpoint budget.
    pub fn set_checkpoint_budget(&mut self, budget: CheckpointBudget) -> io::Result<()> {
        if self.shards.iter().any(|issued| *issued) {
            return Err(invalid(
                "checkpoint budget must be set before issuing shards",
            ));
        }
        *self.pressure.1.lock().unwrap() = budget;
        Ok(())
    }
    /// Daemon startup gate. Pass the **actual** planned total I/O worker count,
    /// after CPU discovery and shard capping, not the per-NUMA environment value.
    /// Existing slabs must already carry the exact supported placement contract.
    /// New slabs publish that contract atomically with the empty checkpoints.
    /// `size` applies only to creation; existing slab length remains authoritative.
    pub fn open_or_create_layout(
        path: impl AsRef<Path>,
        size: u64,
        shards: usize,
        io_workers: usize,
    ) -> io::Result<Self> {
        // Reject invalid counts even when the path does not exist.
        let layout = Layout::new(size, shards, io_workers)?;
        match Self::open(path.as_ref(), shards) {
            Ok(slab) => {
                let file = match slab.file.as_ref() {
                    SlabFile::Os(file, _) => file,
                };
                Layout::new(slab.size, shards, io_workers)?.validate(file)?;
                Ok(slab)
            }
            Err(error) if error.kind() == io::ErrorKind::NotFound => Self::create_inner(
                path.as_ref(),
                size,
                shards,
                Some(layout),
                #[cfg(test)]
                |_| Ok(()),
            ),
            Err(error) => Err(error),
        }
    }
    /// Format privately, then atomically publish without replacing any existing
    /// name. An error after publication (including directory sync failure) can
    /// leave a valid slab; retry by opening it, never by reformatting it.
    pub fn create(path: impl AsRef<Path>, size: u64, shards: usize) -> io::Result<Self> {
        Self::create_inner(
            path.as_ref(),
            size,
            shards,
            None,
            #[cfg(test)]
            |_| Ok(()),
        )
    }
    pub(super) fn create_inner(
        path: &Path,
        size: u64,
        shards: usize,
        layout: Option<Layout>,
        #[cfg(test)] mut step: impl FnMut(CreateStep) -> io::Result<()>,
    ) -> io::Result<Self> {
        Geometry::new(size, shards, 0)?;
        let mut temporary = TemporarySlab::new(path)?;
        let file = &temporary.file;
        lock(file)?;
        validate_page_cache_storage(file)?;
        #[cfg(test)]
        step(CreateStep::Created)?;
        file.set_len(size)?;
        let io = crate::slab_io::Io::current();
        let slab_file = SlabFile::Os(file.try_clone()?, io.clone());
        #[cfg(test)]
        step(CreateStep::Sized)?;
        // Initial empty checkpoints contain no bitmap pages; recovery accepts
        // this special case only for an empty root.
        for shard in 0..shards {
            let g = Geometry::new(size, shards, shard)?;
            for slot in 0..2 {
                slab_file.write_all_at(&magic(g, slot as u64 + 1, 0, &[]).0, g.offset(slot))?;
                #[cfg(test)]
                step(CreateStep::Checkpoint(shard, slot))?;
            }
        }
        if let Some(layout) = layout {
            layout.write(file)?;
            #[cfg(test)]
            step(CreateStep::LayoutWritten)?;
        }
        #[cfg(test)]
        step(CreateStep::BeforeFileSync)?;
        io.blocking(0, || file.sync_all().map(|()| ((), 0)))?;
        #[cfg(test)]
        step(CreateStep::FileSynced)?;
        temporary.publish()?;
        #[cfg(test)]
        step(CreateStep::Published)?;
        temporary.directory.sync_all()?;
        #[cfg(test)]
        step(CreateStep::DirectorySynced)?;
        Ok(Self {
            // The clone shares the locked open file description: publication
            // and transfer to shard owners never introduce an unlocked window.
            file: Arc::new(SlabFile::Os(temporary.file.try_clone()?, io)),
            pressure: Arc::default(),
            size,
            shards: vec![false; shards],
        })
    }
    pub fn open(path: impl AsRef<Path>, shards: usize) -> io::Result<Self> {
        let file = OpenOptions::new().read(true).write(true).open(path)?;
        lock(&file)?;
        validate_page_cache_storage(&file)?;
        Self::open_locked(file, shards)
    }
    pub(super) fn open_locked(file: File, shards: usize) -> io::Result<Self> {
        let size = file.metadata()?.len();
        Geometry::new(size, shards, 0)?;
        let file = SlabFile::Os(file, crate::slab_io::Io::current());
        for shard in 0..shards {
            let g = Geometry::new(size, shards, shard)?;
            for slot in 0..2 {
                let mut page = uring::Page([0; PAGE_SIZE]);
                file.read_exact_at(&mut page.0, g.offset(slot))?;
                reject_version(&page)?;
            }
        }
        Ok(Self {
            file: Arc::new(file),
            pressure: Arc::default(),
            size,
            shards: vec![false; shards],
        })
    }
    pub fn take_shard(&mut self, id: ShardId) -> io::Result<SlabShard> {
        let g = Geometry::new(self.size, self.shards.len(), id.index())?;
        if std::mem::replace(&mut self.shards[id.index()], true) {
            return Err(invalid("shard capability already issued"));
        }
        Ok(SlabShard {
            file: self.file.clone(),
            pressure: self.pressure.clone(),
            geometry: g,
        })
    }
}

#[cfg(test)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum CreateStep {
    Created,
    Sized,
    Checkpoint(usize, usize),
    LayoutWritten,
    BeforeFileSync,
    FileSynced,
    Published,
    DirectorySynced,
}

// Anchor every namespace operation to one directory descriptor, even if an
// ancestor is renamed during setup. A killed creator may leave a private temp
// name, but it cannot leave an incomplete final slab or block the next create.
struct TemporarySlab {
    directory: File,
    name: CString,
    destination: CString,
    file: File,
    published: bool,
}
impl TemporarySlab {
    fn new(path: &Path) -> io::Result<Self> {
        let destination = path
            .file_name()
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "missing slab filename"))?;
        let destination = CString::new(destination.as_bytes())?;
        let parent = path.parent().filter(|p| !p.as_os_str().is_empty());
        let directory = OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_DIRECTORY | libc::O_CLOEXEC)
            .open(parent.unwrap_or_else(|| Path::new(".")))?;
        loop {
            let mut random = [0; 16];
            getrandom::getrandom(&mut random).map_err(|e| io::Error::other(e.to_string()))?;
            let name = CString::new(format!(
                ".racer-slab-{:032x}.tmp",
                u128::from_ne_bytes(random)
            ))
            .unwrap();
            // SAFETY: live directory fd, terminated name, and mode supplied for
            // O_CREAT. O_EXCL never follows or reuses a competing temporary file.
            let fd = unsafe {
                libc::openat(
                    directory.as_raw_fd(),
                    name.as_ptr(),
                    libc::O_RDWR | libc::O_CREAT | libc::O_EXCL | libc::O_CLOEXEC,
                    0o666 as libc::mode_t,
                )
            };
            if fd >= 0 {
                return Ok(Self {
                    directory,
                    name,
                    destination,
                    // SAFETY: openat returned a new owned descriptor.
                    file: unsafe { File::from_raw_fd(fd) },
                    published: false,
                });
            }
            let error = io::Error::last_os_error();
            if error.kind() != io::ErrorKind::AlreadyExists {
                return Err(error);
            }
        }
    }
    fn publish(&mut self) -> io::Result<()> {
        // SAFETY: both names are terminated and the directory descriptor is live.
        // Unlike rename(), NOREPLACE also protects dangling symlinks and racing
        // creators. Never fall back to an overwriting rename.
        if unsafe {
            libc::renameat2(
                self.directory.as_raw_fd(),
                self.name.as_ptr(),
                self.directory.as_raw_fd(),
                self.destination.as_ptr(),
                libc::RENAME_NOREPLACE,
            )
        } != 0
        {
            return Err(io::Error::last_os_error());
        }
        self.published = true;
        Ok(())
    }
}
impl Drop for TemporarySlab {
    fn drop(&mut self) {
        if !self.published {
            // SAFETY: live directory descriptor and terminated private name.
            // Best effort on error/unwind; never unlink the published slab.
            unsafe { libc::unlinkat(self.directory.as_raw_fd(), self.name.as_ptr(), 0) };
        }
    }
}
// Full-page hole punching must detach, rather than zero, pages retained by TCP.
// The supported deployment baseline is ext4 with 4 KiB base pages.
pub(super) fn validate_page_cache_storage(file: &File) -> io::Result<()> {
    let mut stat = std::mem::MaybeUninit::<libc::statfs>::uninit();
    // SAFETY: live descriptor and writable statfs storage.
    if unsafe { libc::fstatfs(file.as_raw_fd(), stat.as_mut_ptr()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    let stat = unsafe { stat.assume_init() };
    if stat.f_type != 0xef53 || unsafe { libc::sysconf(libc::_SC_PAGESIZE) } != PAGE_SIZE as i64 {
        return Err(io::Error::new(
            io::ErrorKind::Unsupported,
            "page-cache slabs require ext4 and 4 KiB base pages",
        ));
    }
    Ok(())
}
pub(super) fn lock(file: &File) -> io::Result<()> {
    // SAFETY: flock borrows a live descriptor and retains no userspace pointers.
    if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Ring-scoped fixed-file capabilities and descriptor validation.
use super::*;

/// Ring-scoped registered descriptor. The ring retains a slot until every handle
/// and request releases it. This capability cannot be constructed from an index.
/// Unused registrations are closed by [`Ring::progress`] or [`Ring::wait`].
#[derive(Clone)]
pub struct FixedFile(Rc<FixedRegistration>);
impl FixedFile {
    pub(super) fn slab_io(&self) -> crate::slab_io::Io {
        self.0._file.slab_io().clone()
    }
    #[cfg(test)]
    pub(super) fn file(&self) -> File {
        self.0._file.clone()
    }
}
pub(super) struct FixedRegistration {
    identity: Rc<Identity>,
    index: u32,
    _file: File,
    unused: Rc<RefCell<VecDeque<u32>>>,
    _inbound: Option<Rc<Inbound>>,
}
impl Drop for FixedFile {
    fn drop(&mut self) {
        // The table owns one reference; requests own ordinary FixedFile handles.
        if Rc::strong_count(&self.0) == 2 {
            self.0.unused.borrow_mut().push_back(self.0.index);
        }
    }
}
impl Core {
    pub(super) fn reclaim_fixed(&mut self, budget: usize) -> io::Result<()> {
        for _ in 0..budget {
            let Some(index) = self.unused_fixed.borrow().front().copied() else {
                break;
            };
            let index = index as usize;
            if self.fixed[index]
                .as_ref()
                .is_some_and(|r| Rc::strong_count(r) == 1)
            {
                let fd = -1i32;
                let update = abi::FilesUpdate {
                    offset: index as u32,
                    reserved: 0,
                    fds: &fd as *const _ as u64,
                };
                self.raw
                    .register(6, (&update as *const abi::FilesUpdate).cast(), 1)?;
                self.fixed[index] = None;
            }
            self.unused_fixed.borrow_mut().pop_front();
        }
        Ok(())
    }
}
impl Ring {
    pub fn register_file(&mut self, file: File) -> io::Result<FixedFile> {
        self.register_file_inner(file, None)
    }
    pub(super) fn register_file_inner(
        &mut self,
        file: File,
        inbound: Option<Rc<Inbound>>,
    ) -> io::Result<FixedFile> {
        if self.stopping {
            return Err(io::Error::new(io::ErrorKind::BrokenPipe, "ring stopping"));
        }
        let core = self.core.as_mut().unwrap();
        if inbound.is_some()
            && core
                .fixed
                .iter()
                .filter(|s| s.as_ref().is_none_or(|r| Rc::strong_count(r) == 1))
                .count()
                <= inbound_reserve(self.config.fixed_files)
        {
            return Err(io::ErrorKind::WouldBlock.into());
        }
        let index = core
            .fixed
            .iter()
            .position(|slot| slot.as_ref().is_none_or(|r| Rc::strong_count(r) == 1))
            .ok_or_else(|| io::Error::from(io::ErrorKind::WouldBlock))?;
        let fd = file.raw_id();
        let update = abi::FilesUpdate {
            offset: index as u32,
            reserved: 0,
            fds: &fd as *const _ as u64,
        };
        core.raw
            .register(6, (&update as *const abi::FilesUpdate).cast(), 1)?;
        let registration = Rc::new(FixedRegistration {
            identity: self.book.identity.clone(),
            index: index as u32,
            _file: file,
            unused: core.unused_fixed.clone(),
            _inbound: inbound,
        });
        core.fixed[index] = Some(registration.clone());
        Ok(FixedFile(registration))
    }
    pub(super) fn descriptor(&self, descriptor: &Descriptor, sqe: &mut abi::Sqe) -> io::Result<()> {
        match descriptor {
            Descriptor::File(file) => {
                sqe.fd = file.raw_id();
            }
            Descriptor::Fixed(file) => {
                if !Rc::ptr_eq(&file.0.identity, &self.book.identity) {
                    return Err(invalid("foreign fixed file"));
                }
                sqe.fd = file.0.index as i32;
                sqe.flags = 1; // IOSQE_FIXED_FILE
            }
        }
        Ok(())
    }
}

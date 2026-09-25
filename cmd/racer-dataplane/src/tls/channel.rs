// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-local TLS records, readiness leases, and connection retirement.

use crate::uring::{Control, File, Progress, Ring, Ticket, Work};
use std::{
    io,
    os::fd::{AsFd, AsRawFd},
    time::Instant,
};

/// Worker-local TLS record layer driven exclusively by io_uring readiness.
pub struct TlsChannel {
    admission: super::Admission,
    revision: u64,
    session: super::TlsSession,
    file: File,
    readiness: Option<Ticket<Control>>,
    read_ready: Option<Ticket<Control>>,
    write_ready: Option<Ticket<Control>>,
    slab_wait: Option<(crate::slab_io::Io, Instant)>,
    ready: bool,
}
impl TlsChannel {
    pub(crate) fn write_diagnostic(
        &self,
        ring: &Ring,
    ) -> (&'static str, Option<crate::failure_diagnostics::IoState>) {
        if let Some(t) = &self.write_ready {
            ("tls_socket_readiness", ring.diagnostic(t))
        } else if let Some(t) = &self.readiness {
            ("tls_handshake_readiness", ring.diagnostic(t))
        } else if self.slab_wait.is_some() {
            ("ktls_slab_rate", None)
        } else {
            ("tls_write_or_admission", None)
        }
    }
    pub(crate) fn ktls_tx(&self) -> bool {
        self.session.offload().tx
    }
    pub(crate) fn new(
        file: File,
        context: &super::TlsContext,
        expected: super::ExpectedPeer,
        server: bool,
    ) -> io::Result<Self> {
        let fd = file.as_fd().try_clone_to_owned()?;
        // SSL socket BIO must never block the worker, including during handshake.
        let flags = unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFL) };
        if flags < 0
            || unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_SETFL, flags | libc::O_NONBLOCK) } < 0
        {
            return Err(io::Error::last_os_error());
        }
        let session = if server {
            super::TlsSession::server(context, fd, expected)?
        } else {
            super::TlsSession::client(context, fd, expected)?
        };
        Ok(Self {
            admission: super::Admission::new(u64::MAX),
            revision: 0,
            session,
            file,
            readiness: None,
            read_ready: None,
            write_ready: None,
            slab_wait: None,
            ready: false,
        })
    }
    pub(crate) fn set_expiry(&mut self, expires_unix: u64) {
        self.admission.expires_unix = expires_unix;
    }
    pub(crate) fn revision(&self) -> u64 {
        self.revision
    }
    pub(crate) fn set_revision(&mut self, revision: u64) {
        TLS_CONNECTIONS.with(|counts| {
            let mut counts = counts.borrow_mut();
            if self.revision != 0 {
                *counts.entry(self.revision).or_default() -= 1;
                counts.retain(|_, count| *count != 0);
            }
            if revision != 0 {
                *counts.entry(revision).or_default() += 1;
            }
        });
        self.revision = revision;
    }
    pub(crate) fn old_connections(revision: u64) -> usize {
        TLS_CONNECTIONS.with(|counts| {
            counts
                .borrow()
                .iter()
                .filter(|(r, _)| **r != revision)
                .map(|(_, count)| *count)
                .sum()
        })
    }
    pub fn peer_identity(&self) -> Option<&super::PeerIdentity> {
        self.session.peer_identity()
    }
    fn wait<T>(
        &mut self,
        ring: &mut Ring,
        progress: super::TlsProgress<T>,
        deadline: Instant,
        operation: u8,
    ) -> io::Result<Progress<T>> {
        use super::TlsProgress;
        let direction = match progress {
            TlsProgress::Complete(value) => return Ok(Progress::Ready(value)),
            TlsProgress::Eof => return Err(io::ErrorKind::UnexpectedEof.into()),
            TlsProgress::WantRead => crate::uring::Readiness::Readable,
            TlsProgress::WantWrite => crate::uring::Readiness::Writable,
        };
        let runnable = match ring.poll_fd(self.file.clone().into(), direction) {
            Ok(ticket) => {
                *match operation {
                    1 => &mut self.read_ready,
                    2 => &mut self.write_ready,
                    _ => &mut self.readiness,
                } = Some(ticket.cancel_on_drop());
                false
            }
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => true,
            Err(e) => return Err(e),
        };
        Ok(Progress::Pending(Work {
            runnable,
            deadline: Some(deadline),
        }))
    }
    pub(crate) fn handshake(
        &mut self,
        ring: &mut Ring,
        deadline: Instant,
    ) -> io::Result<Progress<()>> {
        if !self.ready && self.expired() {
            return Err(io::ErrorKind::ConnectionAborted.into());
        }
        if crate::environment::now() >= deadline {
            return Err(io::ErrorKind::TimedOut.into());
        }
        if let Some(ticket) = &mut self.readiness {
            let Some(done) = ring.take_control(ticket)? else {
                return Ok(Progress::Pending(Work {
                    runnable: false,
                    deadline: Some(deadline),
                }));
            };
            done.result?;
            self.readiness = None;
        }
        if self.ready {
            return Ok(Progress::Ready(()));
        }
        let progress = self.session.handshake()?;
        let progress = self.wait(ring, progress, deadline, 0)?;
        if matches!(progress, Progress::Ready(())) {
            self.ready = true;
        }
        Ok(progress)
    }
    pub fn poll_read(
        &mut self,
        ring: &mut Ring,
        bytes: &mut [u8],
        deadline: Instant,
    ) -> io::Result<Progress<usize>> {
        if let Progress::Pending(work) = self.handshake(ring, deadline)? {
            return Ok(Progress::Pending(work));
        }
        if !Self::poll_ready(ring, &mut self.read_ready)? {
            return Ok(Progress::Pending(Work {
                runnable: false,
                deadline: Some(deadline),
            }));
        }
        let progress = self.session.read(bytes)?;
        self.wait(ring, progress, deadline, 1)
    }
    pub fn poll_write(
        &mut self,
        ring: &mut Ring,
        bytes: &[u8],
        deadline: Instant,
    ) -> io::Result<Progress<usize>> {
        if let Progress::Pending(work) = self.handshake(ring, deadline)? {
            return Ok(Progress::Pending(work));
        }
        if !Self::poll_ready(ring, &mut self.write_ready)? {
            return Ok(Progress::Pending(Work {
                runnable: false,
                deadline: Some(deadline),
            }));
        }
        let progress = self.session.write(&bytes[..bytes.len().min(64 * 1024)])?;
        self.wait(ring, progress, deadline, 2)
    }
    pub(crate) fn poll_sendfile(
        &mut self,
        ring: &mut Ring,
        file: &File,
        offset: u64,
        count: usize,
        deadline: Instant,
    ) -> io::Result<Progress<usize>> {
        if let Progress::Pending(work) = self.handshake(ring, deadline)? {
            return Ok(Progress::Pending(work));
        }
        if !Self::poll_ready(ring, &mut self.write_ready)? {
            return Ok(Progress::Pending(Work {
                runnable: false,
                deadline: Some(deadline),
            }));
        }
        let count = count.min(64 * 1024);
        let charge = match file.slab_io().reserve(count, crate::environment::now()) {
            Ok(charge) => charge,
            Err(ready) => {
                self.slab_wait
                    .get_or_insert_with(|| (file.slab_io().clone(), crate::environment::now()));
                return Ok(Progress::Pending(Work {
                    runnable: false,
                    deadline: Some(deadline.min(ready)),
                }));
            }
        };
        if let Some((io, since)) = self.slab_wait.take() {
            io.waited(since);
        }
        let progress = self.session.sendfile(file.as_fd(), offset, count);
        charge.finish(match &progress {
            Ok(super::TlsProgress::Complete(n)) => *n,
            _ => 0,
        });
        let progress = progress?;
        self.wait(ring, progress, deadline, 2)
    }
    fn poll_ready(ring: &mut Ring, ticket: &mut Option<Ticket<Control>>) -> io::Result<bool> {
        if let Some(pending) = ticket {
            let Some(done) = ring.take_control(pending)? else {
                return Ok(false);
            };
            done.result?;
            *ticket = None;
        }
        Ok(true)
    }
    pub(crate) fn expired(&self) -> bool {
        self.admission
            .expired(self.session.valid_until().unwrap_or(u64::MAX))
    }
    pub fn admits_new_request(&self) -> bool {
        self.ready && !self.expired()
    }
    pub(crate) fn record_fallback_sendfile_bytes(&mut self, bytes: usize) {
        self.session.record_fallback_sendfile_bytes(bytes);
    }
}
impl Drop for TlsChannel {
    fn drop(&mut self) {
        if let Some((io, since)) = self.slab_wait.take() {
            io.waited(since);
        }
        self.file.shutdown_socket();
        if self.revision != 0 {
            TLS_CONNECTIONS.with(|counts| {
                let mut counts = counts.borrow_mut();
                *counts.entry(self.revision).or_default() -= 1;
                counts.retain(|_, count| *count != 0);
            });
        }
    }
}
thread_local! { static TLS_CONNECTIONS: std::cell::RefCell<std::collections::BTreeMap<u64, usize>> = const { std::cell::RefCell::new(std::collections::BTreeMap::new()) }; }

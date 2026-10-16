//! Racer's [`Backend`]: an enum dispatching to the io_uring backend or, with
//! the `simulation` feature, to the `uring-runtime-sim` backend.
//!
//! The descriptor type is `Rc<Descriptor>`, the shape Racer code already
//! passes around. The clock reads [`crate::environment`], so simulated
//! environments are honored through the existing environment seam; without the
//! `simulation` feature [`RacerClock::now`] is exactly [`Instant::now`].

use super::descriptor::Descriptor;
use std::{
    net::Shutdown,
    rc::Rc,
    time::{Duration, Instant, SystemTime},
};
use uring::backend::{
    BackendConfig, CompletionEntry, Op, OpKind, PushError, PushHints, RemoteWaker,
};
use uring::{Backend, Clock, Entropy, Host, SocketAddress};

/// Racer's clock: [`crate::environment::now`] and
/// [`crate::environment::wall_now`]. Zero sized; without the `simulation`
/// feature it compiles to the system clock.
#[derive(Clone, Copy, Debug, Default)]
pub struct RacerClock;

impl Clock for RacerClock {
    #[inline]
    fn now(&self) -> Instant {
        crate::environment::now()
    }

    #[inline]
    fn wall_now(&self) -> SystemTime {
        crate::environment::wall_now()
    }
}

/// Racer's entropy: [`crate::environment::fill_random`].
#[derive(Clone, Copy, Debug, Default)]
pub struct RacerEntropy;

impl Entropy for RacerEntropy {
    fn fill(&self, buf: &mut [u8]) -> uring::Result<()> {
        crate::environment::fill_random(buf).map_err(Into::into)
    }
}

/// The backend a Racer reactor runs on.
// The ring is stored inline: boxing it would add an indirection to every
// backend call, and the enum is created once per reactor.
#[allow(clippy::large_enum_variant)]
pub enum RacerBackend {
    /// The real io_uring backend.
    Uring(uring::Uring),
    /// The deterministic simulator, over the world of a
    /// [`Simulation`](crate::simulation::Simulation). Sockets created through
    /// [`Host::socket`] carry the simulation's current endpoint label.
    #[cfg(feature = "simulation")]
    Sim(uring_runtime_sim::SimBackend, crate::simulation::Simulation),
    /// A scripted backend for adapter tests.
    #[cfg(test)]
    Scripted(super::scripted::Scripted),
}

impl std::fmt::Debug for RacerBackend {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Uring(_) => f.write_str("RacerBackend::Uring"),
            #[cfg(feature = "simulation")]
            Self::Sim(..) => f.write_str("RacerBackend::Sim"),
            #[cfg(test)]
            Self::Scripted(_) => f.write_str("RacerBackend::Scripted"),
        }
    }
}

impl RacerBackend {
    /// Select the backend for the current environment, as the old reactor did
    /// at construction.
    pub fn select() -> Self {
        #[cfg(feature = "simulation")]
        if let Some(simulation) = crate::simulation::Simulation::current() {
            return Self::simulated(&simulation, &crate::environment::Environment::current());
        }
        Self::Uring(uring::Uring::new())
    }

    /// A simulated backend over `simulation`'s world. Its clock is the
    /// environment's simulated clock when there is one (so kernel-style
    /// timeouts follow the same time as [`RacerClock`]), else the
    /// simulation's fallback clock. The backend's own entropy is a stream the
    /// reactor never reads ([`RacerEntropy`] is used instead).
    #[cfg(feature = "simulation")]
    pub fn simulated(
        simulation: &crate::simulation::Simulation,
        environment: &crate::environment::Environment,
    ) -> Self {
        let clock = environment
            .sim_clock()
            .unwrap_or_else(|| simulation.clock())
            .clone();
        let entropy = uring_runtime_sim::SimEntropy::new(
            uring_runtime_sim::DEFAULT_ENTROPY_DOMAIN,
            clock.seed(),
            u64::MAX,
        );
        let mut backend = uring_runtime_sim::SimBackend::new(simulation.world(), clock, entropy);
        backend.set_endpoint(simulation.endpoint());
        Self::Sim(backend, simulation.clone())
    }

    /// Whether this is the simulated backend.
    pub fn is_simulated(&self) -> bool {
        #[cfg(feature = "simulation")]
        if let Self::Sim(..) = self {
            return true;
        }
        false
    }
}

fn unsupported() -> PushError {
    PushError::Failed(
        uring::Error::new(uring::ErrorKind::InvalidInput)
            .with_context("descriptor does not belong to this backend"),
    )
}

fn host(fd: &Rc<Descriptor>) -> Result<&uring::Fd, PushError> {
    fd.host().ok_or_else(unsupported)
}

fn host_err(fd: &Rc<Descriptor>) -> uring::Result<&uring::Fd> {
    host(fd).map_err(uring::Error::from)
}

#[cfg(feature = "simulation")]
fn sim(fd: &Rc<Descriptor>) -> Result<&uring_runtime_sim::SimFd, PushError> {
    fd.as_sim().ok_or_else(unsupported)
}

#[cfg(feature = "simulation")]
fn sim_err(fd: &Rc<Descriptor>) -> uring::Result<&uring_runtime_sim::SimFd> {
    sim(fd).map_err(uring::Error::from)
}

fn wrap<F: Into<Descriptor>>(fd: F) -> Rc<Descriptor> {
    Rc::new(fd.into())
}

/// Translate an operation on Racer descriptors into one on library
/// descriptors, borrowing each through `map`.
fn translate<'a, F>(
    op: &Op<'a, Rc<Descriptor>>,
    map: impl Fn(&'a Rc<Descriptor>) -> Result<&'a F, PushError>,
) -> Result<Op<'a, F>, PushError> {
    let host = &map;
    let host_dir = |fd: Option<&'a Rc<Descriptor>>| fd.map(&map).transpose();
    Ok(match *op {
        Op::Nop => Op::Nop,
        Op::Read {
            fd,
            offset,
            buf,
            len,
        } => Op::Read {
            fd: host(fd)?,
            offset,
            buf,
            len,
        },
        Op::Write {
            fd,
            offset,
            buf,
            len,
        } => Op::Write {
            fd: host(fd)?,
            offset,
            buf,
            len,
        },
        Op::Recv {
            fd,
            buf,
            len,
            flags,
        } => Op::Recv {
            fd: host(fd)?,
            buf,
            len,
            flags,
        },
        Op::Send {
            fd,
            buf,
            len,
            flags,
        } => Op::Send {
            fd: host(fd)?,
            buf,
            len,
            flags,
        },
        Op::PollAdd { fd, events } => Op::PollAdd {
            fd: host(fd)?,
            events,
        },
        Op::Accept { fd, peer, flags } => Op::Accept {
            fd: host(fd)?,
            peer,
            flags,
        },
        Op::Connect { fd, address } => Op::Connect {
            fd: host(fd)?,
            address,
        },
        Op::Timeout { deadline, timespec } => Op::Timeout { deadline, timespec },
        Op::OpenAt2 { dir, path, how } => Op::OpenAt2 {
            dir: host_dir(dir)?,
            path,
            how,
        },
        Op::Statx {
            dir,
            path,
            flags,
            mask,
            out,
        } => Op::Statx {
            dir: host_dir(dir)?,
            path,
            flags,
            mask,
            out,
        },
        Op::Fsync { fd, datasync } => Op::Fsync {
            fd: host(fd)?,
            datasync,
        },
        Op::RenameAt {
            old_dir,
            old_path,
            new_dir,
            new_path,
            flags,
        } => Op::RenameAt {
            old_dir: host_dir(old_dir)?,
            old_path,
            new_dir: host_dir(new_dir)?,
            new_path,
            flags,
        },
        Op::UnlinkAt { dir, path, flags } => Op::UnlinkAt {
            dir: host_dir(dir)?,
            path,
            flags,
        },
        Op::MkDirAt { dir, path, mode } => Op::MkDirAt {
            dir: host_dir(dir)?,
            path,
            mode,
        },
        _ => return Err(PushError::Unsupported),
    })
}

impl Backend for RacerBackend {
    type Fd = Rc<Descriptor>;
    type Clock = RacerClock;
    type Entropy = RacerEntropy;

    fn clock(&self) -> &RacerClock {
        &RacerClock
    }

    fn entropy(&self) -> &RacerEntropy {
        &RacerEntropy
    }

    fn init(&mut self, config: &BackendConfig) -> uring::Result<()> {
        match self {
            Self::Uring(backend) => backend.init(config),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.init(config),
            #[cfg(test)]
            Self::Scripted(backend) => backend.init(config),
        }
    }

    unsafe fn push(&mut self, user_data: u64, op: &Op<'_, Self::Fd>) -> Result<(), PushError> {
        // SAFETY: forwarded from our caller.
        unsafe { self.push_with(user_data, op, PushHints::new()) }
    }

    unsafe fn push_with(
        &mut self,
        user_data: u64,
        op: &Op<'_, Self::Fd>,
        hints: PushHints,
    ) -> Result<(), PushError> {
        match self {
            // SAFETY: the translated op points at the same memory (and borrows
            // the library descriptors owned by the same `Rc<Descriptor>`s), so
            // our caller's guarantees carry over.
            Self::Uring(backend) => unsafe {
                backend.push_with(user_data, &translate(op, host)?, hints)
            },
            // SAFETY: as above, for simulated descriptors.
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => unsafe {
                backend.push_with(user_data, &translate(op, sim)?, hints)
            },
            // SAFETY: forwarded from our caller.
            #[cfg(test)]
            Self::Scripted(backend) => unsafe { backend.push_with(user_data, op, hints) },
        }
    }

    fn cancel(&mut self, user_data: u64, target: u64) -> Result<(), PushError> {
        match self {
            Self::Uring(backend) => backend.cancel(user_data, target),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.cancel(user_data, target),
            #[cfg(test)]
            Self::Scripted(backend) => backend.cancel(user_data, target),
        }
    }

    unsafe fn adopt_fd(&mut self, raw: i32) -> Self::Fd {
        match self {
            // SAFETY: forwarded from our caller.
            Self::Uring(backend) => wrap(unsafe { backend.adopt_fd(raw) }),
            #[cfg(feature = "simulation")]
            // SAFETY: forwarded from our caller.
            Self::Sim(backend, _) => wrap(unsafe { backend.adopt_fd(raw) }),
            // SAFETY: forwarded from our caller.
            #[cfg(test)]
            Self::Scripted(backend) => unsafe { backend.adopt_fd(raw) },
        }
    }

    fn submit(&mut self) -> uring::Result<usize> {
        match self {
            Self::Uring(backend) => backend.submit(),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.submit(),
            #[cfg(test)]
            Self::Scripted(backend) => backend.submit(),
        }
    }

    fn pop(&mut self) -> Option<CompletionEntry> {
        match self {
            Self::Uring(backend) => backend.pop(),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.pop(),
            #[cfg(test)]
            Self::Scripted(backend) => backend.pop(),
        }
    }

    fn wait(&mut self, timeout: Duration) -> uring::Result<()> {
        match self {
            Self::Uring(backend) => backend.wait(timeout),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.wait(timeout),
            #[cfg(test)]
            Self::Scripted(backend) => backend.wait(timeout),
        }
    }

    fn remote_waker(&self) -> uring::Result<RemoteWaker> {
        match self {
            Self::Uring(backend) => backend.remote_waker(),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.remote_waker(),
            #[cfg(test)]
            Self::Scripted(backend) => backend.remote_waker(),
        }
    }

    fn supports(&self, kind: OpKind) -> bool {
        match self {
            Self::Uring(backend) => backend.supports(kind),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.supports(kind),
            #[cfg(test)]
            Self::Scripted(backend) => backend.supports(kind),
        }
    }
}

impl Host for RacerBackend {
    fn socket(&self, domain: i32, ty: i32, protocol: i32) -> uring::Result<Self::Fd> {
        match self {
            Self::Uring(backend) => backend.socket(domain, ty, protocol).map(wrap),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, simulation) => {
                let endpoint = simulation.endpoint();
                backend
                    .world()
                    .labeled_socket(domain, ty, endpoint.as_ref().or(backend.endpoint()))
                    .map(wrap)
            }
            #[cfg(test)]
            Self::Scripted(_) => Err(uring::ErrorKind::Unsupported.into()),
        }
    }

    fn bind(&self, fd: &Self::Fd, address: &SocketAddress) -> uring::Result<()> {
        match self {
            Self::Uring(backend) => backend.bind(host_err(fd)?, address),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.bind(sim_err(fd)?, address),
            #[cfg(test)]
            Self::Scripted(_) => Err(uring::ErrorKind::Unsupported.into()),
        }
    }

    fn listen(&self, fd: &Self::Fd, backlog: i32) -> uring::Result<()> {
        match self {
            Self::Uring(backend) => backend.listen(host_err(fd)?, backlog),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.listen(sim_err(fd)?, backlog),
            #[cfg(test)]
            Self::Scripted(_) => Err(uring::ErrorKind::Unsupported.into()),
        }
    }

    fn local_addr(&self, fd: &Self::Fd) -> uring::Result<SocketAddress> {
        match self {
            Self::Uring(backend) => backend.local_addr(host_err(fd)?),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.local_addr(sim_err(fd)?),
            #[cfg(test)]
            Self::Scripted(_) => Err(uring::ErrorKind::Unsupported.into()),
        }
    }

    fn peer_addr(&self, fd: &Self::Fd) -> uring::Result<SocketAddress> {
        match self {
            Self::Uring(backend) => backend.peer_addr(host_err(fd)?),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.peer_addr(sim_err(fd)?),
            #[cfg(test)]
            Self::Scripted(_) => Err(uring::ErrorKind::Unsupported.into()),
        }
    }

    fn shutdown(&self, fd: &Self::Fd, how: Shutdown) -> uring::Result<()> {
        match self {
            Self::Uring(backend) => backend.shutdown(host_err(fd)?, how),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.shutdown(sim_err(fd)?, how),
            #[cfg(test)]
            Self::Scripted(_) => Err(uring::ErrorKind::Unsupported.into()),
        }
    }

    fn set_nodelay(&self, fd: &Self::Fd, enabled: bool) -> uring::Result<()> {
        match self {
            Self::Uring(backend) => backend.set_nodelay(host_err(fd)?, enabled),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.set_nodelay(sim_err(fd)?, enabled),
            #[cfg(test)]
            Self::Scripted(_) => Err(uring::ErrorKind::Unsupported.into()),
        }
    }

    fn try_send(&self, fd: &Self::Fd, buf: &[u8], flags: i32) -> uring::Result<usize> {
        match self {
            Self::Uring(backend) => backend.try_send(host_err(fd)?, buf, flags),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.try_send(sim_err(fd)?, buf, flags),
            #[cfg(test)]
            Self::Scripted(_) => Err(uring::ErrorKind::Unsupported.into()),
        }
    }

    fn try_recv(&self, fd: &Self::Fd, buf: &mut [u8], flags: i32) -> uring::Result<usize> {
        match self {
            Self::Uring(backend) => backend.try_recv(host_err(fd)?, buf, flags),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.try_recv(sim_err(fd)?, buf, flags),
            #[cfg(test)]
            Self::Scripted(_) => Err(uring::ErrorKind::Unsupported.into()),
        }
    }

    fn poll_now(&self, fd: &Self::Fd, events: i16) -> uring::Result<i16> {
        match self {
            Self::Uring(backend) => backend.poll_now(host_err(fd)?, events),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.poll_now(sim_err(fd)?, events),
            #[cfg(test)]
            Self::Scripted(_) => Err(uring::ErrorKind::Unsupported.into()),
        }
    }

    fn pipe(&self) -> uring::Result<(Self::Fd, Self::Fd)> {
        match self {
            Self::Uring(backend) => backend.pipe().map(|(r, w)| (wrap(r), wrap(w))),
            #[cfg(feature = "simulation")]
            Self::Sim(backend, _) => backend.pipe().map(|(r, w)| (wrap(r), wrap(w))),
            #[cfg(test)]
            Self::Scripted(_) => Err(uring::ErrorKind::Unsupported.into()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clock_and_entropy_follow_the_environment() {
        let before = Instant::now();
        assert!(RacerClock.now() >= before);
        assert!(RacerClock.wall_now() > SystemTime::UNIX_EPOCH);
        let mut bytes = [0u8; 32];
        RacerEntropy.fill(&mut bytes).unwrap();
        assert_ne!(bytes, [0; 32]);
        assert_eq!(std::mem::size_of::<RacerClock>(), 0);
    }

    #[test]
    fn host_backend_creates_wrapped_descriptors() {
        let backend = RacerBackend::select();
        assert!(format!("{backend:?}").contains("Uring"));
        let (read, write) = backend.pipe().unwrap();
        assert_eq!(
            backend
                .try_send(&write, b"x", 0)
                .unwrap_err()
                .raw_os_error(),
            Some(libc::ENOTSOCK)
        );
        assert!(read.is_host() && write.is_host());
        let socket = backend.socket(libc::AF_INET, libc::SOCK_STREAM, 0).unwrap();
        backend
            .bind(
                &socket,
                &SocketAddress::from("127.0.0.1:0".parse::<std::net::SocketAddr>().unwrap()),
            )
            .unwrap();
        backend.listen(&socket, 1).unwrap();
        assert!(backend.local_addr(&socket).unwrap().as_inet().is_some());
        assert_eq!(backend.poll_now(&socket, libc::POLLIN).unwrap(), 0);
    }
}

//! Migration shim over [`racer_io::sim`]: a thread-local world selection for the
//! legacy reactor and synchronous syscalls. The simulated OS itself, its
//! resources, faults, and turn-based driver live in `racer-io`. Delete together
//! with this crate; new code selects a world explicitly with `Driver::Sim`.
pub use racer_io::sim::{CrashDisk, EndpointEnvironment, Event, Fault, Handle, SimOs};
use std::{cell::RefCell, ops::Deref};

thread_local! { static CURRENT: RefCell<Option<Simulation>> = const { RefCell::new(None) }; }

/// Crash-consistent storage images.
pub mod disk {
    pub use racer_io::sim::{CrashDisk, DiskState};
}

/// Shared deterministic OS state retained by reactors and simulated descriptors.
#[derive(Clone, Debug, Default)]
pub struct Simulation(SimOs);

impl Deref for Simulation {
    type Target = SimOs;

    fn deref(&self) -> &SimOs {
        &self.0
    }
}

impl From<SimOs> for Simulation {
    /// Select an existing world.
    fn from(os: SimOs) -> Self {
        Self(os)
    }
}

/// Restores the previous thread-local simulation when its scope ends.
pub struct Environment {
    previous: Option<Simulation>,
}

impl Drop for Environment {
    /// Restore the previous thread-local backend selection.
    fn drop(&mut self) {
        CURRENT.with(|s| *s.borrow_mut() = self.previous.take());
    }
}

impl Simulation {
    /// Creates an empty world with a durable root directory.
    pub fn new() -> Self {
        Self(SimOs::new())
    }

    /// The underlying simulated OS.
    pub fn os(&self) -> &SimOs {
        &self.0
    }

    /// Selection is worker-thread scoped. Reactors and descriptors retain their
    /// environment independently, including while a nested environment is active.
    pub fn enter(&self) -> Environment {
        Environment {
            previous: CURRENT.with(|s| s.borrow_mut().replace(self.clone())),
        }
    }

    /// Returns the simulation selected for the current worker thread.
    pub fn current() -> Option<Self> {
        CURRENT.with(|s| s.borrow().clone())
    }
}

/// The thread-selected world and environment as an explicit [`racer_io::Env`].
// MIGRATION: lets callers that still select a world by thread build reactors
// explicitly; delete with the thread-local selection.
pub fn ambient_env() -> racer_io::Env {
    let ctx = crate::environment::Environment::current().worker_ctx();
    match Simulation::current() {
        Some(sim) => racer_io::Env::sim(sim.os(), ctx),
        None => racer_io::Env::new(racer_io::Driver::Uring, ctx),
    }
}

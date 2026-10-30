//! Durable physical-port reservations supplied to crypto enrollment by the dataplane.

use super::ReactorControlIo;
use crate::{
    error::{Error, Operation},
    runtime::RequestScope,
};
use racer_control_wire::RailMapping;
use std::{cell::Cell, path::PathBuf, rc::Rc, sync::Arc};
use uring_runtime::reactor::filesystem::secure::{self, Attempts, Host};

/// Own one journal namespace and fence abandoned writes before reusing its stage name.
pub struct RailJournal {
    inventory: Arc<crate::rdma::Inventory>,

    directory: PathBuf,

    host: Rc<ReactorControlIo>,

    restored: Cell<bool>,

    attempts: Attempts<RequestScope>,
}

impl RailJournal {
    /// Compose without filesystem or hardware access.
    pub fn new(
        inventory: Arc<crate::rdma::Inventory>,
        directory: PathBuf,
        host: Rc<ReactorControlIo>,
    ) -> Self {
        Self {
            inventory,
            directory,
            host,
            restored: Cell::new(false),
            attempts: Attempts::default(),
        }
    }

    /// Restore once, refresh ports, then durably reserve rails before every CSR prepare.
    pub fn persist<'a>(&'a self, parent: &'a RequestScope) -> Operation<'a, Vec<RailMapping>> {
        Box::pin(async move {
            let (_guard, scope) = self.attempts.begin(&self.host, parent).await?;
            let reactor = Host::reactor(&self.host);
            let directory = secure::directory(reactor, &self.directory, true, true, &scope).await?;
            if !self.restored.get() {
                match secure::read_at(
                    reactor,
                    &directory,
                    "rdma-rails.json",
                    crate::rdma::MAX_JOURNAL_BYTES,
                    true,
                    &scope,
                )
                .await
                {
                    Ok(bytes) => self.inventory.restore(&bytes)?,
                    Err(Error::MissingKey) => (),
                    Err(error) => return Err(error),
                }
                self.restored.set(true);
            }
            self.inventory.refresh()?;
            let reservations = self.inventory.reservations()?;
            if reservations.len() > crate::rdma::MAX_JOURNAL_BYTES {
                return Err(Error::Overloaded);
            }
            secure::atomic_write(
                reactor,
                &directory,
                "rdma-rails.json",
                &reservations,
                &scope,
            )
            .await?;
            scope.check()?;
            Ok(self.inventory.snapshot()?.nics)
        })
    }
}

#[cfg(test)]
mod tests {
    //! Journal durability and abandoned-attempt fencing before enrollment.

    use super::*;
    use crate::test_support::enrollment::drive;
    use racer_crypto::enrollment::Enrollment;
    use std::{
        num::NonZeroU32,
        path::Path,
        task::{Context, Poll},
        time::Duration,
    };
    use uring_runtime::reactor::simulation::{Fault, Simulation};

    /// Failed durability never reaches crypto preparation, including a renewal CSR.
    #[test]
    fn journal_durability_precedes_every_crypto_prepare_and_preserves_phase() {
        for renewal in [false, true] {
            let sim = Simulation::new();
            let _os = sim.enter();
            let reactor = Rc::new(crate::runtime::Reactor::new(Rc::new(
                flow_control::Quotas::new(crate::admission::AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )),
            )));
            let io = Rc::new(ReactorControlIo::new(reactor.clone()));
            let journal =
                RailJournal::new(Arc::new(Default::default()), "/identity".into(), io.clone());
            let e = Enrollment::new(
                crate::test_support::cluster::config(false).cluster,
                "/token".into(),
                "/identity".into(),
                io,
            );
            let scope = crate::test_support::enrollment::scope();
            let first = if renewal {
                let nics = drive(&reactor, journal.persist(&scope)).unwrap();
                Some(
                    drive(
                        &reactor,
                        e.prepare(nics, NonZeroU32::new(4).unwrap(), &scope),
                    )
                    .unwrap(),
                )
            } else {
                None
            };
            sim.inject("rename", Fault::Errno(libc::EIO)).unwrap();
            let result = drive(
                &reactor,
                Box::pin(async {
                    let nics = journal.persist(&scope).await?;
                    e.prepare(nics, NonZeroU32::new(4).unwrap(), &scope).await
                }),
            );
            assert!(matches!(
                result,
                Err(Error::RenameUncertain(crate::error::PublicationCause::Os(
                    libc::EIO
                )))
            ));
            assert_eq!(
                sim.read_file(Path::new("/identity/pending.json")).is_ok(),
                renewal
            );
            let nics = drive(&reactor, journal.persist(&scope)).unwrap();
            let request = drive(
                &reactor,
                e.prepare(nics, NonZeroU32::new(4).unwrap(), &scope),
            )
            .unwrap();
            if let Some(first) = first {
                assert_eq!(first.csr_der, request.csr_der);
            }
            assert!(
                sim.read_file(Path::new("/identity/rdma-rails.json"))
                    .is_ok()
            );
        }
    }

    /// An abandoned journal attempt is fenced before the deterministic stage is reused.
    #[test]
    fn abandoned_journal_write_is_fenced_before_retry_and_crypto_submission() {
        let sim = Simulation::new();
        let _os = sim.enter();
        let reactor = Rc::new(crate::runtime::Reactor::new(Rc::new(
            flow_control::Quotas::new(crate::admission::AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )),
        )));
        let io = Rc::new(ReactorControlIo::new(reactor.clone()));
        let journal =
            RailJournal::new(Arc::new(Default::default()), "/identity".into(), io.clone());
        let e = Enrollment::new(
            crate::test_support::cluster::config(false).cluster,
            "/token".into(),
            "/identity".into(),
            io,
        );
        let scope = crate::test_support::enrollment::scope();
        sim.inject("rename", Fault::HoldCompletion(20)).unwrap();
        let mut pending = journal.persist(&scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..100 {
            assert!(matches!(pending.as_mut().poll(&mut cx), Poll::Pending));
            reactor.poll_budgeted(64).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
            if sim
                .trace()
                .iter()
                .any(|event| event.operation == "complete:rename")
            {
                break;
            }
        }
        assert!(
            sim.read_file(Path::new("/identity/rdma-rails.json"))
                .is_ok()
        );
        assert!(sim.read_file(Path::new("/identity/pending.json")).is_err());
        scope.cancel().unwrap();
        drop(pending);
        let retry = crate::test_support::enrollment::scope();
        let nics = drive(&reactor, journal.persist(&retry)).unwrap();
        drive(
            &reactor,
            e.prepare(nics, NonZeroU32::new(4).unwrap(), &retry),
        )
        .unwrap();
        assert_eq!(reactor.in_flight(), 0);
        sim.disk().crash().unwrap();
        assert!(
            sim.read_file(Path::new("/identity/rdma-rails.json"))
                .is_ok()
        );
        assert!(sim.read_file(Path::new("/identity/pending.json")).is_ok());
    }
}

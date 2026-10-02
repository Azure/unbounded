use super::{lifecycle::*, *};
use crate::{
    http::{Header, MessageHead, StartLine},
    model::*,
    runtime::environment,
    security::connection::{Signatures, VerifiedHead},
};
use std::{
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    task::{Context, Poll},
    time::Duration,
};

pub(super) struct Charge(Arc<AtomicUsize>);
impl Drop for Charge {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}
pub(super) struct Observer(Arc<AtomicUsize>);
impl Observer {
    pub fn get(&self) -> usize {
        self.0.load(Ordering::Acquire)
    }
}
pub(super) fn fixture(
    slots: usize,
) -> (
    simulation::Simulation,
    Rc<IoPort>,
    NativeService,
    Vec<Observer>,
) {
    let sim = simulation::Simulation::new()
        .with_devices(vec![simulation::Device::new("sim0", [1; 16])])
        .unwrap();
    let (io, port) = pair(slots).unwrap();
    let io = Rc::new(io);
    let mut native = {
        let _scope = sim.enter();
        NativeService::new(port)
    };
    let mut observers = Vec::new();
    let guards = (0..slots)
        .map(|_| {
            let count = Arc::new(AtomicUsize::new(1));
            observers.push(Observer(count.clone()));
            Arc::new(Charge(count)) as rdma_verbs::Guard
        })
        .collect();
    futures::executor::block_on(io.configure(rdma_verbs::Configuration {
        discover: true,
        guards,
        bytes: 32,
        selector: Box::new(|ports| {
            assert_eq!(ports[0].device, "sim0");
            Ok(vec![(0, 0)])
        }),
    }))
    .unwrap();
    for _ in 0..=slots {
        native.poll_budgeted(1).unwrap();
    }
    io.activation().unwrap().unwrap();
    (sim, io, native, observers)
}
pub(super) fn immediate<T>(poll: Poll<rdma_verbs::Result<T>>) -> Result<T> {
    match poll {
        Poll::Ready(result) => result.map_err(Into::into),
        Poll::Pending => Err(Error::Overloaded),
    }
}
pub(super) fn claim(io: &Rc<IoPort>) -> Rc<QueuePairHandle> {
    immediate(QueuePairHandle::poll_new(io.device(0))).unwrap()
}
pub(super) fn connect_pair(a: &QueuePairHandle, b: &QueuePairHandle, native: &mut NativeService) {
    immediate(a.poll_connect(b.endpoint)).unwrap();
    immediate(b.poll_connect(a.endpoint)).unwrap();
    assert!(!a.ready() && !b.ready());
    native.poll_budgeted(256).unwrap();
    a.progress().unwrap();
    b.progress().unwrap();
    assert!(a.ready() && b.ready());
}
pub(super) fn mark_connected(qp: &QueuePairHandle, native: &mut NativeService) {
    immediate(qp.poll_connect(qp.endpoint)).unwrap();
    assert!(!qp.ready(), "connect is not executed on I/O");
    native.poll_budgeted(256).unwrap();
    qp.progress().unwrap();
    assert!(qp.ready());
}
pub(super) fn scope() -> RequestScope {
    RequestScope::new(
        RequestId([1; 16]),
        environment::now() + Duration::from_secs(10),
    )
    .unwrap()
}
pub(super) fn poll<T>(operation: &mut Operation<'_, T>) -> Poll<Result<T>> {
    operation
        .as_mut()
        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}
pub(super) fn done<T>(operation: &mut Operation<'_, T>) -> T {
    match poll(operation) {
        Poll::Ready(Ok(value)) => value,
        Poll::Ready(Err(e)) => panic!("unexpected error: {e:?}"),
        Poll::Pending => panic!("unexpected pending"),
    }
}
pub(super) fn verified(signers: &[Rc<Signatures>], mut headers: Vec<Header>) -> VerifiedHead {
    headers.push(Header {
        name: "racer-receiver".into(),
        value: signers[1].node().0.as_bytes().to_vec(),
    });
    signers[1]
        .verify_proof(
            signers[0]
                .sign(MessageHead {
                    start: StartLine::Request {
                        method: "POST".into(),
                        target: "/racer/peer/v1/rdma".into(),
                    },
                    headers,
                })
                .unwrap(),
        )
        .unwrap()
}
pub(super) fn header(name: &str, value: Vec<u8>) -> Header {
    Header {
        name: name.into(),
        value,
    }
}
pub(super) fn envelope() -> PageEnvelope {
    PageEnvelope {
        page: PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("mailbox-test".into()),
                    key: CacheKey([1; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            number: PageNumber(0),
        },
        key_id: KeyId([1; 16]),
        nonce: Nonce([2; 24]),
        plaintext_length: 16,
        ciphertext_length: 32,
    }
}

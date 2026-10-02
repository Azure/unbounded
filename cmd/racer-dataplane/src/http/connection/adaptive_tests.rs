use super::*;
#[test]
fn adaptive_connect_errno_preserves_local_exhaustion_as_neutral() {
    for errno in [
        None,
        Some(libc::ENOBUFS),
        Some(libc::ENOMEM),
        Some(libc::EADDRNOTAVAIL),
        Some(libc::ECANCELED),
        Some(libc::EIO),
    ] {
        assert!(!peer_connect_failure(errno));
    }
    for errno in [libc::ECONNREFUSED, libc::ECONNRESET, libc::EPIPE] {
        assert!(peer_connect_failure(Some(errno)));
    }
}
#[test]
fn adaptive_checkout_attributes_actual_connect_completion_errno() {
    use crate::{
        runtime::reactor::simulation::{Fault, Simulation},
        telemetry::metrics::{Event, Gauge, Metrics},
    };
    for (errno, blame) in [
        (libc::ENOBUFS, false),
        (libc::ENOMEM, false),
        (libc::EADDRNOTAVAIL, false),
        (libc::ECONNREFUSED, true),
    ] {
        let simulation = Simulation::new();
        let _env = simulation.enter();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = HttpPool::new(reactor.clone(), admission, 1);
        let metrics = Metrics::default();
        let peers = crate::peer::adaptive::AdaptivePeers::new(
            crate::peer::adaptive::Config {
                total: 1,
                per_peer: 1,
            },
            metrics.clone(),
        )
        .unwrap();
        let node = crate::model::NodeId("peer".into());
        let permit = peers.acquire(&node).unwrap();
        let failure = Rc::new(std::cell::Cell::new(false));
        let scope = RequestScope::new(
            crate::model::RequestId([88; 16]),
            crate::runtime::environment::now() + Duration::from_secs(5),
        )
        .unwrap();
        let endpoint = Endpoint::Peer("127.0.0.1:9999".into());
        simulation.inject("connect", Fault::Errno(errno));
        let mut checkout = pool.checkout_peer(
            &endpoint,
            None,
            Some(permit.clone()),
            Some(failure.clone()),
            &scope,
        );
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        let mut result = None;
        for _ in 0..32 {
            if let std::task::Poll::Ready(done) = checkout.as_mut().poll(&mut cx) {
                result = Some(done);
                break;
            }
            reactor.poll_budgeted(32).unwrap();
        }
        assert!(matches!(result, Some(Err(Error::Io))));
        drop(checkout);
        drop(permit);
        assert_eq!(failure.get(), blame);
        assert_eq!(peers.available(&node), !blame);
        assert_eq!(metrics.count(Event::PeerLinkFailure), u64::from(blame));
        assert_eq!(metrics.gauge(Gauge::PeerExchanges), 0);
    }
}

//! Real AEAD and signed peer responses, with deterministic ranked source scripts.
use super::*;
use crate::{
    memory::page::CiphertextCopy,
    model::identity::NodeId,
    peer::wire::{PeerRequest, VerifiedResponse},
    security::{
        forwarding::Forwarding,
        signing::tests::{network, node},
    },
};
use std::collections::VecDeque;

enum Reply {
    Copy(CiphertextCopy),
    Miss,
    Unauthorized,
}
struct ScriptedPeers {
    local: usize,
    signing: Vec<Forwarding>,
    replies: RefCell<VecDeque<(NodeId, Reply)>>,
    calls: RefCell<Vec<(NodeId, bool, u32, u8, Instant)>>,
}
impl PeerClient for ScriptedPeers {
    fn request<'a>(
        &'a self,
        request: PeerRequest,
        _: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async move {
            let destination = request.route.destination.clone();
            self.calls.borrow_mut().push((
                destination.clone(),
                matches!(
                    request.operation,
                    PeerOperation::Page {
                        mode: FetchMode::CopyOnly,
                        ..
                    }
                ),
                request.route.remaining_attempts,
                request.route.remaining_links,
                request.route.deadline.0,
            ));
            let (expected, reply) = self
                .replies
                .borrow_mut()
                .pop_front()
                .expect("no repeated source");
            assert_eq!(destination, expected);
            let response = match reply {
                Reply::Copy(copy) => PeerResponse::Page {
                    metadata: copy.metadata,
                    ciphertext: copy.ciphertext,
                },
                Reply::Miss => PeerResponse::Miss,
                Reply::Unauthorized => return Err(Error::Unauthorized),
            };
            let remote = (0..4).find(|i| node(*i) == destination).unwrap();
            let (signed, binding) = self.signing[self.local].sign_request(request)?;
            let admitted = self.signing[remote].verify_request(signed)?;
            let response = self.signing[remote].sign_response(admitted.binding(), response)?;
            self.signing[self.local].verify_response(response, &binding)
        })
    }
}

fn install_peers(f: &mut Fixture, rank: Option<usize>) -> (Rc<ScriptedPeers>, Vec<NodeId>) {
    f.membership = Arc::new(
        Membership::validate(
            crate::model::identity::MembershipVersion(1),
            (0..4)
                .map(|i| Member {
                    node: node(i),
                    shares: NonZeroU32::new(4).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 8000 + i),
                    rails: vec![],
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let placement = Rc::new(Placement::new(16));
    let ordered = placement
        .rank(f.membership.clone(), &f.context.object, f.page.number)
        .unwrap()
        .ordered;
    let local = rank
        .map(|rank| ordered[rank].clone())
        .unwrap_or_else(|| (0..4).map(node).find(|n| !ordered.contains(n)).unwrap());
    let peers = Rc::new(ScriptedPeers {
        local: (0..4).find(|i| node(*i) == local).unwrap(),
        signing: network(4).into_iter().map(Forwarding::new).collect(),
        replies: RefCell::new(VecDeque::new()),
        calls: RefCell::new(Vec::new()),
    });
    let mut dependencies = f.fill.dependencies.clone();
    dependencies.peers = peers.clone();
    dependencies.candidates = Rc::new(CandidatePolicy::new(local, placement, peers.clone()));
    f.fill = Fill::new(dependencies);
    (peers, ordered)
}

fn encrypted_copy(f: &mut Fixture) -> CiphertextCopy {
    let reservation = f
        .fill
        .reserve_progress(&f.context.object.cache, false)
        .unwrap();
    let mut plaintext = f
        .fill
        .dependencies
        .buffers
        .plaintext(reservation.plaintext, 3)
        .unwrap();
    plaintext.bytes_mut().unwrap().copy_from_slice(b"abc");
    let (_, ciphertext) = drive(
        f.fill.dependencies.crypto.encrypt(
            f.page.clone(),
            plaintext,
            reservation.ciphertext,
            &f.scope,
        ),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    CiphertextCopy {
        metadata: f.origin.metadata.clone(),
        ciphertext,
    }
}

fn unusable_copy(f: &Fixture, good: &CiphertextCopy, missing_key: bool) -> CiphertextCopy {
    let mut envelope = good.ciphertext.envelope().clone();
    let mut bytes = good.ciphertext.bytes().to_vec();
    if missing_key {
        envelope.key_id = crate::model::envelope::KeyId([99; 16]);
    } else {
        bytes[0] ^= 1;
    }
    let ciphertext = f
        .fill
        .dependencies
        .buffers
        .ciphertext(
            f.fill
                .dependencies
                .admission
                .reserve(
                    Some(&f.context.object.cache),
                    ResourceClass::Ciphertext,
                    bytes.len(),
                )
                .unwrap(),
            envelope,
            bytes,
        )
        .unwrap();
    let copy = CiphertextCopy {
        metadata: good.metadata.clone(),
        ciphertext,
    };
    validate_copy(&copy, &f.page).unwrap(); // Structural checks cannot detect flipped ciphertext.
    copy
}

fn acquire(f: &mut Fixture, budget: &mut AcquisitionBudget) -> Result<PageResult> {
    drive(
        f.fill.acquire(
            f.page.clone(),
            f.membership.clone(),
            &f.context,
            &f.scope,
            budget,
        ),
        &mut f.engine,
        &f.crypto,
    )
}

fn assert_published(f: &Fixture, result: &PageResult, persist: bool) {
    result.validate_for(&f.page).unwrap();
    assert_eq!(result.plaintext.bytes(), b"abc");
    let cached = f.fill.dependencies.memory.get(&f.page).unwrap().unwrap();
    assert_eq!(cached.ciphertext.bytes(), result.ciphertext.bytes());
    let pending = f.fill.dependencies.writer.copy_only(&f.page).unwrap();
    assert_eq!(pending.is_some(), persist);
    if let Some(copy) = pending {
        assert_eq!(copy.ciphertext.bytes(), result.ciphertext.bytes());
    }
    assert_eq!(f.crypto.outstanding(), 0);
    assert_eq!(super::super::super::drivers::pending(), 0);
}

fn assert_unpublished(f: &Fixture) {
    assert!(f.fill.dependencies.memory.get(&f.page).unwrap().is_none());
    assert!(
        f.fill
            .dependencies
            .writer
            .copy_only(&f.page)
            .unwrap()
            .is_none()
    );
    assert_eq!(
        f.fill.dependencies.admission.used(ResourceClass::Plaintext),
        0
    );
    assert_eq!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::DirtyCiphertext),
        0
    );
    assert_eq!(f.crypto.outstanding(), 0);
}

#[test]
fn unusable_peer_copy_advances_to_alternate_or_authorized_origin() {
    for missing_key in [false, true] {
        for (rank, use_origin) in [(None, false), (Some(2), false), (Some(2), true)] {
            let mut f = fixture();
            let (peers, ordered) = install_peers(&mut f, rank);
            let good = encrypted_copy(&mut f);
            let bad = unusable_copy(&f, &good, missing_key);
            peers.replies.borrow_mut().extend([
                (ordered[0].clone(), Reply::Copy(bad.clone())),
                (
                    ordered[1].clone(),
                    Reply::Copy(if use_origin { bad } else { good.clone() }),
                ),
            ]);
            let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
            let result = acquire(&mut f, &mut budget).unwrap();
            assert_published(&f, &result, rank.is_some());
            assert_eq!(f.origin.calls.get(), usize::from(use_origin));
            if !use_origin {
                assert_eq!(result.ciphertext.bytes(), good.ciphertext.bytes());
            }
            let calls = peers.calls.borrow();
            assert_eq!(calls.len(), 2);
            assert!(
                calls
                    .iter()
                    .all(|(_, copy, _, links, deadline)| *copy == rank.is_some()
                        && *links == 4
                        && *deadline == f.scope.deadline.0)
            );
            let spent: u32 = calls.iter().map(|(_, _, credits, _, _)| 1 + credits).sum();
            assert_eq!(
                budget.remaining_attempts(),
                8 - spent - f.origin.calls.get() as u32
            );
            assert_eq!(budget.remaining_links(), 8);
            assert_eq!(budget.deadline(), f.scope.deadline.0);
        }
    }
}

#[test]
fn peer_ciphertext_from_another_version_cannot_be_published_under_the_pin() {
    let mut f = fixture();
    let (peers, ordered) = install_peers(&mut f, Some(1));
    let pinned = f.page.version.etag.clone();
    f.page.version.etag = StrongEtag::test_value("different-version");
    let other = encrypted_copy(&mut f);
    f.page.version.etag = pinned;
    // The signed response claims the requested pin but the AEAD bytes were
    // encrypted under another version. Structural and signature checks pass.
    let mut envelope = other.ciphertext.envelope().clone();
    envelope.page = f.page.clone();
    let bytes = other.ciphertext.bytes().to_vec();
    let ciphertext = f
        .fill
        .dependencies
        .buffers
        .ciphertext(
            f.fill
                .dependencies
                .admission
                .reserve(
                    Some(&f.context.object.cache),
                    ResourceClass::Ciphertext,
                    bytes.len(),
                )
                .unwrap(),
            envelope,
            bytes,
        )
        .unwrap();
    peers.replies.borrow_mut().push_back((
        ordered[0].clone(),
        Reply::Copy(CiphertextCopy {
            metadata: f.origin.metadata.clone(),
            ciphertext,
        }),
    ));
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
    let result = acquire(&mut f, &mut budget).unwrap();
    assert_published(&f, &result, true);
    assert_eq!(f.origin.calls.get(), 1);
    assert_eq!(peers.calls.borrow().len(), 1);
    assert_eq!(result.metadata.version, f.page.version);
    assert_ne!(result.ciphertext.bytes(), other.ciphertext.bytes());
}

#[test]
fn origin_version_miss_checks_every_later_copy_without_claiming_false_absence() {
    for healthy_last in [false, true] {
        let mut f = fixture();
        let (peers, ordered) = install_peers(&mut f, Some(0));
        f.origin.version_unavailable.set(true);
        let good = encrypted_copy(&mut f);
        let bad = unusable_copy(&f, &good, false);
        peers.replies.borrow_mut().extend([
            (ordered[1].clone(), Reply::Copy(bad.clone())),
            (
                ordered[2].clone(),
                Reply::Copy(if healthy_last { good.clone() } else { bad }),
            ),
        ]);
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
        let result = acquire(&mut f, &mut budget);
        if healthy_last {
            let result = result.unwrap();
            assert_eq!(result.ciphertext.bytes(), good.ciphertext.bytes());
            assert_published(&f, &result, true);
        } else {
            assert!(matches!(result, Err(Error::Unavailable)));
            assert_unpublished(&f);
        }
        assert_eq!(f.origin.calls.get(), 1);
        assert_eq!(peers.calls.borrow().len(), 2);
        assert!(
            peers
                .calls
                .borrow()
                .iter()
                .all(|(_, copy, credits, _, _)| *copy && *credits == 0)
        );
        assert_eq!(budget.remaining_attempts(), 5);
        assert_eq!(budget.remaining_links(), 8);
    }
}

#[test]
fn corrupt_predecessor_evidence_survives_origin_and_later_copy_misses() {
    let mut f = fixture();
    let (peers, ordered) = install_peers(&mut f, Some(1));
    f.origin.version_unavailable.set(true);
    let good = encrypted_copy(&mut f);
    let bad = unusable_copy(&f, &good, false);
    peers.replies.borrow_mut().extend([
        (ordered[0].clone(), Reply::Copy(bad)),
        (ordered[2].clone(), Reply::Miss),
    ]);
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
    assert!(matches!(
        acquire(&mut f, &mut budget),
        Err(Error::Unavailable)
    ));
    assert_eq!(f.origin.calls.get(), 1);
    assert_eq!(peers.calls.borrow().len(), 2);
    assert_eq!(budget.remaining_attempts(), 5);
    assert_unpublished(&f);
}

#[test]
fn corrupt_copies_exhaust_sources_or_original_credits_without_origin_for_noncandidate() {
    for (attempts, links, expected_calls, expected_error) in [
        (8, 16, 3, Error::Unavailable),
        (1, 16, 1, Error::Unavailable),
        (8, 4, 1, Error::HopBudgetExhausted),
    ] {
        let mut f = fixture();
        let (peers, ordered) = install_peers(&mut f, None);
        let good = encrypted_copy(&mut f);
        let bad = unusable_copy(&f, &good, false);
        peers.replies.borrow_mut().extend(
            ordered
                .iter()
                .map(|n| (n.clone(), Reply::Copy(bad.clone()))),
        );
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, attempts, links);
        assert!(matches!(acquire(&mut f, &mut budget), Err(error) if error == expected_error));
        let calls = peers.calls.borrow();
        assert_eq!(calls.len(), expected_calls);
        assert_eq!(
            budget.remaining_attempts()
                + calls
                    .iter()
                    .map(|(_, _, credits, _, _)| 1 + credits)
                    .sum::<u32>(),
            attempts
        );
        assert_eq!(
            budget.remaining_links() + calls.iter().map(|(_, _, _, links, _)| links).sum::<u8>(),
            links
        );
        assert_eq!(f.origin.calls.get(), 0);
        assert_unpublished(&f);
    }
}

#[test]
fn unauthorized_after_corrupt_copy_is_terminal_not_origin_evidence() {
    let mut f = fixture();
    let (peers, ordered) = install_peers(&mut f, Some(2));
    let good = encrypted_copy(&mut f);
    let bad = unusable_copy(&f, &good, false);
    peers.replies.borrow_mut().extend([
        (ordered[0].clone(), Reply::Copy(bad)),
        (ordered[1].clone(), Reply::Unauthorized),
    ]);
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
    assert!(matches!(
        acquire(&mut f, &mut budget),
        Err(Error::Unauthorized)
    ));
    assert_eq!(f.origin.calls.get(), 0);
    assert_eq!(peers.calls.borrow().len(), 2);
    assert_eq!(budget.remaining_attempts(), 6);
    assert_unpublished(&f);
}

#[test]
fn corrupt_copy_cannot_extend_original_budget_deadline() {
    let mut f = fixture();
    let (peers, ordered) = install_peers(&mut f, Some(2));
    let good = encrypted_copy(&mut f);
    let bad = unusable_copy(&f, &good, false);
    peers
        .replies
        .borrow_mut()
        .push_back((ordered[0].clone(), Reply::Copy(bad)));
    let deadline = Instant::now() + Duration::from_millis(100);
    let mut budget = AcquisitionBudget::new(deadline, 8, 16);
    let mut future = Box::pin(f.fill.acquire_once(
        &f.page,
        f.membership.clone(),
        &f.context,
        &f.scope,
        &mut budget,
    ));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.crypto.outstanding(), 1);
    // Complete AEAD while live, then resume fallback after the tighter budget
    // deadline. The caller scope is still live and must not renew that deadline.
    f.engine.poll_budgeted(64).unwrap();
    std::thread::sleep(deadline.saturating_duration_since(Instant::now()));
    assert!(matches!(
        drive(future, &mut f.engine, &f.crypto),
        Err(Error::DeadlineExceeded)
    ));
    assert_eq!(peers.calls.borrow().len(), 1);
    assert_eq!(budget.remaining_attempts(), 7);
    assert_eq!(budget.remaining_links(), 12);
    assert_eq!(budget.deadline(), deadline);
    assert_eq!(f.origin.calls.get(), 0);
    assert_unpublished(&f);
}

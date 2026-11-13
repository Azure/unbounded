use super::*;
use crate::rng::SplitMix64;
use crate::write::Durability;

const LEASE: u64 = 100;

fn n(s: &str) -> NodeIdBytes {
    s.as_bytes().to_vec()
}

fn spec(upload: UploadMode) -> WriteSpec {
    WriteSpec {
        length: 10,
        page_size: 4,
        content_type: None,
        replicas: 2,
        durability: Durability::Replicas(2),
        upload,
        hold_until_millis: 10_000,
    }
}

fn identity() -> WriteIdentity {
    WriteIdentity {
        cache: "c".into(),
        key: [7; 32],
        etag: "\"racer-00000000000000000000000000000000\"".into(),
        session: SessionId([1; 16]),
    }
}

fn holders() -> Holders {
    Holders(vec![
        vec![n("a"), n("b")],
        vec![n("b"), n("c")],
        vec![n("a"), n("c")],
    ])
}

fn open(upload: UploadMode) -> Session {
    Session::open(identity(), spec(upload), 0, LEASE)
}

fn committed(upload: UploadMode) -> Session {
    let mut s = open(upload);
    s.on_event(SessionEvent::Committed(holders()), 1);
    s
}

fn rejects(effects: &[SessionEffect], why: Rejection) -> bool {
    effects == [SessionEffect::Reject(why)]
}

fn kinds(effects: &[SessionEffect]) -> Vec<&'static str> {
    effects
        .iter()
        .map(|e| match e {
            SessionEffect::Journal(r) => match r {
                JournalRecord::Commit { .. } => "journal-commit",
                JournalRecord::State { .. } => "journal-state",
                JournalRecord::Abort { .. } => "journal-abort",
                JournalRecord::Delete { .. } => "journal-delete",
                JournalRecord::Tombstone { .. } => "journal-tombstone",
                _ => "journal-other",
            },
            SessionEffect::Release { .. } => "release",
            SessionEffect::ReleaseTargets => "release-targets",
            SessionEffect::Upload { .. } => "upload",
            SessionEffect::CommitBlockList => "block-list",
            SessionEffect::Restore { .. } => "restore",
            SessionEffect::Publish => "publish",
            SessionEffect::Unpublish => "unpublish",
            SessionEffect::Notify(_) => "notify",
            SessionEffect::Reject(_) => "reject",
        })
        .collect()
}

#[test]
fn open_record_and_lease() {
    let s = open(UploadMode::Auto);
    assert_eq!(s.lease_until_millis(), Some(LEASE));
    assert_eq!(s.state(), UploadState::Pending);
    assert!(matches!(
        s.open_record(),
        JournalRecord::Open {
            lease_until_millis: LEASE,
            ..
        }
    ));
    assert_eq!(open(UploadMode::Manual).state(), UploadState::Manual);
    assert_eq!(open(UploadMode::Never).state(), UploadState::Never);
}

#[test]
fn renew_extends_and_expiry_aborts() {
    let mut s = open(UploadMode::Never);
    assert!(s.on_event(SessionEvent::Renewed, 50).is_empty());
    assert_eq!(s.lease_until_millis(), Some(150));
    assert!(s.on_event(SessionEvent::LeaseTick, 150).is_empty());
    let effects = s.on_event(SessionEvent::LeaseTick, 151);
    assert_eq!(kinds(&effects), ["journal-abort", "release-targets"]);
    assert_eq!(s.phase(), Phase::Aborted);
    assert!(s.on_event(SessionEvent::LeaseTick, 152).is_empty());
    assert!(rejects(
        &s.on_event(SessionEvent::Renewed, 152),
        Rejection::Terminal
    ));
}

#[test]
fn commit_after_lease_is_rejected_and_aborts() {
    let mut s = open(UploadMode::Auto);
    let effects = s.on_event(SessionEvent::Committed(holders()), LEASE + 1);
    assert_eq!(
        kinds(&effects),
        ["journal-abort", "release-targets", "reject"]
    );
    assert_eq!(
        effects.last(),
        Some(&SessionEffect::Reject(Rejection::LeaseExpired))
    );
    assert_eq!(s.phase(), Phase::Aborted);
}

#[test]
fn commit_requires_valid_holders() {
    let bad = [
        Holders(vec![vec![n("a"), n("b")]; 2]),
        Holders(vec![vec![n("a"), n("b")]; 4]),
        Holders(vec![
            vec![n("a"), n("b")],
            vec![n("a")],
            vec![n("a"), n("b")],
        ]),
        Holders(vec![
            vec![n("a"), n("b")],
            vec![n("a"), n("a")],
            vec![n("a"), n("b")],
        ]),
    ];
    for holders in bad {
        let mut s = open(UploadMode::Never);
        let effects = s.on_event(SessionEvent::Committed(holders), 1);
        assert!(rejects(&effects, Rejection::InvalidHolders));
        assert!(matches!(s.phase(), Phase::Open { .. }));
    }
}

#[test]
fn commit_publishes_by_upload_mode() {
    let mut s = open(UploadMode::Never);
    let effects = s.on_event(SessionEvent::Committed(holders()), 1);
    assert_eq!(kinds(&effects), ["journal-commit", "publish", "notify"]);
    assert_eq!(s.state(), UploadState::Never);
    assert_eq!(s.manifest().holders, holders());

    let mut s = open(UploadMode::Auto);
    let effects = s.on_event(SessionEvent::Committed(holders()), 1);
    assert_eq!(
        kinds(&effects),
        [
            "journal-commit",
            "publish",
            "notify",
            "journal-state",
            "notify",
            "upload",
            "upload",
            "upload",
        ]
    );
    assert_eq!(s.state(), UploadState::Uploading);
    let uploads: Vec<_> = effects
        .iter()
        .filter_map(|e| match e {
            SessionEffect::Upload { node, pages } => Some((node.clone(), pages.clone())),
            _ => None,
        })
        .collect();
    assert_eq!(
        uploads,
        vec![(n("a"), vec![0]), (n("b"), vec![1]), (n("a"), vec![2])]
    );
}

#[test]
fn upload_completes_only_after_all_pages_and_block_list() {
    let mut s = committed(UploadMode::Auto);
    assert!(rejects(
        &s.on_event(
            SessionEvent::BlockListCommitted {
                blob_etag: "x".into()
            },
            2
        ),
        Rejection::InvalidState
    ));
    assert!(s.on_event(SessionEvent::PageUploaded(0), 2).is_empty());
    assert!(s.on_event(SessionEvent::PageUploaded(0), 2).is_empty());
    assert!(s.on_event(SessionEvent::PageUploaded(2), 2).is_empty());
    assert!(rejects(
        &s.on_event(SessionEvent::PageUploaded(3), 2),
        Rejection::InvalidState
    ));
    assert_eq!(
        s.on_event(SessionEvent::PageUploaded(1), 2),
        vec![SessionEffect::CommitBlockList]
    );
    // Late duplicates do not resend the block list.
    assert!(s.on_event(SessionEvent::PageUploaded(1), 2).is_empty());
    let effects = s.on_event(
        SessionEvent::BlockListCommitted {
            blob_etag: "\"0x1\"".into(),
        },
        3,
    );
    assert_eq!(kinds(&effects), ["journal-state", "release", "notify"]);
    assert_eq!(
        effects[1],
        SessionEffect::Release {
            nodes: vec![n("a"), n("b"), n("c")]
        }
    );
    assert_eq!(s.state(), UploadState::Committed);
    assert_eq!(s.blob_etag(), Some("\"0x1\""));
    assert!(rejects(
        &s.on_event(SessionEvent::DeleteRequested, 4),
        Rejection::Uploaded
    ));
    assert!(rejects(
        &s.on_event(SessionEvent::PersistRequested, 4),
        Rejection::NotManual
    ));
    assert!(s.on_event(SessionEvent::HoldExpired, 4).is_empty());
    assert_eq!(s.phase(), Phase::Committed);
}

#[test]
fn conflict_and_failure() {
    for event in [SessionEvent::BlockListConflict, SessionEvent::UploadFailed] {
        let mut s = committed(UploadMode::Auto);
        let effects = s.on_event(event.clone(), 2);
        assert_eq!(kinds(&effects), ["journal-state", "notify"]);
        assert_eq!(s.state(), UploadState::Failed);
        assert!(rejects(&s.on_event(event, 2), Rejection::InvalidState));
        // Failed objects stay pinned until the hold ends.
        let effects = s.on_event(SessionEvent::HoldExpired, 3);
        assert_eq!(kinds(&effects), ["journal-delete", "release", "unpublish"]);
        assert_eq!(s.phase(), Phase::Expired);
    }
    let mut s = committed(UploadMode::Never);
    assert!(rejects(
        &s.on_event(SessionEvent::UploadFailed, 2),
        Rejection::InvalidState
    ));
}

#[test]
fn persist_only_in_manual() {
    let mut s = committed(UploadMode::Manual);
    assert_eq!(s.state(), UploadState::Manual);
    let effects = s.on_event(SessionEvent::PersistRequested, 2);
    assert_eq!(s.state(), UploadState::Uploading);
    assert_eq!(kinds(&effects)[..2], ["journal-state", "notify"]);
    assert!(rejects(
        &s.on_event(SessionEvent::PersistRequested, 2),
        Rejection::NotManual
    ));
    let mut never = committed(UploadMode::Never);
    assert!(rejects(
        &never.on_event(SessionEvent::PersistRequested, 2),
        Rejection::NotManual
    ));
    let mut open = open(UploadMode::Manual);
    assert!(rejects(
        &open.on_event(SessionEvent::PersistRequested, 2),
        Rejection::NotCommitted
    ));
}

#[test]
fn delete_is_terminal() {
    let mut s = committed(UploadMode::Never);
    let effects = s.on_event(SessionEvent::DeleteRequested, 2);
    assert_eq!(
        kinds(&effects),
        ["journal-tombstone", "release", "unpublish"]
    );
    // The tombstone lasts as long as the version's hold.
    assert!(matches!(
        &effects[0],
        SessionEffect::Journal(JournalRecord::Tombstone {
            hold_until_millis, ..
        }) if *hold_until_millis == s.spec().hold_until_millis
    ));
    assert_eq!(s.phase(), Phase::Deleted);
    assert!(s.is_terminal());
    for event in [
        SessionEvent::DeleteRequested,
        SessionEvent::PersistRequested,
        SessionEvent::PageUploaded(0),
    ] {
        assert!(rejects(&s.on_event(event, 3), Rejection::Terminal));
    }
    let mut open = open(UploadMode::Auto);
    let effects = open.on_event(SessionEvent::DeleteRequested, 2);
    assert_eq!(kinds(&effects), ["journal-tombstone", "release-targets"]);
    assert_eq!(open.phase(), Phase::Deleted);
}

#[test]
fn delete_during_upload_stops_upload() {
    let mut s = committed(UploadMode::Auto);
    s.on_event(SessionEvent::DeleteRequested, 2);
    assert!(rejects(
        &s.on_event(SessionEvent::PageUploaded(0), 3),
        Rejection::Terminal
    ));
}

#[test]
fn hold_expiry_keeps_started_upload() {
    let mut s = committed(UploadMode::Auto);
    assert!(s.on_event(SessionEvent::HoldExpired, 2).is_empty());
    assert_eq!(s.phase(), Phase::Committed);
    let mut open = open(UploadMode::Never);
    assert!(open.on_event(SessionEvent::HoldExpired, 2).is_empty());
}

#[test]
fn open_rejects_committed_only_events() {
    for event in [
        SessionEvent::HolderLost {
            page: 0,
            node: n("a"),
        },
        SessionEvent::PageUploaded(0),
        SessionEvent::BlockListConflict,
        SessionEvent::UploadFailed,
    ] {
        let mut s = open(UploadMode::Auto);
        assert!(rejects(&s.on_event(event, 1), Rejection::NotCommitted));
    }
    let mut s = committed(UploadMode::Never);
    for event in [
        SessionEvent::Renewed,
        SessionEvent::Aborted,
        SessionEvent::Committed(holders()),
    ] {
        assert!(rejects(&s.on_event(event, 2), Rejection::NotOpen));
    }
    assert!(s.on_event(SessionEvent::LeaseTick, 10_000_000).is_empty());
}

#[test]
fn holder_loss_restores_or_waits() {
    let mut s = committed(UploadMode::Manual);
    let lost = |s: &mut Session, page, node: &str| {
        s.on_event(
            SessionEvent::HolderLost {
                page,
                node: n(node),
            },
            2,
        )
    };
    assert!(lost(&mut s, 0, "zzz").is_empty());
    assert!(rejects(&lost(&mut s, 9, "a"), Rejection::InvalidState));
    let effects = lost(&mut s, 0, "a");
    assert_eq!(kinds(&effects), ["journal-commit", "restore"]);
    assert_eq!(
        effects[1],
        SessionEffect::Restore {
            page: 0,
            from: n("b")
        }
    );
    let effects = s.on_event(
        SessionEvent::HolderRestored {
            page: 0,
            node: n("d"),
        },
        2,
    );
    assert_eq!(kinds(&effects), ["journal-commit"]);
    assert_eq!(s.holders().page(0), &[n("b"), n("d")]);
    lost(&mut s, 2, "a");
    // A page's last holder is kept: the page waits for it to return
    // instead of failing the object.
    assert!(lost(&mut s, 2, "c").is_empty());
    assert_eq!(s.holders().page(2), &[n("c")]);
    assert_eq!(s.state(), UploadState::Manual);
    // After failure, losses are recorded but not repaired.
    s.on_event(SessionEvent::PersistRequested, 3);
    s.on_event(SessionEvent::UploadFailed, 3);
    assert_eq!(s.state(), UploadState::Failed);
    assert_eq!(kinds(&lost(&mut s, 1, "b")), ["journal-commit"]);
}

#[test]
fn nodes_lost_journals_once_and_restores_each_page() {
    let mut s = committed(UploadMode::Never);
    assert!(
        s.on_event(SessionEvent::NodesLost(vec![n("zzz")]), 2)
            .is_empty()
    );
    let effects = s.on_event(SessionEvent::NodesLost(vec![n("a")]), 2);
    assert_eq!(kinds(&effects), ["journal-commit", "restore", "restore"]);
    assert_eq!(
        effects[1..],
        [
            SessionEffect::Restore {
                page: 0,
                from: n("b")
            },
            SessionEffect::Restore {
                page: 2,
                from: n("c")
            },
        ]
    );
    assert_eq!(s.under_replicated(), vec![(0, n("b")), (2, n("c"))]);
    // A merge restores the copies in one record; repeats change nothing.
    let merge = Holders(vec![
        vec![n("d")],
        vec![],
        vec![n("d"), n("c")],
        vec![n("x")],
    ]);
    let effects = s.on_event(SessionEvent::HoldersMerged(merge.clone()), 3);
    assert_eq!(kinds(&effects), ["journal-commit"]);
    assert_eq!(s.holders().page(0), &[n("b"), n("d")]);
    assert_eq!(s.holders().page(2), &[n("c"), n("d")]);
    assert_eq!(s.holders().pages(), 3);
    assert!(s.on_event(SessionEvent::HoldersMerged(merge), 3).is_empty());
    assert!(s.under_replicated().is_empty());
    // Losing every holder of a page keeps them all and never fails the
    // object; other pages still drop the lost nodes and restore.
    let before = s.holders().clone();
    let effects = s.on_event(SessionEvent::NodesLost(vec![n("b"), n("d")]), 4);
    assert_eq!(kinds(&effects), ["journal-commit", "restore", "restore"]);
    assert_eq!(
        effects[1..],
        [
            SessionEffect::Restore {
                page: 1,
                from: n("c")
            },
            SessionEffect::Restore {
                page: 2,
                from: n("c")
            },
        ]
    );
    assert_eq!(s.holders().page(0), before.page(0));
    assert_eq!(s.holders().page(2), &[n("c")]);
    assert_eq!(s.state(), UploadState::Never);
    // A loss that touches only fully missing pages changes nothing.
    assert!(
        s.on_event(SessionEvent::NodesLost(vec![n("b"), n("d")]), 5)
            .is_empty()
    );
    // The node returns: it is still a holder, so nothing needs restoring
    // from scratch and a merge from repair fills the page back up.
    let merge = Holders(vec![vec![n("e")], vec![], vec![n("e")]]);
    assert_eq!(
        kinds(&s.on_event(SessionEvent::HoldersMerged(merge), 6)),
        ["journal-commit"]
    );
    assert_eq!(s.holders().page(0).len(), 3);
    assert_eq!(s.state(), UploadState::Never);
}

#[test]
fn merges_are_refused_before_commit() {
    let mut s = open(UploadMode::Auto);
    assert!(rejects(
        &s.on_event(SessionEvent::HoldersMerged(holders()), 1),
        Rejection::NotCommitted
    ));
    assert!(rejects(
        &s.on_event(SessionEvent::NodesLost(vec![n("a")]), 1),
        Rejection::NotCommitted
    ));
    assert!(rejects(
        &s.on_event(SessionEvent::UploadedElsewhere { blob_etag: None }, 1),
        Rejection::NotCommitted
    ));
    assert!(s.under_replicated().is_empty());
}

#[test]
fn uploaded_elsewhere_settles_any_upload_state() {
    for upload in [UploadMode::Auto, UploadMode::Manual] {
        let mut s = committed(upload);
        let effects = s.on_event(
            SessionEvent::UploadedElsewhere {
                blob_etag: Some("\"b\"".into()),
            },
            2,
        );
        assert_eq!(kinds(&effects), ["journal-state", "release", "notify"]);
        assert_eq!(s.state(), UploadState::Committed);
        assert_eq!(s.blob_etag(), Some("\"b\""));
        // Idempotent, and nothing restarts it.
        assert!(
            s.on_event(SessionEvent::UploadedElsewhere { blob_etag: None }, 3)
                .is_empty()
        );
        assert!(rejects(
            &s.on_event(SessionEvent::PageUploaded(0), 3),
            Rejection::InvalidState
        ));
        assert!(rejects(
            &s.on_event(SessionEvent::DeleteRequested, 3),
            Rejection::Uploaded
        ));
    }
    // A coordinator that failed on a block-list conflict converges too.
    let mut s = committed(UploadMode::Auto);
    for page in 0..3 {
        s.on_event(SessionEvent::PageUploaded(page), 2);
    }
    s.on_event(SessionEvent::BlockListConflict, 2);
    assert_eq!(s.state(), UploadState::Failed);
    s.on_event(SessionEvent::UploadedElsewhere { blob_etag: None }, 3);
    assert_eq!(s.state(), UploadState::Committed);
}

#[test]
fn from_manifest_restarts_upload() {
    let mut s = committed(UploadMode::Auto);
    let manifest = s.manifest();
    assert_eq!(manifest.state, UploadState::Uploading);
    let restored = Session::from_manifest("c".into(), [7; 32], manifest, LEASE);
    assert_eq!(restored.phase(), Phase::Committed);
    assert_eq!(restored.state(), UploadState::Pending);
    s.on_event(SessionEvent::DeleteRequested, 2);
    let mut manual = committed(UploadMode::Manual).manifest();
    manual.state = UploadState::Manual;
    let mut r = Session::from_manifest("c".into(), [7; 32], manual, LEASE);
    assert_eq!(r.on_event(SessionEvent::PersistRequested, 0).len(), 5);
}

#[test]
fn upload_resumes_only_from_pending() {
    let manifest = committed(UploadMode::Auto).manifest();
    let mut restored = Session::from_manifest("c".into(), [7; 32], manifest, LEASE);
    assert_eq!(restored.state(), UploadState::Pending);
    let effects = restored.on_event(SessionEvent::UploadResumed, 1);
    assert_eq!(restored.state(), UploadState::Uploading);
    assert!(
        effects
            .iter()
            .any(|e| matches!(e, SessionEffect::Upload { .. }))
    );
    // Already uploading, manual, or still open: refused.
    assert!(rejects(
        &restored.on_event(SessionEvent::UploadResumed, 2),
        Rejection::InvalidState
    ));
    let mut manual = committed(UploadMode::Manual);
    assert!(rejects(
        &manual.on_event(SessionEvent::UploadResumed, 2),
        Rejection::InvalidState
    ));
    let mut fresh = open(UploadMode::Auto);
    assert!(rejects(
        &fresh.on_event(SessionEvent::UploadResumed, 2),
        Rejection::NotCommitted
    ));
}

/// Random event sequences never publish before commit, never send the block
/// list before every page uploaded, and never act after a terminal phase.
#[test]
fn random_sequences_keep_invariants() {
    let mut rng = SplitMix64::new(0x5eed);
    for _ in 0..2_000 {
        let upload =
            [UploadMode::Auto, UploadMode::Manual, UploadMode::Never][rng.below(3) as usize];
        let mut s = open(upload);
        let mut now = 0;
        let mut committed = false;
        let mut uploaded = [false; 3];
        let mut terminal = false;
        for _ in 0..40 {
            now += rng.below(40);
            let page = rng.below(4);
            let node = n(["a", "b", "c", "d"][rng.below(4) as usize]);
            let event = match rng.below(17) {
                14 => SessionEvent::NodesLost(vec![node]),
                15 => SessionEvent::HoldersMerged(Holders(vec![vec![node]; 3])),
                16 => SessionEvent::UploadedElsewhere {
                    blob_etag: Some("e".into()),
                },
                0 => SessionEvent::Renewed,
                1 => SessionEvent::Committed(if rng.below(4) == 0 {
                    Holders(vec![vec![n("a")]; 3])
                } else {
                    holders()
                }),
                2 => SessionEvent::Aborted,
                3 => SessionEvent::LeaseTick,
                4 => SessionEvent::HolderLost { page, node },
                5 => SessionEvent::HolderRestored { page, node },
                6..=8 => SessionEvent::PageUploaded(page),
                9 => SessionEvent::BlockListCommitted {
                    blob_etag: "e".into(),
                },
                10 => SessionEvent::BlockListConflict,
                11 => SessionEvent::PersistRequested,
                12 => SessionEvent::DeleteRequested,
                _ => SessionEvent::HoldExpired,
            };
            let state_before = s.state();
            let page_uploaded = matches!(event, SessionEvent::PageUploaded(_));
            let effects = s.on_event(event, now);
            if terminal {
                assert!(effects.is_empty() || rejects(&effects, Rejection::Terminal));
            }
            for effect in &effects {
                match effect {
                    SessionEffect::Publish => {
                        assert!(!committed, "published twice");
                        committed = true;
                    }
                    SessionEffect::Journal(JournalRecord::State {
                        state: UploadState::Uploading,
                        ..
                    }) => uploaded = [false; 3],
                    SessionEffect::CommitBlockList => assert!(committed),
                    SessionEffect::Upload { .. } | SessionEffect::Unpublish => {
                        assert!(committed)
                    }
                    _ => {}
                }
            }
            if page_uploaded
                && state_before == UploadState::Uploading
                && !effects
                    .iter()
                    .any(|e| matches!(e, SessionEffect::Reject(_)))
                && page < 3
            {
                uploaded[page as usize] = true;
            }
            // Checked after applying this event's page mark.
            if effects.contains(&SessionEffect::CommitBlockList) {
                assert!(uploaded.iter().all(|&u| u), "block list early");
            }
            if s.state() == UploadState::Committed {
                assert!(s.blob_etag().is_some());
            }
            terminal = s.is_terminal();
        }
    }
}

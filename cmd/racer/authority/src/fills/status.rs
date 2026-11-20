//! ClusterCacheFill status computation and the primary's status writer.
//!
//! The primary aggregates node progress from Pod fills annotations whose
//! generation, digest, and membership match its own, then writes the status
//! subresource with resourceVersion CAS.

use super::{FillView, snapshot};
use crate::{
    annotations,
    kube::{self, KubeError, paths},
    limits, rt,
    rules::{
        election,
        members::{NodeInfo, Ownership, PodInfo, select_pod},
        rails::FillProgress,
    },
    shared::Shared,
};
use racer_wire::control::Member;
use serde_json::{Value, json};
use std::{collections::BTreeMap, time::Duration};

/// How often the primary looks for a status update.
const TICK: Duration = Duration::from_secs(10);

/// Aggregated node progress for one fill.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Progress {
    pub resident_bytes: u64,
    pub refused_bytes: u64,
    pub nodes: i32,
}

/// Sum the reports for `view` whose generation, digest, and membership match.
pub fn aggregate(reports: &BTreeMap<String, String>, view: &FillView) -> Progress {
    let Some(manifest) = &view.manifest else {
        return Progress::default();
    };
    let Some(membership) = view.membership else {
        return Progress::default();
    };
    let mut progress = Progress::default();
    for report in reports.values() {
        let Ok(fills) = crate::rules::rails::parse_fills(report) else {
            continue;
        };
        let entry: Option<&FillProgress> = fills.fills.iter().find(|f| f.fill == view.uid);
        let Some(entry) = entry else {
            continue;
        };
        if entry.generation != view.generation
            || entry.digest != manifest.digest
            || entry.membership != membership
        {
            continue;
        }
        progress.resident_bytes = progress.resident_bytes.saturating_add(entry.pinned_bytes);
        progress.refused_bytes = progress.refused_bytes.saturating_add(entry.refused_bytes);
        if entry.pinned_pages > 0 {
            progress.nodes = progress.nodes.saturating_add(1);
        }
    }
    progress
}

/// Fills annotations of the selected managed Pod for each current member Node.
/// Keyed by Node UID so a Node contributes at most once, and Pods the
/// dataplane DaemonSet does not own are ignored.
fn member_reports(
    pods: &BTreeMap<String, PodInfo>,
    nodes: &BTreeMap<String, NodeInfo>,
    ownership: &Ownership,
    members: &[Member],
) -> BTreeMap<String, String> {
    let mut by_node: BTreeMap<&str, Vec<PodInfo>> = BTreeMap::new();
    for pod in pods.values() {
        by_node
            .entry(pod.node_name.as_str())
            .or_default()
            .push(pod.clone());
    }
    let mut reports = BTreeMap::new();
    let empty = Vec::new();
    for member in members {
        let Some(node) = nodes.get(&member.node.0) else {
            continue;
        };
        let Some((pod, _)) = select_pod(
            by_node.get(node.name.as_str()).unwrap_or(&empty),
            ownership,
            &node.name,
        ) else {
            continue;
        };
        if let Some(fills) = pod.annotations.get(annotations::FILLS) {
            reports.insert(member.node.0.clone(), fills.clone());
        }
    }
    reports
}

/// Compute the `status` object for one fill. `previous` is the status read
/// from the API, used only to keep `lastTransitionTime` and to detect a fill
/// that was Ready and is now short.
pub fn status(
    view: &FillView,
    reports: &BTreeMap<String, String>,
    previous: Option<&Value>,
) -> Value {
    let now = election::now_micro(rt::wall_now());
    let progress = aggregate(reports, view);
    let manifest_current = view.manifest.is_some()
        && view.listed_generation == Some(view.generation)
        && view.listed_cache_uid == view.cache_uid;
    let listed_ok = manifest_current && view.error.is_none();
    let footprint = view.planned_footprint_bytes.unwrap_or(0);
    let under_replicated_bytes = footprint.saturating_sub(progress.resident_bytes);
    let missing_members = view.replicas > view.available_nodes;
    let ready = view.accepted
        && manifest_current
        && !missing_members
        && progress.resident_bytes >= footprint
        && under_replicated_bytes == 0;
    // Completion history belongs to this spec generation, not the object
    // name: a changed spec starts a new initial fill.
    let history = previous
        .filter(|p| p.get("observedGeneration").and_then(Value::as_u64) == Some(view.generation));
    let was_ready = history
        .map(|p| condition_status(p, "Ready") == Some("True"))
        .unwrap_or(false);
    // Progressing records the initial fill for this generation. Once it
    // completes it stays False during subsequent refills, including across
    // primary failover. Degraded alone is not completion history: capacity
    // can be refused during the initial fill too.
    let completed = was_ready
        || history
            .map(|p| condition_status(p, "Progressing") == Some("False"))
            .unwrap_or(false);
    let previous_nodes = history
        .and_then(|p| p.get("nodes"))
        .and_then(Value::as_i64)
        .unwrap_or(0);
    // A member that leaves takes its report with it, so the remaining reports
    // cannot show the shortfall through `under_replicated_bytes`: compare
    // confirmed bytes with the footprint instead. Only a fill that was once
    // complete is Degraded, so initial filling stays Progressing alone.
    let short =
        view.accepted && manifest_current && footprint > 0 && progress.resident_bytes < footprint;
    let degraded = if progress.refused_bytes > 0 {
        Some(("CapacityLost", "nodes refused pinned bytes".to_owned()))
    } else if completed && view.accepted && manifest_current && missing_members {
        Some((
            "MemberLost",
            format!(
                "{} replicas requested but only {} nodes are available",
                view.replicas, view.available_nodes
            ),
        ))
    } else if completed && (short || under_replicated_bytes > 0) {
        if progress.nodes == 0 || previous_nodes > i64::from(progress.nodes) {
            Some(("MemberLost", "a node with pins left".to_owned()))
        } else {
            Some(("Refilling", "copies are missing".to_owned()))
        }
    } else {
        None
    };

    let admission_message = match view.message.as_str() {
        "CacheNotFound" => format!("ClusterCache {:?} was not found", view.cache_name),
        "UnsupportedCache" => format!(
            "ClusterCache {:?} uses passthrough mode and cannot retain pins",
            view.cache_name
        ),
        "UnsupportedOrigin" => format!(
            "ClusterCache {:?} does not have a supported Azure origin",
            view.cache_name
        ),
        "ExceedsMaxFootprint" => format!(
            "planned footprint of {footprint} bytes exceeds maxFootprint of {} bytes",
            view.max_footprint
        ),
        "TooManyObjects" => format!(
            "origin selection exceeds the {} object manifest limit",
            super::MAX_FILL_OBJECTS
        ),
        _ => "fill admission has not succeeded".to_owned(),
    };
    let accepted = condition(
        view,
        previous,
        &now,
        "Accepted",
        view.accepted,
        if view.accepted {
            "Accepted"
        } else {
            &view.message
        },
        if view.accepted {
            "admission succeeded"
        } else {
            &admission_message
        },
    );
    let list_reason = match view.error.as_deref() {
        Some("Forbidden") => "Forbidden",
        Some("TooManyObjects") => "TooManyObjects",
        Some(_) => "ListFailed",
        None => "ListFailed",
    };
    let listing_message = match view.error.as_deref() {
        Some("Forbidden") => format!(
            "origin listing for ClusterCache {:?} was forbidden; check origin credentials and permissions",
            view.cache_name
        ),
        Some("TooManyObjects") => format!(
            "origin selection exceeds the {} object manifest limit; narrow spec.prefixes",
            super::MAX_FILL_OBJECTS
        ),
        Some(_) => format!(
            "origin listing for ClusterCache {:?} failed; a retry is scheduled",
            view.cache_name
        ),
        None => "manifest is not listed yet".to_owned(),
    };
    let listed = condition(
        view,
        previous,
        &now,
        "Listed",
        listed_ok,
        if listed_ok { "Listed" } else { list_reason },
        if listed_ok {
            "manifest is current"
        } else {
            &listing_message
        },
    );
    let progressing = condition(
        view,
        previous,
        &now,
        "Progressing",
        !ready && !completed,
        if ready || completed {
            "Complete"
        } else {
            "Filling"
        },
        if ready || completed {
            "initial fill for this generation is complete"
        } else {
            "residency is below the planned footprint"
        },
    );
    let waiting = if missing_members {
        format!(
            "{} replicas requested but only {} nodes are available",
            view.replicas, view.available_nodes
        )
    } else {
        "waiting for confirmed copies".to_owned()
    };
    let ready_condition = condition(
        view,
        previous,
        &now,
        "Ready",
        ready,
        if ready { "Complete" } else { "Filling" },
        if ready {
            "every copy is confirmed"
        } else {
            &waiting
        },
    );
    let degraded_true = degraded.is_some();
    let (degraded_reason, degraded_message) =
        degraded.unwrap_or_else(|| ("Complete", "healthy".to_owned()));
    let degraded_condition = condition(
        view,
        previous,
        &now,
        "Degraded",
        degraded_true,
        degraded_reason,
        &degraded_message,
    );

    let mut status = json!({
        "observedGeneration": view.generation,
        "cacheUID": view.cache_uid,
        "footprintBytes": footprint,
        "residentBytes": progress.resident_bytes,
        "underReplicatedBytes": under_replicated_bytes,
        "nodes": progress.nodes,
        "conditions": [accepted, listed, progressing, ready_condition, degraded_condition],
    });
    if let Some(manifest) = &view.manifest {
        status["manifest"] = json!({
            "objects": manifest.objects.len(),
            "logicalBytes": manifest.logical_bytes,
            "digest": manifest.digest,
            "listedAt": election::micro_time(Duration::from_secs(manifest.listed_at)),
        });
    }
    status
}

fn condition_status<'a>(status: &'a Value, kind: &str) -> Option<&'a str> {
    status
        .get("conditions")?
        .as_array()?
        .iter()
        .find(|c| c.get("type").and_then(Value::as_str) == Some(kind))?
        .get("status")?
        .as_str()
}

fn previous_condition<'a>(previous: Option<&'a Value>, kind: &str) -> Option<&'a Value> {
    previous?
        .get("conditions")?
        .as_array()?
        .iter()
        .find(|c| c.get("type").and_then(Value::as_str) == Some(kind))
}

fn condition(
    view: &FillView,
    previous: Option<&Value>,
    now: &str,
    kind: &str,
    status: bool,
    reason: &str,
    message: &str,
) -> Value {
    let status = if status { "True" } else { "False" };
    let transition = previous_condition(previous, kind)
        .filter(|c| c.get("status").and_then(Value::as_str) == Some(status))
        .and_then(|c| c.get("lastTransitionTime"))
        .and_then(Value::as_str)
        .unwrap_or(now);
    json!({
        "type": kind,
        "status": status,
        "reason": reason,
        "message": message,
        "lastTransitionTime": transition,
        "observedGeneration": view.generation,
    })
}

/// Primary-only: write status for changed fills. At most one PUT per pass, so
/// the primary Lease renewal stays on schedule. `cursor` is the first fill to
/// try next; it advances after every write attempt, so a fill whose status
/// stays dirty cannot starve the rest. A fill whose live object has a
/// different UID or generation than the snapshot is skipped: the snapshot
/// describes an older object, so its status must not be written.
pub async fn reconcile(shared: &Shared, cursor: &mut usize) {
    let Some(term) = shared.primary_term(rt::now()) else {
        return;
    };
    if shared.fill_status_term.borrow().as_ref() != Some(&term)
        || !shared.fills_synced.get()
        || !shared.fills_caches_synced.get()
    {
        return;
    }
    let views = snapshot(shared);
    if views.is_empty() {
        return;
    }
    let reports = {
        let serving = shared.serving.borrow();
        let members: &[Member] = serving
            .image
            .as_ref()
            .map_or(&[], |image| image.publication.members.as_slice());
        let pods = shared.pods.borrow();
        let nodes = shared.nodes.borrow();
        let ownership = shared.ownership.borrow();
        member_reports(&pods, &nodes, &ownership, members)
    };
    let len = views.len();
    let start = *cursor % len;
    for offset in 0..len {
        let index = (start + offset) % len;
        let view = &views[index];
        let current = match shared.api.get(&paths::cluster_cache_fill(&view.name)).await {
            Ok(v) => v,
            Err(KubeError::NotFound) => continue,
            Err(_) => {
                *cursor = (index + 1) % len;
                return;
            }
        };
        // The snapshot can outlive the object it was taken from: the fill may
        // have been updated (new generation) or recreated (new UID) before
        // this GET. The resourceVersion below belongs to the object just
        // read, so a PUT would publish the stale snapshot's manifest,
        // admission, and Ready=True onto the newer object. Only write the
        // fill the snapshot actually describes; otherwise skip it.
        if kube::str_at(&current, &["metadata", "uid"]) != view.uid.as_str()
            || current
                .pointer("/metadata/generation")
                .and_then(Value::as_u64)
                != Some(view.generation)
        {
            continue;
        }
        let resource_version = kube::str_at(&current, &["metadata", "resourceVersion"]).to_owned();
        if resource_version.is_empty() {
            continue;
        }
        let previous = current.get("status").filter(|s| s.is_object());
        let next = status(view, &reports, previous);
        if previous == Some(&next) {
            continue;
        }
        // A GET may have outlived this primary term or its Lease freshness.
        if shared.primary_term(rt::now()).as_ref() != Some(&term) || shared.stopping.get() {
            return;
        }
        let body = json!({
            "apiVersion": "racer.unbounded-cloud.io/v1alpha1",
            "kind": "ClusterCacheFill",
            "metadata": {
                "name": view.name,
                "resourceVersion": resource_version,
            },
            "status": next,
        });
        let result = rt::timeout(
            limits::API_TIMEOUT,
            shared
                .api
                .put(&paths::cluster_cache_fill_status(&view.name), &body),
        )
        .await;
        *cursor = (index + 1) % len;
        match result {
            Ok(Ok(_)) => return,
            // Conflict: re-read next turn. NotFound: deleted.
            Ok(Err(KubeError::Conflict | KubeError::NotFound)) => return,
            _ => return,
        }
    }
}

/// Loop while this process lives; only the primary actually writes.
pub async fn run(shared: std::rc::Rc<Shared>) {
    let mut cursor = 0;
    while !shared.stopping.get() {
        reconcile(&shared, &mut cursor).await;
        rt::sleep(TICK).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::fills::manifest;
    use racer_wire::control::NodeId;
    use std::{num::NonZeroU32, sync::Arc};

    fn view() -> FillView {
        FillView {
            listing: Default::default(),
            uid: "11111111-1111-4111-8111-111111111111".into(),
            name: "fill".into(),
            generation: 2,
            cache_uid: "22222222-2222-4222-8222-222222222222".into(),
            cache_name: "models".into(),
            replicas: 2,
            available_nodes: 2,
            max_footprint: 1024,
            resync_secs: 3600,
            manifest: None,
            membership: Some(7),
            planned_footprint_bytes: None,
            accepted: true,
            message: String::new(),
            error: None,
            prefixes: vec!["".into()],
            listed_generation: None,
            listed_cache_uid: String::new(),
            next_list: None,
            failures: 0,
        }
    }

    fn progress_for(v: &FillView) -> FillProgress {
        FillProgress {
            fill: v.uid.clone(),
            generation: v.generation,
            digest: v.manifest.as_ref().unwrap().digest.clone(),
            membership: v.membership.unwrap(),
            owned_pages: 10,
            pinned_pages: 10,
            owned_bytes: 100,
            pinned_bytes: 100,
            refused_bytes: 0,
        }
    }

    fn with_manifest(mut v: FillView) -> FillView {
        let m = manifest::build(
            "models",
            &[manifest::ListedObject {
                name: "k".into(),
                etag: "\"e\"".into(),
                length: 100,
                content_type: None,
            }],
            &["".into()],
            10,
        )
        .unwrap();
        v.manifest = Some(Arc::new(m));
        v.listed_generation = Some(v.generation);
        v.listed_cache_uid = v.cache_uid.clone();
        v.planned_footprint_bytes = Some(200);
        v.error = None;
        v
    }

    fn report(fill: FillProgress) -> String {
        let f = crate::rules::rails::Fills {
            fills: vec![fill],
            pinnable_bytes: None,
        };
        crate::rules::rails::encode_fills(&f).unwrap()
    }

    fn statuses(status: &Value) -> BTreeMap<String, (String, String)> {
        status["conditions"]
            .as_array()
            .unwrap()
            .iter()
            .map(|c| {
                (
                    c["type"].as_str().unwrap().to_owned(),
                    (
                        c["status"].as_str().unwrap().to_owned(),
                        c["reason"].as_str().unwrap().to_owned(),
                    ),
                )
            })
            .collect()
    }

    #[test]
    fn aggregates_only_matching_reports() {
        let v = with_manifest(view());
        let mut reports = BTreeMap::new();
        reports.insert("p1".into(), report(progress_for(&v)));
        let mut other = progress_for(&v);
        other.generation = 1;
        reports.insert("p2".into(), report(other));
        let p = aggregate(&reports, &v);
        assert_eq!(p.resident_bytes, 100);
        assert_eq!(p.nodes, 1);
        let s = status(&v, &reports, None);
        assert_eq!(s["observedGeneration"], 2);
        assert_eq!(s["cacheUID"], "22222222-2222-4222-8222-222222222222");
        assert_eq!(s["manifest"]["objects"], 1);
        assert_eq!(s["manifest"]["logicalBytes"], 100);
        assert_eq!(s["footprintBytes"], 200);
        assert_eq!(s["residentBytes"], 100);
        assert_eq!(s["underReplicatedBytes"], 100);
        assert_eq!(s["nodes"], 1);
        let c = statuses(&s);
        assert_eq!(c["Accepted"], ("True".into(), "Accepted".into()));
        assert_eq!(c["Listed"], ("True".into(), "Listed".into()));
        assert_eq!(c["Progressing"], ("True".into(), "Filling".into()));
        assert_eq!(c["Ready"], ("False".into(), "Filling".into()));
        assert_eq!(c["Degraded"], ("False".into(), "Complete".into()));
        // The shortfall is the full planned footprint minus confirmed pins.
        let mut short = progress_for(&v);
        short.pinned_pages = 5;
        short.pinned_bytes = 50;
        let mut reports = BTreeMap::new();
        reports.insert("p1".into(), report(short));
        let p = aggregate(&reports, &v);
        assert_eq!(p.resident_bytes, 50);
        assert_eq!(status(&v, &reports, None)["underReplicatedBytes"], 150);
        // A missing report leaves no resident copies and the whole plan short.
        let missing = status(&v, &BTreeMap::new(), None);
        assert_eq!(missing["residentBytes"], 0);
        assert_eq!(missing["underReplicatedBytes"], 200);
    }

    #[test]
    fn ready_and_degraded_follow_current_state_and_previous_ready() {
        let mut v = with_manifest(view());
        v.planned_footprint_bytes = Some(100);
        let full = progress_for(&v);
        let mut reports = BTreeMap::new();
        reports.insert("p1".into(), report(full.clone()));
        let s = status(&v, &reports, None);
        let c = statuses(&s);
        assert_eq!(c["Ready"], ("True".into(), "Complete".into()));
        assert_eq!(c["Progressing"], ("False".into(), "Complete".into()));
        assert_eq!(c["Degraded"], ("False".into(), "Complete".into()));
        // Was Ready, now a copy is missing: Degraded Refilling.
        let mut short = full;
        short.pinned_pages = 5;
        short.pinned_bytes = 50;
        let mut reports = BTreeMap::new();
        reports.insert("p1".into(), report(short));
        let c = statuses(&status(&v, &reports, Some(&s)));
        assert_eq!(c["Degraded"], ("True".into(), "Refilling".into()));
        assert_eq!(c["Progressing"], ("False".into(), "Complete".into()));
        let degraded = status(&v, &reports, Some(&s));
        let still_short = status(&v, &reports, Some(&degraded));
        assert_eq!(statuses(&still_short)["Progressing"].0, "False");
        assert_eq!(statuses(&still_short)["Degraded"].0, "True");
        // Was Ready, no node reports: Degraded MemberLost.
        let c = statuses(&status(&v, &BTreeMap::new(), Some(&s)));
        assert_eq!(c["Degraded"], ("True".into(), "MemberLost".into()));
        // Never Ready: not Degraded.
        let c = statuses(&status(&v, &BTreeMap::new(), None));
        assert_eq!(c["Degraded"], ("False".into(), "Complete".into()));
        // Refused bytes are CapacityLost even before Ready.
        let mut refused = progress_for(&v);
        refused.refused_bytes = 5;
        let mut reports = BTreeMap::new();
        reports.insert("p1".into(), report(refused));
        let c = statuses(&status(&v, &reports, None));
        assert_eq!(c["Degraded"], ("True".into(), "CapacityLost".into()));
        // A refusal before initial completion cannot make the next short
        // report look like a formerly complete fill.
        let mut refused = progress_for(&v);
        refused.pinned_bytes = 50;
        refused.refused_bytes = 5;
        reports.insert("p1".into(), report(refused));
        let initial_refusal = status(&v, &reports, None);
        assert_eq!(statuses(&initial_refusal)["Progressing"].0, "True");
        reports.clear();
        let next = status(&v, &reports, Some(&initial_refusal));
        assert_eq!(statuses(&next)["Progressing"].0, "True");
        assert_eq!(statuses(&next)["Degraded"].0, "False");
    }

    #[test]
    fn insufficient_initial_pool_is_not_reported_as_member_loss() {
        let mut v = with_manifest(view());
        // The resident copy covers the whole footprint, so only the missing
        // replica keeps the fill from being Ready.
        v.planned_footprint_bytes = Some(100);
        v.replicas = 2;
        v.available_nodes = 1;
        let mut reports = BTreeMap::new();
        reports.insert("p1".into(), report(progress_for(&v)));
        let s = status(&v, &reports, None);
        let c = statuses(&s);
        assert_eq!(s["footprintBytes"], 100);
        assert_eq!(s["residentBytes"], 100);
        assert_eq!(c["Ready"], ("False".into(), "Filling".into()));
        assert_eq!(c["Degraded"], ("False".into(), "Complete".into()));
        assert_eq!(c["Progressing"], ("True".into(), "Filling".into()));
        let message = s["conditions"]
            .as_array()
            .unwrap()
            .iter()
            .find(|c| c["type"] == "Ready")
            .and_then(|c| c["message"].as_str())
            .unwrap();
        assert!(message.contains("2 replicas"), "{message}");
        assert!(message.contains("1 nodes"), "{message}");
        // The same shortage after initial completion really is MemberLost.
        v.available_nodes = 2;
        let ready = status(&v, &reports, None);
        assert_eq!(statuses(&ready)["Ready"].0, "True");
        v.available_nodes = 1;
        let lost = status(&v, &reports, Some(&ready));
        assert_eq!(statuses(&lost)["Degraded"].1, "MemberLost");
        assert_eq!(statuses(&lost)["Progressing"].0, "False");
    }

    #[test]
    fn previous_generation_completion_does_not_degrade_a_new_spec() {
        let mut v = with_manifest(view());
        v.planned_footprint_bytes = Some(100);
        let reports = BTreeMap::from([("p1".into(), report(progress_for(&v)))]);
        let ready = status(&v, &reports, None);
        assert_eq!(statuses(&ready)["Ready"].0, "True");
        let degraded = status(&v, &BTreeMap::new(), Some(&ready));
        assert_eq!(statuses(&degraded)["Degraded"].0, "True");
        v.generation += 1;
        v.listed_generation = Some(v.generation);
        for previous in [&ready, &degraded] {
            let next = status(&v, &BTreeMap::new(), Some(previous));
            assert_eq!(statuses(&next)["Degraded"].0, "False");
            assert_eq!(statuses(&next)["Progressing"].0, "True");
        }
    }

    #[test]
    fn admission_reasons_and_timestamps_are_reported() {
        let mut v = view();
        v.accepted = false;
        v.message = "CacheNotFound".into();
        let s = status(&v, &BTreeMap::new(), None);
        let c = statuses(&s);
        assert_eq!(c["Accepted"], ("False".into(), "CacheNotFound".into()));
        assert_eq!(c["Listed"], ("False".into(), "ListFailed".into()));
        assert_eq!(s["observedGeneration"], 2);
        assert!(s["manifest"].is_null());
        // An unchanged status keeps its transition time.
        let first = status(&v, &BTreeMap::new(), None);
        let later = status(&v, &BTreeMap::new(), Some(&first));
        assert_eq!(
            first["conditions"][0]["lastTransitionTime"],
            later["conditions"][0]["lastTransitionTime"]
        );
    }

    #[test]
    fn rejection_and_listing_diagnostics_explain_the_reason() {
        for reason in [
            "CacheNotFound",
            "UnsupportedCache",
            "UnsupportedOrigin",
            "ExceedsMaxFootprint",
            "TooManyObjects",
        ] {
            let mut v = view();
            v.accepted = false;
            v.message = reason.into();
            let s = status(&v, &BTreeMap::new(), None);
            let c = previous_condition(Some(&s), "Accepted").unwrap();
            assert_eq!(c["reason"], reason);
            assert_ne!(c["message"], reason);
            assert!(c["message"].as_str().unwrap().contains(' '));
        }
        for reason in ["Forbidden", "ListFailed", "TooManyObjects"] {
            let mut v = view();
            v.error = Some(reason.into());
            let s = status(&v, &BTreeMap::new(), None);
            let c = previous_condition(Some(&s), "Listed").unwrap();
            assert_eq!(c["reason"], reason);
            assert_eq!(c["status"], "False");
            assert_ne!(c["message"], reason);
            assert!(c["message"].as_str().unwrap().contains(' '));
        }
    }

    const DS: &str = "dddddddd-dddd-4ddd-8ddd-dddddddddddd";
    const NODE_UID: &str = "33333333-3333-4333-8333-333333333333";

    fn ownership() -> Ownership {
        Ownership {
            namespace: "racer-system".into(),
            daemonset: "racer".into(),
            uid: DS.into(),
            service_account: "racer".into(),
        }
    }

    fn node_map() -> BTreeMap<String, NodeInfo> {
        BTreeMap::from([(
            NODE_UID.to_owned(),
            NodeInfo {
                name: "n1".into(),
                uid: NODE_UID.into(),
                ..Default::default()
            },
        )])
    }

    fn managed_pod(uid: &str, created: &str, fills: &str, owner: &str) -> PodInfo {
        PodInfo {
            namespace: "racer-system".into(),
            name: format!("racer-{uid}"),
            uid: uid.into(),
            node_name: "n1".into(),
            pod_ip: "10.0.0.1".into(),
            phase: "Running".into(),
            creation: created.into(),
            service_account: "racer".into(),
            owner: Some(crate::rules::members::Owner {
                api_version: "apps/v1".into(),
                kind: "DaemonSet".into(),
                name: "racer".into(),
                uid: owner.into(),
            }),
            annotations: BTreeMap::from([(annotations::FILLS.to_owned(), fills.to_owned())]),
            ..Default::default()
        }
    }

    fn pod_map(list: Vec<PodInfo>) -> BTreeMap<String, PodInfo> {
        list.into_iter().map(|p| (p.uid.clone(), p)).collect()
    }

    fn member() -> Member {
        Member {
            node: NodeId(NODE_UID.into()),
            shares: NonZeroU32::new(4).unwrap(),
            peer_endpoint: "10.0.0.1:8082".into(),
            rails: Vec::new(),
            site: String::new(),
        }
    }

    #[test]
    fn unrelated_pods_do_not_contribute_member_reports() {
        let v = with_manifest(view());
        let real = report(progress_for(&v));
        // The unrelated Pod is newer and carries a byte-for-byte matching
        // annotation, but the DaemonSet does not own it.
        let pods = pod_map(vec![
            managed_pod("real", "2024-01-01T00:00:00Z", &real, DS),
            managed_pod("spoof", "2025-01-01T00:00:00Z", &real, "other"),
        ]);
        let reports = member_reports(&pods, &node_map(), &ownership(), &[member()]);
        assert_eq!(reports.len(), 1);
        assert_eq!(reports[NODE_UID], real);
        assert_eq!(aggregate(&reports, &v).nodes, 1);
    }

    #[test]
    fn two_managed_pods_on_one_node_contribute_once() {
        let v = with_manifest(view());
        let mut old = progress_for(&v);
        old.pinned_bytes = 100;
        let mut new = old.clone();
        new.owned_bytes = 250;
        new.pinned_bytes = 250;
        let old_report = report(old);
        let new_report = report(new);
        let pods = pod_map(vec![
            managed_pod("old", "2024-01-01T00:00:00Z", &old_report, DS),
            managed_pod("new", "2024-01-02T00:00:00Z", &new_report, DS),
        ]);
        let reports = member_reports(&pods, &node_map(), &ownership(), &[member()]);
        assert_eq!(reports.len(), 1);
        assert_eq!(reports[NODE_UID], new_report);
        let progress = aggregate(&reports, &v);
        assert_eq!(progress.nodes, 1);
        assert_eq!(progress.resident_bytes, 250);
    }

    #[test]
    fn selected_managed_pod_contributes_normally() {
        let v = with_manifest(view());
        let full = progress_for(&v);
        let reported = report(full.clone());
        let pods = pod_map(vec![managed_pod(
            "selected",
            "2024-01-01T00:00:00Z",
            &reported,
            DS,
        )]);
        let reports = member_reports(&pods, &node_map(), &ownership(), &[member()]);
        assert_eq!(reports[NODE_UID], reported);
        let progress = aggregate(&reports, &v);
        assert_eq!(progress.nodes, 1);
        assert_eq!(progress.resident_bytes, full.pinned_bytes);
    }

    #[test]
    fn status_waits_for_this_terms_member_inventory_and_ownership() {
        crate::kube::fake::run(|api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let term = crate::rules::state::Term {
                lease_uid: "lease".into(),
                transitions: 2,
                holder: "p1/00".into(),
            };
            shared.is_primary.set(true);
            *shared.term.borrow_mut() = Some((
                term.clone(),
                crate::rules::state::Freshness::confirmed(rt::now(), Duration::from_secs(3600)),
            ));
            shared.fills_synced.set(true);
            shared.fills_caches_synced.set(true);
            let v = with_manifest(view());
            shared.fills.borrow_mut().insert(v.uid.clone(), v.clone());
            api.put_object(
                &paths::cluster_cache_fill(&v.name),
                json!({"metadata": {"name": v.name, "uid": v.uid, "generation": v.generation}}),
            );
            let mut cursor = 0;
            let start = api.requests().len();
            reconcile(&shared, &mut cursor).await;
            assert_eq!(api.requests().len(), start, "unsynced member mirrors");
            let mut old = term.clone();
            old.transitions -= 1;
            *shared.fill_status_term.borrow_mut() = Some(old);
            reconcile(&shared, &mut cursor).await;
            assert_eq!(api.requests().len(), start, "previous term's mirrors");
            *shared.fill_status_term.borrow_mut() = Some(term);
            reconcile(&shared, &mut cursor).await;
            assert!(api.requests()[start..].iter().any(|(m, _)| m == "PUT"));
        });
    }

    #[test]
    fn round_robin_cursor_advances_after_a_failed_status_write() {
        crate::kube::fake::run(|api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            shared.is_primary.set(true);
            *shared.term.borrow_mut() = Some((
                crate::rules::state::Term {
                    lease_uid: "lease".into(),
                    transitions: 1,
                    holder: "p1/00".into(),
                },
                crate::rules::state::Freshness::confirmed(rt::now(), Duration::from_secs(3600)),
            ));
            shared.fills_synced.set(true);
            shared.fills_caches_synced.set(true);
            *shared.fill_status_term.borrow_mut() = shared.primary_term(rt::now());
            for name in ["fill-a", "fill-b"] {
                let mut v = view();
                v.name = name.into();
                v.uid = format!("{name}-uid");
                api.put_object(
                    &paths::cluster_cache_fill(name),
                    json!({
                        "metadata": {
                            "name": name,
                            "uid": v.uid,
                            "generation": v.generation,
                        },
                        "spec": {"cacheName": "models"},
                    }),
                );
                shared.fills.borrow_mut().insert(v.uid.clone(), v);
            }

            // Both writes conflict. The first fill stays dirty, but the
            // cursor still moves past it.
            let mut cursor = 0;
            api.fail_next_write(409);
            let start = api.requests().len();
            reconcile(&shared, &mut cursor).await;
            let first: Vec<_> = api.requests()[start..]
                .iter()
                .filter(|(method, _)| method == "PUT")
                .map(|(_, path)| path.clone())
                .collect();
            assert_eq!(first.len(), 1);
            assert!(first[0].ends_with("/fill-a/status"), "{first:?}");
            assert_eq!(cursor, 1);

            let start = api.requests().len();
            api.fail_next_write(409);
            reconcile(&shared, &mut cursor).await;
            let second: Vec<_> = api.requests()[start..]
                .iter()
                .filter(|(method, _)| method == "PUT")
                .map(|(_, path)| path.clone())
                .collect();
            assert_eq!(second.len(), 1);
            assert!(second[0].ends_with("/fill-b/status"), "{second:?}");
            assert_eq!(cursor, 0);
        });
    }

    #[test]
    fn stale_snapshot_is_not_written_onto_a_newer_fill() {
        crate::kube::fake::run(|api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            shared.is_primary.set(true);
            *shared.term.borrow_mut() = Some((
                crate::rules::state::Term {
                    lease_uid: "lease".into(),
                    transitions: 1,
                    holder: "p1/00".into(),
                },
                crate::rules::state::Freshness::confirmed(rt::now(), Duration::from_secs(3600)),
            ));
            shared.fills_synced.set(true);
            shared.fills_caches_synced.set(true);
            *shared.fill_status_term.borrow_mut() = shared.primary_term(rt::now());

            let mut v = view();
            v.name = "fill".into();
            v.uid = "old-uid".into();
            let path = paths::cluster_cache_fill("fill");
            shared.fills.borrow_mut().insert(v.uid.clone(), v.clone());

            // The live object was recreated under the same name: new UID.
            api.put_object(
                &path,
                json!({
                    "metadata": {
                        "name": "fill",
                        "uid": "new-uid",
                        "generation": v.generation,
                    },
                    "spec": {"cacheName": "models"},
                }),
            );
            let mut cursor = 0;
            let start = api.requests().len();
            reconcile(&shared, &mut cursor).await;
            assert!(
                api.requests()[start..].iter().all(|(m, _)| m == "GET"),
                "a stale UID must not be written"
            );
            assert_eq!(cursor, 0);

            // Same UID but a newer generation is stale too.
            api.put_object(
                &path,
                json!({
                    "metadata": {
                        "name": "fill",
                        "uid": v.uid,
                        "generation": v.generation + 1,
                    },
                    "spec": {"cacheName": "models"},
                }),
            );
            let start = api.requests().len();
            reconcile(&shared, &mut cursor).await;
            assert!(
                api.requests()[start..].iter().all(|(m, _)| m == "GET"),
                "a newer generation must not be written"
            );
            assert_eq!(cursor, 0);

            // The matching object is written, so the guard does not block the
            // normal path, and the status PUT succeeds.
            api.put_object(
                &path,
                json!({
                    "metadata": {
                        "name": "fill",
                        "uid": v.uid,
                        "generation": v.generation,
                    },
                    "spec": {"cacheName": "models"},
                }),
            );
            let start = api.requests().len();
            reconcile(&shared, &mut cursor).await;
            let writes: Vec<_> = api.requests()[start..]
                .iter()
                .filter(|(method, _)| method == "PUT")
                .map(|(_, path)| path.clone())
                .collect();
            assert_eq!(writes.len(), 1, "{writes:?}");
            assert!(writes[0].ends_with("/fill/status"), "{writes:?}");
            assert_eq!(cursor, 0);
            let written = api.object(&path).unwrap();
            assert_eq!(written["status"]["observedGeneration"], v.generation);
            assert_eq!(written["spec"]["cacheName"], "models");
            assert_eq!(written["metadata"]["uid"], v.uid);

            // A successful write is stable: its transition timestamps are
            // retained and the next pass makes no redundant status PUT.
            let start = api.requests().len();
            reconcile(&shared, &mut cursor).await;
            assert!(api.requests()[start..].iter().all(|(m, _)| m == "GET"));
        });
    }
}

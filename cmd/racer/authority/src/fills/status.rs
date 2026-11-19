//! ClusterCacheFill status computation and the primary's status writer.
//!
//! The primary aggregates node progress from Pod report annotations whose
//! generation, digest, and membership match its own, then writes the status
//! subresource with resourceVersion CAS.

use super::{FillView, snapshot};
use crate::{
    kube::{self, KubeError, paths},
    limits, rt,
    rules::{election, rails::FillProgress},
    shared::Shared,
};
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
        let Ok(report) = crate::rules::rails::parse_report(report) else {
            continue;
        };
        let entry: Option<&FillProgress> = report.fills.iter().find(|f| f.fill == view.uid);
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
    let was_ready = previous
        .map(|p| condition_status(p, "Ready") == Some("True"))
        .unwrap_or(false);
    let was_degraded = previous
        .map(|p| condition_status(p, "Degraded") == Some("True"))
        .unwrap_or(false);
    let previous_nodes = previous
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
    } else if view.accepted && manifest_current && missing_members {
        Some((
            "MemberLost",
            format!(
                "{} replicas requested but only {} nodes are available",
                view.replicas, view.available_nodes
            ),
        ))
    } else if (was_ready || was_degraded) && (short || under_replicated_bytes > 0) {
        if progress.nodes == 0 || previous_nodes > i64::from(progress.nodes) {
            Some(("MemberLost", "a node with pins left".to_owned()))
        } else {
            Some(("Refilling", "copies are missing".to_owned()))
        }
    } else {
        None
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
            &view.message
        },
    );
    let list_reason = match view.error.as_deref() {
        Some("Forbidden") => "Forbidden",
        Some(_) => "ListFailed",
        None => "ListFailed",
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
        } else if view.error.is_some() {
            list_reason
        } else {
            "manifest is not listed yet"
        },
    );
    let progressing = condition(
        view,
        previous,
        &now,
        "Progressing",
        !ready,
        if ready { "Complete" } else { "Filling" },
        if ready {
            "fill is complete"
        } else {
            "residency is below the planned footprint"
        },
    );
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
            "waiting for confirmed copies"
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
/// the primary Lease renewal stays on schedule.
pub async fn reconcile(shared: &Shared) {
    if shared.primary_term(rt::now()).is_none()
        || !shared.fills_synced.get()
        || !shared.fills_caches_synced.get()
    {
        return;
    }
    let views = snapshot(shared);
    if views.is_empty() {
        return;
    }
    let reports = shared.pod_reports.borrow().clone();
    for view in views {
        let current = match shared.api.get(&paths::cluster_cache_fill(&view.name)).await {
            Ok(v) => v,
            Err(KubeError::NotFound) => continue,
            Err(_) => return,
        };
        let resource_version = kube::str_at(&current, &["metadata", "resourceVersion"]).to_owned();
        if resource_version.is_empty() {
            continue;
        }
        let previous = current.get("status").filter(|s| s.is_object());
        let next = status(&view, &reports, previous);
        if previous == Some(&next) {
            continue;
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
    while !shared.stopping.get() {
        reconcile(&shared).await;
        rt::sleep(TICK).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::fills::manifest;
    use std::sync::Arc;

    fn view() -> FillView {
        FillView {
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
        let r = crate::rules::rails::Report {
            shares: std::num::NonZeroU32::new(1).unwrap(),
            rdma_nics: vec![],
            fills: vec![fill],
            pinnable_bytes: None,
        };
        crate::rules::rails::encode_report(&r).unwrap()
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
    }

    #[test]
    fn requested_replicas_above_pool_size_are_degraded_and_not_ready() {
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
        assert_eq!(c["Degraded"], ("True".into(), "MemberLost".into()));
        let message = s["conditions"]
            .as_array()
            .unwrap()
            .iter()
            .find(|c| c["type"] == "Degraded")
            .and_then(|c| c["message"].as_str())
            .unwrap();
        assert!(message.contains("2 replicas"), "{message}");
        assert!(message.contains("1 nodes"), "{message}");
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
}

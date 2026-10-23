//! Bearer token authorization: TokenReview plus live Pod, ServiceAccount,
//! DaemonSet and Node binding checks.
//!
//! TokenReview alone is not enough. Token extras name a Pod and Node; both are
//! read back from the API and must match by UID, and the Pod must be owned by
//! the live managed DaemonSet and run as the dataplane ServiceAccount.

use crate::{
    authority::members::{NodeInfo, Ownership, PodInfo},
    kube::{KubeError, paths, str_at},
    limits,
    shared::Shared,
};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use racer_control_wire::{ProtocolFailure, TOKEN_AUDIENCE};
use serde_json::{Value, json};

/// An authorized dataplane caller.
#[derive(Clone, Debug)]
pub struct Caller {
    pub node: NodeInfo,
    /// Token expiry, Unix seconds.
    pub expires: u64,
}

/// Extract a bearer token from an `Authorization` value.
pub fn bearer(value: Option<&str>) -> Result<&str, ProtocolFailure> {
    let v = value.ok_or(ProtocolFailure::Unauthenticated)?;
    let (scheme, token) = v.split_once(' ').ok_or(ProtocolFailure::Unauthenticated)?;
    if !scheme.eq_ignore_ascii_case("bearer")
        || token.is_empty()
        || token.len() > limits::MAX_TOKEN_BYTES
        || token.bytes().any(|b| b.is_ascii_whitespace() || b == b',')
    {
        return Err(ProtocolFailure::Unauthenticated);
    }
    Ok(token)
}

/// JWT `exp`, only to shorten authorization. Never trusted for identity.
pub fn token_expiry(token: &str, now: u64) -> Result<u64, ProtocolFailure> {
    let mut parts = token.split('.');
    let (Some(_), Some(payload), Some(_), None) =
        (parts.next(), parts.next(), parts.next(), parts.next())
    else {
        return Err(ProtocolFailure::Unauthenticated);
    };
    let bytes = URL_SAFE_NO_PAD
        .decode(payload.trim_end_matches('='))
        .map_err(|_| ProtocolFailure::Unauthenticated)?;
    let claims: Value =
        serde_json::from_slice(&bytes).map_err(|_| ProtocolFailure::Unauthenticated)?;
    let exp = claims["exp"]
        .as_u64()
        .ok_or(ProtocolFailure::Unauthenticated)?;
    if exp <= now {
        return Err(ProtocolFailure::Unauthenticated);
    }
    Ok(exp)
}

fn api(e: KubeError) -> ProtocolFailure {
    match e {
        KubeError::NotFound => ProtocolFailure::Forbidden,
        _ => ProtocolFailure::Unavailable,
    }
}

/// Validated TokenReview user.
pub struct User {
    pub uid: String,
    pub pod_name: String,
    pub pod_uid: String,
    pub node_name: String,
    pub node_uid: String,
}

fn single_extra(user: &Value, key: &str) -> String {
    match user["extra"][format!("authentication.kubernetes.io/{key}")].as_array() {
        Some(a) if a.len() == 1 => a[0].as_str().unwrap_or_default().to_owned(),
        _ => String::new(),
    }
}

/// Check a TokenReview status for the dataplane ServiceAccount.
pub fn check_review(
    status: &Value,
    namespace: &str,
    service_account: &str,
) -> Result<User, ProtocolFailure> {
    if status["authenticated"].as_bool() != Some(true)
        || status["error"].as_str().is_some_and(|e| !e.is_empty())
        || !status["audiences"]
            .as_array()
            .is_some_and(|a| a.iter().any(|v| v == TOKEN_AUDIENCE))
    {
        return Err(ProtocolFailure::Unauthenticated);
    }
    let user = &status["user"];
    if user["username"].as_str()
        != Some(&format!(
            "system:serviceaccount:{namespace}:{service_account}"
        ))
    {
        return Err(ProtocolFailure::Forbidden);
    }
    let u = User {
        uid: str_at(user, &["uid"]).into(),
        pod_name: single_extra(user, "pod-name"),
        pod_uid: single_extra(user, "pod-uid"),
        node_name: single_extra(user, "node-name"),
        node_uid: single_extra(user, "node-uid"),
    };
    if u.uid.is_empty() || u.pod_name.is_empty() || u.pod_uid.is_empty() {
        return Err(ProtocolFailure::Unauthenticated);
    }
    Ok(u)
}

/// TokenReview for the `racer-control` audience.
pub async fn review(shared: &Shared, token: &str) -> Result<User, ProtocolFailure> {
    let body = json!({
        "apiVersion": "authentication.k8s.io/v1",
        "kind": "TokenReview",
        "spec": {"token": token, "audiences": [TOKEN_AUDIENCE]},
    });
    let r = shared
        .api
        .post(paths::TOKEN_REVIEWS, &body)
        .await
        .map_err(|_| ProtocolFailure::Unavailable)?;
    check_review(
        &r["status"],
        &shared.config.namespace,
        &shared.config.service_account,
    )
}

/// Full bearer authorization of a dataplane Pod.
pub async fn authorize(
    shared: &Shared,
    header: Option<&str>,
    now: u64,
) -> Result<Caller, ProtocolFailure> {
    let token = bearer(header)?;
    let expires = token_expiry(token, now)?;
    let user = review(shared, token).await?;
    let ns = &shared.config.namespace;
    let pod = PodInfo::from_json(
        &shared
            .api
            .get(&paths::pod(ns, &user.pod_name))
            .await
            .map_err(api)?,
    );
    if pod.uid != user.pod_uid {
        return Err(ProtocolFailure::Forbidden);
    }
    let ds = shared
        .api
        .get(&paths::daemonset(ns, &shared.config.daemonset))
        .await
        .map_err(api)?;
    let ownership = Ownership {
        namespace: ns.clone(),
        daemonset: shared.config.daemonset.clone(),
        uid: if str_at(&ds, &["metadata", "deletionTimestamp"]).is_empty() {
            str_at(&ds, &["metadata", "uid"]).into()
        } else {
            String::new()
        },
        service_account: shared.config.service_account.clone(),
    };
    if !ownership.manages(&pod) {
        return Err(ProtocolFailure::Forbidden);
    }
    let sa = shared
        .api
        .get(&paths::service_account(ns, &shared.config.service_account))
        .await
        .map_err(api)?;
    if str_at(&sa, &["metadata", "uid"]) != user.uid
        || !str_at(&sa, &["metadata", "deletionTimestamp"]).is_empty()
    {
        return Err(ProtocolFailure::Forbidden);
    }
    let node = NodeInfo::from_json(
        &shared
            .api
            .get(&paths::node(&pod.node_name))
            .await
            .map_err(api)?,
    );
    if !node.authorized() || node.name != user.node_name || node.uid != user.node_uid {
        return Err(ProtocolFailure::Forbidden);
    }
    Ok(Caller { node, expires })
}

/// An authenticated seat holder calling the primary's replica endpoint.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SeatCaller {
    pub pod_name: String,
    pub pod_uid: String,
    /// Token expiry, Unix seconds.
    pub expires: u64,
}

impl SeatCaller {
    /// Reauthorize without API calls, for example after a long poll: the
    /// token has not expired, this process is still primary, and the same Pod
    /// name and UID still hold a live seat.
    #[allow(
        dead_code,
        reason = "the replica route adopts this when server.rs is integrated; remove then"
    )]
    pub fn still_authorized(&self, shared: &Shared, now: u64) -> Result<(), ProtocolFailure> {
        if self.expires <= now {
            return Err(ProtocolFailure::Unauthenticated);
        }
        if !shared.is_primary.get() {
            return Err(ProtocolFailure::Unavailable);
        }
        if !shared
            .seat_holders
            .borrow()
            .values()
            .any(|h| h.pod_uid == self.pod_uid && h.pod_name == self.pod_name)
        {
            return Err(ProtocolFailure::Forbidden);
        }
        Ok(())
    }
}

/// Authorize a seat holder calling the primary's replica endpoint and return
/// its identity for [`SeatCaller::still_authorized`].
pub async fn authorize_seat_caller(
    shared: &Shared,
    header: Option<&str>,
    now: u64,
) -> Result<SeatCaller, ProtocolFailure> {
    let token = bearer(header)?;
    let expires = token_expiry(token, now)?;
    let user = review(shared, token).await?;
    let caller = SeatCaller {
        pod_name: user.pod_name,
        pod_uid: user.pod_uid,
        expires,
    };
    // Checks seat membership; primary role is checked by the route.
    if !shared
        .seat_holders
        .borrow()
        .values()
        .any(|h| h.pod_uid == caller.pod_uid && h.pod_name == caller.pod_name)
    {
        return Err(ProtocolFailure::Forbidden);
    }
    Ok(caller)
}

/// Like [`authorize_seat_caller`], returning only the token expiry.
pub async fn authorize_seat(
    shared: &Shared,
    header: Option<&str>,
    now: u64,
) -> Result<u64, ProtocolFailure> {
    authorize_seat_caller(shared, header, now)
        .await
        .map(|c| c.expires)
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    pub fn jwt(exp: u64) -> String {
        format!(
            "e30.{}.sig",
            URL_SAFE_NO_PAD.encode(format!("{{\"exp\":{exp}}}"))
        )
    }

    #[test]
    fn bearer_parsing() {
        assert_eq!(bearer(Some("Bearer abc")), Ok("abc"));
        assert_eq!(bearer(Some("bearer abc")), Ok("abc"));
        for bad in [
            None,
            Some("Basic x"),
            Some("Bearer "),
            Some("Bearer a b"),
            Some("Bearer a,b"),
            Some("Bearer"),
        ] {
            assert_eq!(
                bearer(bad),
                Err(ProtocolFailure::Unauthenticated),
                "{bad:?}"
            );
        }
        let long = format!("Bearer {}", "a".repeat(limits::MAX_TOKEN_BYTES + 1));
        assert!(bearer(Some(&long)).is_err());
    }

    #[test]
    fn expiry() {
        assert_eq!(token_expiry(&jwt(100), 50), Ok(100));
        assert!(token_expiry(&jwt(100), 100).is_err());
        assert!(token_expiry("a.b", 0).is_err());
        assert!(token_expiry("a.!!.c", 0).is_err());
    }

    pub fn review_status(user: &str) -> Value {
        json!({
            "authenticated": true,
            "audiences": ["racer-control"],
            "user": {
                "username": user,
                "uid": "sa-uid",
                "extra": {
                    "authentication.kubernetes.io/pod-name": ["p"],
                    "authentication.kubernetes.io/pod-uid": ["pu"],
                    "authentication.kubernetes.io/node-name": ["n"],
                    "authentication.kubernetes.io/node-uid": ["nu"]
                }
            }
        })
    }

    #[test]
    fn review_checks() {
        let ok = review_status("system:serviceaccount:ns:sa");
        let u = check_review(&ok, "ns", "sa").unwrap();
        assert_eq!((u.pod_name.as_str(), u.node_uid.as_str()), ("p", "nu"));
        assert_eq!(
            check_review(&ok, "ns", "other").err(),
            Some(ProtocolFailure::Forbidden)
        );
        let mut bad = ok.clone();
        bad["audiences"] = json!(["api"]);
        assert_eq!(
            check_review(&bad, "ns", "sa").err(),
            Some(ProtocolFailure::Unauthenticated)
        );
        let mut bad = ok.clone();
        bad["authenticated"] = false.into();
        assert!(check_review(&bad, "ns", "sa").is_err());
        let mut bad = ok.clone();
        bad["user"]["extra"]["authentication.kubernetes.io/pod-uid"] = json!(["a", "b"]);
        assert_eq!(
            check_review(&bad, "ns", "sa").err(),
            Some(ProtocolFailure::Unauthenticated)
        );
    }

    #[test]
    fn live_bindings_are_all_checked() {
        crate::kube::fake::run(|api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let ns = "racer-system";
            let seed = |pod_uid: &str, ds_uid: &str, sa_uid: &str, node_uid: &str| {
                api.put_object(
                    &paths::daemonset(ns, "racer-dataplane"),
                    json!({"metadata":{"uid":ds_uid}}),
                );
                api.put_object(
                    &paths::service_account(ns, "racer-dataplane"),
                    json!({"metadata":{"uid":sa_uid}}),
                );
                api.put_object(
                    &paths::node("n"),
                    json!({"metadata":{"name":"n","uid":node_uid}}),
                );
                api.put_object(&paths::pod(ns, "p"), json!({
                    "metadata":{"name":"p","namespace":ns,"uid":pod_uid,
                        "ownerReferences":[{"apiVersion":"apps/v1","kind":"DaemonSet","name":"racer-dataplane","uid":"ds","controller":true}]},
                    "spec":{"nodeName":"n","serviceAccountName":"racer-dataplane"},
                    "status":{"phase":"Running","podIP":"10.0.0.1"}}));
            };
            api.on_post(|_, _| {
                let mut s = review_status("system:serviceaccount:racer-system:racer-dataplane");
                s["user"]["uid"] = "sa".into();
                s["user"]["extra"]["authentication.kubernetes.io/node-uid"] =
                    json!(["11111111-1111-4111-8111-111111111111"]);
                Some((201, json!({"status": s})))
            });
            let header = format!("Bearer {}", jwt(u64::MAX / 2));
            let node = "11111111-1111-4111-8111-111111111111";
            seed("pu", "ds", "sa", node);
            let ok = authorize(&shared, Some(&header), 1).await.unwrap();
            assert_eq!(ok.node.uid, node);
            for (pod, ds, sa, n, want) in [
                ("other", "ds", "sa", node, ProtocolFailure::Forbidden),
                ("pu", "ds2", "sa", node, ProtocolFailure::Forbidden),
                ("pu", "ds", "sa2", node, ProtocolFailure::Forbidden),
                (
                    "pu",
                    "ds",
                    "sa",
                    "22222222-2222-4222-8222-222222222222",
                    ProtocolFailure::Forbidden,
                ),
            ] {
                seed(pod, ds, sa, n);
                assert_eq!(
                    authorize(&shared, Some(&header), 1).await.err(),
                    Some(want),
                    "{pod} {ds} {sa} {n}"
                );
            }
            seed("pu", "ds", "sa", node);
            assert_eq!(
                authorize(&shared, Some(&format!("Bearer {}", jwt(1))), 5)
                    .await
                    .err(),
                Some(ProtocolFailure::Unauthenticated)
            );
        });
    }

    #[test]
    fn seat_caller_reauthorization() {
        crate::kube::fake::run(|api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            api.on_post(|_, _| {
                Some((
                    201,
                    json!({"status": review_status("system:serviceaccount:racer-system:racer-dataplane")}),
                ))
            });
            let header = format!("Bearer {}", jwt(1000));
            // Not a seat holder.
            assert_eq!(
                authorize_seat_caller(&shared, Some(&header), 1).await.err(),
                Some(ProtocolFailure::Forbidden)
            );
            let holder = |uid: &str| crate::shared::SeatHolder {
                pod_name: "p".into(),
                pod_uid: uid.into(),
            };
            shared.seat_holders.borrow_mut().insert(1, holder("pu"));
            shared.is_primary.set(true);
            let c = authorize_seat_caller(&shared, Some(&header), 1)
                .await
                .unwrap();
            assert_eq!(
                (c.pod_name.as_str(), c.pod_uid.as_str(), c.expires),
                ("p", "pu", 1000)
            );
            assert_eq!(authorize_seat(&shared, Some(&header), 1).await, Ok(1000));
            assert_eq!(c.still_authorized(&shared, 999), Ok(()));
            assert_eq!(
                c.still_authorized(&shared, 1000),
                Err(ProtocolFailure::Unauthenticated)
            );
            // Seat taken over by a new Pod with the same name.
            shared.seat_holders.borrow_mut().insert(1, holder("pu2"));
            assert_eq!(
                c.still_authorized(&shared, 2),
                Err(ProtocolFailure::Forbidden)
            );
            shared.seat_holders.borrow_mut().insert(1, holder("pu"));
            shared.is_primary.set(false);
            assert_eq!(
                c.still_authorized(&shared, 2),
                Err(ProtocolFailure::Unavailable)
            );
        });
    }
}

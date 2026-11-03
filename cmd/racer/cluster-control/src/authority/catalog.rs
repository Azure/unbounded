//! Cache catalog from ClusterCaches, like members.BuildCatalog in the old controller.

use crate::kube;
use racer_control_wire::{self as wire, CacheDefinition, CacheId};
use serde_json::Value;
use std::collections::HashSet;

/// ClusterCache fields used here.
#[derive(Clone, Debug, Default)]
pub struct CacheInfo {
    pub uid: String,
    pub name: String,
    /// spec.priority; None when unset or not an integer.
    pub priority: Option<i64>,
    /// spec.passthrough as written; None when unset. Parsed by `build` so an
    /// invalid value fails the catalog instead of silently caching.
    pub passthrough: Option<Value>,
    /// spec.origin as written; None when unset. Parsed by `build` like
    /// passthrough, so an invalid origin fails the catalog.
    pub origin: Option<Value>,
}

impl CacheInfo {
    pub fn from_json(v: &Value) -> Self {
        Self {
            uid: kube::str_at(v, &["metadata", "uid"]).into(),
            name: kube::str_at(v, &["metadata", "name"]).into(),
            priority: v.pointer("/spec/priority").and_then(Value::as_i64),
            passthrough: v
                .pointer("/spec/passthrough")
                .filter(|p| !p.is_null())
                .cloned(),
            origin: v.pointer("/spec/origin").filter(|o| !o.is_null()).cloned(),
        }
    }
}

/// Map spec.passthrough to a cache mode. Absence caches; an object without a
/// route uses the CRD default, Peers. Anything else is invalid.
fn mode(passthrough: Option<&Value>) -> Result<wire::CacheMode, wire::Error> {
    let Some(passthrough) = passthrough else {
        return Ok(wire::CacheMode::Cached);
    };
    let fields = passthrough.as_object().ok_or(wire::Error::InvalidRequest)?;
    match fields.get("route") {
        None | Some(Value::Null) => Ok(wire::CacheMode::PassthroughPeers),
        Some(Value::String(route)) if route == "Peers" => Ok(wire::CacheMode::PassthroughPeers),
        Some(Value::String(route)) if route == "Local" => Ok(wire::CacheMode::PassthroughLocal),
        Some(_) => Err(wire::Error::InvalidRequest),
    }
}

/// Map spec.origin (the ClusterCache ObjectOrigin) to an origin definition.
/// Unset tuning fields take the racer-object defaults. The removed
/// `credential.default.secretRef` is invalid: the dataplane cannot read it.
fn origin_definition(
    origin: Option<&Value>,
) -> Result<Option<wire::OriginDefinition>, wire::Error> {
    let invalid = wire::Error::InvalidRequest;
    let Some(origin) = origin else {
        return Ok(None);
    };
    let present = |v: &Value, key: &str| v.get(key).filter(|x| !x.is_null()).cloned();
    let string = |v: &Value, key: &str| -> Result<Option<String>, wire::Error> {
        match present(v, key) {
            None => Ok(None),
            Some(Value::String(s)) if !s.is_empty() => Ok(Some(s)),
            Some(Value::String(_)) => Ok(None),
            Some(_) => Err(invalid),
        }
    };
    let duration = |key: &str, default| -> Result<std::time::Duration, wire::Error> {
        match present(origin, key) {
            None => Ok(default),
            Some(Value::String(s)) => go_duration(&s).ok_or(invalid),
            Some(_) => Err(invalid),
        }
    };
    if !origin.is_object() {
        return Err(invalid);
    }
    let azure = present(origin, "azure").ok_or(invalid)?;
    let url = string(&azure, "containerURL")?.ok_or(invalid)?;
    let credential = match present(&azure, "credential") {
        None => wire::AzureCredential::Anonymous,
        Some(c) => match (present(&c, "default"), present(&c, "workloadIdentity")) {
            (Some(d), None) if d.is_object() => {
                if present(&d, "secretRef").is_some() {
                    return Err(invalid);
                }
                wire::AzureCredential::ManagedIdentity {
                    client_id: string(&d, "clientID")?,
                }
            }
            (None, Some(w)) if w.is_object() => wire::AzureCredential::WorkloadIdentity {
                client_id: string(&w, "clientID")?.ok_or(invalid)?,
                tenant_id: string(&w, "tenantID")?,
            },
            _ => return Err(invalid),
        },
    };
    let max_concurrent_requests = match present(origin, "maxConcurrentRequests") {
        None => wire::DEFAULT_ORIGIN_MAX_CONCURRENT_REQUESTS,
        Some(n) => n
            .as_u64()
            .and_then(|n| u32::try_from(n).ok())
            .ok_or(invalid)?,
    };
    let definition = wire::OriginDefinition {
        azure: wire::AzureOriginDefinition {
            container_url: wire::AzureContainerUrl::parse(&url)?,
            credential,
        },
        metadata_ttl: duration("metadataTTL", wire::DEFAULT_ORIGIN_METADATA_TTL)?,
        request_timeout: duration("requestTimeout", wire::DEFAULT_ORIGIN_REQUEST_TIMEOUT)?,
        max_concurrent_requests,
    };
    definition.validate()?;
    Ok(Some(definition))
}

/// Parse a Go `time.Duration` string such as "30s", "1m30s", or "1.5h", as
/// written by metav1.Duration. Negative durations and durations that do not
/// fit the wire's millisecond precision are rejected; "-0" is zero.
fn go_duration(s: &str) -> Option<std::time::Duration> {
    let (negative, mut rest) = match s.as_bytes().first()? {
        b'-' => (true, &s[1..]),
        b'+' => (false, &s[1..]),
        _ => (false, s),
    };
    if rest == "0" {
        return Some(std::time::Duration::ZERO);
    }
    if rest.is_empty() {
        return None;
    }
    let mut total: u128 = 0;
    while !rest.is_empty() {
        let digits = |r: &str| r.bytes().take_while(u8::is_ascii_digit).count();
        let n = digits(rest);
        let (whole, after) = rest.split_at(n);
        let (fraction, after) = match after.strip_prefix('.') {
            Some(after) => after.split_at(digits(after)),
            None => ("", after),
        };
        if whole.is_empty() && fraction.is_empty() {
            return None;
        }
        let unit_len = after
            .bytes()
            .take_while(|b| *b != b'.' && !b.is_ascii_digit())
            .count();
        let (unit, after) = after.split_at(unit_len);
        let scale: u128 = match unit {
            "ns" => 1,
            "us" | "\u{b5}s" | "\u{3bc}s" => 1_000,
            "ms" => 1_000_000,
            "s" => 1_000_000_000,
            "m" => 60_000_000_000,
            "h" => 3_600_000_000_000,
            _ => return None,
        };
        // Go caps each component and the total at 1<<63 - 1 nanoseconds.
        let limit = u128::from(i64::MAX as u64);
        let whole: u128 = if whole.is_empty() {
            0
        } else {
            whole.parse().ok()?
        };
        let mut value = whole.checked_mul(scale)?;
        // Go keeps the fraction digits that fit and ignores the rest. The
        // first 25 digits already resolve far below one nanosecond for every
        // unit (scale < 4e12), and 10^25 * scale stays well inside u128.
        let (mut numerator, mut denominator) = (0u128, 1u128);
        for b in fraction.bytes().take(25) {
            numerator = numerator
                .checked_mul(10)?
                .checked_add(u128::from(b - b'0'))?;
            denominator = denominator.checked_mul(10)?;
        }
        value = value.checked_add(numerator.checked_mul(scale)? / denominator)?;
        total = total.checked_add(value)?;
        if value > limit || total > limit {
            return None;
        }
        rest = after;
    }
    if negative && total != 0 {
        return None;
    }
    let total = u64::try_from(total).ok()?;
    millisecond_precision(std::time::Duration::from_nanos(total))
}

/// Accept only durations the wire can carry exactly in milliseconds.
fn millisecond_precision(d: std::time::Duration) -> Option<std::time::Duration> {
    d.subsec_nanos().is_multiple_of(1_000_000).then_some(d)
}

/// Build the catalog sorted by UID. Any invalid cache fails the whole catalog.
pub fn build(caches: &[CacheInfo]) -> Result<Vec<CacheDefinition>, wire::Error> {
    let (mut ids, mut names) = (HashSet::new(), HashSet::new());
    let mut out = Vec::with_capacity(caches.len());
    for c in caches {
        if !wire::valid_uuid(&c.uid) || !ids.insert(&c.uid) || !names.insert(&c.name) {
            return Err(wire::Error::InvalidRequest);
        }
        let (client, origin) = wire::canonical_socket_paths(&c.name)?;
        // The CRD bounds spec.priority; recheck so a skewed schema cannot publish
        // a catalog that every subscriber would reject.
        let priority = c.priority.unwrap_or(i64::from(wire::DEFAULT_PRIORITY));
        if !(0..=i64::from(wire::MAX_PRIORITY)).contains(&priority) {
            return Err(wire::Error::InvalidRequest);
        }
        out.push(CacheDefinition {
            id: CacheId(c.uid.clone()),
            name: c.name.clone(),
            client_socket: client,
            origin_socket: origin,
            priority: priority as u16,
            mode: mode(c.passthrough.as_ref())?,
            origin: origin_definition(c.origin.as_ref())?,
        });
    }
    out.sort_by(|a, b| a.id.cmp(&b.id));
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn c(uid: &str, name: &str) -> CacheInfo {
        CacheInfo {
            uid: uid.into(),
            name: name.into(),
            priority: None,
            passthrough: None,
            origin: None,
        }
    }

    #[test]
    fn sorted_by_uid() {
        let out = build(&[
            c("22222222-2222-4222-8222-222222222222", "b"),
            c("11111111-1111-4111-8111-111111111111", "a"),
        ])
        .unwrap();
        assert_eq!(out[0].name, "a");
        assert_eq!(out[1].name, "b");
        let info = CacheInfo::from_json(&serde_json::json!({"metadata":{"uid":"u","name":"n"}}));
        assert_eq!((info.uid.as_str(), info.name.as_str()), ("u", "n"));
        assert_eq!(info.priority, None);
    }

    #[test]
    fn mixed_priorities_sorted_by_uid() {
        let with = |uid: &str, name: &str, priority: Option<i64>| CacheInfo {
            priority,
            ..c(uid, name)
        };
        let out = build(&[
            with("33333333-3333-4333-8333-333333333333", "max", Some(1000)),
            with("11111111-1111-4111-8111-111111111111", "min", Some(0)),
            with("22222222-2222-4222-8222-222222222222", "unset", None),
        ])
        .unwrap();
        let got: Vec<_> = out.iter().map(|d| (d.name.as_str(), d.priority)).collect();
        assert_eq!(
            got,
            [
                ("min", 0),
                ("unset", wire::DEFAULT_PRIORITY),
                ("max", wire::MAX_PRIORITY)
            ]
        );
    }

    #[test]
    fn priority_defaults_and_bounds() {
        let u = "11111111-1111-4111-8111-111111111111";
        let info = CacheInfo::from_json(
            &serde_json::json!({"metadata":{"uid":u,"name":"a"},"spec":{"priority":0}}),
        );
        assert_eq!(info.priority, Some(0));
        assert_eq!(build(&[info]).unwrap()[0].priority, 0);
        assert_eq!(
            build(&[c(u, "a")]).unwrap()[0].priority,
            wire::DEFAULT_PRIORITY
        );
        for (p, ok) in [(1000, true), (1001, false), (-1, false)] {
            let mut x = c(u, "a");
            x.priority = Some(p);
            assert_eq!(build(&[x]).is_ok(), ok, "priority {p}");
        }
    }

    #[test]
    fn passthrough_route_maps_to_mode() {
        let u = "11111111-1111-4111-8111-111111111111";
        let parse = |spec: Value| {
            let info = CacheInfo::from_json(
                &serde_json::json!({"metadata":{"uid":u,"name":"a"},"spec":spec}),
            );
            build(&[info]).map(|out| out[0].mode)
        };
        assert_eq!(parse(serde_json::json!({})), Ok(wire::CacheMode::Cached));
        assert_eq!(
            parse(serde_json::json!({"passthrough":null})),
            Ok(wire::CacheMode::Cached)
        );
        assert_eq!(
            build(&[c(u, "a")]).unwrap()[0].mode,
            wire::CacheMode::Cached
        );
        for (spec, mode) in [
            (serde_json::json!({}), wire::CacheMode::PassthroughPeers),
            (
                serde_json::json!({"route":null}),
                wire::CacheMode::PassthroughPeers,
            ),
            (
                serde_json::json!({"route":"Peers"}),
                wire::CacheMode::PassthroughPeers,
            ),
            (
                serde_json::json!({"route":"Local"}),
                wire::CacheMode::PassthroughLocal,
            ),
        ] {
            assert_eq!(
                parse(serde_json::json!({"passthrough":spec})),
                Ok(mode),
                "{spec}"
            );
        }
        for bad in [
            serde_json::json!({"route":"peers"}),
            serde_json::json!({"route":"local"}),
            serde_json::json!({"route":""}),
            serde_json::json!({"route":"Mesh"}),
            serde_json::json!({"route":1}),
            serde_json::json!(true),
            serde_json::json!("Peers"),
            serde_json::json!([]),
        ] {
            assert!(
                parse(serde_json::json!({"passthrough":bad.clone()})).is_err(),
                "{bad}"
            );
        }
    }

    #[test]
    fn rejects_invalid() {
        let u = "11111111-1111-4111-8111-111111111111";
        assert!(build(&[c("bad", "a")]).is_err());
        assert!(build(&[c(u, "a"), c(u, "b")]).is_err());
        assert!(build(&[c(u, "a"), c("22222222-2222-4222-8222-222222222222", "a")]).is_err());
        assert!(build(&[c(u, "A/B")]).is_err());
    }

    #[test]
    fn go_durations() {
        let ms = std::time::Duration::from_millis;
        for (s, d) in [
            ("0", ms(0)),
            ("-0", ms(0)),
            ("0s", ms(0)),
            ("30s", ms(30_000)),
            ("1m30s", ms(90_000)),
            ("1h", ms(3_600_000)),
            ("1.5s", ms(1_500)),
            (".5s", ms(500)),
            ("1.s", ms(1_000)),
            ("+2ms", ms(2)),
            ("1000000ns", ms(1)),
            ("1000us", ms(1)),
            ("1000\u{b5}s", ms(1)),
            ("1000\u{3bc}s", ms(1)),
            ("1h2m3s4ms", ms(3_723_004)),
            // Long fractions keep the precision that fits and ignore the rest.
            ("0.100000000000000000000000000000h", ms(360_000)),
            ("0.500000000000000000000000000000000000000000m", ms(30_000)),
            ("1.250000000000000000000000000000000000000000s", ms(1_250)),
            ("1.000000000000000000000000000000000000000009ms", ms(1)),
            ("1000.000000000000000000000000000000000000009us", ms(1)),
            ("1000000.00000000000000000000000000000000009ns", ms(1)),
        ] {
            assert_eq!(go_duration(s), Some(d), "{s}");
        }
        for s in [
            "",
            "-",
            "+",
            "s",
            "1",
            "30",
            "1x",
            "-1s",
            ".s",
            "1..5s",
            "1s ",
            " 1s",
            "1ms5",
            "1500us",
            "1ns",
            "9223372036854775808ns",
            "3000000h",
            "1S",
            // Truncated to 999999ns, below millisecond precision.
            "0.999999999999999999999999999999999999999999ms",
            // Overflow is rejected without a panic.
            "2562048h",
            "2562047.9999999999999999999999999999999h",
            "99999999999999999999999999999999999999999h",
            "9223372036854775807ns1ns",
            "2562047h2562047h",
        ] {
            assert_eq!(go_duration(s), None, "{s}");
        }
    }

    #[test]
    fn origin_maps_to_definition() {
        let u = "11111111-1111-4111-8111-111111111111";
        let g = "0123abcd-0123-4abc-89ab-0123456789ab";
        let parse = |origin: Value| {
            let info = CacheInfo::from_json(
                &serde_json::json!({"metadata":{"uid":u,"name":"a"},"spec":{"origin":origin}}),
            );
            build(&[info]).map(|out| out[0].origin.clone())
        };
        let url = "https://acct.blob.core.windows.net/models";
        let azure = |credential: Value| serde_json::json!({"azure":{"containerURL":url,"credential":credential}});

        assert_eq!(parse(Value::Null), Ok(None));
        assert_eq!(build(&[c(u, "a")]).unwrap()[0].origin, None);
        let o = parse(serde_json::json!({"azure":{"containerURL":url}}))
            .unwrap()
            .unwrap();
        assert_eq!(
            o,
            wire::OriginDefinition::azure(
                wire::AzureContainerUrl::parse(url).unwrap(),
                wire::AzureCredential::Anonymous
            )
        );
        let o = parse(serde_json::json!({
            "metadataTTL":"1m30s","requestTimeout":"5s","maxConcurrentRequests":7,
            "azure":{"containerURL":"http://127.0.0.1:10000/acct/models"}
        }))
        .unwrap()
        .unwrap();
        assert_eq!(o.metadata_ttl, std::time::Duration::from_secs(90));
        assert_eq!(o.request_timeout, std::time::Duration::from_secs(5));
        assert_eq!(o.max_concurrent_requests, 7);
        assert_eq!(o.azure.container_url.account_path(), Some("acct"));

        for (credential, want) in [
            (
                serde_json::json!({"default":{}}),
                wire::AzureCredential::ManagedIdentity { client_id: None },
            ),
            (
                serde_json::json!({"default":{"clientID":g}}),
                wire::AzureCredential::ManagedIdentity {
                    client_id: Some(g.into()),
                },
            ),
            (
                serde_json::json!({"workloadIdentity":{"clientID":g}}),
                wire::AzureCredential::WorkloadIdentity {
                    client_id: g.into(),
                    tenant_id: None,
                },
            ),
            (
                serde_json::json!({"workloadIdentity":{"clientID":g,"tenantID":g}}),
                wire::AzureCredential::WorkloadIdentity {
                    client_id: g.into(),
                    tenant_id: Some(g.into()),
                },
            ),
        ] {
            assert_eq!(
                parse(azure(credential.clone()))
                    .unwrap()
                    .unwrap()
                    .azure
                    .credential,
                want,
                "{credential}"
            );
        }

        for bad in [
            serde_json::json!({}),
            serde_json::json!("x"),
            serde_json::json!({"azure":{}}),
            serde_json::json!({"azure":{"containerURL":"https://acct/Models"}}),
            serde_json::json!({"azure":{"containerURL":"http://acct/models","credential":{"default":{}}}}),
            serde_json::json!({"azure":{"containerURL":url},"metadataTTL":"-1s"}),
            serde_json::json!({"azure":{"containerURL":url},"metadataTTL":30}),
            serde_json::json!({"azure":{"containerURL":url},"requestTimeout":"0s"}),
            serde_json::json!({"azure":{"containerURL":url},"requestTimeout":"1500us"}),
            serde_json::json!({"azure":{"containerURL":url},"maxConcurrentRequests":0}),
            serde_json::json!({"azure":{"containerURL":url},"maxConcurrentRequests":65537}),
            serde_json::json!({"azure":{"containerURL":url},"maxConcurrentRequests":-1}),
            serde_json::json!({"azure":{"containerURL":url},"maxConcurrentRequests":"8"}),
            azure(serde_json::json!({})),
            azure(serde_json::json!({"default":{},"workloadIdentity":{"clientID":g}})),
            azure(serde_json::json!({"default":{"secretRef":{"name":"s"}}})),
            azure(serde_json::json!({"default":{"clientID":"nope"}})),
            azure(serde_json::json!({"workloadIdentity":{}})),
            azure(serde_json::json!({"workloadIdentity":{"clientID":g,"tenantID":"x"}})),
        ] {
            assert!(parse(bad.clone()).is_err(), "{bad}");
        }

        // An invalid origin fails the whole catalog, like any other invalid cache.
        let mut bad = c("22222222-2222-4222-8222-222222222222", "b");
        bad.origin = Some(serde_json::json!({"azure":{}}));
        assert!(build(&[c(u, "a"), bad]).is_err());
    }
}

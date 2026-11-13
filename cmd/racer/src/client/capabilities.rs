//! Serve `GET /v1/capabilities` on the client socket: which delivery modes
//! the dataplane can offer, so a client can choose before it attempts a
//! `gpu-read`. The answer never starts the GPU helper and never waits on a
//! start in flight. The connection closes after the response.

use crate::error::Result;
use crate::http::{ConnectionLease, HttpIo};
use crate::runtime::Cancellation;
use http1::MessageHead;
use std::time::Duration;

/// The capabilities route.
pub(crate) const CAPABILITIES_PATH: &str = "/v1/capabilities";

/// The response schema version.
const SCHEMA: u32 = 1;

/// Page cipher names this build decodes. Every page is sealed, so
/// `xaes256gcm` is the only one.
const PAGE_CIPHERS: [&'static str; 1] = ["xaes256gcm"];

/// The capabilities response body.
#[derive(serde::Serialize)]
struct Capabilities {
    schema: u32,
    delivery: Vec<&'static str>,
    gpu_uuids: Vec<String>,
    page_bytes: u64,
    page_ciphers: [&'static str; 1],
}

/// Whether a request head selects the capabilities route.
pub(super) fn is_capabilities(head: &MessageHead) -> bool {
    matches!(
        &head.start,
        http1::StartLine::Request { method, target }
            if method == "GET" && target == CAPABILITIES_PATH
    )
}

/// The JSON body for a node that can target `gpu_uuids` (sorted and
/// deduplicated here). `gpu` is offered only when the `cuda` feature is
/// built and at least one GPU is known.
pub(crate) fn body(built: bool, gpu_uuids: Vec<String>) -> Vec<u8> {
    let mut gpu_uuids = if built { gpu_uuids } else { Vec::new() };
    gpu_uuids.sort();
    gpu_uuids.dedup();
    let mut delivery = Vec::with_capacity(3);
    if !gpu_uuids.is_empty() {
        delivery.push("gpu");
    }
    delivery.extend(["host", "stream"]);
    let capabilities = Capabilities {
        schema: SCHEMA,
        delivery,
        gpu_uuids,
        page_bytes: crate::model::PAGE_BYTES,
        page_ciphers: PAGE_CIPHERS,
    };
    serde_json::to_vec(&capabilities).unwrap_or_default()
}

/// GPU UUIDs the NVIDIA kernel driver reports under `root`
/// (`/proc/driver/nvidia/gpus`), without loading CUDA. Empty when the
/// driver is absent.
pub(crate) fn procfs_gpu_uuids(root: &std::path::Path) -> Vec<String> {
    let Ok(entries) = std::fs::read_dir(root) else {
        return Vec::new();
    };
    entries
        .flatten()
        .filter_map(|entry| std::fs::read_to_string(entry.path().join("information")).ok())
        .filter_map(|text| {
            text.lines().find_map(|line| {
                let (key, value) = line.split_once(':')?;
                let value = value.trim();
                let id = value.strip_prefix("GPU-")?;
                (key.trim() == "GPU UUID").then(|| format!("GPU-{}", id.to_ascii_lowercase()))
            })
        })
        .collect()
}

/// Answer one capabilities request and close the connection.
pub(super) async fn serve(
    mut connection: ConnectionLease,
    body: Vec<u8>,
    io: &HttpIo,
    timeout: Duration,
    cancellation: Cancellation,
) -> Result<()> {
    let scope = super::listener::new_scope(io.reactor().ctx(), timeout, cancellation)?;
    connection.poison();
    let head = racer_wire::object::prefetch::json_head(200, body.len());
    let sent = io.send_head(connection, head, &scope).await?;
    let _ = io.write_body_bytes(sent.connection, &body, &scope).await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn head(method: &str, target: &str) -> MessageHead {
        MessageHead {
            start: http1::StartLine::Request {
                method: method.into(),
                target: target.into(),
            },
            headers: Vec::new(),
        }
    }

    #[test]
    fn route_matches_only_get_capabilities() {
        assert!(is_capabilities(&head("GET", "/v1/capabilities")));
        assert!(!is_capabilities(&head("POST", "/v1/capabilities")));
        assert!(!is_capabilities(&head("GET", "/v1/capabilities/x")));
        assert!(!is_capabilities(&head("GET", "/v1/prefetches")));
        assert!(!is_capabilities(&head("GET", "/v1/objects")));
    }

    #[test]
    fn body_without_gpu() {
        let text = String::from_utf8(body(true, Vec::new())).unwrap();
        assert_eq!(
            text,
            format!(
                r#"{{"schema":1,"delivery":["host","stream"],"gpu_uuids":[],"page_bytes":{},"page_ciphers":["xaes256gcm"]}}"#,
                crate::model::PAGE_BYTES
            )
        );
    }

    #[test]
    fn body_with_gpus_sorts_and_offers_gpu() {
        let uuids = vec![
            "GPU-b".to_string(),
            "GPU-a".to_string(),
            "GPU-b".to_string(),
        ];
        let value: serde_json::Value = serde_json::from_slice(&body(true, uuids.clone())).unwrap();
        assert_eq!(
            value["delivery"],
            serde_json::json!(["gpu", "host", "stream"])
        );
        assert_eq!(value["gpu_uuids"], serde_json::json!(["GPU-a", "GPU-b"]));
        // Without the `cuda` feature no GPU is offered.
        let value: serde_json::Value = serde_json::from_slice(&body(false, uuids)).unwrap();
        assert_eq!(value["delivery"], serde_json::json!(["host", "stream"]));
        assert_eq!(value["gpu_uuids"], serde_json::json!([]));
    }

    #[test]
    fn procfs_reads_driver_uuids() {
        let dir = std::env::temp_dir().join(format!("racer-caps-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        for (bus, uuid) in [("0000:3b:00.0", "GPU-BBBB"), ("0000:1a:00.0", "GPU-aaaa")] {
            std::fs::create_dir_all(dir.join(bus)).unwrap();
            std::fs::write(
                dir.join(bus).join("information"),
                format!("Model: \t\t X\nGPU UUID: \t {uuid}\nBus Location: \t {bus}\n"),
            )
            .unwrap();
        }
        std::fs::create_dir_all(dir.join("broken")).unwrap();
        let mut got = procfs_gpu_uuids(&dir);
        got.sort();
        assert_eq!(got, ["GPU-aaaa", "GPU-bbbb"]);
        assert!(procfs_gpu_uuids(&dir.join("missing")).is_empty());
        std::fs::remove_dir_all(&dir).unwrap();
    }
}

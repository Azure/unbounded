//! Local performance controller, reusing the executable restart TLS fixture.
//! Only control-plane responses are fixtures; the dataplane is a separate binary.
use std::{
    collections::BTreeMap,
    fs,
    io::{self, Read, Write},
    net::TcpListener,
    path::Path,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    thread,
    time::Duration,
};

const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
const NODE: &str = "22222222-2222-4222-8222-222222222222";
const CACHE: &str = "44444444-4444-4444-8444-444444444444";
const NAME: &str = "gantry";

#[path = "../../../internal/racer-test/control.rs"]
mod control;

fn read_head(stream: &mut impl Read) -> io::Result<String> {
    let mut head = Vec::new();
    while !head.ends_with(b"\r\n\r\n") {
        let mut byte = [0];
        stream.read_exact(&mut byte)?;
        head.push(byte[0]);
        assert!(head.len() <= 32768);
    }
    String::from_utf8(head).map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))
}

fn fields(head: &str) -> BTreeMap<String, String> {
    head.lines()
        .skip(1)
        .filter_map(|line| line.split_once(':'))
        .map(|(key, value)| (key.to_ascii_lowercase(), value.trim().to_owned()))
        .collect()
}

/// Optional bench cache settings: `RACER_BENCH_AZURE_CONTAINER_URL` gives the
/// cache an anonymous native Azure origin, and `RACER_BENCH_MODE` is `cached`
/// (default) or `passthrough` (peer-routed passthrough).
fn bench_cache(url: Option<String>, mode: Option<String>) -> Result<control::CacheSpec, String> {
    let mut spec = control::CacheSpec::new(CACHE, NAME);
    spec.mode = match mode.as_deref().unwrap_or("cached") {
        "cached" => racer_control_wire::CacheMode::Cached,
        "passthrough" => racer_control_wire::CacheMode::PassthroughPeers,
        _ => return Err("RACER_BENCH_MODE must be cached or passthrough".into()),
    };
    if let Some(url) = url.filter(|url| !url.is_empty()) {
        let url = racer_control_wire::AzureContainerUrl::parse(&url)
            .map_err(|_| "RACER_BENCH_AZURE_CONTAINER_URL is not a valid container URL")?;
        spec.origin = Some(racer_control_wire::OriginDefinition::azure(
            url,
            racer_control_wire::AzureCredential::Anonymous,
        ));
    }
    Ok(spec)
}

/// The `RACER_BENCH_MODE` name of a published cache mode.
fn mode_name(mode: racer_control_wire::CacheMode) -> &'static str {
    match mode {
        racer_control_wire::CacheMode::PassthroughPeers => "passthrough",
        _ => "cached",
    }
}

fn main() {
    let root = std::env::args().nth(1).expect("scratch directory required");
    let spec = bench_cache(
        std::env::var("RACER_BENCH_AZURE_CONTAINER_URL").ok(),
        std::env::var("RACER_BENCH_MODE").ok(),
    )
    .unwrap_or_else(|error| panic!("{error}"));
    let mode = mode_name(spec.mode);
    let control = control::Control::start_with_specs(Path::new(&root), vec![spec]);
    println!("{}", control.endpoint);
    // The bench driver rejects runs whose published mode differs from the
    // requested one.
    println!("mode={mode}");
    io::stdout().flush().unwrap();
    let _ = io::stdin().read(&mut [0]);
    eprintln!(
        "enrollments={}",
        control.enrollments.load(Ordering::Acquire)
    );
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bench_cache_env() {
        let spec = bench_cache(None, None).unwrap();
        assert_eq!(spec.mode, racer_control_wire::CacheMode::Cached);
        assert!(spec.origin.is_none());
        let spec = bench_cache(
            Some("http://127.0.0.1:10000/acct/models".into()),
            Some("passthrough".into()),
        )
        .unwrap();
        assert_eq!(spec.mode, racer_control_wire::CacheMode::PassthroughPeers);
        let origin = spec.origin.unwrap();
        assert_eq!(origin.azure.container_url.container(), "models");
        assert_eq!(
            origin.azure.credential,
            racer_control_wire::AzureCredential::Anonymous
        );
        assert!(
            bench_cache(Some(String::new()), None)
                .unwrap()
                .origin
                .is_none()
        );
        assert!(bench_cache(None, Some("local".into())).is_err());
        for name in ["cached", "passthrough"] {
            let spec = bench_cache(None, Some(name.into())).unwrap();
            assert_eq!(mode_name(spec.mode), name);
        }
        assert!(bench_cache(Some("https://host/Models".into()), None).is_err());
    }
}

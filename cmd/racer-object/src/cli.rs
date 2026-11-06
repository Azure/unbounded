// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Command line parsing. `--flag=value`, `--flag value`, and a single dash
//! all work, so existing pod specs keep working unchanged.

use crate::object::Api;
use std::net::{IpAddr, SocketAddr};
use std::time::Duration;

/// Parsed `sidecar` options.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Options {
    pub cache: String,
    pub api: Api,
    pub listen: SocketAddr,
    pub debug_listen: Option<String>,
    pub request_timeout: Duration,
    pub max_connections: usize,
    /// Serving threads, each with its own single-threaded runtime.
    pub workers: usize,
}

/// What `main` should do.
#[derive(Debug, Eq, PartialEq)]
pub enum Command {
    /// Print this text to stdout and exit 0 (help or version).
    Print(String),
    Run(Options),
}

const MAX_CONNECTIONS: i64 = 65536;
const MAX_WORKERS: i64 = 256;
/// The default `--workers` is the usable CPUs (cgroup quota and affinity
/// included), at most this many.
const DEFAULT_WORKERS_MAX: usize = 4;

/// Default `--workers`: `min(available_parallelism, 4)`.
pub fn default_workers() -> usize {
    std::thread::available_parallelism()
        .map_or(1, |n| n.get())
        .min(DEFAULT_WORKERS_MAX)
}
const SOCKET_PATH_LIMIT: usize = 107;

/// Version string (`<version> (commit: <commit>, built: <time>)`), set at
/// build time from `RACER_OBJECT_*` environment variables.
pub fn version() -> String {
    format!(
        "{} (commit: {}, built: {})",
        option_env!("RACER_OBJECT_VERSION").unwrap_or("dev"),
        option_env!("RACER_OBJECT_GIT_COMMIT").unwrap_or("unknown"),
        option_env!("RACER_OBJECT_BUILD_TIME").unwrap_or("unknown"),
    )
}

/// Flag usage text.
pub fn sidecar_usage() -> String {
    "Usage: racer-object sidecar [flags]
  -api string
    \tSidecar API: s3 or azure (default \"s3\")
  -cache string
    \tRacer cache name (ClusterCache); required
  -debug-listen string
    \tOptional diagnostics address serving /healthz (e.g. 127.0.0.1:6060); disabled when empty. Trusted diagnostic access only: nonloopback addresses such as :6060 are reachable from the network
  -listen string
    \tLoopback IP literal and port (default \"127.0.0.1:8080\")
  -max-connections int
    \tConcurrent client connections and Racer reads; 1..65536 (default 128)
  -request-timeout duration
    \tPositive sidecar request timeout (default 5m0s)
  -version
    \tPrint version
  -workers int
    \tServing threads, each with its own event loop; 1..256 (default: usable CPUs, at most 4)
"
    .to_string()
}

/// Parse `args` (without the program name). Errors are safe to print: they
/// never echo argument values.
pub fn parse(args: &[String]) -> Result<Command, String> {
    let Some(command) = args.first() else {
        return Err("expected sidecar (use --help)".into());
    };
    match command.as_str() {
        "--help" | "-h" => {
            return Ok(Command::Print(
                "Usage: racer-object sidecar [flags]\nUse racer-object sidecar --help for flags.\n"
                    .into(),
            ));
        }
        "--version" => return Ok(Command::Print(version() + "\n")),
        "sidecar" => {}
        _ => return Err("unknown command (use --help)".into()),
    }

    let invalid = || "invalid flags (use --help)".to_string();
    let mut cache = String::new();
    let mut api = "s3".to_string();
    let mut listen = "127.0.0.1:8080".to_string();
    let mut debug_listen = String::new();
    let mut timeout = Some(Duration::from_secs(300));
    let mut max_connections: i64 = 128;
    let mut workers = default_workers() as i64;
    let mut show_version = false;

    let mut rest = &args[1..];
    // Flags stop at the first non-flag, "-" or "--".
    while let Some(arg) = rest.first() {
        if arg.len() < 2 || !arg.starts_with('-') {
            break;
        }
        rest = &rest[1..];
        if arg == "--" {
            break;
        }
        let body = arg.strip_prefix("--").unwrap_or(&arg[1..]);
        if body.is_empty() || body.starts_with('-') || body.starts_with('=') {
            return Err(invalid());
        }
        let (name, inline) = match body.split_once('=') {
            Some((n, v)) => (n, Some(v.to_string())),
            None => (body, None),
        };
        if name == "version" {
            show_version = match inline.as_deref() {
                None => true,
                Some(v) => parse_bool(v).ok_or_else(invalid)?,
            };
            continue;
        }
        if name == "help" || name == "h" {
            return Ok(Command::Print(sidecar_usage()));
        }
        if !matches!(
            name,
            "cache"
                | "api"
                | "listen"
                | "debug-listen"
                | "request-timeout"
                | "max-connections"
                | "workers"
        ) {
            return Err(invalid());
        }
        let value = match inline {
            Some(v) => v,
            None => {
                let Some(v) = rest.first() else {
                    return Err(invalid());
                };
                rest = &rest[1..];
                v.clone()
            }
        };
        match name {
            "cache" => cache = value,
            "api" => api = value,
            "listen" => listen = value,
            "debug-listen" => debug_listen = value,
            "request-timeout" => {
                let d = parse_duration(&value).ok_or_else(invalid)?;
                // Negative durations parse but are not positive.
                timeout = (!value.starts_with('-')).then_some(d);
            }
            "max-connections" => max_connections = parse_int(&value).ok_or_else(invalid)?,
            "workers" => workers = parse_int(&value).ok_or_else(invalid)?,
            _ => unreachable!("flag names are checked above"),
        }
    }
    if !rest.is_empty() {
        return Err("unexpected positional arguments".into());
    }
    if show_version {
        return Ok(Command::Print(version() + "\n"));
    }
    if cache.is_empty() {
        return Err("--cache is required".into());
    }
    if !valid_cache(&cache) {
        return Err("invalid --cache".into());
    }
    let request_timeout = timeout
        .filter(|d| !d.is_zero())
        .ok_or_else(|| "--request-timeout must be positive".to_string())?;
    if !(1..=MAX_CONNECTIONS).contains(&max_connections) {
        return Err("--max-connections must be between 1 and 65536".into());
    }
    if !(1..=MAX_WORKERS).contains(&workers) {
        return Err("--workers must be between 1 and 256".into());
    }
    let api = match api.as_str() {
        "s3" => Api::S3,
        "azure" => Api::Azure,
        _ => return Err("--api must be s3 or azure".into()),
    };
    let listen = validate_listen(&listen)?;
    Ok(Command::Run(Options {
        cache,
        api,
        listen,
        debug_listen: (!debug_listen.is_empty()).then_some(debug_listen),
        request_timeout,
        max_connections: max_connections as usize,
        workers: workers as usize,
    }))
}

fn parse_bool(s: &str) -> Option<bool> {
    match s {
        "1" | "t" | "T" | "true" | "TRUE" | "True" => Some(true),
        "0" | "f" | "F" | "false" | "FALSE" | "False" => Some(false),
        _ => None,
    }
}

/// Integer flag value: optional sign,
/// base prefixes 0x/0o/0b/0, and underscores only with a prefix.
pub fn parse_int(s: &str) -> Option<i64> {
    let (negative, digits) = match s.as_bytes().first()? {
        b'-' => (true, &s[1..]),
        b'+' => (false, &s[1..]),
        _ => (false, s),
    };
    let lower = digits.to_ascii_lowercase();
    let (radix, body, prefixed) = if let Some(b) = lower.strip_prefix("0x") {
        (16, b.to_string(), true)
    } else if let Some(b) = lower.strip_prefix("0o") {
        (8, b.to_string(), true)
    } else if let Some(b) = lower.strip_prefix("0b") {
        (2, b.to_string(), true)
    } else if lower.len() > 1 && lower.starts_with('0') {
        (8, lower[1..].to_string(), true)
    } else {
        (10, lower.clone(), false)
    };
    if body.contains('_') {
        // Underscores are allowed only between digits after a base prefix.
        if !prefixed || body.ends_with('_') || body.contains("__") {
            return None;
        }
    }
    let body: String = body.chars().filter(|&c| c != '_').collect();
    if body.is_empty() || body.starts_with(['+', '-']) {
        return None;
    }
    let magnitude = i128::from(u64::from_str_radix(&body, radix).ok()?);
    let value = if negative { -magnitude } else { magnitude };
    i64::try_from(value).ok()
}

/// Duration magnitude in the `300ms`, `1.5h`, `5m` syntax (the sign is
/// handled by the caller,
/// which rejects negatives). `None` on syntax error or overflow.
pub fn parse_duration(s: &str) -> Option<Duration> {
    let mut s = s.strip_prefix(['-', '+']).unwrap_or(s);
    if s == "0" {
        return Some(Duration::ZERO);
    }
    if s.is_empty() {
        return None;
    }
    let mut total: u128 = 0;
    while !s.is_empty() {
        let int_len = s.bytes().take_while(u8::is_ascii_digit).count();
        let (int, after) = s.split_at(int_len);
        let (frac, after) = match after.strip_prefix('.') {
            Some(a) => {
                let n = a.bytes().take_while(u8::is_ascii_digit).count();
                a.split_at(n)
            }
            None => ("", after),
        };
        if int.is_empty() && frac.is_empty() {
            return None;
        }
        let unit_len = after
            .bytes()
            .take_while(|c| !c.is_ascii_digit() && *c != b'.')
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
        let int: u128 = if int.is_empty() { 0 } else { int.parse().ok()? };
        let mut nanos = int.checked_mul(scale)?;
        if !frac.is_empty() {
            // Mirror Go's leadingFraction: keep digits while they fit in
            // an i64, then scale in floating point so nothing overflows.
            let (mut f, mut div) = (0u64, 1f64);
            for c in frac.bytes() {
                let Some(y) = f
                    .checked_mul(10)
                    .and_then(|x| x.checked_add(u64::from(c - b'0')))
                    .filter(|&y| y <= i64::MAX as u64)
                else {
                    break;
                };
                f = y;
                div *= 10.0;
            }
            let extra = (f as f64 * (scale as f64 / div)) as u128;
            nanos = nanos.checked_add(extra)?;
        }
        total = total.checked_add(nanos)?;
        if total > i64::MAX as u128 {
            return None;
        }
        s = after;
    }
    Some(Duration::from_nanos(total as u64))
}

/// Racer cache names are DNS subdomains whose origin socket path fits in
/// `sun_path` (the same rule as `pkg/racersdk`).
pub fn valid_cache(s: &str) -> bool {
    if s.is_empty()
        || s.len() > 253
        || "/run/racer/".len() + s.len() + "/origin/socket".len() > SOCKET_PATH_LIMIT
    {
        return false;
    }
    s.split('.').all(|label| {
        !label.is_empty()
            && label.len() <= 63
            && !label.starts_with('-')
            && !label.ends_with('-')
            && label
                .bytes()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'-')
    })
}

/// Split `host:port` or `[host]:port`.
fn split_host_port(s: &str) -> Option<(&str, &str)> {
    if let Some(rest) = s.strip_prefix('[') {
        let close = rest.find(']')?;
        let host = &rest[..close];
        let port = rest[close + 1..].strip_prefix(':')?;
        if port.contains([':', '[', ']']) {
            return None;
        }
        return Some((host, port));
    }
    let colon = s.rfind(':')?;
    let (host, port) = (&s[..colon], &s[colon + 1..]);
    if host.contains([':', '[', ']']) || port.contains(['[', ']']) {
        return None;
    }
    Some((host, port))
}

/// `--listen` must be a loopback IP literal and a decimal port.
pub fn validate_listen(value: &str) -> Result<SocketAddr, String> {
    let loopback = || "--listen must use a loopback IP literal and port".to_string();
    let port_error = || "--listen port must be between 0 and 65535".to_string();
    let (host, port) = split_host_port(value).ok_or_else(loopback)?;
    let ip: IpAddr = host.parse().map_err(|_| loopback())?;
    // IPv4-mapped IPv6 loopback counts as loopback.
    let ip = ip.to_canonical();
    if !ip.is_loopback() {
        return Err(loopback());
    }
    if port.is_empty() || !port.bytes().all(|c| c.is_ascii_digit()) {
        return Err(port_error());
    }
    let port: u16 = port.parse().map_err(|_| port_error())?;
    Ok(SocketAddr::new(ip, port))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(list: &[&str]) -> Vec<String> {
        list.iter().map(|s| s.to_string()).collect()
    }

    fn options(list: &[&str]) -> Result<Options, String> {
        match parse(&args(list))? {
            Command::Run(o) => Ok(o),
            Command::Print(text) => Err(format!("printed {text}")),
        }
    }

    const CACHE: &str = "--cache=test-cache";

    #[test]
    fn defaults_and_flags() {
        let o = options(&["sidecar", CACHE]).unwrap();
        assert_eq!(o.cache, "test-cache");
        assert_eq!(o.api, Api::S3);
        assert_eq!(o.listen, "127.0.0.1:8080".parse().unwrap());
        assert_eq!(o.request_timeout, Duration::from_secs(300));
        assert_eq!(o.max_connections, 128);
        assert_eq!(o.debug_listen, None);
        assert_eq!(o.workers, default_workers());
        assert!((1..=4).contains(&o.workers));

        let o = options(&[
            "sidecar",
            CACHE,
            "--api=azure",
            "--max-connections=65536",
            "--request-timeout=2s",
        ])
        .unwrap();
        assert_eq!(o.api, Api::Azure);
        assert_eq!(o.max_connections, 65536);
        assert_eq!(o.request_timeout, Duration::from_secs(2));

        // Separate values, single dashes, and repeated flags (last wins).
        let o = options(&[
            "sidecar",
            "-cache",
            "c",
            "--api",
            "azure",
            "-listen=[::1]:9",
            "--max-connections",
            "0x10",
            "--api=s3",
            "--debug-listen=:6060",
        ])
        .unwrap();
        assert_eq!(o.cache, "c");
        assert_eq!(o.api, Api::S3);
        assert_eq!(o.listen, "[::1]:9".parse().unwrap());
        assert_eq!(o.max_connections, 16);
        assert_eq!(o.debug_listen.as_deref(), Some(":6060"));
    }

    #[test]
    fn workers_flag() {
        for (list, want) in [
            (&["sidecar", CACHE, "--workers=4"][..], 4),
            (&["sidecar", CACHE, "--workers", "2"], 2),
            (&["sidecar", CACHE, "-workers=1"], 1),
            (&["sidecar", CACHE, "--workers=256"], 256),
            (&["sidecar", CACHE, "--workers=0x8"], 8),
            (&["sidecar", CACHE, "--workers=3", "--workers=5"], 5),
        ] {
            assert_eq!(options(list).unwrap().workers, want, "{list:?}");
        }
        for value in ["0", "-1", "257", "65536"] {
            assert_eq!(
                options(&["sidecar", CACHE, &format!("--workers={value}")]).unwrap_err(),
                "--workers must be between 1 and 256",
            );
        }
        for value in ["", "x", "1.5", "4x", "99999999999999999999"] {
            assert_eq!(
                options(&["sidecar", CACHE, &format!("--workers={value}")]).unwrap_err(),
                "invalid flags (use --help)",
            );
        }
        assert_eq!(
            options(&["sidecar", CACHE, "--workers"]).unwrap_err(),
            "invalid flags (use --help)",
        );
        assert!(sidecar_usage().contains("  -workers int\n"));
    }

    #[test]
    fn cache_flag() {
        let long_ok = format!("{}.{}", "a".repeat(63), "b".repeat(18));
        for name in ["custom", "a-b.c0", long_ok.as_str()] {
            let o = options(&["sidecar", &format!("--cache={name}")]).unwrap();
            assert_eq!(o.cache, name);
        }
        let too_long = format!("{}.{}", "a".repeat(63), "b".repeat(19));
        let label = "a".repeat(64);
        for name in [
            "../bad",
            "Upper",
            label.as_str(),
            too_long.as_str(),
            "a..b",
            "-a",
            "a-",
        ] {
            let err = options(&["sidecar", &format!("--cache={name}")]).unwrap_err();
            assert_eq!(err, "invalid --cache", "{name}");
        }
        for list in [&["sidecar"][..], &["sidecar", "--cache="]] {
            assert_eq!(options(list).unwrap_err(), "--cache is required");
        }
    }

    #[test]
    fn errors() {
        for list in [
            &[][..],
            &["unknown"],
            &["origin", CACHE],
            &["sidecar", CACHE, "--namespace=store"],
            &["sidecar", CACHE, "--bucket=bucket"],
            &["sidecar", CACHE, "--cache=../bad"],
            &["sidecar", CACHE, "--request-timeout=-1s"],
            &["sidecar", CACHE, "--request-timeout=0"],
            &["sidecar", CACHE, "--request-timeout=5"],
            &["sidecar", CACHE, "--region=us-east-1"],
            &["sidecar", CACHE, "--api=gcs"],
            &["sidecar", CACHE, "extra"],
            &["sidecar", CACHE, "--listen=0.0.0.0:8080"],
            &["sidecar", CACHE, "--max-connections=0"],
            &["sidecar", CACHE, "--max-connections=-1"],
            &["sidecar", CACHE, "--max-connections=65537"],
            &["sidecar", CACHE, "--max-connections=x"],
            &["sidecar", CACHE, "--max-concurrent-requests=512"],
            &["sidecar", CACHE, "---api=s3"],
            &["sidecar", CACHE, "--api"],
            &["sidecar", CACHE, "--version=maybe"],
        ] {
            assert!(options(list).is_err(), "{list:?}");
        }
    }

    #[test]
    fn help_version_and_redaction() {
        for list in [
            &["--help"][..],
            &["-h"],
            &["sidecar", "--help"],
            &["sidecar", "-h"],
            &["--version"],
            &["sidecar", "--version"],
            &["sidecar", "--version=true"],
        ] {
            match parse(&args(list)).unwrap() {
                Command::Print(text) => {
                    assert!(!text.is_empty());
                    if list.last().unwrap().starts_with("--version") {
                        assert!(text.contains(&version()));
                    }
                }
                Command::Run(_) => panic!("{list:?} ran"),
            }
        }
        let Command::Print(help) = parse(&args(&["sidecar", "--help"])).unwrap() else {
            panic!("no help");
        };
        assert!(help.contains("-cache") && help.contains("required"));
        for list in [
            &["sidecar", CACHE, "--request-timeout=private-token"][..],
            &["sidecar", CACHE, "--private-token"],
        ] {
            let err = options(list).unwrap_err();
            assert!(!err.contains("private-token"));
        }
        // An explicit false version flag still runs.
        assert!(options(&["sidecar", CACHE, "--version=false"]).is_ok());
    }

    #[test]
    fn loopback_validation() {
        for value in [
            "127.0.0.1:0",
            "127.1.2.3:65535",
            "[::1]:8080",
            "[::ffff:127.0.0.1]:80",
        ] {
            assert!(validate_listen(value).is_ok(), "{value}");
        }
        for value in [
            "localhost:8080",
            ":8080",
            "0.0.0.0:8080",
            "[::]:8080",
            "192.0.2.1:8080",
            "127.0.0.1",
            "127.0.0.1:",
            "127.0.0.1:http",
            "127.0.0.1:-1",
            "127.0.0.1:+80",
            "127.0.0.1:65536",
            "[::1%lo]:80",
            "::1:80",
        ] {
            assert!(validate_listen(value).is_err(), "{value}");
        }
    }

    #[test]
    fn durations() {
        for (s, want) in [
            ("300ms", Duration::from_millis(300)),
            ("1.5s", Duration::from_millis(1500)),
            ("5m", Duration::from_secs(300)),
            ("1h2m", Duration::from_secs(3720)),
            ("1h2m3.5s", Duration::from_millis(3_723_500)),
            ("2us", Duration::from_micros(2)),
            ("2\u{b5}s", Duration::from_micros(2)),
            ("7ns", Duration::from_nanos(7)),
            (".5s", Duration::from_millis(500)),
            ("0", Duration::ZERO),
            ("+1s", Duration::from_secs(1)),
        ] {
            assert_eq!(parse_duration(s), Some(want), "{s}");
        }
        for s in ["", "5", "1x", "s", "1.s5", "..1s", "9999999999h", "1 s"] {
            assert_eq!(parse_duration(s), None, "{s}");
        }
    }

    #[test]
    fn ints() {
        for (s, want) in [
            ("10", 10),
            ("+5", 5),
            ("-1", -1),
            ("0x10", 16),
            ("010", 8),
            ("0b11", 3),
            ("0o17", 15),
            ("0x_1", 1),
        ] {
            assert_eq!(parse_int(s), Some(want), "{s}");
        }
        for s in ["", "x", "1_0", "0x", "--1", "9223372036854775808"] {
            assert_eq!(parse_int(s), None, "{s}");
        }
    }

    #[test]
    fn parse_duration_long_fractions() {
        let d = parse_duration("0.123456789012345678901234567890h").unwrap();
        assert!((d.as_secs_f64() - 444.444_440_4).abs() < 1e-3, "{d:?}");
        let long = format!("1.{}s", "9".repeat(500));
        let d = parse_duration(&long).unwrap();
        assert!(d >= Duration::from_millis(1999) && d <= Duration::from_secs(2));
        let zeros = format!("0.{}1h", "0".repeat(400));
        assert_eq!(parse_duration(&zeros), Some(Duration::ZERO));
        assert_eq!(
            parse_duration("1h2m3.5s"),
            Some(Duration::from_millis(3_723_500))
        );
        assert_eq!(parse_duration(".5ms"), Some(Duration::from_micros(500)));
    }

    #[test]
    fn parse_duration_limits() {
        assert_eq!(
            parse_duration("9223372036854775807ns"),
            Some(Duration::from_nanos(i64::MAX as u64))
        );
        assert_eq!(parse_duration("9223372036854775808ns"), None);
        assert_eq!(
            parse_duration("2562047h47m16.854775807s").map(|d| d.as_nanos()),
            Some(i64::MAX as u128)
        );
        assert_eq!(parse_duration("2562048h"), None);
        assert_eq!(parse_duration("2562047h47m17s"), None);
        assert_eq!(parse_duration(&format!("{}h", "9".repeat(60))), None);
        assert_eq!(
            parse_duration(&format!("2562047.{}h", "9".repeat(60))),
            None
        );
        for s in ["", ".", "1", "1x", "h", "1..5s"] {
            assert_eq!(parse_duration(s), None, "{s}");
        }
    }
}

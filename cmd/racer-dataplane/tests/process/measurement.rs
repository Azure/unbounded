//! Client-side completion oracle. Partial bodies never contribute to goodput.
use serde::Serialize;
use std::{
    collections::BTreeMap,
    io::{self, BufRead},
    time::Duration,
};

#[derive(Debug, PartialEq, Eq)]
pub enum Failure {
    Http(u16),
    Truncated,
    Transport(io::ErrorKind),
    Invalid,
    Corrupt,
}
impl From<io::Error> for Failure {
    fn from(error: io::Error) -> Self {
        if error.kind() == io::ErrorKind::UnexpectedEof {
            Self::Truncated
        } else {
            Self::Transport(error.kind())
        }
    }
}

pub struct Expected {
    pub start: u64,
    pub end: u64,
    pub length: u64,
}

pub fn response(
    stream: &mut impl BufRead,
    expected: &Expected,
    byte: impl Fn(u64) -> u8,
    pause: Duration,
) -> Result<u64, Failure> {
    let mut head = Vec::new();
    // Bounded even for an unterminated malicious header.
    while !head.ends_with(b"\r\n\r\n") {
        if head.len() == 32768 {
            return Err(Failure::Invalid);
        }
        let mut b = [0];
        stream.read_exact(&mut b)?;
        head.push(b[0]);
    }
    let head = std::str::from_utf8(&head).map_err(|_| Failure::Invalid)?;
    let mut lines = head.split("\r\n");
    let mut status = lines.next().ok_or(Failure::Invalid)?.split_whitespace();
    if status.next() != Some("HTTP/1.1") {
        return Err(Failure::Invalid);
    }
    let status: u16 = status
        .next()
        .ok_or(Failure::Invalid)?
        .parse()
        .map_err(|_| Failure::Invalid)?;
    let mut fields = BTreeMap::new();
    for line in lines.filter(|line| !line.is_empty()) {
        let (name, value) = line.split_once(':').ok_or(Failure::Invalid)?;
        if fields
            .insert(name.to_ascii_lowercase(), value.trim())
            .is_some()
        {
            return Err(Failure::Invalid);
        }
    }
    if status != 206 {
        return Err(Failure::Http(status));
    }
    let length = expected
        .end
        .checked_sub(expected.start)
        .ok_or(Failure::Invalid)?;
    if length == 0
        || expected.end > expected.length
        || fields
            .get("content-length")
            .and_then(|s| s.parse::<u64>().ok())
            != Some(length)
        || fields.get("etag") != Some(&"\"restart-v1\"")
        || fields.get("content-range").copied()
            != Some(
                format!(
                    "bytes {}-{}/{}",
                    expected.start,
                    expected.end - 1,
                    expected.length
                )
                .as_str(),
            )
        || fields.contains_key("transfer-encoding")
    {
        return Err(Failure::Invalid);
    }
    let mut scratch = [0; 64 << 10];
    let mut offset = expected.start;
    while offset < expected.end {
        let n = scratch.len().min((expected.end - offset) as usize);
        stream.read_exact(&mut scratch[..n])?;
        if scratch[..n]
            .iter()
            .enumerate()
            .any(|(i, actual)| *actual != byte(offset + i as u64))
        {
            return Err(Failure::Corrupt);
        }
        offset += n as u64;
        if !pause.is_zero() {
            std::thread::sleep(pause);
        }
    }
    Ok(length)
}

#[derive(Default, Debug, Serialize)]
pub struct Measurements {
    pub attempted: usize,
    pub completed: usize,
    pub completed_bytes: u64,
    pub failures: BTreeMap<String, usize>,
    pub elapsed_seconds: f64,
    pub goodput_bytes_per_second: f64,
    pub completed_p50_ms: Option<f64>,
    pub completed_p99_ms: Option<f64>,
    #[serde(skip)]
    latencies: Vec<Duration>,
}
impl Measurements {
    pub fn record(&mut self, result: Result<u64, Failure>, latency: Duration) {
        self.attempted += 1;
        match result {
            Ok(bytes) => {
                self.completed += 1;
                self.completed_bytes += bytes;
                self.latencies.push(latency);
            }
            Err(error) => *self.failures.entry(format!("{error:?}")).or_default() += 1,
        }
    }
    pub fn finish(&mut self, elapsed: Duration) {
        self.elapsed_seconds = elapsed.as_secs_f64();
        self.goodput_bytes_per_second = if elapsed.is_zero() {
            0.
        } else {
            self.completed_bytes as f64 / self.elapsed_seconds
        };
        self.latencies.sort_unstable();
        let percentile = |p: usize| {
            self.latencies
                .get((self.latencies.len() * p).div_ceil(100).saturating_sub(1))
                .map(|d| d.as_secs_f64() * 1000.)
        };
        self.completed_p50_ms = percentile(50);
        self.completed_p99_ms = percentile(99);
    }
    pub fn accept(&self, scheduled: usize) -> bool {
        scheduled > 0
            && self.attempted == scheduled
            && self.completed == scheduled
            && self.completed_bytes > 0
            && self.failures.is_empty()
            && self.elapsed_seconds > 0.
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn check(bytes: &[u8]) -> Result<u64, Failure> {
        response(
            &mut io::Cursor::new(bytes),
            &Expected {
                start: 0,
                end: 3,
                length: 3,
            },
            |i| i as u8,
            Duration::ZERO,
        )
    }
    #[test]
    fn complete_error_truncated_malformed_and_corrupt_responses() {
        let head = b"HTTP/1.1 206 Partial Content\r\nContent-Length: 3\r\nETag: \"restart-v1\"\r\nContent-Range: bytes 0-2/3\r\n\r\n";
        assert_eq!(check(&[head.as_slice(), &[0, 1, 2]].concat()), Ok(3));
        assert_eq!(
            check(&[head.as_slice(), &[0, 1]].concat()),
            Err(Failure::Truncated)
        );
        assert_eq!(
            check(&[head.as_slice(), &[0, 1, 7]].concat()),
            Err(Failure::Corrupt)
        );
        assert_eq!(
            check(b"HTTP/1.1 503 Unavailable\r\nContent-Length: 0\r\n\r\n"),
            Err(Failure::Http(503))
        );
        assert_eq!(check(b""), Err(Failure::Truncated));
        assert_eq!(check(&vec![b'x'; 32769]), Err(Failure::Invalid));
        assert_eq!(
            check(b"HTTP/1.1 206 OK\r\nContent-Length: 3\r\ncontent-length: 3\r\n\r\n"),
            Err(Failure::Invalid)
        );
    }
    #[test]
    fn strict_gate_rejects_partial_success_empty_and_unfinished_runs() {
        let mut m = Measurements::default();
        m.finish(Duration::from_secs(1));
        assert!(!m.accept(0));
        m.record(Ok(3), Duration::from_millis(10));
        m.finish(Duration::from_secs(1));
        assert!(m.accept(1));
        assert!(!m.accept(2));
        m.record(Err(Failure::Truncated), Duration::from_millis(20));
        m.finish(Duration::from_secs(2));
        assert!(!m.accept(2));
        assert_eq!(m.completed_bytes, 3);
        assert_eq!(m.goodput_bytes_per_second, 1.5);
        assert_eq!(m.completed_p99_ms, Some(10.));
        m.finish(Duration::ZERO);
        assert!(!m.accept(1));
    }
}

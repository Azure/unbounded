//! Client-side completion oracle. Partial bodies never contribute to goodput.
use serde::Serialize;
use std::{
    collections::BTreeMap,
    io::{self, BufReader, Read, Write},
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

pub fn response<S: Read + Write>(
    stream: &mut BufReader<S>,
    expected: &Expected,
    byte: impl FnMut(u64) -> u8,
    pause: Duration,
) -> Result<u64, Failure> {
    response_fields(stream, expected, byte, pause).map(|(length, _)| length)
}

pub fn response_fields<S: Read + Write>(
    stream: &mut BufReader<S>,
    expected: &Expected,
    mut byte: impl FnMut(u64) -> u8,
    pause: Duration,
) -> Result<(u64, BTreeMap<String, String>), Failure> {
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
            .insert(name.to_ascii_lowercase(), value.trim().to_owned())
            .is_some()
        {
            return Err(Failure::Invalid);
        }
    }
    if status != 200 {
        return Err(Failure::Http(status));
    }
    let length = expected
        .end
        .checked_sub(expected.start)
        .ok_or(Failure::Invalid)?;
    let pages = if length == 0 {
        0
    } else {
        (expected.end - 1) / super::P - expected.start / super::P + 1
    };
    if expected.end > expected.length
        || fields
            .get("content-length")
            .and_then(|s| s.parse::<u64>().ok())
            != Some(length + 21 * (pages + 1))
        || fields.get("etag").map(String::as_str) != Some("\"restart-v1\"")
        || fields.get("racer-object-length") != Some(&expected.length.to_string())
        || fields.get("racer-range-start") != Some(&expected.start.to_string())
        || fields.get("racer-range-end") != Some(&expected.end.to_string())
        || fields.get("racer-expires-at") != Some(&"0".to_owned())
        || fields.get("content-type").map(String::as_str) != Some("application/octet-stream")
        || fields.get("connection").map(String::as_str) != Some("close")
        || fields.contains_key("content-range")
        || fields.contains_key("transfer-encoding")
    {
        return Err(Failure::Invalid);
    }
    let mut scratch = [0; 64 << 10];
    let mut offset = expected.start;
    while offset < expected.end {
        let number = offset / super::P;
        let end = expected.end.min((number + 1) * super::P);
        let length = (end - offset) as u32;
        let mut frame = [0; 21];
        stream.read_exact(&mut frame)?;
        if frame[0] != 1
            || u64::from_be_bytes(frame[1..9].try_into().unwrap()) != number
            || u64::from_be_bytes(frame[9..17].try_into().unwrap()) != offset
            || u32::from_be_bytes(frame[17..].try_into().unwrap()) != length
        {
            return Err(Failure::Invalid);
        }
        while offset < end {
            let n = scratch.len().min((end - offset) as usize);
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
        // Completion releases the final lease; earlier pages return exact credit.
        if offset < expected.end {
            stream.get_mut().write_all(&number.to_be_bytes())?;
            stream.get_mut().write_all(&length.to_be_bytes())?;
        }
    }
    let mut complete = [0; 21];
    stream.read_exact(&mut complete)?;
    if complete[0] != 2
        || u64::from_be_bytes(complete[1..9].try_into().unwrap()) != pages
        || u64::from_be_bytes(complete[9..17].try_into().unwrap()) != length
        || u32::from_be_bytes(complete[17..].try_into().unwrap()) != 0
    {
        return Err(Failure::Invalid);
    }
    let mut extra = [0];
    if stream.read(&mut extra)? != 0 {
        return Err(Failure::Invalid);
    }
    Ok((length, fields))
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
            &mut BufReader::new(io::Cursor::new(bytes.to_vec())),
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
        let head = b"HTTP/1.1 200 OK\r\nContent-Length: 45\r\nETag: \"restart-v1\"\r\nRacer-Object-Length: 3\r\nRacer-Range-Start: 0\r\nRacer-Range-End: 3\r\nRacer-Expires-At: 0\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n";
        let page = frame(1, 0, 0, 3);
        let complete = frame(2, 1, 3, 0);
        let valid = [head.as_slice(), &page, &[0, 1, 2], &complete].concat();
        assert_eq!(check(&valid), Ok(3));
        assert_eq!(
            check(&[head.as_slice(), &page, &[0, 1]].concat()),
            Err(Failure::Truncated)
        );
        assert_eq!(
            check(&[head.as_slice(), &page, &[0, 1, 7], &complete].concat()),
            Err(Failure::Corrupt)
        );
        assert_eq!(
            check(b"HTTP/1.1 503 Unavailable\r\nContent-Length: 0\r\n\r\n"),
            Err(Failure::Http(503))
        );
        assert_eq!(check(b""), Err(Failure::Truncated));
        assert_eq!(check(&valid[..valid.len() - 1]), Err(Failure::Truncated));
        assert_eq!(
            check(&[valid.as_slice(), &[0]].concat()),
            Err(Failure::Invalid)
        );
        assert_eq!(
            check(&[head.as_slice(), &frame(1, 1, 0, 3), &[0, 1, 2], &complete].concat()),
            Err(Failure::Invalid)
        );
        assert_eq!(
            check(&[head.as_slice(), &page, &[0, 1, 2], &frame(2, 2, 3, 0)].concat()),
            Err(Failure::Invalid)
        );
        assert_eq!(check(&vec![b'x'; 32769]), Err(Failure::Invalid));
        assert_eq!(
            check(b"HTTP/1.1 206 OK\r\nContent-Length: 3\r\ncontent-length: 3\r\n\r\n"),
            Err(Failure::Invalid)
        );
    }
    fn frame(kind: u8, number: u64, offset: u64, length: u32) -> [u8; 21] {
        let mut frame = [0; 21];
        frame[0] = kind;
        frame[1..9].copy_from_slice(&number.to_be_bytes());
        frame[9..17].copy_from_slice(&offset.to_be_bytes());
        frame[17..].copy_from_slice(&length.to_be_bytes());
        frame
    }

    #[test]
    fn partial_pages_return_exact_credit_and_empty_requires_completion() {
        let (mut client, mut server) = std::os::unix::net::UnixStream::pair().unwrap();
        client
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        server
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        let worker = std::thread::spawn(move || {
            write!(server, "HTTP/1.1 200 OK\r\nContent-Length: 65\r\nETag: \"restart-v1\"\r\nRacer-Object-Length: {}\r\nRacer-Range-Start: {}\r\nRacer-Range-End: {}\r\nRacer-Expires-At: 0\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n", super::super::P + 1, super::super::P - 1, super::super::P + 1).unwrap();
            server
                .write_all(&frame(1, 0, super::super::P - 1, 1))
                .unwrap();
            server.write_all(&[7]).unwrap();
            let mut release = [0; 12];
            server.read_exact(&mut release).unwrap();
            assert_eq!(&release[..8], &0u64.to_be_bytes());
            assert_eq!(&release[8..], &1u32.to_be_bytes());
            server.write_all(&frame(1, 1, super::super::P, 1)).unwrap();
            server.write_all(&[7]).unwrap();
            server.write_all(&frame(2, 2, 2, 0)).unwrap();
        });
        assert_eq!(
            response(
                &mut BufReader::new(&mut client),
                &Expected {
                    start: super::super::P - 1,
                    end: super::super::P + 1,
                    length: super::super::P + 1
                },
                |_| 7,
                Duration::ZERO
            ),
            Ok(2)
        );
        worker.join().unwrap();
        let head = b"HTTP/1.1 200 OK\r\nContent-Length: 21\r\nETag: \"restart-v1\"\r\nRacer-Object-Length: 0\r\nRacer-Range-Start: 0\r\nRacer-Range-End: 0\r\nRacer-Expires-At: 0\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n";
        for complete in [true, false] {
            let mut raw = head.to_vec();
            if complete {
                raw.extend_from_slice(&frame(2, 0, 0, 0));
            }
            assert_eq!(
                response(
                    &mut BufReader::new(io::Cursor::new(raw)),
                    &Expected {
                        start: 0,
                        end: 0,
                        length: 0
                    },
                    |_| panic!("empty payload"),
                    Duration::ZERO
                ),
                if complete {
                    Ok(0)
                } else {
                    Err(Failure::Truncated)
                }
            );
        }
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

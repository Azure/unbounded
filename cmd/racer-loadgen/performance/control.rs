//! Local performance controller, reusing the executable restart TLS fixture.
//! Only control-plane responses are fixtures; the dataplane is a separate binary.
use std::{
    collections::BTreeMap,
    fs,
    io::{self, Read, Write},
    net::TcpListener,
    path::Path,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    thread,
    time::Duration,
};

const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
const NODE: &str = "22222222-2222-4222-8222-222222222222";
const CACHE: &str = "44444444-4444-4444-8444-444444444444";
const NAME: &str = "gantry";

#[path = "../../racer-dataplane/tests/process/control.rs"]
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

fn main() {
    let root = std::env::args().nth(1).expect("scratch directory required");
    let control = control::Control::start(Path::new(&root));
    println!("{}", control.endpoint);
    io::stdout().flush().unwrap();
    let _ = io::stdin().read(&mut [0]);
    eprintln!(
        "enrollments={}",
        control.enrollments.load(Ordering::Acquire)
    );
}

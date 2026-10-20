//! Command line parsing.

use std::path::PathBuf;
use std::time::Duration;

/// Benchmark scenarios.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Scenario {
    Write,
    Held,
    Read,
    Commit,
    Cpu,
    Lookup,
}

impl Scenario {
    pub const ALL: [Scenario; 6] = [
        Scenario::Cpu,
        Scenario::Commit,
        Scenario::Lookup,
        Scenario::Write,
        Scenario::Held,
        Scenario::Read,
    ];

    pub fn name(self) -> &'static str {
        match self {
            Scenario::Write => "write",
            Scenario::Held => "held",
            Scenario::Read => "read",
            Scenario::Commit => "commit",
            Scenario::Cpu => "cpu",
            Scenario::Lookup => "lookup",
        }
    }

    /// Whether the scenario runs once per `--page-bytes` value.
    pub fn per_page_size(self) -> bool {
        matches!(self, Scenario::Write | Scenario::Held | Scenario::Read)
    }
}

/// Parsed options.
#[derive(Clone, Debug)]
pub struct Args {
    pub devices: Vec<PathBuf>,
    pub destroy_data: bool,
    pub file: Option<PathBuf>,
    pub file_bytes: u64,
    pub keep_file: bool,
    pub workers: usize,
    pub pin: bool,
    pub cpu_offset: usize,
    pub slots_per_worker: Option<u64>,
    pub scenarios: Vec<Scenario>,
    pub duration: Duration,
    pub qd: usize,
    pub page_bytes: Vec<u64>,
    pub commit_interval: Duration,
    pub entries: Vec<u64>,
    pub mutations: Vec<u64>,
    pub read_fill_bytes: Option<u64>,
    pub hold_percent: u64,
    pub retain_secs: u64,
    pub pages_per_version: u32,
    pub cpu_entries: u64,
    pub lookup_entries: u64,
    pub metadata_cache: u64,
    pub spin: u32,
    pub ring_entries: usize,
    pub verify: bool,
    pub wipe: bool,
}

pub const USAGE: &str = "\
Extent volume benchmark (page-alloc).

BACKEND (one of):
  --devices A,B,...       raw block devices, opened O_DIRECT|O_EXCL. DESTROYS DATA.
  --destroy-data          required with --devices
  --file PATH             file-backed slab (O_DIRECT, fallocated)
  --file-bytes SIZE       file size (default 4G); suffixes K M G T (binary)
  --keep-file             do not delete the file at exit

LAYOUT:
  --workers N             worker threads, each with its own reactor and volume (default 1)
  --pin                   pin worker i to CPU (cpu-offset + i)
  --cpu-offset N          first CPU for --pin (default 0)
  --slots-per-worker N    use only the first N slots of each worker's slice

RUN:
  --scenario LIST         comma list or repeat: write,held,read,commit,cpu,lookup,all (default all)
  --duration SECS         per timed scenario (default 10, fractional ok)
  --qd N                  in-flight lanes per worker (default 16)
  --page-bytes LIST       page sizes for write/held/read (default 16M,1M,64K,4K)
  --commit-interval-ms N  lazy commit interval for write/read fill (default 1000)
  --entries LIST          commit scenario entry counts (default 1000,10000,100000)
  --mutations LIST        commit scenario incremental sizes (default 1,100,10000)
  --read-fill-bytes SIZE  bytes to fill per worker before reads (default: until full)
  --hold-percent N        storage slot limit as % of slots for held (default 100)
  --retain-secs N         held retain_until = now + N seconds (default 0)
  --pages-per-version N   keys share a version prefix for N pages (default 16)
  --cpu-entries N         entries for the cpu scenario (default 1000000, capped by capacity)
  --lookup-entries N      4 KiB entries the lookup scenario fills per worker (default 1000000)
  --metadata-cache SIZE   metadata page cache budget per volume (default 64M)
  --spin N                idle executor turns before blocking in wait (default 0)
  --ring-entries N        io_uring queue entries per worker (default 1024)
  --no-verify             skip record header checks on reads
  --wipe                  wipe buffers on release, as the unit charge does (default: no wipe, as production ciphertext)
";

fn parse_size(text: &str) -> Result<u64, String> {
    let t = text.trim();
    let lower = t.to_ascii_lowercase();
    let lower = lower
        .strip_suffix("ib")
        .or_else(|| lower.strip_suffix('b'))
        .unwrap_or(&lower);
    let (digits, shift) = match lower.chars().last() {
        Some('k') => (&lower[..lower.len() - 1], 10),
        Some('m') => (&lower[..lower.len() - 1], 20),
        Some('g') => (&lower[..lower.len() - 1], 30),
        Some('t') => (&lower[..lower.len() - 1], 40),
        _ => (lower, 0),
    };
    let value: u64 = digits
        .parse()
        .map_err(|_| format!("invalid size {text:?}"))?;
    value
        .checked_shl(shift)
        .filter(|v| v >> shift == value)
        .ok_or_else(|| format!("size {text:?} overflows"))
}

fn parse_list(text: &str) -> Result<Vec<u64>, String> {
    text.split(',')
        .filter(|s| !s.is_empty())
        .map(parse_size)
        .collect()
}

fn parse_scenarios(text: &str, out: &mut Vec<Scenario>) -> Result<(), String> {
    for name in text.split(',').filter(|s| !s.is_empty()) {
        if name == "all" {
            out.extend(Scenario::ALL);
            continue;
        }
        let scenario = Scenario::ALL
            .into_iter()
            .find(|s| s.name() == name)
            .ok_or_else(|| format!("unknown scenario {name:?}"))?;
        out.push(scenario);
    }
    Ok(())
}

impl Args {
    /// Parse `std::env::args`, ignoring the flags cargo's bench runner adds.
    pub fn parse() -> Result<Self, String> {
        Self::parse_from(std::env::args().skip(1))
    }

    pub fn parse_from(raw: impl IntoIterator<Item = String>) -> Result<Self, String> {
        let mut args = Args {
            devices: Vec::new(),
            destroy_data: false,
            file: None,
            file_bytes: 4 << 30,
            keep_file: false,
            workers: 1,
            pin: false,
            cpu_offset: 0,
            slots_per_worker: None,
            scenarios: Vec::new(),
            duration: Duration::from_secs(10),
            qd: 16,
            page_bytes: vec![16 << 20, 1 << 20, 64 << 10, 4 << 10],
            commit_interval: Duration::from_millis(1000),
            entries: vec![1_000, 10_000, 100_000],
            mutations: vec![1, 100, 10_000],
            read_fill_bytes: None,
            hold_percent: 100,
            retain_secs: 0,
            pages_per_version: 16,
            cpu_entries: 1_000_000,
            lookup_entries: 1_000_000,
            metadata_cache: 64 << 20,
            spin: 0,
            ring_entries: 1024,
            verify: true,
            wipe: false,
        };
        let mut raw = raw.into_iter();
        while let Some(flag) = raw.next() {
            let (flag, inline) = match flag.split_once('=') {
                Some((f, v)) if f.starts_with("--") => (f.to_string(), Some(v.to_string())),
                _ => (flag, None),
            };
            let mut value = || -> Result<String, String> {
                inline
                    .clone()
                    .or_else(|| raw.next())
                    .ok_or_else(|| format!("{flag} needs a value"))
            };
            let number = |v: String| -> Result<u64, String> { parse_size(&v) };
            match flag.as_str() {
                "-h" | "--help" => return Err(String::new()),
                // Added by `cargo bench`.
                "--bench" => {}
                "--devices" => args.devices.extend(
                    value()?
                        .split(',')
                        .filter(|s| !s.is_empty())
                        .map(PathBuf::from),
                ),
                "--destroy-data" => args.destroy_data = true,
                "--file" => args.file = Some(PathBuf::from(value()?)),
                "--file-bytes" => args.file_bytes = number(value()?)?,
                "--keep-file" => args.keep_file = true,
                "--workers" => args.workers = number(value()?)? as usize,
                "--pin" => args.pin = true,
                "--cpu-offset" => args.cpu_offset = number(value()?)? as usize,
                "--slots-per-worker" => args.slots_per_worker = Some(number(value()?)?),
                "--scenario" | "--scenarios" => parse_scenarios(&value()?, &mut args.scenarios)?,
                "--duration" => {
                    let v = value()?;
                    let secs: f64 = v.parse().map_err(|_| format!("invalid duration {v:?}"))?;
                    args.duration = Duration::from_secs_f64(secs);
                }
                "--qd" => args.qd = number(value()?)? as usize,
                "--page-bytes" => args.page_bytes = parse_list(&value()?)?,
                "--commit-interval-ms" => {
                    args.commit_interval = Duration::from_millis(number(value()?)?)
                }
                "--entries" => args.entries = parse_list(&value()?)?,
                "--mutations" => args.mutations = parse_list(&value()?)?,
                "--read-fill-bytes" => args.read_fill_bytes = Some(number(value()?)?),
                "--hold-percent" => args.hold_percent = number(value()?)?,
                "--retain-secs" => args.retain_secs = number(value()?)?,
                "--pages-per-version" => {
                    args.pages_per_version = number(value()?)?.clamp(1, u32::MAX.into()) as u32
                }
                "--cpu-entries" => args.cpu_entries = number(value()?)?,
                "--lookup-entries" => args.lookup_entries = number(value()?)?,
                "--metadata-cache" => args.metadata_cache = number(value()?)?,
                "--spin" => args.spin = number(value()?)? as u32,
                "--ring-entries" => args.ring_entries = number(value()?)? as usize,
                "--no-verify" => args.verify = false,
                "--wipe" => args.wipe = true,
                other => return Err(format!("unknown flag {other:?}")),
            }
        }
        if args.scenarios.is_empty() {
            args.scenarios.extend(Scenario::ALL);
        }
        args.scenarios.dedup();
        if args.devices.is_empty() == args.file.is_none() {
            return Err("give exactly one of --devices or --file".into());
        }
        if !args.devices.is_empty() && !args.destroy_data {
            return Err("--devices overwrites the devices; add --destroy-data to confirm".into());
        }
        if args.workers == 0 || args.qd == 0 || args.ring_entries < 64 {
            return Err("--workers and --qd must be positive, --ring-entries >= 64".into());
        }
        for &bytes in &args.page_bytes {
            if bytes == 0 || !bytes.is_multiple_of(4096) || bytes > page_alloc::volume::SLOT_BYTES {
                return Err(format!(
                    "--page-bytes {bytes} must be a positive multiple of 4096 up to SLOT_BYTES"
                ));
            }
        }
        Ok(args)
    }
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Watch the daemon's TOML config file and emit debounced reload events.
//!
//! A `ConfigWatcher` installs a `notify` recursive=false watcher on the
//! config file's parent directory (so atomic rename / symlink swap, as
//! used by Kubernetes ConfigMap mounts, is observed) and forwards the
//! raw events to a debounce thread. The debounce thread coalesces
//! bursts inside a 200ms quiet window and then attempts to re-parse the
//! file; only successful parses are surfaced as `ConfigUpdate` events.
//!
//! Reconciling those updates back into running subsystems is the job of
//! later phases; this module's surface is intentionally event-only.

use std::fmt;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::thread::{self, JoinHandle};
use std::time::Duration;

use notify::{RecommendedWatcher, RecursiveMode, Watcher};

use super::load;
use super::schema::Config;

const DEBOUNCE: Duration = Duration::from_millis(200);

#[derive(Debug)]
pub struct ConfigUpdate {
    pub config: Arc<Config>,
    pub generation: u64,
}

pub struct ConfigWatcher {
    shutdown: Arc<AtomicBool>,
    // `_watcher` is held so the underlying notify backend stays alive.
    // Dropping it (in `Drop`) closes its internal event channel and
    // unblocks the debounce thread's `recv`.
    _watcher: Option<RecommendedWatcher>,
    worker: Option<JoinHandle<()>>,
}

impl ConfigWatcher {
    pub fn new(path: PathBuf) -> Result<(Self, mpsc::Receiver<ConfigUpdate>), WatchError> {
        let (raw_tx, raw_rx) = mpsc::channel::<()>();
        let (update_tx, update_rx) = mpsc::channel::<ConfigUpdate>();

        let event_tx = raw_tx.clone();
        let filter_path = path.clone();
        let mut watcher: RecommendedWatcher =
            notify::recommended_watcher(move |res: Result<notify::Event, notify::Error>| {
                // The watch is on the config file's *parent* directory (so
                // atomic rename / symlink swaps are observed), which means
                // it also sees writes to unrelated sibling files living in
                // that directory (e.g. log or data files). Forward only
                // events that actually touch the config file; otherwise an
                // unrelated sibling write would trigger a needless reload
                // and apply churn. The debounce thread re-reads the file.
                if let Ok(event) = res {
                    if event_affects_config(&event, &filter_path) {
                        let _ = event_tx.send(());
                    }
                }
            })?;

        let watch_target = match path.parent() {
            Some(p) if !p.as_os_str().is_empty() && p.exists() => p.to_path_buf(),
            _ => path.clone(),
        };
        watcher.watch(&watch_target, RecursiveMode::NonRecursive)?;

        let shutdown = Arc::new(AtomicBool::new(false));
        let worker_shutdown = shutdown.clone();
        let worker_path = path.clone();
        let worker = thread::Builder::new()
            .name("ub-storage-config-watch".into())
            .spawn(move || {
                debounce_loop(worker_path, raw_rx, update_tx, worker_shutdown);
            })
            .map_err(WatchError::Io)?;

        Ok((
            Self {
                shutdown,
                _watcher: Some(watcher),
                worker: Some(worker),
            },
            update_rx,
        ))
    }
}

impl Drop for ConfigWatcher {
    fn drop(&mut self) {
        self.shutdown.store(true, Ordering::Release);
        // Drop the notify watcher first so its internal sender side
        // closes, unblocking any pending recv in the debounce loop.
        self._watcher.take();
        if let Some(h) = self.worker.take() {
            let _ = h.join();
        }
    }
}

// Kubernetes mounts a ConfigMap by writing the data into a timestamped
// directory and atomically swapping a `..data` symlink to point at it;
// the config file itself is a symlink into `..data`. Such an update
// surfaces as an event on the `..data` entry rather than on the config
// file's own name, so it must be treated as a config change too.
const CONFIGMAP_DATA_SENTINEL: &str = "..data";

/// Whether `event` touches `config_path` (or the ConfigMap `..data`
/// symlink that aliases it), as opposed to an unrelated sibling file in
/// the watched parent directory.
///
/// Events carrying no paths are treated as relevant: some backends emit
/// pathless rescan notifications, and dropping those could miss a real
/// change. The cost of a false positive is only a redundant re-read.
fn event_affects_config(event: &notify::Event, config_path: &Path) -> bool {
    if event.paths.is_empty() {
        return true;
    }
    let config_name = config_path.file_name();
    event.paths.iter().any(|p| {
        p == config_path
            || (config_name.is_some() && p.file_name() == config_name)
            || p.file_name() == Some(CONFIGMAP_DATA_SENTINEL.as_ref())
    })
}

fn debounce_loop(
    path: PathBuf,
    raw_rx: mpsc::Receiver<()>,
    update_tx: mpsc::Sender<ConfigUpdate>,
    shutdown: Arc<AtomicBool>,
) {
    debounce_loop_with_receiver(path, update_tx, shutdown, |timeout| {
        raw_rx.recv_timeout(timeout)
    });
}

fn debounce_loop_with_receiver(
    path: PathBuf,
    update_tx: mpsc::Sender<ConfigUpdate>,
    shutdown: Arc<AtomicBool>,
    mut recv: impl FnMut(Duration) -> Result<(), mpsc::RecvTimeoutError>,
) {
    let mut generation: u64 = 0;
    loop {
        if shutdown.load(Ordering::Acquire) {
            return;
        }
        // Block until we see *some* event, then drain follow-on
        // events for up to `DEBOUNCE` of quiet to coalesce bursts.
        match recv(DEBOUNCE) {
            Ok(()) => {}
            Err(mpsc::RecvTimeoutError::Timeout) => continue,
            Err(mpsc::RecvTimeoutError::Disconnected) => return,
        }
        loop {
            if shutdown.load(Ordering::Acquire) {
                return;
            }
            match recv(DEBOUNCE) {
                Ok(()) => continue,
                Err(mpsc::RecvTimeoutError::Timeout) => break,
                Err(mpsc::RecvTimeoutError::Disconnected) => return,
            }
        }
        match load::load(Path::new(&path)) {
            Ok(cfg) => {
                generation = generation.wrapping_add(1);
                let update = ConfigUpdate {
                    config: Arc::new(cfg),
                    generation,
                };
                if update_tx.send(update).is_err() {
                    return;
                }
            }
            Err(e) => {
                eprintln!(
                    "config watch: reload of {} failed; keeping previous: {e}",
                    path.display()
                );
            }
        }
    }
}

#[derive(Debug)]
pub enum WatchError {
    Io(io::Error),
    Notify(notify::Error),
}

impl fmt::Display for WatchError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            WatchError::Io(e) => write!(f, "config watch io error: {e}"),
            WatchError::Notify(e) => write!(f, "config watch notify error: {e}"),
        }
    }
}

impl std::error::Error for WatchError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            WatchError::Io(e) => Some(e),
            WatchError::Notify(e) => Some(e),
        }
    }
}

impl From<io::Error> for WatchError {
    fn from(e: io::Error) -> Self {
        WatchError::Io(e)
    }
}

impl From<notify::Error> for WatchError {
    fn from(e: notify::Error) -> Self {
        WatchError::Notify(e)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::VecDeque;
    use std::fs;
    use std::io::Write;
    use tempfile::TempDir;

    const VALID_A: &str = r#"
fingers_per_node = 1234

[[backends]]
name = "b"

[backends.config.fake]
"#;

    const VALID_B: &str = r#"
fingers_per_node = 5678

[[backends]]
name = "b"

[backends.config.fake]
"#;

    fn valid_config_with_fingers(fingers_per_node: u32) -> String {
        format!(
            r#"
fingers_per_node = {fingers_per_node}

[[backends]]
name = "b"

[backends.config.fake]
"#,
        )
    }

    fn write(path: &Path, contents: &str) {
        let mut f = fs::OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(path)
            .unwrap();
        f.write_all(contents.as_bytes()).unwrap();
        f.flush().unwrap();
    }

    fn recv_within(rx: &mpsc::Receiver<ConfigUpdate>, timeout: Duration) -> Option<ConfigUpdate> {
        rx.recv_timeout(timeout).ok()
    }

    #[test]
    fn bad_toml_does_not_emit() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("config.toml");
        write(&path, VALID_A);

        let (_w, rx) = ConfigWatcher::new(path.clone()).unwrap();
        thread::sleep(Duration::from_millis(100));

        // First a valid edit, then garbage. We should see at most one
        // update (for the valid edit), and definitely none for the
        // garbage write.
        write(&path, VALID_B);
        // Wait for the valid update to arrive before corrupting the
        // file so the two writes don't get coalesced.
        let _ = recv_within(&rx, Duration::from_secs(3));

        write(&path, "this is = not = valid = toml");
        // No further update should come through for the garbage.
        let got = rx.recv_timeout(Duration::from_millis(800));
        assert!(got.is_err(), "garbage write must not yield an update");
    }

    #[test]
    fn burst_is_coalesced() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("config.toml");
        write(&path, &valid_config_with_fingers(4004));

        let (update_tx, update_rx) = mpsc::channel();
        let shutdown = Arc::new(AtomicBool::new(false));
        let mut events = VecDeque::from([
            Ok(()),
            Ok(()),
            Ok(()),
            Ok(()),
            Ok(()),
            Err(mpsc::RecvTimeoutError::Timeout),
            Err(mpsc::RecvTimeoutError::Disconnected),
        ]);

        debounce_loop_with_receiver(path, update_tx, shutdown, |_| {
            events.pop_front().expect("scripted receive result")
        });
        assert!(
            events.is_empty(),
            "the debounce loop should consume the scripted burst completely",
        );

        let updates: Vec<_> = update_rx.iter().collect();
        assert_eq!(updates.len(), 1, "the burst must emit exactly one update");
        assert_eq!(updates[0].config.fingers_per_node, Some(4004));
    }

    #[test]
    fn drop_joins_cleanly() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("config.toml");
        write(&path, VALID_A);

        {
            let (_w, _rx) = ConfigWatcher::new(path.clone()).unwrap();
            thread::sleep(Duration::from_millis(50));
        }
        // If Drop didn't join cleanly we'd hang or panic above.
    }

    #[test]
    fn modification_emits_update_and_sibling_is_filtered() {
        // End-to-end coverage through the real notify watcher, replacing
        // the old `emits_update_on_modification` test. It pins down two
        // properties in a single watcher lifetime (so the suite does not
        // pay for an extra concurrent watcher):
        //   1. A valid config modification surfaces a `ConfigUpdate`
        //      whose parsed contents reflect the new file, with an
        //      advancing generation.
        //   2. A write to an unrelated sibling file in the watched parent
        //      directory is filtered out (`event_affects_config`) and
        //      yields no update; a subsequent real config change still
        //      does, which also proves the watcher stayed live.
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("config.toml");
        let sibling = dir.path().join("node1.disk");
        write(&path, VALID_A);

        let (_w, rx) = ConfigWatcher::new(path.clone()).unwrap();
        // Let the notify backend finish installing the watch before the
        // first edit so the modification is observed.
        thread::sleep(Duration::from_millis(100));

        // (1) A valid modification is surfaced with the new contents.
        write(&path, VALID_B);
        let update = recv_within(&rx, Duration::from_secs(3))
            .expect("a valid modification must yield a ConfigUpdate");
        assert_eq!(update.config.fingers_per_node, Some(5678));
        assert!(update.generation >= 1, "generation must advance");

        // (2a) An unrelated sibling write is filtered out.
        write(&sibling, "not a config file");
        assert!(
            rx.recv_timeout(Duration::from_millis(800)).is_err(),
            "a sibling write must be filtered out and yield no update",
        );

        // (2b) A real config change after the sibling write still emits,
        // proving the watcher is alive and the silence above was genuine
        // filtering rather than a dead watch.
        write(&path, VALID_A);
        let update = recv_within(&rx, Duration::from_secs(3))
            .expect("a config change after a sibling write must still emit");
        assert_eq!(update.config.fingers_per_node, Some(1234));
    }

    fn event_for(paths: &[&Path]) -> notify::Event {
        notify::Event {
            kind: notify::EventKind::Any,
            paths: paths.iter().map(|p| p.to_path_buf()).collect(),
            attrs: Default::default(),
        }
    }

    #[test]
    fn filter_matches_config_path() {
        let cfg = Path::new("/etc/storage/config.toml");
        assert!(event_affects_config(&event_for(&[cfg]), cfg));
    }

    #[test]
    fn filter_ignores_unrelated_sibling() {
        let cfg = Path::new("/var/run/smoke/node1.toml");
        let disk = Path::new("/var/run/smoke/node1.disk");
        let log = Path::new("/var/run/smoke/node1.log");
        assert!(!event_affects_config(&event_for(&[disk]), cfg));
        assert!(!event_affects_config(&event_for(&[log]), cfg));
        // A batched event touching both a sibling and the config still
        // counts as a config change.
        assert!(event_affects_config(&event_for(&[disk, cfg]), cfg));
    }

    #[test]
    fn filter_matches_configmap_data_swap() {
        let cfg = Path::new("/etc/storage/config.toml");
        let data = Path::new("/etc/storage/..data");
        assert!(event_affects_config(&event_for(&[data]), cfg));
    }

    #[test]
    fn filter_treats_pathless_event_as_relevant() {
        let cfg = Path::new("/etc/storage/config.toml");
        assert!(event_affects_config(&event_for(&[]), cfg));
    }
}

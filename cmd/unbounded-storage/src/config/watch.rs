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
        let mut watcher: RecommendedWatcher =
            notify::recommended_watcher(move |res: Result<notify::Event, notify::Error>| {
                // Surface only the fact that *something* changed; the
                // debounce thread re-reads the file from disk.
                if res.is_ok() {
                    let _ = event_tx.send(());
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

fn debounce_loop(
    path: PathBuf,
    raw_rx: mpsc::Receiver<()>,
    update_tx: mpsc::Sender<ConfigUpdate>,
    shutdown: Arc<AtomicBool>,
) {
    let mut generation: u64 = 0;
    loop {
        if shutdown.load(Ordering::Acquire) {
            return;
        }
        // Block until we see *some* event, then drain follow-on
        // events for up to `DEBOUNCE` of quiet to coalesce bursts.
        match raw_rx.recv_timeout(DEBOUNCE) {
            Ok(()) => {}
            Err(mpsc::RecvTimeoutError::Timeout) => continue,
            Err(mpsc::RecvTimeoutError::Disconnected) => return,
        }
        loop {
            if shutdown.load(Ordering::Acquire) {
                return;
            }
            match raw_rx.recv_timeout(DEBOUNCE) {
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
    use std::fs;
    use std::io::Write;
    use std::time::Instant;
    use tempfile::TempDir;

    const VALID_A: &str = r#"
[fabric]
listen_addr = "0.0.0.0:1234"
"#;

    const VALID_B: &str = r#"
[fabric]
listen_addr = "0.0.0.0:5678"
"#;

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
    fn emits_update_on_modification() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("config.toml");
        write(&path, VALID_A);

        let (_w, rx) = ConfigWatcher::new(path.clone()).unwrap();
        // Small settle so the watcher is fully armed.
        thread::sleep(Duration::from_millis(100));
        write(&path, VALID_B);

        let upd = recv_within(&rx, Duration::from_secs(3))
            .expect("expected a ConfigUpdate after modification");
        assert_eq!(upd.generation, 1);
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
        write(&path, VALID_A);

        let (_w, rx) = ConfigWatcher::new(path.clone()).unwrap();
        thread::sleep(Duration::from_millis(100));

        // Five quick back-to-back writes. The debounce window is
        // 200ms, so these should fold down into far fewer updates.
        // We deliberately do not sleep between writes: any inter-write
        // delay just eats into the debounce budget on slow/loaded
        // systems (e.g. CI) and turns this into a flaky timing test.
        let start = Instant::now();
        for i in 0..5 {
            let body = format!("[fabric]\nlisten_addr = \"0.0.0.0:{}\"\n", 4000 + i);
            write(&path, &body);
        }
        assert!(
            start.elapsed() < Duration::from_millis(200),
            "test precondition: burst must finish inside the debounce window",
        );

        // Drain for ~2s.
        let mut updates = 0;
        let deadline = Instant::now() + Duration::from_secs(2);
        while Instant::now() < deadline {
            match rx.recv_timeout(Duration::from_millis(250)) {
                Ok(_) => updates += 1,
                Err(_) => {}
            }
        }
        assert!(updates >= 1, "expected at least one update from the burst");
        assert!(
            updates < 5,
            "expected debounce to coalesce 5 writes into <5 updates, got {updates}",
        );
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
}

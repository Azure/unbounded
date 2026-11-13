//! Managed-mode startup binding between embedded cluster control and the dataplane.
//!
//! The first managed start records the cluster UID in a sentinel and moves
//! enrollment into a cluster-scoped subdirectory. Legacy standalone identity
//! files in the root are never read, rewritten, or removed. A later start that
//! reports a different cluster UID re-enrolls the node after cluster control has
//! checked the installation binding, removing stale managed enrollment state.

use super::Managed;
use crate::config::Config;
use crate::error::{Error, Result};
use racer_wire::control::ClusterId;
use std::io::{Read, Write};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::time::Duration;

/// Subdirectory of the identity root reserved for managed enrollment.
const MANAGED_DIRECTORY: &str = "managed";

/// Sentinel recording the current managed enrollment cluster UID.
const CLUSTER_SENTINEL: &str = "cluster";

/// Embedded authority service as seen by dataplane startup.
pub trait ClusterControl: Send + Sync + 'static {
    /// Wait up to `timeout` for durable cluster state; return cluster and device selector.
    fn bootstrap(&self, timeout: Duration) -> std::result::Result<Bootstrapped, BootstrapError>;

    /// Shared, discovery-refreshed elected endpoint set.
    fn endpoints(&self) -> crate::control::rest::DiscoveredEndpoints;

    /// Enqueue a Pod report of shares and rails; must not block the caller.
    fn report(&self, shares: u32, rails: Vec<racer_wire::control::RailMapping>);
}

/// Cluster state returned once cluster control has initialized or joined it.
pub struct Bootstrapped {
    /// Cluster UID derived from the cluster state Secret.
    pub cluster: ClusterId,

    /// Optional device selector for this node.
    pub block_devices: Option<String>,
}

/// Whether bootstrap may still succeed on a later attempt.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BootstrapError {
    /// Not ready yet; retry after the retry floor.
    Retry,

    /// The requested wait elapsed with no outcome yet; keep waiting.
    Pending,

    /// Stopped or invalid; startup must fail.
    Fatal,
}

/// Interval between bootstrap progress messages while cluster control converges.
const BOOTSTRAP_WAIT: Duration = Duration::from_secs(30);

/// Least time between bootstrap attempts, so a fast `Retry` cannot spin.
const BOOTSTRAP_RETRY_FLOOR: Duration = Duration::from_secs(1);

/// Longest single wait on control before `stop` is checked again.
const BOOTSTRAP_POLL: Duration = Duration::from_millis(100);

/// Reserve the control thread, wait for bootstrap, and bind `config` to its result.
///
/// Waits without a deadline while control reports retryable progress, because
/// enrollment cannot start before cluster state exists. `stop` is checked
/// at least every [`BOOTSTRAP_POLL`], so shutdown is not held by a full wait.
pub fn start(
    config: &mut Config,
    control: std::sync::Arc<dyn ClusterControl>,
    stop: &dyn Fn() -> bool,
) -> Result<()> {
    start_with(config, control, stop, BOOTSTRAP_RETRY_FLOOR)
}

/// [`start`] with an explicit retry floor.
fn start_with(
    config: &mut Config,
    control: std::sync::Arc<dyn ClusterControl>,
    stop: &dyn Fn() -> bool,
    floor: Duration,
) -> Result<()> {
    config.reserve_control_thread()?;
    let bootstrapped = loop {
        if stop() {
            return Err(Error::Canceled);
        }
        let started = std::time::Instant::now();
        // One logical attempt waits up to BOOTSTRAP_WAIT in short slices so a stop
        // request ends it within one poll; control itself is never restarted.
        let outcome = loop {
            let slice = BOOTSTRAP_POLL.min(BOOTSTRAP_WAIT.saturating_sub(started.elapsed()));
            match control.bootstrap(slice) {
                Err(BootstrapError::Pending) if started.elapsed() < BOOTSTRAP_WAIT => {
                    if stop() {
                        return Err(Error::Canceled);
                    }
                }
                Err(BootstrapError::Pending) => break Err(BootstrapError::Retry),
                outcome => break outcome,
            }
        };
        match outcome {
            Ok(bootstrapped) => break bootstrapped,
            Err(BootstrapError::Retry | BootstrapError::Pending) => {
                eprintln!("racer: stage=cluster-bootstrap state=waiting");
                let resume = started + floor;
                while let Some(left) = resume.checked_duration_since(std::time::Instant::now()) {
                    if left.is_zero() {
                        break;
                    }
                    if stop() {
                        return Err(Error::Canceled);
                    }
                    std::thread::sleep(left.min(BOOTSTRAP_POLL));
                }
            }
            Err(BootstrapError::Fatal) => {
                eprintln!("racer: stage=cluster-bootstrap fatal error=control stopped");
                return Err(Error::Unavailable);
            }
        }
    };
    let reporter = control.clone();
    let managed = Managed {
        endpoints: control.endpoints(),
        report: std::sync::Arc::new(move |shares, rails| reporter.report(shares, rails)),
        block_devices: bootstrapped.block_devices,
    };
    adopt(config, bootstrapped.cluster, managed)
}

/// Bind `config` to the bootstrap cluster and attach managed control bindings.
///
/// Full validation runs again when the application starts.
pub fn adopt(config: &mut Config, cluster: ClusterId, managed: Managed) -> Result<()> {
    if !config.managed() || config.managed_control.is_some() {
        return Err(Error::InvalidConfiguration);
    }
    config.adopt_cluster(cluster.clone())?;
    config.enrollment_directory = Some(bind_cluster(&config.identity_directory, &cluster)?);
    config.managed_control = Some(managed);
    Ok(())
}

/// Record or verify the managed cluster sentinel and return the enrollment directory.
pub fn bind_cluster(root: &Path, cluster: &ClusterId) -> Result<PathBuf> {
    let managed = root.join(MANAGED_DIRECTORY);
    create_private(root)?;
    create_private(&managed)?;
    let sentinel = managed.join(CLUSTER_SENTINEL);
    match read_sentinel(&sentinel)? {
        Some(recorded) if recorded == cluster.0 => {}
        Some(recorded) => {
            eprintln!(
                "racer: stage=cluster-binding state=rebinding from={recorded} to={}",
                cluster.0
            );
            write_sentinel(&managed, &sentinel, &cluster.0)?;
        }
        None => write_sentinel(&managed, &sentinel, &cluster.0)?,
    }
    remove_stale_enrollment(&managed, cluster)?;
    let directory = managed.join(&cluster.0);
    create_private(&directory)?;
    Ok(directory)
}

/// Remove stale enrollment only after the current sentinel is durable.
///
/// Run on every bind so a restart finishes cleanup interrupted by a crash.
/// Symlinks are removed themselves, without touching their targets.
fn remove_stale_enrollment(managed: &Path, cluster: &ClusterId) -> Result<()> {
    let temporary = format!("{CLUSTER_SENTINEL}.tmp");
    for entry in std::fs::read_dir(managed).map_err(|_| Error::Io)? {
        let entry = entry.map_err(|_| Error::Io)?;
        let name = entry.file_name();
        if name == cluster.0.as_str() || name == CLUSTER_SENTINEL || name == temporary.as_str() {
            continue;
        }
        if entry.file_type().map_err(|_| Error::Io)?.is_dir() {
            std::fs::remove_dir_all(entry.path()).map_err(|_| Error::Io)?;
        } else {
            std::fs::remove_file(entry.path()).map_err(|_| Error::Io)?;
        }
    }
    std::fs::File::open(managed)
        .and_then(|d| d.sync_all())
        .map_err(|_| Error::Io)
}

/// Create a node-private directory, accepting an existing real directory only.
fn create_private(path: &Path) -> Result<()> {
    match std::fs::DirBuilder::new().mode(0o700).create(path) {
        Ok(()) => {}
        Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {}
        Err(_) => return Err(Error::Io),
    }
    let metadata = std::fs::symlink_metadata(path).map_err(|_| Error::Io)?;
    if !metadata.is_dir() {
        return Err(Error::InvalidConfiguration);
    }
    Ok(())
}

/// Read the sentinel without following symlinks; absence means first adoption.
fn read_sentinel(path: &Path) -> Result<Option<String>> {
    let file = match std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW)
        .open(path)
    {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err(Error::Io),
    };
    let mut text = String::new();
    file.take(128)
        .read_to_string(&mut text)
        .map_err(|_| Error::CorruptRecord)?;
    let recorded = text.trim_end_matches('\n');
    if !racer_wire::control::valid_uuid(recorded) {
        return Err(Error::CorruptRecord);
    }
    Ok(Some(recorded.to_owned()))
}

/// Durably publish the sentinel with write, fsync, rename, and directory fsync.
fn write_sentinel(directory: &Path, path: &Path, cluster: &str) -> Result<()> {
    let temporary = directory.join(format!("{CLUSTER_SENTINEL}.tmp"));
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW)
        .open(&temporary)
        .map_err(|_| Error::Io)?;
    file.write_all(format!("{cluster}\n").as_bytes())
        .and_then(|()| file.sync_all())
        .map_err(|_| Error::Io)?;
    std::fs::rename(&temporary, path).map_err(|_| Error::Io)?;
    std::fs::File::open(directory)
        .and_then(|d| d.sync_all())
        .map_err(|_| Error::Io)
}

#[cfg(test)]
mod tests {
    use super::*;

    const FIRST: &str = "11111111-1111-4111-8111-111111111111";
    const SECOND: &str = "22222222-2222-4222-8222-222222222222";

    /// Fixture directory inside the crate target tree, removed on drop.
    struct Directory(PathBuf);

    impl Directory {
        fn new(name: &str) -> Self {
            let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("target/managed-tests")
                .join(format!("{}-{name}", std::process::id()));
            let _ = std::fs::remove_dir_all(&path);
            std::fs::create_dir_all(&path).unwrap();
            Self(path)
        }
    }

    impl Drop for Directory {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    fn managed() -> Managed {
        Managed {
            endpoints: Default::default(),
            report: std::sync::Arc::new(|_, _| {}),
            block_devices: Some("nvme*".into()),
        }
    }

    fn managed_config(root: &Path) -> Config {
        let mut config = crate::test_support::cluster::config(false);
        config.control_endpoint = None;
        config.cluster = ClusterId(crate::config::UNRESOLVED_CLUSTER_ID.into());
        config.identity_directory = root.join("identity");
        config.max_threads = crate::config::DEFAULT_MAX_THREADS;
        config
    }

    #[test]
    fn first_adoption_preserves_legacy_identity_and_scopes_enrollment() {
        let d = Directory::new("first");
        let root = d.0.join("identity");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::write(root.join("identity.json"), b"legacy").unwrap();
        let mut config = managed_config(&d.0);
        adopt(&mut config, ClusterId(FIRST.into()), managed()).unwrap();
        assert_eq!(config.cluster.0, FIRST);
        assert_eq!(
            config.enrollment_directory.as_deref(),
            Some(root.join("managed").join(FIRST).as_path())
        );
        assert_eq!(
            std::fs::read(root.join("identity.json")).unwrap(),
            b"legacy"
        );
        assert_eq!(
            std::fs::read_to_string(root.join("managed/cluster")).unwrap(),
            format!("{FIRST}\n")
        );
        // Adoption is one-shot per process.
        assert_eq!(
            adopt(&mut config, ClusterId(FIRST.into()), managed()),
            Err(Error::InvalidConfiguration)
        );
    }

    #[test]
    fn restart_with_same_cluster_reuses_and_changed_cluster_reenrolls() {
        let d = Directory::new("restart");
        let first = bind_cluster(&d.0, &ClusterId(FIRST.into())).unwrap();
        std::fs::write(first.join("identity.json"), b"managed").unwrap();
        assert_eq!(bind_cluster(&d.0, &ClusterId(FIRST.into())).unwrap(), first);
        assert_eq!(
            std::fs::read(first.join("identity.json")).unwrap(),
            b"managed"
        );
        let second = bind_cluster(&d.0, &ClusterId(SECOND.into())).unwrap();
        assert_eq!(second, d.0.join("managed").join(SECOND));
        assert!(second.is_dir());
        assert!(!second.join("identity.json").exists());
        assert!(!first.exists());
        assert_eq!(
            std::fs::read_to_string(d.0.join("managed/cluster")).unwrap(),
            format!("{SECOND}\n")
        );
    }

    #[test]
    fn corrupt_or_symlinked_sentinel_is_rejected() {
        let d = Directory::new("corrupt");
        std::fs::create_dir_all(d.0.join("managed")).unwrap();
        std::fs::write(d.0.join("managed/cluster"), b"not-a-uuid").unwrap();
        assert_eq!(
            bind_cluster(&d.0, &ClusterId(FIRST.into())),
            Err(Error::CorruptRecord)
        );
        std::fs::remove_file(d.0.join("managed/cluster")).unwrap();
        std::fs::write(d.0.join("target"), format!("{FIRST}\n")).unwrap();
        std::os::unix::fs::symlink(d.0.join("target"), d.0.join("managed/cluster")).unwrap();
        assert_eq!(bind_cluster(&d.0, &ClusterId(FIRST.into())), Err(Error::Io));
    }

    #[test]
    fn lost_sentinel_beside_other_cluster_reenrolls() {
        let d = Directory::new("lost");
        let first = bind_cluster(&d.0, &ClusterId(FIRST.into())).unwrap();
        std::fs::write(first.join("identity.json"), b"managed").unwrap();
        std::fs::remove_file(d.0.join("managed/cluster")).unwrap();
        let second = bind_cluster(&d.0, &ClusterId(SECOND.into())).unwrap();
        assert!(second.is_dir());
        assert!(!first.exists());
        assert_eq!(
            std::fs::read_to_string(d.0.join("managed/cluster")).unwrap(),
            format!("{SECOND}\n")
        );
        // The same cluster may restore its own lost sentinel.
        std::fs::write(second.join("identity.json"), b"new-managed").unwrap();
        std::fs::remove_file(d.0.join("managed/cluster")).unwrap();
        assert_eq!(
            bind_cluster(&d.0, &ClusterId(SECOND.into())).unwrap(),
            second
        );
        assert_eq!(
            std::fs::read(second.join("identity.json")).unwrap(),
            b"new-managed"
        );
    }

    #[test]
    fn failed_sentinel_update_preserves_old_enrollment() {
        let d = Directory::new("sentinel-write-failure");
        let first = bind_cluster(&d.0, &ClusterId(FIRST.into())).unwrap();
        std::fs::write(first.join("identity.json"), b"managed").unwrap();
        let target = d.0.join("untouched");
        std::fs::write(&target, b"untouched").unwrap();
        std::os::unix::fs::symlink(&target, d.0.join("managed/cluster.tmp")).unwrap();

        assert_eq!(
            bind_cluster(&d.0, &ClusterId(SECOND.into())),
            Err(Error::Io)
        );
        assert_eq!(
            std::fs::read_to_string(d.0.join("managed/cluster")).unwrap(),
            format!("{FIRST}\n")
        );
        assert_eq!(
            std::fs::read(first.join("identity.json")).unwrap(),
            b"managed"
        );
        assert_eq!(std::fs::read(target).unwrap(), b"untouched");
        assert!(!d.0.join("managed").join(SECOND).exists());
    }

    #[test]
    fn restart_finishes_cleanup_after_sentinel_was_updated() {
        let d = Directory::new("interrupted-cleanup");
        let first = bind_cluster(&d.0, &ClusterId(FIRST.into())).unwrap();
        std::fs::write(first.join("identity.json"), b"old-managed").unwrap();
        std::fs::write(d.0.join("identity.json"), b"legacy").unwrap();
        let managed = d.0.join("managed");
        write_sentinel(&managed, &managed.join(CLUSTER_SENTINEL), SECOND).unwrap();
        std::fs::write(managed.join("stale-file"), b"stale").unwrap();
        let second = managed.join(SECOND);
        create_private(&second).unwrap();
        std::fs::write(second.join("identity.json"), b"new-managed").unwrap();

        assert_eq!(
            bind_cluster(&d.0, &ClusterId(SECOND.into())).unwrap(),
            second
        );
        assert!(!first.exists());
        assert!(!managed.join("stale-file").exists());
        assert_eq!(std::fs::read(d.0.join("identity.json")).unwrap(), b"legacy");
        assert_eq!(
            std::fs::read(second.join("identity.json")).unwrap(),
            b"new-managed"
        );
        assert_eq!(
            bind_cluster(&d.0, &ClusterId(SECOND.into())).unwrap(),
            second
        );
    }

    #[test]
    fn stale_symlinks_are_removed_without_following_targets() {
        let d = Directory::new("stale-symlinks");
        let first = bind_cluster(&d.0, &ClusterId(FIRST.into())).unwrap();
        let target = d.0.join("untouched");
        create_private(&target).unwrap();
        std::fs::write(target.join("identity.json"), b"untouched").unwrap();
        let managed = d.0.join("managed");
        std::os::unix::fs::symlink(&target, managed.join("stale-link")).unwrap();
        std::os::unix::fs::symlink(&target, first.join("nested-link")).unwrap();
        std::os::unix::fs::symlink(d.0.join("absent"), managed.join("dangling-link")).unwrap();

        bind_cluster(&d.0, &ClusterId(SECOND.into())).unwrap();
        assert!(!first.exists());
        assert!(std::fs::symlink_metadata(managed.join("stale-link")).is_err());
        assert!(std::fs::symlink_metadata(managed.join("dangling-link")).is_err());
        assert_eq!(
            std::fs::read(target.join("identity.json")).unwrap(),
            b"untouched"
        );
    }

    #[test]
    fn symlinked_current_enrollment_is_rejected_without_following_target() {
        let d = Directory::new("current-symlink");
        let first = bind_cluster(&d.0, &ClusterId(FIRST.into())).unwrap();
        let target = d.0.join("untouched");
        create_private(&target).unwrap();
        std::fs::write(target.join("identity.json"), b"untouched").unwrap();
        std::os::unix::fs::symlink(&target, d.0.join("managed").join(SECOND)).unwrap();

        assert_eq!(
            bind_cluster(&d.0, &ClusterId(SECOND.into())),
            Err(Error::InvalidConfiguration)
        );
        assert!(!first.exists());
        assert_eq!(
            std::fs::read(target.join("identity.json")).unwrap(),
            b"untouched"
        );
    }

    #[test]
    fn leftover_temporary_sentinel_does_not_block_first_binding() {
        let d = Directory::new("tmp");
        std::fs::create_dir_all(d.0.join("managed")).unwrap();
        std::fs::write(d.0.join("managed/cluster.tmp"), b"partial").unwrap();
        bind_cluster(&d.0, &ClusterId(FIRST.into())).unwrap();
        assert_eq!(
            std::fs::read_to_string(d.0.join("managed/cluster")).unwrap(),
            format!("{FIRST}\n")
        );
    }

    #[test]
    fn symlinked_managed_directory_is_rejected() {
        let d = Directory::new("symlink-dir");
        std::fs::create_dir_all(d.0.join("elsewhere")).unwrap();
        std::os::unix::fs::symlink(d.0.join("elsewhere"), d.0.join("managed")).unwrap();
        assert_eq!(
            bind_cluster(&d.0, &ClusterId(FIRST.into())),
            Err(Error::InvalidConfiguration)
        );
        assert!(!d.0.join("elsewhere/cluster").exists());
    }

    #[test]
    fn fast_retry_is_paced_by_the_floor() {
        let d = Directory::new("pace");
        let mut config = managed_config(&d.0);
        let control = std::sync::Arc::new(Scripted::new(vec![
            Err(BootstrapError::Retry),
            Err(BootstrapError::Retry),
            bootstrapped(FIRST),
        ]));
        let started = std::time::Instant::now();
        start_with(&mut config, control, &|| false, Duration::from_millis(50)).unwrap();
        assert!(started.elapsed() >= Duration::from_millis(100));
    }

    /// Scripted control: retries, then a fixed bootstrap result.
    struct Scripted {
        results: std::sync::Mutex<Vec<std::result::Result<Bootstrapped, BootstrapError>>>,

        reports: std::sync::Mutex<Vec<(u32, usize)>>,

        endpoints: crate::control::rest::DiscoveredEndpoints,
    }

    impl Scripted {
        fn new(results: Vec<std::result::Result<Bootstrapped, BootstrapError>>) -> Self {
            Self {
                results: std::sync::Mutex::new(results),
                reports: Default::default(),
                endpoints: Default::default(),
            }
        }
    }

    impl ClusterControl for Scripted {
        fn bootstrap(&self, _: Duration) -> std::result::Result<Bootstrapped, BootstrapError> {
            self.results.lock().unwrap().remove(0)
        }

        fn endpoints(&self) -> crate::control::rest::DiscoveredEndpoints {
            self.endpoints.clone()
        }

        fn report(&self, shares: u32, rails: Vec<racer_wire::control::RailMapping>) {
            self.reports.lock().unwrap().push((shares, rails.len()));
        }
    }

    fn bootstrapped(cluster: &str) -> std::result::Result<Bootstrapped, BootstrapError> {
        Ok(Bootstrapped {
            cluster: ClusterId(cluster.into()),
            block_devices: Some("nvme*".into()),
        })
    }

    #[test]
    fn start_retries_until_bootstrap_then_binds_endpoints_and_report() {
        let d = Directory::new("start");
        let mut config = managed_config(&d.0);
        config.max_threads = 3;
        let control = std::sync::Arc::new(Scripted::new(vec![
            Err(BootstrapError::Retry),
            Err(BootstrapError::Retry),
            bootstrapped(FIRST),
        ]));
        start(&mut config, control.clone(), &|| false).unwrap();
        assert_eq!(config.max_threads, 2);
        assert_eq!(config.cluster.0, FIRST);
        let managed = config.managed_control.as_ref().unwrap();
        assert!(std::sync::Arc::ptr_eq(
            &managed.endpoints,
            &control.endpoints
        ));
        assert_eq!(managed.block_devices.as_deref(), Some("nvme*"));
        (managed.report)(7, Vec::new());
        assert_eq!(*control.reports.lock().unwrap(), vec![(7, 0)]);
    }

    #[test]
    fn start_fails_on_fatal_stop_and_budget_but_recovers_changed_cluster() {
        let d = Directory::new("start-fail");
        let mut config = managed_config(&d.0);
        let fatal = std::sync::Arc::new(Scripted::new(vec![Err(BootstrapError::Fatal)]));
        assert_eq!(
            start(&mut config, fatal, &|| false),
            Err(Error::Unavailable)
        );
        let mut config = managed_config(&d.0);
        let never = std::sync::Arc::new(Scripted::new(vec![]));
        assert_eq!(
            start(&mut config, never.clone(), &|| true),
            Err(Error::Canceled)
        );
        let mut config = managed_config(&d.0);
        config.max_threads = 2;
        assert_eq!(
            start(&mut config, never, &|| false),
            Err(Error::InvalidConfiguration)
        );
        let mut config = managed_config(&d.0);
        start(
            &mut config,
            std::sync::Arc::new(Scripted::new(vec![bootstrapped(FIRST)])),
            &|| false,
        )
        .unwrap();
        let mut config = managed_config(&d.0);
        start(
            &mut config,
            std::sync::Arc::new(Scripted::new(vec![bootstrapped(SECOND)])),
            &|| false,
        )
        .unwrap();
        assert_eq!(config.cluster.0, SECOND);
        assert_eq!(
            config.enrollment_directory,
            Some(config.identity_directory.join("managed").join(SECOND))
        );
        assert!(config.managed_control.is_some());
    }

    #[test]
    fn standalone_configuration_cannot_adopt() {
        let d = Directory::new("standalone");
        let mut config = crate::test_support::cluster::config(false);
        config.identity_directory = d.0.join("identity");
        assert_eq!(
            adopt(&mut config, ClusterId(FIRST.into()), managed()),
            Err(Error::InvalidConfiguration)
        );
        assert!(!d.0.join("identity").exists());
    }
}

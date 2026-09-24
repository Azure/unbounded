// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Process isolation for tests that can retain provider-owned DMA resources.

use std::{
    io,
    os::unix::process::CommandExt,
    process::{Child, Command, ExitStatus, Stdio},
    time::{Duration, Instant},
};

pub(super) const COMMAND_TIMEOUT: Duration = Duration::from_secs(5);
const REAP_TIMEOUT: Duration = Duration::from_secs(2);
const CHILD_ENV: &str = "RACER_RDMA_TEST_CHILD";

struct ProcessGroup(Child);

impl ProcessGroup {
    fn wait_until(&mut self, deadline: Instant) -> io::Result<Option<ExitStatus>> {
        loop {
            if let Some(status) = self.0.try_wait()? {
                return Ok(Some(status));
            }
            if Instant::now() >= deadline {
                return Ok(None);
            }
            std::thread::sleep(Duration::from_millis(10));
        }
    }
}

impl Drop for ProcessGroup {
    fn drop(&mut self) {
        // Kill descendants too, including a command blocked deleting an RXE
        // device. Never use wait()/wait_with_output() on this failure path:
        // a provider syscall can remain uninterruptible even after SIGKILL.
        // SAFETY: spawn assigned the child's PID as a new process group ID.
        let result = unsafe { libc::kill(-(self.0.id() as libc::pid_t), libc::SIGKILL) };
        if result != 0 && io::Error::last_os_error().raw_os_error() != Some(libc::ESRCH) {
            eprintln!(
                "RDMA test: kill process group {}: {}",
                self.0.id(),
                io::Error::last_os_error()
            );
        }
        match self.wait_until(Instant::now() + REAP_TIMEOUT) {
            Ok(Some(_)) => {}
            Ok(None) => eprintln!(
                "RDMA test: process {} did not reap after SIGKILL within {REAP_TIMEOUT:?}",
                self.0.id()
            ),
            Err(error) => eprintln!("RDMA test: reap process {}: {error}", self.0.id()),
        }
    }
}

pub(super) fn run(command: &mut Command, timeout: Duration) -> io::Result<()> {
    let description = format!("{command:?}");
    eprintln!("RDMA test: starting {description} (deadline {timeout:?})");
    // Inherit output so diagnostics remain visible and a full pipe cannot
    // prevent either the child or its supervisor from making progress.
    let mut child = ProcessGroup(
        command
            .process_group(0)
            .stdin(Stdio::null())
            .stdout(Stdio::inherit())
            .stderr(Stdio::inherit())
            .spawn()?,
    );
    match child.wait_until(Instant::now() + timeout)? {
        Some(status) if status.success() => Ok(()),
        Some(status) => Err(io::Error::other(format!("{description}: {status}"))),
        None => Err(io::Error::new(
            io::ErrorKind::TimedOut,
            format!(
                "{description}: exceeded {timeout:?}; killing process group {}",
                child.0.id()
            ),
        )),
    }
}

pub(super) fn is_child(test: &str) -> bool {
    std::env::var(CHILD_ENV).as_deref() == Ok(test)
}

pub(super) fn test_command(test: &str) -> Command {
    let mut command = Command::new(std::env::current_exe().unwrap());
    // module_path! includes the crate name; libtest's exact filter does not.
    let (_, filter) = test.split_once("::").unwrap();
    command
        .args([
            "--exact",
            filter,
            "--ignored",
            "--nocapture",
            "--test-threads=1",
        ])
        .env(CHILD_ENV, test)
        .env("RUST_BACKTRACE", "1");
    command
}

#[test]
fn command_success_failure_and_deadline_are_observable() {
    run(Command::new("sh").args(["-c", "exit 0"]), COMMAND_TIMEOUT).unwrap();
    let error = run(Command::new("sh").args(["-c", "exit 7"]), COMMAND_TIMEOUT).unwrap_err();
    assert_eq!(error.kind(), io::ErrorKind::Other);
    assert!(error.to_string().contains("exit status: 7"));
    let start = Instant::now();
    let error = run(
        Command::new("sh").args(["-c", "sleep 60 & wait"]),
        Duration::from_millis(50),
    )
    .unwrap_err();
    assert_eq!(error.kind(), io::ErrorKind::TimedOut);
    assert!(start.elapsed() < REAP_TIMEOUT + Duration::from_secs(2));
}

#[test]
fn unwinding_kills_and_reaps_the_command() {
    let mut child = ProcessGroup(
        Command::new("sh")
            .args(["-c", "sleep 60 & wait"])
            .process_group(0)
            .spawn()
            .unwrap(),
    );
    assert!(child.0.try_wait().unwrap().is_none());
    let pid = child.0.id();
    let start = Instant::now();
    let result = std::panic::catch_unwind(move || {
        let _child = child;
        panic!("exercise process cleanup during unwinding");
    });
    assert!(result.is_err());
    assert!(start.elapsed() < REAP_TIMEOUT + Duration::from_secs(2));
    // SAFETY: waitpid only observes whether our child remains unreaped.
    assert_eq!(
        unsafe { libc::waitpid(pid as libc::pid_t, std::ptr::null_mut(), libc::WNOHANG) },
        -1
    );
    assert_eq!(
        io::Error::last_os_error().raw_os_error(),
        Some(libc::ECHILD)
    );
}

#[test]
fn test_child_failure_and_blocked_panic_cleanup_are_bounded() {
    let test = concat!(module_path!(), "::panic_cleanup_child");
    let error = run(&mut test_command(test), COMMAND_TIMEOUT).unwrap_err();
    // This also checks that the exact libtest filter actually runs the child:
    // selecting zero tests would incorrectly return success.
    assert_eq!(error.kind(), io::ErrorKind::Other);
    let error = run(
        test_command(test).env("RACER_RDMA_TEST_BLOCK_DROP", "1"),
        Duration::from_millis(250),
    )
    .unwrap_err();
    assert_eq!(error.kind(), io::ErrorKind::TimedOut);
}

#[test]
#[ignore = "subprocess fixture for bounded panic cleanup"]
fn panic_cleanup_child() {
    let test = concat!(module_path!(), "::panic_cleanup_child");
    assert!(is_child(test), "invoke through its supervisor test");
    struct Cleanup;
    impl Drop for Cleanup {
        fn drop(&mut self) {
            if std::env::var_os("RACER_RDMA_TEST_BLOCK_DROP").is_some() {
                std::thread::sleep(Duration::from_secs(60));
            }
        }
    }
    let _cleanup = Cleanup;
    panic!("exercise transport panic before provider cleanup");
}

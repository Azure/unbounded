// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Tests of the real binary: exit codes, output streams, and signals.

use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

fn bin() -> Command {
    Command::new(env!("CARGO_BIN_EXE_racer-object"))
}

#[test]
fn help_and_version_exit_zero() {
    for args in [
        &["--help"][..],
        &["-h"],
        &["--version"],
        &["sidecar", "--help"],
        &["sidecar", "--version"],
    ] {
        let out = bin().args(args).output().unwrap();
        assert!(out.status.success(), "{args:?}");
        assert!(!out.stdout.is_empty(), "{args:?}");
        assert!(out.stderr.is_empty(), "{args:?}");
    }
    let out = bin().args(["sidecar", "--help"]).output().unwrap();
    let text = String::from_utf8(out.stdout).unwrap();
    assert!(text.starts_with("Usage: racer-object sidecar [flags]\n"));
    assert!(text.contains("  -cache string\n"));
}

#[test]
fn errors_exit_one_without_leaking() {
    for (args, message) in [
        (&[][..], "racer-object: expected sidecar (use --help)\n"),
        (&["origin"], "racer-object: unknown command (use --help)\n"),
        (&["sidecar"], "racer-object: --cache is required\n"),
        (
            &["sidecar", "--cache=Bad"],
            "racer-object: invalid --cache\n",
        ),
        (
            &["sidecar", "--cache=c", "--private-token"],
            "racer-object: invalid flags (use --help)\n",
        ),
        (
            &["sidecar", "--cache=c", "--listen=0.0.0.0:1"],
            "racer-object: --listen must use a loopback IP literal and port\n",
        ),
        (
            &["sidecar", "--cache=c", "--workers=0"],
            "racer-object: --workers must be between 1 and 256\n",
        ),
    ] {
        let out = bin().args(args).output().unwrap();
        assert_eq!(out.status.code(), Some(1), "{args:?}");
        assert_eq!(String::from_utf8(out.stderr).unwrap(), message);
        assert!(out.stdout.is_empty());
    }
}

#[test]
fn sigterm_exits_zero() {
    sigterm_exits_zero_with(&[]);
    sigterm_exits_zero_with(&["--workers=4", "--debug-listen=127.0.0.1:0"]);
}

fn sigterm_exits_zero_with(extra: &[&str]) {
    let mut child = bin()
        .args(["sidecar", "--cache=c", "--listen=127.0.0.1:0"])
        .args(extra)
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    // Give the runtime time to install signal handlers.
    std::thread::sleep(Duration::from_millis(300));
    // SAFETY: sending a signal to our own child process.
    unsafe { libc::kill(child.id() as libc::pid_t, libc::SIGTERM) };
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        if let Some(status) = child.try_wait().unwrap() {
            assert!(status.success(), "{status:?}");
            return;
        }
        assert!(Instant::now() < deadline, "did not exit");
        std::thread::sleep(Duration::from_millis(20));
    }
}

#[test]
fn bind_failure_exits_one() {
    let taken = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let listen = format!("--listen={}", taken.local_addr().unwrap());
    let out = bin()
        .args(["sidecar", "--cache=c", &listen])
        .output()
        .unwrap();
    assert_eq!(out.status.code(), Some(1));
    assert_eq!(
        String::from_utf8(out.stderr).unwrap(),
        "racer-object: could not bind sidecar listener\n"
    );
    let debug = format!("--debug-listen={}", taken.local_addr().unwrap());
    let out = bin()
        .args(["sidecar", "--cache=c", "--listen=127.0.0.1:0", &debug])
        .output()
        .unwrap();
    assert_eq!(out.status.code(), Some(1));
    assert_eq!(
        String::from_utf8(out.stderr).unwrap(),
        "racer-object: could not bind debug listener\n"
    );
    let out = bin()
        .args(["sidecar", "--cache=c", &listen, "--workers=3"])
        .output()
        .unwrap();
    assert_eq!(out.status.code(), Some(1));
    assert_eq!(
        String::from_utf8(out.stderr).unwrap(),
        "racer-object: could not bind sidecar listener\n"
    );
}

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::{env, process::Command};

#[test]
#[ignore = "subprocess helper isolates CLI arguments and runtime environment"]
fn cli_child() {
    let Some(args) = env::var_os("RACER_VERSION_TEST_ARGS") else {
        return;
    };
    let args = args.to_str().unwrap().split_whitespace().map(Into::into);
    match super::main_with_args(args) {
        Ok(()) => std::process::exit(0),
        Err(error) => {
            eprintln!("{error}");
            std::process::exit(1);
        }
    }
}

fn cli(args: &str, invalid_config: bool) -> std::process::Output {
    let mut command = Command::new(env::current_exe().unwrap());
    command
        .args([
            "--exact",
            "version_tests::cli_child",
            "--ignored",
            "--nocapture",
        ])
        .env_clear()
        .env("RACER_VERSION_TEST_ARGS", args)
        // Runtime environment must not override the compiled identity.
        .env("VERSION", "runtime-version")
        .env("GIT_COMMIT", "runtime-commit")
        .env("BUILD_TIME", "runtime-time");
    if invalid_config {
        command
            .env("RACER_STARTUP_SECONDS", "invalid")
            .env("RACER_BUFFERS_PER_NODE", "invalid")
            .env("RACER_SLAB_SIZE", "invalid")
            .env("RACER_SLAB_PATH", "/nonexistent/racer-version/cache.slab");
    }
    command.output().unwrap()
}

#[test]
fn version_before_runtime_requirements() {
    let expected = format!(
        "{} (commit: {}, built: {})",
        env!("RACER_BUILD_VERSION"),
        env!("RACER_BUILD_GIT_COMMIT"),
        env!("RACER_BUILD_BUILD_TIME")
    );
    for invalid_config in [false, true] {
        for args in ["version", "--version", "-version"] {
            let output = cli(args, invalid_config);
            assert!(output.status.success(), "{args}: {output:?}");
            assert!(output.stderr.is_empty(), "{args}: {output:?}");
            let stdout = String::from_utf8(output.stdout).unwrap();
            // The libtest child prints its banner before entering the real CLI.
            assert!(stdout.contains("running 1 test"), "{stdout}");
            assert!(stdout.ends_with(&format!("{expected}\n")), "{stdout}");
            assert_eq!(stdout.lines().filter(|line| *line == expected).count(), 1);
        }
    }
}

#[test]
fn invalid_version_arguments_fail_before_runtime() {
    for args in [
        "--unknown",
        "--version extra",
        "version extra",
        "--version=invalid",
    ] {
        let output = cli(args, true);
        assert!(!output.status.success(), "{args}: {output:?}");
        let stderr = String::from_utf8(output.stderr).unwrap();
        assert!(stderr.contains("expected no arguments"), "{args}: {stderr}");
        assert!(!String::from_utf8_lossy(&output.stdout).contains("(commit:"));
    }
}

#[test]
fn no_arguments_still_validate_runtime_configuration() {
    let output = cli("", true);
    assert!(!output.status.success(), "{output:?}");
    let stderr = String::from_utf8(output.stderr).unwrap();
    assert!(stderr.contains("RACER_"), "{stderr}");
    assert!(!stderr.contains("expected no arguments"), "{stderr}");
    assert!(!String::from_utf8_lossy(&output.stdout).contains("(commit:"));
}

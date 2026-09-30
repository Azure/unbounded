//! Startup configuration is exercised in subprocesses, before allocator setup.

fn launch(address: Option<&str>, malloc_conf: &str) -> String {
    let mut command = std::process::Command::new("timeout");
    command
        .args(["--signal=TERM", "--kill-after=10s", "15s"])
        .arg(env!("CARGO_BIN_EXE_racer-dataplane"))
        .env_clear()
        .env("_RJEM_MALLOC_CONF", malloc_conf);
    if let Some(address) = address {
        command.env("RACER_HEAP_PROFILE_ADDR", address);
    }
    let output = command.output().unwrap();
    assert_eq!(output.status.code(), Some(1), "startup must fail, not hang");
    String::from_utf8(output.stderr).unwrap()
}

#[test]
fn unset_address_preserves_application_configuration_failure() {
    assert_eq!(
        launch(None, "prof:false"),
        "racer-dataplane: InvalidConfiguration\n"
    );
}

#[cfg(not(feature = "heap-profiling"))]
#[test]
fn default_build_rejects_configured_profiling() {
    for address in ["127.0.0.1:6060", "", "untrusted-secret"] {
        let stderr = launch(Some(address), "prof:false");
        assert!(stderr.contains("requires a build with the heap-profiling Cargo feature"));
        assert!(!stderr.contains("untrusted-secret"));
    }
}

#[cfg(feature = "heap-profiling")]
#[test]
fn rejects_malformed_address_without_echoing_value() {
    for address in [
        "",
        "localhost:6060",
        "127.0.0.1:0",
        "127.0.0.1:65536",
        "untrusted-secret\n",
    ] {
        let stderr = launch(Some(address), "prof:false");
        assert!(stderr.contains("must be a numeric IP:port"));
        assert!(!stderr.contains("untrusted-secret"));
    }
}

#[cfg(feature = "heap-profiling")]
#[test]
fn configured_address_requires_enabled_and_active_profiler() {
    for config in ["prof:false", "prof:true,prof_active:false"] {
        assert!(launch(Some("127.0.0.1:6060"), config).contains("heap profiler is disabled"));
    }
}

#[cfg(feature = "heap-profiling")]
#[test]
fn configured_address_bind_failure_fails_startup() {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap().to_string();
    assert!(
        launch(
            Some(&address),
            "prof:true,prof_active:true,lg_prof_sample:19"
        )
        .contains("cannot bind RACER_HEAP_PROFILE_ADDR")
    );
}

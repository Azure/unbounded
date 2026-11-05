use super::*;
use crate::remote::tests::{MapResolver, serve, test_identity};
use crate::test_support::enrollment::{Directory, drive, reactor, scope};
use std::sync::Mutex as StdMutex;
use std::sync::atomic::Ordering;
use std::time::Instant;

fn imds_endpoints(port: u16) -> AuthEndpoints {
    AuthEndpoints {
        imds: RemoteEndpoint::http("127.0.0.1", port).unwrap(),
        authority: DEFAULT_AUTHORITY_HOST.into(),
        federated_token_file: None,
        tls: None,
    }
}

fn token_json(token: &str, expires: &str) -> String {
    format!("{{\"access_token\":\"{token}\",{expires},\"token_type\":\"Bearer\"}}")
}

fn ok(body: &str, chunked: bool) -> Vec<u8> {
    if chunked {
        format!(
            "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n{:x}\r\n{body}\r\n0\r\n\r\n",
            body.len()
        )
        .into_bytes()
    } else {
        format!(
            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n{body}",
            body.len()
        )
        .into_bytes()
    }
}

fn status(code: u16) -> Vec<u8> {
    format!("HTTP/1.1 {code} X\r\nContent-Length: 2\r\n\r\n{{}}").into_bytes()
}

fn source(cache: AzureTokenCache, resolver: MapResolver) -> Option<AzureTokenSource> {
    let reactor = reactor()?;
    Some(AzureTokenSource::with_resolver(
        Arc::new(cache),
        reactor,
        Rc::new(resolver),
    ))
}

fn bearer(source: &AzureTokenSource) -> Result<Arc<str>, AuthError> {
    let scope = scope();
    drive(
        &source.reactor,
        Box::pin(async { Ok(source.bearer(&scope).await) }),
    )
    .unwrap()
}

#[test]
fn challenge_parsing() {
    let parsed = parse_bearer_challenge(
        b"Bearer authorization_uri=https://login.microsoftonline.com/72f988bf-86f1-41af-91ab-2d7cd011db47/oauth2/authorize resource_id=https://storage.azure.com",
    )
    .unwrap();
    assert_eq!(parsed.tenant_id, "72f988bf-86f1-41af-91ab-2d7cd011db47");
    assert_eq!(
        parsed.resource_id.as_deref(),
        Some("https://storage.azure.com")
    );
    let quoted = parse_bearer_challenge(
        b"bearer  authorization_uri=\"https://login.example/tenant-1/oauth2/authorize\", error=\"invalid_token\"",
    )
    .unwrap();
    assert_eq!(quoted.tenant_id, "tenant-1");
    assert_eq!(quoted.resource_id, None);
    for bad in [
        &b"Basic realm=x"[..],
        b"Bearer resource_id=https://storage.azure.com",
        b"Bearer authorization_uri=https://login.example/",
        b"Bearer authorization_uri=https://login.example/a%20b/x",
        b"Bearer authorization_uri=\"unterminated",
        b"Bearer",
        b"\xff",
    ] {
        assert_eq!(parse_bearer_challenge(bad), None, "{bad:?}");
    }
}

#[test]
fn token_parsing_and_refresh_schedule() {
    let now = SystemTime::now();
    let epoch = now
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let on = parse_token(
        token_json(
            "t1",
            &format!("\"expires_on\":\"{}\",\"expires_in\":\"1\"", epoch + 3600),
        )
        .as_bytes(),
        now,
        now,
    )
    .unwrap();
    assert_eq!(&*on.token, "t1");
    assert_eq!(
        on.expires,
        SystemTime::UNIX_EPOCH + Duration::from_secs(epoch + 3600)
    );
    let lead = on.expires.duration_since(on.refresh).unwrap();
    assert!(lead.as_secs() == REFRESH_LEAD.as_secs(), "{lead:?}");
    let within = parse_token(token_json("t2", "\"expires_in\":60").as_bytes(), now, now).unwrap();
    let lead = within.expires.duration_since(within.refresh).unwrap();
    assert!(
        (29..=31).contains(&lead.as_secs()),
        "half-life lead {lead:?}"
    );
    for bad in [
        "{}".to_owned(),
        token_json("t", "\"other\":1"),
        token_json("", "\"expires_in\":60"),
        token_json("t", "\"expires_on\":1"),
        token_json("t", "\"expires_in\":\"soon\""),
        "not json".to_owned(),
    ] {
        assert!(parse_token(bad.as_bytes(), now, now).is_err(), "{bad}");
    }
    assert_eq!(
        refresh_time(now, Duration::from_secs(3600)),
        now + Duration::from_secs(3300)
    );
    assert_eq!(
        refresh_time(now, Duration::from_secs(10)),
        now + Duration::from_secs(5)
    );
}

#[test]
fn form_encoding_and_configuration_validation() {
    assert_eq!(
        form_encode("https://storage.azure.com/.default"),
        "https%3A%2F%2Fstorage.azure.com%2F.default"
    );
    assert_eq!(form_encode("a b&c=d~"), "a%20b%26c%3Dd~");
    assert!(
        AzureTokenCache::new(
            AzureCredential::ManagedIdentity {
                client_id: Some("x&resource=evil".into())
            },
            imds_endpoints(80)
        )
        .is_err()
    );
    // Workload identity requires a federated token file.
    assert_eq!(
        AzureTokenCache::new(
            AzureCredential::WorkloadIdentity {
                client_id: "c".into(),
                tenant_id: None
            },
            imds_endpoints(80)
        )
        .err(),
        Some(AuthError::Configuration)
    );
    for bad in [
        "ftp://x/",
        "https://",
        "https://u@h/",
        "https://h:x/",
        "https://h/?q",
    ] {
        assert!(parse_authority(bad, None).is_err(), "{bad}");
    }
    // An https authority needs the node's trust roots.
    assert_eq!(
        parse_authority("https://login.example/", None).err(),
        Some(AuthError::Configuration)
    );
    let (endpoint, prefix) = parse_authority("http://login.example:8080/base", None).unwrap();
    assert_eq!(endpoint.authority(), "login.example:8080");
    assert_eq!(prefix, "/base/");
}

#[test]
fn managed_identity_caches_refreshes_and_invalidates() {
    let requests = Arc::new(StdMutex::new(Vec::<String>::new()));
    let seen = requests.clone();
    let (address, accepted, server) = serve(None, 4, move |head, _| {
        let mut seen = seen.lock().unwrap();
        seen.push(head.to_owned());
        let index = seen.len();
        ok(
            &token_json(&format!("token-{index}"), "\"expires_in\":\"3600\""),
            index.is_multiple_of(2),
        )
    });
    let cache = AzureTokenCache::new(
        AzureCredential::ManagedIdentity {
            client_id: Some("1a2b-3c".into()),
        },
        imds_endpoints(address.port()),
    )
    .unwrap();
    let Some(source) = source(cache, MapResolver::new(Vec::new())) else {
        return;
    };
    assert_eq!(&*bearer(&source).unwrap(), "token-1");
    assert_eq!(&*bearer(&source).unwrap(), "token-1");
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
    // Reaching the refresh time fetches again; no sleeping on wall time.
    if let Some(token) = source.cache.token.lock().unwrap().as_mut() {
        token.refresh = SystemTime::now() - Duration::from_secs(1);
    }
    assert_eq!(&*bearer(&source).unwrap(), "token-2");
    source.invalidate();
    assert_eq!(&*bearer(&source).unwrap(), "token-3");
    // A second worker shares the node-wide cache.
    let other = AzureTokenSource::with_resolver(
        source.cache().clone(),
        source.reactor.clone(),
        Rc::new(MapResolver::new(Vec::new())),
    );
    assert_eq!(&*bearer(&other).unwrap(), "token-3");
    let requests = requests.lock().unwrap();
    assert_eq!(requests.len(), 3);
    for head in requests.iter() {
        assert!(head.starts_with(
            "GET /metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fstorage.azure.com%2F&client_id=1a2b-3c HTTP/1.1\r\n"
        ));
        assert!(head.contains("\r\nMetadata: true\r\n"));
        assert!(head.contains("\r\nHost: 127.0.0.1:"));
    }
    drop(requests);
    drop(source);
    drop(other);
    // Unblock the server's remaining accept.
    let _ = std::net::TcpStream::connect(address);
    server.join().unwrap();
}

#[test]
fn managed_identity_failures_and_stale_fallback() {
    let responses = Arc::new(StdMutex::new(vec![status(500), status(400), status(404)]));
    let queue = responses.clone();
    let (address, _, server) = serve(None, 3, move |_, _| queue.lock().unwrap().remove(0));
    let cache = AzureTokenCache::new(
        AzureCredential::ManagedIdentity { client_id: None },
        imds_endpoints(address.port()),
    )
    .unwrap();
    let Some(source) = source(cache, MapResolver::new(Vec::new())) else {
        return;
    };
    // A token due for refresh but unexpired survives a failed refresh.
    source.cache.store(CachedToken {
        token: Arc::from("stale"),
        expires: SystemTime::now() + Duration::from_secs(120),
        refresh: SystemTime::now() - Duration::from_secs(1),
    });
    assert_eq!(&*bearer(&source).unwrap(), "stale");
    source.invalidate();
    assert_eq!(bearer(&source).err(), Some(AuthError::Rejected(400)));
    assert_eq!(bearer(&source).err(), Some(AuthError::Unavailable));
    server.join().unwrap();
}

#[test]
fn workload_identity_exchanges_federated_token_with_discovered_tenant() {
    let identity = test_identity(&["login.example"]);
    let directory = Directory::new();
    let token_file = directory.0.join("token");
    std::fs::write(&token_file, "assertion-1\n").unwrap();
    let bodies = Arc::new(StdMutex::new(Vec::<(String, String)>::new()));
    let seen = bodies.clone();
    let (address, _, server) = serve(Some(identity.server.clone()), 2, move |head, body| {
        let mut seen = seen.lock().unwrap();
        seen.push((head.to_owned(), String::from_utf8(body.to_vec()).unwrap()));
        ok(
            &token_json(&format!("wi-{}", seen.len()), "\"expires_in\":3599"),
            false,
        )
    });
    let cache = AzureTokenCache::new(
        AzureCredential::WorkloadIdentity {
            client_id: "app-id".into(),
            tenant_id: None,
        },
        AuthEndpoints {
            imds: RemoteEndpoint::http(IMDS_ADDRESS, 80).unwrap(),
            authority: format!("https://login.example:{}/", address.port()),
            federated_token_file: Some(token_file.clone()),
            tls: Some(http1::tls::client_config(identity.roots.clone())),
        },
    )
    .unwrap();
    let Some(source) = source(cache, MapResolver::new(vec![address])) else {
        return;
    };
    assert_eq!(bearer(&source).err(), Some(AuthError::TenantUnknown));
    assert!(!source.cache().learn_tenant(b"Basic realm=x"));
    assert!(source.cache().learn_tenant(
        b"Bearer authorization_uri=https://login.microsoftonline.com/tenant-1/oauth2/authorize resource_id=https://storage.azure.com"
    ));
    assert_eq!(source.cache().tenant().as_deref(), Some("tenant-1"));
    assert_eq!(&*bearer(&source).unwrap(), "wi-1");
    assert_eq!(&*bearer(&source).unwrap(), "wi-1");
    std::fs::write(&token_file, "assertion-2").unwrap();
    source.invalidate();
    assert_eq!(&*bearer(&source).unwrap(), "wi-2");
    server.join().unwrap();
    let bodies = bodies.lock().unwrap();
    assert_eq!(bodies.len(), 2);
    for (index, (head, body)) in bodies.iter().enumerate() {
        assert!(head.starts_with("POST /tenant-1/oauth2/v2.0/token HTTP/1.1\r\n"));
        assert!(head.contains("\r\nContent-Type: application/x-www-form-urlencoded\r\n"));
        assert!(head.contains(&format!("\r\nHost: login.example:{}\r\n", address.port())));
        assert_eq!(
            body,
            &format!(
                "client_assertion=assertion-{}&client_assertion_type=urn%3Aietf%3Aparams%3Aoauth%3Aclient-assertion-type%3Ajwt-bearer&client_id=app-id&grant_type=client_credentials&scope=https%3A%2F%2Fstorage.azure.com%2F.default",
                index + 1
            )
        );
    }
}

fn stalled_source() -> Option<AzureTokenSource> {
    let cache = AzureTokenCache::new(
        AzureCredential::ManagedIdentity { client_id: None },
        // Port 9 is never contacted: every case below waits on the lock or
        // hits the cache.
        imds_endpoints(9),
    )
    .unwrap();
    source(cache, MapResolver::new(Vec::new()))
}

fn bearer_in(source: &AzureTokenSource, scope: &RequestScope) -> Result<Arc<str>, AuthError> {
    drive(
        &source.reactor,
        Box::pin(async { Ok(source.bearer(scope).await) }),
    )
    .unwrap()
}

#[test]
fn refresh_lock_wait_observes_waiter_deadline() {
    let Some(source) = stalled_source() else {
        return;
    };
    // A stalled fetch leads the node-wide refresh.
    let fetch = source.cache.begin().ok().unwrap();
    let started = Instant::now();
    let short = RequestScope::host(
        crate::model::RequestId([8; 16]),
        started + Duration::from_millis(100),
    )
    .unwrap();
    assert_eq!(
        bearer_in(&source, &short).err(),
        Some(AuthError::DeadlineExceeded)
    );
    assert!(started.elapsed() < Duration::from_secs(5));
    // The stalled fetch completes and fills the cache for later requests.
    source.cache.store(CachedToken {
        token: "late".into(),
        refresh: SystemTime::now() + Duration::from_secs(600),
        expires: SystemTime::now() + Duration::from_secs(3600),
    });
    fetch.finish(None);
    assert_eq!(&*bearer(&source).unwrap(), "late");
}

#[test]
fn refresh_lock_wait_observes_waiter_cancellation() {
    let Some(source) = stalled_source() else {
        return;
    };
    let fetch = source.cache.begin().ok().unwrap();
    let waiter = scope();
    let canceller = waiter.cancellation.clone();
    let started = Instant::now();
    let thread = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(50));
        canceller.cancel().unwrap();
    });
    assert_eq!(
        bearer_in(&source, &waiter).err(),
        Some(AuthError::Cancelled)
    );
    assert!(started.elapsed() < Duration::from_secs(5));
    thread.join().unwrap();
    drop(fetch);
    // Leadership is free again for the next refresh.
    assert!(source.cache.begin().is_ok());
}

/// Requests on many workers that find the token due at once share one
/// fetch: one worker leads it and the others wait for its token.
#[test]
fn concurrent_workers_share_one_refresh() {
    const WORKERS: usize = 8;
    let (address, accepted, server) = serve(None, 1, move |_, _| {
        // Hold the fetch open so every worker finds it running.
        std::thread::sleep(Duration::from_millis(300));
        ok(&token_json("shared", "\"expires_in\":\"3600\""), false)
    });
    let cache = Arc::new(
        AzureTokenCache::new(
            AzureCredential::ManagedIdentity { client_id: None },
            imds_endpoints(address.port()),
        )
        .unwrap(),
    );
    // A token at its refresh time is due on every worker.
    cache.store(CachedToken {
        token: Arc::from("old"),
        expires: SystemTime::now() + Duration::from_secs(30),
        refresh: SystemTime::now() - Duration::from_secs(1),
    });
    let barrier = Arc::new(std::sync::Barrier::new(WORKERS));
    let workers: Vec<_> = (0..WORKERS)
        .map(|_| {
            let cache = cache.clone();
            let barrier = barrier.clone();
            std::thread::spawn(move || {
                let reactor = reactor();
                barrier.wait();
                let reactor = reactor?;
                let source = AzureTokenSource::with_resolver(
                    cache,
                    reactor,
                    Rc::new(MapResolver::new(Vec::new())),
                );
                Some(bearer(&source).map(|token| token.to_string()))
            })
        })
        .collect();
    let results: Vec<_> = workers.into_iter().map(|w| w.join().unwrap()).collect();
    if results.iter().any(Option::is_none) {
        let _ = std::net::TcpStream::connect(address);
        server.join().unwrap();
        return;
    }
    for result in results {
        assert_eq!(result.unwrap().unwrap(), "shared");
    }
    server.join().unwrap();
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
}

#[test]
fn federated_assertions_are_trimmed_and_bounded() {
    assert_eq!(&*parse_assertion(b" jwt\n").unwrap(), "jwt");
    for bad in [&b""[..], b" \n", b"\xff"] {
        assert_eq!(
            parse_assertion(bad).err(),
            Some(AuthError::Configuration),
            "{bad:?}"
        );
    }
    let full = vec![b'a'; MAX_ASSERTION as usize];
    assert_eq!(parse_assertion(&full).unwrap().len(), full.len());
    let over = vec![b'a'; MAX_ASSERTION as usize + 1];
    assert_eq!(parse_assertion(&over).err(), Some(AuthError::Configuration));
}

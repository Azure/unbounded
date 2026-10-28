use super::*;

const IMAGE: &str = "example.com/racer-object:v1";
const CACHE: &str = "test-cache";

/// Opted-in pod naming CACHE. A `null` annotation value removes the key.
fn pod(anno: Value) -> Value {
    let mut a = json!({CACHE_ANNOTATION: CACHE});
    for (k, v) in anno.as_object().cloned().unwrap_or_default() {
        if v.is_null() {
            a.as_object_mut().unwrap().remove(&k);
        } else {
            a[k] = v;
        }
    }
    json!({
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": "p", "namespace": "ns", "labels": {ENABLED_LABEL: ENABLED_VALUE}, "annotations": a},
        "spec": {"containers": [{"name": "app", "image": "app"}]},
    })
}

fn review_with(request: Value) -> Vec<u8> {
    serde_json::to_vec(&json!({"apiVersion": API_VERSION, "kind": REVIEW_KIND, "request": request}))
        .unwrap()
}

fn review(op: &str, pod: &Value) -> Vec<u8> {
    review_with(json!({
        "uid": "uid-1",
        "kind": {"group": "", "version": "v1", "kind": "Pod"},
        "resource": {"group": "", "version": "v1", "resource": "pods"},
        "operation": op,
        "object": pod,
    }))
}

fn response(out: &[u8]) -> Value {
    let v: Value = serde_json::from_slice(out).unwrap();
    assert_eq!(v["apiVersion"], API_VERSION);
    assert_eq!(v["kind"], REVIEW_KIND);
    v["response"].clone()
}

/// Minimal RFC 6902 applier for the add, replace, and remove operations.
fn apply(doc: &Value, ops: &[Value]) -> Value {
    let mut doc = doc.clone();
    for op in ops {
        let kind = op["op"].as_str().unwrap();
        let path = op["path"].as_str().unwrap();
        assert!(path.starts_with('/'), "{path}");
        let toks: Vec<String> = path[1..]
            .split('/')
            .map(|t| t.replace("~1", "/").replace("~0", "~"))
            .collect();
        let (last, parents) = toks.split_last().unwrap();
        let mut cur = &mut doc;
        for t in parents {
            cur = match cur {
                Value::Object(m) => m.get_mut(t).expect("parent exists"),
                Value::Array(a) => &mut a[t.parse::<usize>().unwrap()],
                _ => panic!("bad parent at {path}"),
            };
        }
        match (kind, cur) {
            ("add", Value::Object(m)) => {
                m.insert(last.clone(), op["value"].clone());
            }
            ("add", Value::Array(a)) if last == "-" => a.push(op["value"].clone()),
            ("add", Value::Array(a)) => {
                let i: usize = last.parse().unwrap();
                assert!(i <= a.len());
                a.insert(i, op["value"].clone());
            }
            ("replace", Value::Object(m)) => {
                *m.get_mut(last).expect("replace target exists") = op["value"].clone();
            }
            ("replace", Value::Array(a)) => a[last.parse::<usize>().unwrap()] = op["value"].clone(),
            ("remove", Value::Object(m)) => {
                m.remove(last).expect("remove target exists");
            }
            _ => panic!("unsupported {kind} at {path}"),
        }
    }
    doc
}

/// Runs the full review and returns the patched pod, or the response on denial.
fn run(pod: &Value) -> Result<Value, Value> {
    let resp = response(&admit(&review("CREATE", pod), IMAGE).unwrap());
    assert_eq!(resp["uid"], "uid-1");
    if resp["allowed"] != true {
        assert!(resp.get("patch").is_none());
        return Err(resp);
    }
    assert_eq!(resp["status"]["code"], 200);
    let Some(patch) = resp.get("patch") else {
        assert!(resp.get("patchType").is_none());
        return Ok(pod.clone());
    };
    assert_eq!(resp["patchType"], "JSONPatch");
    let raw = base64::engine::general_purpose::STANDARD
        .decode(patch.as_str().unwrap())
        .unwrap();
    let ops: Vec<Value> = serde_json::from_slice(&raw).unwrap();
    assert!(!ops.is_empty());
    Ok(apply(pod, &ops))
}

fn denied(pod: &Value) -> String {
    let resp = run(pod).expect_err("expected denial");
    assert_eq!(resp["status"]["code"], 403);
    assert_eq!(resp["status"]["reason"], "Forbidden");
    resp["status"]["message"].as_str().unwrap().to_owned()
}

fn assert_denied(pod: &Value, want: &str) {
    let msg = denied(pod);
    assert!(msg.contains(want), "message {msg:?} lacks {want:?}");
}

fn excl() -> Value {
    exclusion()
}

#[test]
fn success_cases() {
    let cases = [
        (json!({}), vec!["sidecar", "--cache=test-cache"], CACHE),
        (
            json!({CACHE_ANNOTATION: "shared"}),
            vec!["sidecar", "--cache=shared"],
            "shared",
        ),
        (
            json!({CACHE_ANNOTATION: "my.cache", API_ANNOTATION: "s3", LISTEN_ANNOTATION: "127.0.0.1:9000"}),
            vec![
                "sidecar",
                "--cache=my.cache",
                "--api=s3",
                "--listen=127.0.0.1:9000",
            ],
            "my.cache",
        ),
        (
            json!({API_ANNOTATION: "azure", LISTEN_ANNOTATION: "[::1]:8081"}),
            vec![
                "sidecar",
                "--cache=test-cache",
                "--api=azure",
                "--listen=[::1]:8081",
            ],
            CACHE,
        ),
    ];
    for (anno, args, cache) in cases {
        let out = run(&pod(anno)).unwrap();
        let init = out["spec"]["initContainers"].as_array().unwrap();
        assert_eq!(init.len(), 1);
        let c = &init[0];
        let dir = format!("/run/racer/{cache}/client");
        assert_eq!(
            c,
            &json!({
                "name": CONTAINER_NAME,
                "image": IMAGE,
                "args": args,
                "resources": {},
                "volumeMounts": [{"name": VOLUME_NAME, "mountPath": dir, "readOnly": true}],
                "restartPolicy": "Always",
                "securityContext": {
                    "runAsUser": 65532,
                    "runAsGroup": 65532,
                    "runAsNonRoot": true,
                    "allowPrivilegeEscalation": false,
                    "readOnlyRootFilesystem": true,
                    "capabilities": {"drop": ["ALL"]},
                    "seccompProfile": {"type": "RuntimeDefault"},
                },
            })
        );
        assert_eq!(
            out["spec"]["volumes"],
            json!([{"name": VOLUME_NAME, "hostPath": {"path": dir, "type": "Directory"}}])
        );
        assert_eq!(out["spec"]["nodeSelector"], json!({OS_LABEL: "linux"}));
        assert_eq!(
            out["spec"]["affinity"],
            json!({"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution":
                {"nodeSelectorTerms": [{"matchExpressions": [excl()]}]}}})
        );
        assert_eq!(out["metadata"]["annotations"][INJECTED_ANNOTATION], IMAGE);
        assert_eq!(
            out["spec"]["containers"],
            pod(json!({}))["spec"]["containers"]
        );
    }
}

#[test]
fn denial_cases() {
    let removed_ns = "racer.unbounded-cloud.io/object-namespace";
    let removed_buckets = "racer.unbounded-cloud.io/object-buckets";
    for (anno, want) in [
        (json!({removed_ns: "store"}), removed_ns),
        (json!({removed_ns: ""}), removed_ns),
        (json!({removed_buckets: "one"}), removed_buckets),
        (json!({API_ANNOTATION: "gcs"}), API_ANNOTATION),
        (json!({API_ANNOTATION: ""}), API_ANNOTATION),
        (
            json!({LISTEN_ANNOTATION: "0.0.0.0:8080"}),
            LISTEN_ANNOTATION,
        ),
        (
            json!({LISTEN_ANNOTATION: "localhost:8080"}),
            LISTEN_ANNOTATION,
        ),
        (json!({LISTEN_ANNOTATION: "127.0.0.1"}), LISTEN_ANNOTATION),
        (
            json!({LISTEN_ANNOTATION: "127.0.0.1:70000"}),
            LISTEN_ANNOTATION,
        ),
        (json!({CACHE_ANNOTATION: "../x"}), CACHE_ANNOTATION),
        (json!({CACHE_ANNOTATION: "a/b"}), CACHE_ANNOTATION),
        (json!({CACHE_ANNOTATION: "Abc"}), CACHE_ANNOTATION),
        (json!({CACHE_ANNOTATION: ""}), CACHE_ANNOTATION),
        (json!({CACHE_ANNOTATION: "a$b"}), CACHE_ANNOTATION),
        (json!({CACHE_ANNOTATION: null}), CACHE_ANNOTATION),
        (
            json!({CACHE_ANNOTATION: null, API_ANNOTATION: "s3"}),
            CACHE_ANNOTATION,
        ),
    ] {
        assert_denied(&pod(anno), want);
    }

    let mut p = pod(json!({}));
    p["spec"]["volumes"] = json!([{"name": VOLUME_NAME}]);
    assert_denied(&p, VOLUME_NAME);

    let mut p = pod(json!({}));
    p["spec"]["nodeSelector"] = json!({OS_LABEL: "windows"});
    assert_denied(&p, OS_LABEL);
}

#[test]
fn pod_os() {
    for (os, selector, want) in [
        (None, None, None),
        (Some(json!({"name": "linux"})), None, None),
        (Some(json!({"name": "windows"})), None, Some("spec.os.name")),
        (Some(json!({"name": "plan9"})), None, Some("spec.os.name")),
        (Some(json!({})), None, Some("spec.os.name")),
        (
            Some(json!({"name": "linux"})),
            Some("windows"),
            Some(OS_LABEL),
        ),
        (
            Some(json!({"name": "windows"})),
            Some("linux"),
            Some("spec.os.name"),
        ),
    ] {
        let mut p = pod(json!({}));
        if let Some(os) = os {
            p["spec"]["os"] = os;
        }
        if let Some(s) = selector {
            p["spec"]["nodeSelector"] = json!({OS_LABEL: s});
        }
        match want {
            None => assert_eq!(
                run(&p).unwrap()["spec"]["initContainers"][0]["name"],
                CONTAINER_NAME
            ),
            Some(w) => assert_denied(&p, w),
        }
    }
}

#[test]
fn host_network() {
    let mut p = pod(json!({}));
    p["spec"]["hostNetwork"] = json!(false);
    assert_eq!(
        run(&p).unwrap()["spec"]["initContainers"][0]["name"],
        CONTAINER_NAME
    );

    p["spec"]["hostNetwork"] = json!(true);
    assert_denied(&p, "spec.hostNetwork");

    // Skipped pods are not checked.
    let mut q = p.clone();
    q["metadata"]["labels"] = json!({});
    assert_eq!(run(&q).unwrap(), q);
    let mut q = p.clone();
    q["spec"]["containers"]
        .as_array_mut()
        .unwrap()
        .push(json!({"name": CONTAINER_NAME}));
    assert_eq!(run(&q).unwrap(), q);
}

/// Kubelet expansion rules for container args, as in the Go test.
fn kubelet_expand(input: &str, env: &[(&str, &str)]) -> String {
    let b = input.as_bytes();
    let mut out = String::new();
    let mut i = 0;
    while i < b.len() {
        if b[i] != b'$' || i + 1 >= b.len() {
            out.push(b[i] as char);
            i += 1;
            continue;
        }
        match b[i + 1] {
            b'$' => {
                out.push('$');
                i += 2;
            }
            b'(' => match input[i + 2..].find(')') {
                None => {
                    out.push_str("$(");
                    i += 2;
                }
                Some(end) => {
                    let name = &input[i + 2..i + 2 + end];
                    match env.iter().find(|(k, _)| *k == name) {
                        Some((_, v)) => out.push_str(v),
                        None => out.push_str(&format!("$({name})")),
                    }
                    i += 3 + end;
                }
            },
            _ => {
                out.push('$');
                i += 1;
            }
        }
    }
    out
}

#[test]
fn kubelet_expand_rules() {
    let env = [("HOST", "10.0.0.1")];
    for (inp, want) in [
        ("a$(HOST)b", "a10.0.0.1b"),
        ("$(NOPE)", "$(NOPE)"),
        ("$$(HOST)", "$(HOST)"),
        ("$$$$", "$$"),
        ("a$b$", "a$b$"),
        ("$(open", "$(open"),
    ] {
        assert_eq!(kubelet_expand(inp, &env), want, "{inp}");
    }
}

#[test]
fn args_survive_kubelet_expansion() {
    let env = [("KUBERNETES_SERVICE_HOST", "10.0.0.1"), ("X", "y")];
    for anno in [
        json!({}),
        json!({CACHE_ANNOTATION: "a-b.c0", API_ANNOTATION: "azure", LISTEN_ANNOTATION: "[::1]:8081"}),
    ] {
        let out = run(&pod(anno)).unwrap();
        for arg in out["spec"]["initContainers"][0]["args"].as_array().unwrap() {
            let arg = arg.as_str().unwrap();
            assert_eq!(kubelet_expand(arg, &env), arg);
        }
    }
}

#[test]
fn resources() {
    for (anno, want) in [
        (
            json!({CPU_REQUEST_ANNOTATION: "100m"}),
            json!({"requests": {"cpu": "100m"}}),
        ),
        (
            json!({CPU_LIMIT_ANNOTATION: "1"}),
            json!({"limits": {"cpu": "1"}}),
        ),
        (
            json!({MEMORY_REQUEST_ANNOTATION: "64Mi"}),
            json!({"requests": {"memory": "64Mi"}}),
        ),
        (
            json!({MEMORY_LIMIT_ANNOTATION: " 1Gi "}),
            json!({"limits": {"memory": "1Gi"}}),
        ),
        (
            json!({CPU_REQUEST_ANNOTATION: "100m", CPU_LIMIT_ANNOTATION: "100m", MEMORY_REQUEST_ANNOTATION: "64Mi", MEMORY_LIMIT_ANNOTATION: "1Gi"}),
            json!({"requests": {"cpu": "100m", "memory": "64Mi"}, "limits": {"cpu": "100m", "memory": "1Gi"}}),
        ),
        (
            json!({CPU_REQUEST_ANNOTATION: "1000m", CPU_LIMIT_ANNOTATION: "1"}),
            json!({"requests": {"cpu": "1000m"}, "limits": {"cpu": "1"}}),
        ),
    ] {
        assert_eq!(
            run(&pod(anno)).unwrap()["spec"]["initContainers"][0]["resources"],
            want
        );
    }
    for (anno, want) in [
        (
            json!({CPU_REQUEST_ANNOTATION: "lots"}),
            CPU_REQUEST_ANNOTATION,
        ),
        (
            json!({MEMORY_LIMIT_ANNOTATION: "0"}),
            MEMORY_LIMIT_ANNOTATION,
        ),
        (json!({CPU_LIMIT_ANNOTATION: "-1"}), CPU_LIMIT_ANNOTATION),
        (
            json!({MEMORY_REQUEST_ANNOTATION: ""}),
            MEMORY_REQUEST_ANNOTATION,
        ),
        (
            json!({CPU_REQUEST_ANNOTATION: "2", CPU_LIMIT_ANNOTATION: "1"}),
            CPU_REQUEST_ANNOTATION,
        ),
        (
            json!({MEMORY_REQUEST_ANNOTATION: "2Gi", MEMORY_LIMIT_ANNOTATION: "1Gi"}),
            MEMORY_REQUEST_ANNOTATION,
        ),
        (
            json!({MEMORY_REQUEST_ANNOTATION: "1Gi", MEMORY_LIMIT_ANNOTATION: "1G"}),
            MEMORY_REQUEST_ANNOTATION,
        ),
    ] {
        assert_denied(&pod(anno), want);
    }
}

#[test]
fn placement_and_ordering() {
    let mut p = pod(json!({}));
    p["spec"]["initContainers"] = json!([{"name": "first"}, {"name": "second"}]);
    p["spec"]["nodeSelector"] = json!({"zone": "a", OS_LABEL: "linux"});
    let pref = json!([{"weight": 1, "preference": {"matchExpressions": [{"key": "x", "operator": "Exists"}]}}]);
    let anti = json!({"preferredDuringSchedulingIgnoredDuringExecution": [{"weight": 2, "podAffinityTerm": {"topologyKey": "t"}}]});
    p["spec"]["volumes"] = json!([{"name": "data", "emptyDir": {}}]);
    p["spec"]["affinity"] = json!({
        "nodeAffinity": {
            "requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [
                {"matchExpressions": [{"key": "a", "operator": "Exists"}]},
                {"matchFields": [{"key": "metadata.name", "operator": "In", "values": ["n"]}]},
                {"matchExpressions": [excl()]},
            ]},
            "preferredDuringSchedulingIgnoredDuringExecution": pref,
        },
        "podAntiAffinity": anti,
    });

    let out = run(&p).unwrap();
    let names: Vec<_> = out["spec"]["initContainers"]
        .as_array()
        .unwrap()
        .iter()
        .map(|c| c["name"].clone())
        .collect();
    assert_eq!(
        names,
        vec![json!(CONTAINER_NAME), json!("first"), json!("second")]
    );
    assert_eq!(
        out["spec"]["nodeSelector"],
        json!({"zone": "a", OS_LABEL: "linux"})
    );
    let vols: Vec<_> = out["spec"]["volumes"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v["name"].clone())
        .collect();
    assert_eq!(vols, vec![json!("data"), json!(VOLUME_NAME)]);
    let terms = out["spec"]["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        .as_array()
        .unwrap();
    assert_eq!(
        terms,
        &vec![
            json!({"matchExpressions": [{"key": "a", "operator": "Exists"}, excl()]}),
            json!({"matchFields": [{"key": "metadata.name", "operator": "In", "values": ["n"]}], "matchExpressions": [excl()]}),
            json!({"matchExpressions": [excl()]}),
        ]
    );
    assert_eq!(
        out["spec"]["affinity"]["nodeAffinity"]["preferredDuringSchedulingIgnoredDuringExecution"],
        pref
    );
    assert_eq!(out["spec"]["affinity"]["podAntiAffinity"], anti);
}

#[test]
fn exclusion_affinity() {
    let field = json!({"key": "metadata.name", "operator": "In", "values": ["n1"]});
    let user = |k: &str| json!({"key": k, "operator": "In", "values": ["a"]});
    let only = json!({"nodeSelectorTerms": [{"matchExpressions": [excl()]}]});
    let req = |sel: Value| json!({"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": sel}});
    for (input, want) in [
        (None, only.clone()),
        (Some(json!({"nodeAffinity": {}})), only.clone()),
        (Some(json!({"nodeAffinity": null})), only.clone()),
        (Some(req(json!({}))), json!({})),
        (
            Some(req(json!({"nodeSelectorTerms": []}))),
            json!({"nodeSelectorTerms": []}),
        ),
        (
            Some(req(json!({"nodeSelectorTerms": [{}]}))),
            json!({"nodeSelectorTerms": [{}]}),
        ),
        (
            Some(req(
                json!({"nodeSelectorTerms": [{"matchExpressions": [], "matchFields": null}]}),
            )),
            json!({"nodeSelectorTerms": [{"matchExpressions": [], "matchFields": null}]}),
        ),
        (
            Some(req(
                json!({"nodeSelectorTerms": [{}, {"matchExpressions": [user("zone")]}]}),
            )),
            json!({"nodeSelectorTerms": [{}, {"matchExpressions": [user("zone"), excl()]}]}),
        ),
        (
            Some(req(
                json!({"nodeSelectorTerms": [{"matchFields": [field]}]}),
            )),
            json!({"nodeSelectorTerms": [{"matchFields": [field], "matchExpressions": [excl()]}]}),
        ),
        (
            Some(req(json!({"nodeSelectorTerms": [
                {"matchExpressions": [user("zone")]},
                {"matchExpressions": [user("rack"), excl()]},
            ]}))),
            json!({"nodeSelectorTerms": [
                {"matchExpressions": [user("zone"), excl()]},
                {"matchExpressions": [user("rack"), excl()]},
            ]}),
        ),
        (
            // An exclusion with values is not the same requirement.
            Some(req(json!({"nodeSelectorTerms": [{"matchExpressions": [
                {"key": EXCLUSION_LABEL, "operator": "DoesNotExist", "values": ["x"]}
            ]}]}))),
            json!({"nodeSelectorTerms": [{"matchExpressions": [
                {"key": EXCLUSION_LABEL, "operator": "DoesNotExist", "values": ["x"]}, excl()
            ]}]}),
        ),
    ] {
        let mut p = pod(json!({}));
        if let Some(a) = input {
            p["spec"]["affinity"] = a;
        }
        let out = run(&p).unwrap();
        assert_eq!(
            out["spec"]["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"],
            want
        );
    }
}

#[test]
fn noop_cases() {
    let mods: Vec<fn(&mut Value)> = vec![
        |p| p["metadata"]["labels"] = Value::Null,
        |p| {
            p["metadata"].as_object_mut().unwrap().remove("labels");
        },
        |p| p["metadata"]["labels"][ENABLED_LABEL] = json!("disabled"),
        |p| p["spec"]["initContainers"] = json!([{"name": CONTAINER_NAME}]),
        |p| {
            p["spec"]["containers"]
                .as_array_mut()
                .unwrap()
                .push(json!({"name": CONTAINER_NAME}))
        },
        |p| {
            p["metadata"]["labels"] = Value::Null;
            p["metadata"]["annotations"] = Value::Null;
        },
        |p| {
            p["metadata"]["annotations"] = Value::Null;
            p["spec"]["initContainers"] = json!([{"name": CONTAINER_NAME}]);
        },
    ];
    for m in mods {
        let mut p = pod(json!({}));
        m(&mut p);
        assert_eq!(run(&p).unwrap(), p);
    }
}

#[test]
fn idempotent() {
    let p = pod(json!({API_ANNOTATION: "s3"}));
    let once = run(&p).unwrap();
    assert_eq!(run(&once).unwrap(), once);
}

#[test]
fn missing_containers_and_maps() {
    let p = json!({"metadata": {"labels": {ENABLED_LABEL: ENABLED_VALUE}, "annotations": {CACHE_ANNOTATION: CACHE}}, "spec": {}});
    let out = run(&p).unwrap();
    assert_eq!(out["spec"]["initContainers"][0]["name"], CONTAINER_NAME);
    assert_eq!(out["spec"]["volumes"][0]["name"], VOLUME_NAME);

    // Null collections are replaced, not appended to.
    let p = json!({"metadata": {"labels": {ENABLED_LABEL: ENABLED_VALUE}, "annotations": {CACHE_ANNOTATION: CACHE}},
        "spec": {"initContainers": null, "volumes": null, "nodeSelector": null, "affinity": null}});
    let out = run(&p).unwrap();
    assert_eq!(out["spec"]["volumes"].as_array().unwrap().len(), 1);
    assert_eq!(out["spec"]["nodeSelector"], json!({OS_LABEL: "linux"}));

    // Annotation key with "/" is escaped in the patch path.
    let mut p = pod(json!({}));
    p["metadata"]["annotations"]["other"] = json!("x");
    let out = run(&p).unwrap();
    assert_eq!(out["metadata"]["annotations"]["other"], "x");
    assert_eq!(out["metadata"]["annotations"][INJECTED_ANNOTATION], IMAGE);
}

#[test]
fn listen_rules() {
    for ok in [
        "127.0.0.1:1",
        "127.0.0.1:65535",
        "127.1.2.3:80",
        "[::1]:80",
        "[127.0.0.1]:80",
        "[::ffff:127.0.0.1]:80",
        "127.0.0.1:0080",
    ] {
        assert!(valid_listen(ok), "{ok}");
    }
    for bad in [
        "127.0.0.1:0",
        "127.0.0.1:",
        "127.0.0.1:65536",
        "127.0.0.1:+80",
        "::1:80",
        "[::1]80",
        "[::1%lo]:80",
        "[::1]:80:1",
        "10.0.0.1:80",
        "[::]:80",
        "127.0.0.01:80",
        ":80",
        "127.0.0.1:99999999999999999999",
        "[[::1]]:80",
    ] {
        assert!(!valid_listen(bad), "{bad}");
    }
}

#[test]
fn cache_rules() {
    assert!(valid_cache("a"));
    assert!(valid_cache("a-b.c0"));
    let long = "a".repeat(63);
    assert!(!valid_cache(&format!("{long}x")));
    assert!(!valid_cache("-a"));
    assert!(!valid_cache("a-"));
    assert!(!valid_cache("a..b"));
    // Fits the label limit but not the socket path limit.
    let path_long = format!("{long}.{}", "b".repeat(30));
    assert!(!valid_cache(&path_long));
}

#[test]
fn blank_image_denies_pod_create() {
    for image in ["", "  "] {
        let resp = response(&admit(&review("CREATE", &pod(json!({}))), image).unwrap());
        assert_eq!(resp["allowed"], false);
        assert_eq!(resp["status"]["code"], 403);
    }
}

#[test]
fn skipped_requests() {
    let p = pod(json!({}));
    for op in ["UPDATE", "DELETE", "CONNECT"] {
        let resp = response(&admit(&review(op, &p), IMAGE).unwrap());
        assert_eq!(
            resp,
            json!({"uid": "uid-1", "allowed": true, "status": {"code": 200}})
        );
    }
    for kind in [
        json!({"group": "apps", "version": "v1", "kind": "Deployment"}),
        json!({"group": "", "version": "v2", "kind": "Pod"}),
        json!({"group": "x", "version": "v1", "kind": "Pod"}),
    ] {
        let r = review_with(json!({"uid": "u", "kind": kind, "operation": "CREATE", "object": p}));
        assert_eq!(response(&admit(&r, IMAGE).unwrap())["allowed"], true);
        assert!(response(&admit(&r, IMAGE).unwrap()).get("patch").is_none());
    }
}

#[test]
fn dry_run_matches_normal() {
    let p = pod(json!({}));
    let mut r: Value = serde_json::from_slice(&review("CREATE", &p)).unwrap();
    let normal = admit(&serde_json::to_vec(&r).unwrap(), IMAGE).unwrap();
    r["request"]["dryRun"] = json!(true);
    assert_eq!(
        admit(&serde_json::to_vec(&r).unwrap(), IMAGE).unwrap(),
        normal
    );
}

#[test]
fn bad_pod_object_is_400() {
    let mut cases = vec![Value::Null, json!("x"), json!([])];
    let mut p = pod(json!({}));
    p["metadata"]["labels"] = json!([]);
    cases.push(p);
    let mut p = pod(json!({}));
    p["metadata"]["annotations"][CACHE_ANNOTATION] = json!(1);
    cases.push(p);
    let mut p = pod(json!({}));
    p["spec"]["hostNetwork"] = json!("true");
    cases.push(p);
    let mut p = pod(json!({}));
    p["spec"]["containers"] = json!({});
    cases.push(p);
    let mut p = pod(json!({}));
    p["spec"]["affinity"] = json!({"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [1]}}});
    cases.push(p);
    for p in cases {
        let resp = response(&admit(&review("CREATE", &p), IMAGE).unwrap());
        assert_eq!(resp["allowed"], false, "{p}");
        assert_eq!(resp["status"]["code"], 400, "{p}");
        assert_eq!(resp["uid"], "uid-1");
    }
    // Missing object.
    let r = review_with(
        json!({"uid": "u", "kind": {"version": "v1", "kind": "Pod"}, "operation": "CREATE"}),
    );
    assert_eq!(response(&admit(&r, IMAGE).unwrap())["status"]["code"], 400);
}

#[test]
fn invalid_reviews() {
    let good = review("CREATE", &pod(json!({})));
    assert!(admit(&good, IMAGE).is_ok());

    let cases: Vec<Vec<u8>> = vec![
        b"".to_vec(),
        b"{".to_vec(),
        b"[]".to_vec(),
        b"null".to_vec(),
        [good.as_slice(), b" {}"].concat(),
        br#"{"apiVersion":"admission.k8s.io/v1beta1","kind":"AdmissionReview","request":{"uid":"u"}}"#.to_vec(),
        br#"{"apiVersion":"admission.k8s.io/v1","kind":"Other","request":{"uid":"u"}}"#.to_vec(),
        br#"{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview"}"#.to_vec(),
        br#"{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{}}"#.to_vec(),
        br#"{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{"uid":""}}"#.to_vec(),
        br#"{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{"uid":7}}"#.to_vec(),
        br#"{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","kind":"AdmissionReview","request":{"uid":"u"}}"#
            .to_vec(),
        format!(r#"{{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{{"uid":"{}"}}}}"#, "u".repeat(UID_MAX + 1))
            .into_bytes(),
        [b"[".repeat(200), b"]".repeat(200)].concat(),
    ];
    for c in cases {
        assert!(
            admit(&c, IMAGE).is_err(),
            "{}",
            String::from_utf8_lossy(&c[..c.len().min(120)])
        );
    }
}

#[test]
fn duplicate_keys_anywhere_are_rejected() {
    // A duplicate label could make this webhook and the API server see
    // different values.
    let body = format!(
        r#"{{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{{"uid":"u","kind":{{"group":"","version":"v1","kind":"Pod"}},"operation":"CREATE","object":{{"metadata":{{"labels":{{"{l}":"enabled","{l}":"no"}}}}}}}}}}"#,
        l = ENABLED_LABEL
    );
    assert!(
        matches!(admit(body.as_bytes(), IMAGE), Err(Error::Json(m)) if m.contains("duplicate"))
    );
}

#[test]
fn size_limit() {
    let mut big = review("UPDATE", &pod(json!({})));
    assert!(admit(&big, IMAGE).is_ok());
    let pad = MAX_REQUEST_BYTES + 1 - big.len();
    big.extend(std::iter::repeat_n(b' ', pad));
    assert_eq!(
        admit(&big, IMAGE),
        Err(Error::TooLarge(MAX_REQUEST_BYTES + 1))
    );
    big.pop();
    assert!(admit(&big, IMAGE).is_ok());
}

#[test]
fn response_shape_and_error_display() {
    let out = admit(&review("CREATE", &pod(json!({}))), IMAGE).unwrap();
    let v: Value = serde_json::from_slice(&out).unwrap();
    let keys: Vec<_> = v["response"].as_object().unwrap().keys().cloned().collect();
    assert_eq!(keys, ["allowed", "patch", "patchType", "status", "uid"]);
    assert!(Error::TooLarge(1).to_string().contains("limit"));
    assert_eq!(PATH, "/mutate-racer-object-sidecar");
}

#[test]
fn patch_paths_escape_and_create_parents() {
    // No metadata.annotations and no spec: the patch must create both.
    let p = json!({"metadata": {"labels": {ENABLED_LABEL: ENABLED_VALUE, "a/b~c": "x"}}});
    let resp = response(&admit(&review("CREATE", &p), IMAGE).unwrap());
    assert_eq!(resp["status"]["code"], 403); // cache annotation is required
    let p = json!({
        "metadata": {"labels": {ENABLED_LABEL: ENABLED_VALUE}, "annotations": {CACHE_ANNOTATION: CACHE, "a/b~c": "x"}},
    });
    let out = run(&p).unwrap();
    assert_eq!(out["metadata"]["annotations"]["a/b~c"], "x");
    assert_eq!(out["spec"]["volumes"][0]["name"], VOLUME_NAME);
}

const CLIENT_DIR: &str = "/run/racer/test-cache/client";

/// Pod opted into the client socket only. A `null` annotation value removes the key.
fn client_pod(anno: Value) -> Value {
    let mut a = json!({CLIENT_CACHE_ANNOTATION: CACHE});
    for (k, v) in anno.as_object().cloned().unwrap_or_default() {
        if v.is_null() {
            a.as_object_mut().unwrap().remove(&k);
        } else {
            a[k] = v;
        }
    }
    json!({
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": "p", "namespace": "ns", "labels": {CLIENT_SOCKET_LABEL: ENABLED_VALUE}, "annotations": a},
        "spec": {
            "initContainers": [{"name": "setup", "image": "app"}],
            "containers": [{"name": "app", "image": "app"}, {"name": "helper", "image": "app"}],
        },
    })
}

fn socket_mounted(c: &Value, volume: &str) -> bool {
    let mounts = c["volumeMounts"].as_array().cloned().unwrap_or_default();
    let env = c["env"].as_array().cloned().unwrap_or_default();
    mounts.contains(&json!({"name": volume, "mountPath": CLIENT_DIR, "readOnly": true}))
        && env.contains(&json!({"name": SOCKET_ENV, "value": format!("{CLIENT_DIR}/socket")}))
}

#[test]
fn client_socket_all_containers() {
    let out = run(&client_pod(json!({}))).unwrap();
    for c in out["spec"]["containers"].as_array().unwrap() {
        assert!(socket_mounted(c, CLIENT_VOLUME_NAME), "{c}");
    }
    let init = out["spec"]["initContainers"].as_array().unwrap();
    assert_eq!(init.len(), 1, "no sidecar added: {init:?}");
    assert!(socket_mounted(&init[0], CLIENT_VOLUME_NAME), "{init:?}");
    assert_eq!(
        out["spec"]["volumes"],
        json!([{"name": CLIENT_VOLUME_NAME, "hostPath": {"path": CLIENT_DIR, "type": "Directory"}}])
    );
    assert_eq!(out["spec"]["nodeSelector"][OS_LABEL], "linux");
    assert_eq!(
        out["spec"]["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"],
        json!({"nodeSelectorTerms": [{"matchExpressions": [excl()]}]})
    );
    let a = &out["metadata"]["annotations"];
    assert_eq!(a[CLIENT_SOCKET_INJECTED_ANNOTATION], CACHE);
    assert!(a.get(INJECTED_ANNOTATION).is_none());

    // A second admission pass must not change anything.
    assert_eq!(run(&out).unwrap(), out);
}

#[test]
fn client_socket_selected_containers() {
    let out = run(&client_pod(
        json!({CLIENT_CONTAINERS_ANNOTATION: " helper "}),
    ))
    .unwrap();
    let c = out["spec"]["containers"].as_array().unwrap();
    assert!(!socket_mounted(&c[0], CLIENT_VOLUME_NAME));
    assert!(socket_mounted(&c[1], CLIENT_VOLUME_NAME));
}

#[test]
fn client_socket_selected_init_container() {
    let out = run(&client_pod(
        json!({CLIENT_CONTAINERS_ANNOTATION: "setup, helper"}),
    ))
    .unwrap();
    assert!(socket_mounted(
        &out["spec"]["initContainers"][0],
        CLIENT_VOLUME_NAME
    ));
    let c = out["spec"]["containers"].as_array().unwrap();
    assert!(!socket_mounted(&c[0], CLIENT_VOLUME_NAME));
    assert!(socket_mounted(&c[1], CLIENT_VOLUME_NAME));
}

#[test]
fn client_socket_native_sidecar_init_container() {
    let mut p = client_pod(json!({}));
    p["spec"]["initContainers"] = json!([
        {"name": "setup", "image": "app"},
        {"name": "agent", "image": "app", "restartPolicy": "Always"},
    ]);
    let out = run(&p).unwrap();
    for c in out["spec"]["initContainers"].as_array().unwrap() {
        assert!(socket_mounted(c, CLIENT_VOLUME_NAME), "{c}");
        assert_eq!(c["image"], "app");
    }
}

#[test]
fn client_socket_skips_object_sidecar() {
    let mut p = pod(json!({CLIENT_CACHE_ANNOTATION: CACHE}));
    p["metadata"]["labels"][CLIENT_SOCKET_LABEL] = json!(ENABLED_VALUE);
    let out = run(&p).unwrap();
    let sidecar = &out["spec"]["initContainers"][0];
    assert_eq!(sidecar["name"], CONTAINER_NAME);
    let env = sidecar["env"].as_array().cloned().unwrap_or_default();
    assert!(
        !env.iter().any(|e| e["name"] == SOCKET_ENV),
        "sidecar must not get {SOCKET_ENV}: {sidecar}"
    );
}

#[test]
fn client_socket_with_sidecar_shares_volume() {
    let mut p = pod(json!({CLIENT_CACHE_ANNOTATION: CACHE}));
    p["metadata"]["labels"][CLIENT_SOCKET_LABEL] = json!(ENABLED_VALUE);
    let out = run(&p).unwrap();
    let volumes = out["spec"]["volumes"].as_array().unwrap();
    assert_eq!(volumes.len(), 1, "{volumes:?}");
    assert_eq!(volumes[0]["name"], VOLUME_NAME);
    assert_eq!(out["spec"]["initContainers"][0]["name"], CONTAINER_NAME);
    assert!(socket_mounted(&out["spec"]["containers"][0], VOLUME_NAME));

    // Distinct caches get distinct volumes.
    let mut p = pod(json!({CLIENT_CACHE_ANNOTATION: "other"}));
    p["metadata"]["labels"][CLIENT_SOCKET_LABEL] = json!(ENABLED_VALUE);
    let out = run(&p).unwrap();
    let volumes = out["spec"]["volumes"].as_array().unwrap();
    assert_eq!(volumes.len(), 2, "{volumes:?}");
    assert_eq!(volumes[1]["name"], CLIENT_VOLUME_NAME);
    assert_eq!(volumes[1]["hostPath"]["path"], "/run/racer/other/client");
}

#[test]
fn client_socket_host_network_allowed() {
    let mut p = client_pod(json!({}));
    p["spec"]["hostNetwork"] = json!(true);
    assert!(run(&p).is_ok());
}

#[test]
fn client_socket_noop() {
    let mut unlabeled = client_pod(json!({}));
    unlabeled["metadata"]
        .as_object_mut()
        .unwrap()
        .retain(|k, _| k != "labels" && k != "annotations");
    let mut disabled = client_pod(json!({}));
    disabled["metadata"]["labels"][CLIENT_SOCKET_LABEL] = json!("disabled");
    let mut injected = client_pod(json!({}));
    injected["metadata"]["annotations"] = json!({CLIENT_SOCKET_INJECTED_ANNOTATION: CACHE});
    for p in [unlabeled, disabled, injected] {
        assert_eq!(run(&p).unwrap(), p);
    }
}

#[test]
fn client_socket_denials() {
    let with = |anno: Value, modify: &dyn Fn(&mut Value)| {
        let mut p = client_pod(anno);
        modify(&mut p);
        p
    };
    let none = |_: &mut Value| {};
    let cases = [
        (
            with(json!({CLIENT_CACHE_ANNOTATION: null}), &none),
            CLIENT_CACHE_ANNOTATION,
        ),
        (
            with(json!({CLIENT_CACHE_ANNOTATION: "../x"}), &none),
            CLIENT_CACHE_ANNOTATION,
        ),
        (
            with(json!({CLIENT_CONTAINERS_ANNOTATION: "nope"}), &none),
            CLIENT_CONTAINERS_ANNOTATION,
        ),
        (
            with(json!({CLIENT_CONTAINERS_ANNOTATION: "setup,setup"}), &none),
            CLIENT_CONTAINERS_ANNOTATION,
        ),
        (
            with(
                json!({CLIENT_CONTAINERS_ANNOTATION: CONTAINER_NAME}),
                &|p| p["spec"]["initContainers"] = json!([{"name": CONTAINER_NAME, "image": "x"}]),
            ),
            CLIENT_CONTAINERS_ANNOTATION,
        ),
        (
            with(json!({}), &|p| {
                p["spec"]["initContainers"][0]["env"] = json!([{"name": SOCKET_ENV, "value": "x"}])
            }),
            SOCKET_ENV,
        ),
        (
            with(json!({CLIENT_CONTAINERS_ANNOTATION: "app,app"}), &none),
            CLIENT_CONTAINERS_ANNOTATION,
        ),
        (
            with(json!({CLIENT_CONTAINERS_ANNOTATION: ""}), &none),
            CLIENT_CONTAINERS_ANNOTATION,
        ),
        (
            with(json!({}), &|p| {
                p["spec"]["volumes"] = json!([{"name": CLIENT_VOLUME_NAME}])
            }),
            CLIENT_VOLUME_NAME,
        ),
        (
            with(json!({}), &|p| {
                p["spec"]["containers"][0]["volumeMounts"] =
                    json!([{"name": "x", "mountPath": CLIENT_DIR}])
            }),
            "already mounts",
        ),
        (
            with(json!({}), &|p| {
                p["spec"]["containers"][1]["env"] = json!([{"name": SOCKET_ENV, "value": "x"}])
            }),
            SOCKET_ENV,
        ),
        (
            with(json!({}), &|p| p["spec"]["os"] = json!({"name": "windows"})),
            "spec.os.name",
        ),
        (
            with(json!({}), &|p| {
                p["spec"]["nodeSelector"] = json!({OS_LABEL: "windows"})
            }),
            OS_LABEL,
        ),
    ];
    for (p, want) in cases {
        assert_denied(&p, want);
    }
}

#[test]
fn client_socket_bad_container_fields_are_400() {
    for field in ["volumeMounts", "env"] {
        let mut p = client_pod(json!({}));
        p["spec"]["containers"][0][field] = json!("x");
        let resp = run(&p).expect_err("expected rejection");
        assert_eq!(resp["status"]["code"], 400, "{field}: {resp}");
    }
}

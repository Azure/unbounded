//! Pod mutation: the racer-object sidecar and the client socket mount.

use serde_json::{Map, Value, json};

use super::*;

/// Ports Go `Mutate`. A failure denies the pod with no change. Returns JSON
/// Patch operations that turn `pod` into the mutated pod.
pub(crate) fn mutate(pod: &Value, image: &str) -> Result<Vec<Value>, Reject> {
    if image.trim().is_empty() {
        return Err(deny("racer-object sidecar image must not be empty"));
    }
    let mut out = pod.clone();
    mutate_sidecar(&mut out, image)?;
    mutate_client_socket(&mut out)?;
    let mut ops = Vec::new();
    diff(pod, &out, &mut String::new(), &mut ops);
    Ok(ops)
}

/// Returns the named child object of `v`, creating it when absent or null.
/// Callers check types first with [`Pod::read`].
fn child<'a>(v: &'a mut Value, key: &str) -> &'a mut Map<String, Value> {
    let m = v.as_object_mut().expect("checked by Pod::read");
    child_in(m, key)
}

fn child_in<'a>(m: &'a mut Map<String, Value>, key: &str) -> &'a mut Map<String, Value> {
    let e = m.entry(key).or_insert(Value::Null);
    if e.is_null() {
        *e = Value::Object(Map::new());
    }
    e.as_object_mut().expect("checked by Pod::read")
}

/// Returns the named array in `m`, creating it when absent or null.
fn child_array<'a>(m: &'a mut Map<String, Value>, key: &str) -> &'a mut Vec<Value> {
    let e = m.entry(key).or_insert(Value::Null);
    if e.is_null() {
        *e = Value::Array(Vec::new());
    }
    e.as_array_mut().expect("checked by Pod::read")
}

fn set_annotation(pod: &mut Value, key: &str, value: &str) {
    child_in(child(pod, "metadata"), "annotations").insert(key.into(), json!(value));
}

/// Places the pod on Linux nodes that run the dataplane. Ports Go
/// `requireRacerNode`.
fn require_racer_node(pod: &mut Value) -> Result<(), Reject> {
    let affinity = with_exclusion(Pod::read(pod)?.affinity)?;
    let spec = child(pod, "spec");
    child_in(spec, "nodeSelector").insert(OS_LABEL.into(), json!("linux"));
    spec.insert("affinity".into(), affinity);
    Ok(())
}

fn check_linux(p: &Pod, what: &str) -> Result<(), Reject> {
    if let Some(os) = p.os_name
        && os != "linux"
    {
        return Err(deny(format!(
            "pod spec.os.name {os:?} conflicts with the {what}, which requires linux"
        )));
    }
    if let Some(v) = get_str(p.node_selector, OS_LABEL)
        && v != "linux"
    {
        return Err(deny(format!(
            "pod nodeSelector {OS_LABEL}={v:?} conflicts with the {what}, which requires linux"
        )));
    }
    Ok(())
}

/// Injects the racer-object sidecar. Ports Go `mutateSidecar`.
fn mutate_sidecar(pod: &mut Value, image: &str) -> Result<(), Reject> {
    let p = Pod::read(pod)?;

    if get_str(p.labels, ENABLED_LABEL) != Some(ENABLED_VALUE) {
        return Ok(());
    }
    if has_name(p.init_containers, CONTAINER_NAME) || has_name(p.containers, CONTAINER_NAME) {
        return Ok(());
    }
    // With host networking the loopback listener would be shared with every
    // hostNetwork workload on the node, and the sidecar has no per-client
    // authorization.
    if p.host_network {
        return Err(deny(
            "pod spec.hostNetwork is true, which the racer-object sidecar does not support because its loopback listener would be reachable from the node network namespace",
        ));
    }

    let spec = parse_annotations(p.annotations)?;

    if has_name(p.volumes, VOLUME_NAME) {
        return Err(deny(format!(
            "pod already defines a volume named {VOLUME_NAME:?}"
        )));
    }
    check_linux(&p, "racer-object sidecar")?;

    let client_dir = format!("/run/racer/{}/client", spec.cache);
    let container = json!({
        "name": CONTAINER_NAME,
        "image": image,
        "args": spec.args(),
        "resources": spec.resources,
        "volumeMounts": [{"name": VOLUME_NAME, "mountPath": client_dir, "readOnly": true}],
        "restartPolicy": "Always",
        "securityContext": {
            "runAsUser": SIDECAR_UID,
            "runAsGroup": SIDECAR_UID,
            "runAsNonRoot": true,
            "allowPrivilegeEscalation": false,
            "readOnlyRootFilesystem": true,
            "capabilities": {"drop": ["ALL"]},
            "seccompProfile": {"type": "RuntimeDefault"},
        },
    });
    let volume =
        json!({"name": VOLUME_NAME, "hostPath": {"path": client_dir, "type": "Directory"}});

    let s = child(pod, "spec");
    child_array(s, "initContainers").insert(0, container);
    child_array(s, "volumes").push(volume);
    require_racer_node(pod)?;
    set_annotation(pod, INJECTED_ANNOTATION, image);
    Ok(())
}

/// Mounts `/run/racer/<cache>/client` read-only at the same path in the
/// selected app containers and sets [`SOCKET_ENV`]. A read-only mount still
/// permits connect(2) on the socket: write permission on the socket inode is
/// checked, not the mount's read-only flag. Pods without the label, or already
/// marked with [`CLIENT_SOCKET_INJECTED_ANNOTATION`], are left unchanged.
/// Ports Go `mutateClientSocket`.
fn mutate_client_socket(pod: &mut Value) -> Result<(), Reject> {
    let p = Pod::read(pod)?;

    if get_str(p.labels, CLIENT_SOCKET_LABEL) != Some(ENABLED_VALUE) {
        return Ok(());
    }
    if p.annotations
        .is_some_and(|a| a.contains_key(CLIENT_SOCKET_INJECTED_ANNOTATION))
    {
        return Ok(());
    }

    let Some(cache) = get_str(p.annotations, CLIENT_CACHE_ANNOTATION) else {
        return Err(deny(format!(
            "annotation {CLIENT_CACHE_ANNOTATION} is required and must name the ClusterCache to bind to"
        )));
    };
    if !valid_cache(cache) {
        return Err(deny(format!(
            "annotation {CLIENT_CACHE_ANNOTATION} has invalid cache name {cache:?}"
        )));
    }
    let cache = cache.to_owned();

    let containers = p.containers.map(Vec::as_slice).unwrap_or_default();
    let targets = client_targets(p.annotations, containers)?;
    check_linux(&p, "Racer client socket")?;

    let client_dir = format!("/run/racer/{cache}/client");
    let socket = format!("{client_dir}/socket");

    // Reuse a volume that already mounts this directory, such as the one the
    // racer-object sidecar adds for the same cache.
    let mut volume = p.volumes.into_iter().flatten().find_map(|v| {
        let path = v.get("hostPath")?.get("path")?.as_str()?;
        (path == client_dir)
            .then(|| v.get("name").and_then(Value::as_str).unwrap_or(""))
            .map(str::to_owned)
    });
    if volume.is_none() && has_name(p.volumes, CLIENT_VOLUME_NAME) {
        return Err(deny(format!(
            "pod already defines a volume named {CLIENT_VOLUME_NAME:?}"
        )));
    }

    for &i in &targets {
        let c = containers[i].as_object().expect("checked by Pod::read");
        let name = get_str(Some(c), "name").unwrap_or("");
        let base = format!("spec.containers[{i}]");
        let mounts = array(c, "volumeMounts", &format!("{base}.volumeMounts"))?;
        for m in mounts.into_iter().flatten() {
            let m = m
                .as_object()
                .ok_or_else(|| bad(format!("{base}.volumeMounts items must be objects")))?;
            if string(m, "mountPath", &format!("{base}.volumeMounts.mountPath"))?
                == Some(client_dir.as_str())
            {
                return Err(deny(format!(
                    "container {name:?} already mounts {client_dir}"
                )));
            }
        }
        let env = array(c, "env", &format!("{base}.env"))?;
        named_items(env, &format!("{base}.env"))?;
        if has_name(env, SOCKET_ENV) {
            return Err(deny(format!(
                "container {name:?} already sets {SOCKET_ENV}"
            )));
        }
    }

    let s = child(pod, "spec");
    let volume = match volume.take() {
        Some(v) => v,
        None => {
            child_array(s, "volumes").push(json!({
                "name": CLIENT_VOLUME_NAME,
                "hostPath": {"path": client_dir, "type": "Directory"},
            }));
            CLIENT_VOLUME_NAME.to_owned()
        }
    };
    let containers = child_array(s, "containers");
    for &i in &targets {
        let c = containers[i].as_object_mut().expect("checked by Pod::read");
        child_array(c, "volumeMounts")
            .push(json!({"name": volume, "mountPath": client_dir, "readOnly": true}));
        child_array(c, "env").push(json!({"name": SOCKET_ENV, "value": socket}));
    }
    require_racer_node(pod)?;
    set_annotation(pod, CLIENT_SOCKET_INJECTED_ANNOTATION, &cache);
    Ok(())
}

/// Returns indexes into `spec.containers`. Ports Go `clientTargets`.
fn client_targets(
    annotations: Option<&Map<String, Value>>,
    containers: &[Value],
) -> Result<Vec<usize>, Reject> {
    let Some(value) = get_str(annotations, CLIENT_CONTAINERS_ANNOTATION) else {
        return Ok((0..containers.len()).collect());
    };
    let mut targets = Vec::new();
    for name in value.split(',').map(str::trim) {
        let i = containers
            .iter()
            .position(|c| c.get("name").and_then(Value::as_str) == Some(name));
        match i {
            Some(i) if !targets.contains(&i) => targets.push(i),
            _ => {
                return Err(deny(format!(
                    "annotation {CLIENT_CONTAINERS_ANNOTATION} must list distinct app container names, separated by commas"
                )));
            }
        }
    }
    Ok(targets)
}

/// Appends JSON Patch operations that turn `a` into `b`. Objects are diffed
/// by key. Any other change, including to an array, replaces the value.
fn diff(a: &Value, b: &Value, path: &mut String, ops: &mut Vec<Value>) {
    match (a, b) {
        (Value::Object(x), Value::Object(y)) => {
            for (k, vb) in y {
                let len = path.len();
                path.push('/');
                path.push_str(&escape(k));
                match x.get(k) {
                    None => ops.push(json!({"op": "add", "path": path.as_str(), "value": vb})),
                    Some(va) => diff(va, vb, path, ops),
                }
                path.truncate(len);
            }
            for k in x.keys().filter(|k| !y.contains_key(*k)) {
                ops.push(json!({"op": "remove", "path": format!("{path}/{}", escape(k))}));
            }
        }
        _ if a == b => {}
        _ => ops.push(json!({"op": "replace", "path": path.as_str(), "value": b})),
    }
}

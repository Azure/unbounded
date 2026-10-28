use std::io::Write;
use std::path::PathBuf;
use std::process::{Command, Stdio};

fn rejects(source: &str, code: &str, message: &str) {
    let deps = std::env::current_exe()
        .unwrap()
        .parent()
        .unwrap()
        .to_owned();
    let library = |name: &str| -> PathBuf {
        let mut paths: Vec<_> = std::fs::read_dir(&deps)
            .unwrap()
            .map(|entry| entry.unwrap().path())
            .filter(|path| {
                path.file_name()
                    .unwrap()
                    .to_string_lossy()
                    .starts_with(&format!("lib{name}-"))
                    && path
                        .extension()
                        .is_some_and(|extension| extension == "rlib")
            })
            .collect();
        paths.sort_by_key(|path| std::fs::metadata(path).unwrap().modified().unwrap());
        paths.pop().expect("Cargo must build the tested dependency")
    };
    let mut child = Command::new("timeout")
        .args(["--signal=TERM", "--kill-after=10s", "60s"])
        .arg(std::env::var_os("RUSTC").unwrap_or_else(|| "rustc".into()))
        .args(["--edition=2024", "--crate-name=contract", "--emit=metadata"])
        .arg("-L")
        .arg(format!("dependency={}", deps.display()))
        .arg("--extern")
        .arg(format!(
            "racer_crypto={}",
            library("racer_crypto").display()
        ))
        .arg("--extern")
        .arg(format!(
            "racer_control_wire={}",
            library("racer_control_wire").display()
        ))
        .args(["-o", "/dev/null", "-"])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    child
        .stdin
        .take()
        .unwrap()
        .write_all(format!("fn main() {{ {source} }}").as_bytes())
        .unwrap();
    let output = child.wait_with_output().unwrap();
    let stderr = String::from_utf8(output.stderr).unwrap();
    assert_eq!(output.status.code(), Some(1), "{stderr}");
    assert!(stderr.contains(code), "expected {code}: {stderr}");
    assert!(stderr.contains(message), "expected {message}: {stderr}");
}

#[test]
fn key_lease_material_is_private() {
    rejects(
        "fn raw(key: &racer_crypto::identity::KeyLease) {
            key.material(racer_crypto::identity::KeyPurpose::Page);
        }",
        "E0624",
        "private method",
    );
}

#[test]
fn key_lease_is_not_debug() {
    rejects(
        "fn diagnostic(key: racer_crypto::identity::KeyLease) { println!(\"{key:?}\"); }",
        "E0277",
        "Debug",
    );
}

#[test]
fn certificates_are_not_send() {
    rejects(
        "fn send<T: Send>() {}
        send::<racer_crypto::identity::Certificates>();",
        "E0277",
        "Send",
    );
}

#[test]
fn verified_peer_cannot_be_constructed() {
    // Infer NodeId from crypto so separately built control-wire rlibs cannot conflict.
    rejects(
        "fn forge(identity: &racer_crypto::identity::SigningIdentity) {
            let peer = racer_crypto::identity::VerifiedPeer { node: identity.node().clone() };
        }",
        "E0451",
        "private",
    );
}

#[test]
fn cache_encryption_key_is_not_debug() {
    rejects(
        "fn diagnostic(key: racer_control_wire::CacheEncryptionKey) {
            println!(\"{key:?}\");
        }",
        "E0277",
        "Debug",
    );
}

#[test]
fn cache_encryption_key_material_is_private() {
    rejects(
        "fn expose(key: racer_control_wire::CacheEncryptionKey) -> [u8; 32] {
            key.material
        }",
        "E0616",
        "private",
    );
}

#[test]
fn into_installation_consumes_receiver() {
    rejects(
        "fn transfer_twice(key: racer_control_wire::CacheEncryptionKey) {
            let _first = key.into_installation();
            let _second = key.into_installation();
        }",
        "E0382",
        "moved value",
    );
}

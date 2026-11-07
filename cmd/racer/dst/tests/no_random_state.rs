//! Determinism lint: no `RandomState`-hashed collections in racer-dst.
//!
//! Iterating a `std::collections::HashMap`/`HashSet` with the default hasher
//! visits entries in a per-process random order, which breaks the double-run
//! trace check whenever iteration order reaches behavior. Use `BTreeMap`,
//! `Vec`, or a map with a fixed-seed `BuildHasher` (and name the hasher on
//! the same line so this check can see it).
use std::path::Path;

fn scan(dir: &Path, hits: &mut Vec<String>) {
    for entry in std::fs::read_dir(dir).unwrap() {
        let path = entry.unwrap().path();
        if path.is_dir() {
            scan(&path, hits);
        } else if path.extension().is_some_and(|e| e == "rs") {
            let text = std::fs::read_to_string(&path).unwrap();
            for (n, line) in text.lines().enumerate() {
                let code = line.split("//").next().unwrap_or("");
                let hashed = ["HashMap", "HashSet", "RandomState"]
                    .iter()
                    .any(|t| code.contains(t));
                if hashed && !code.contains("BuildHasherDefault") {
                    hits.push(format!("{}:{}: {}", path.display(), n + 1, line.trim()));
                }
            }
        }
    }
}

#[test]
fn no_random_state_collections() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR"));
    let mut hits = Vec::new();
    for dir in ["src", "tests"] {
        scan(&root.join(dir), &mut hits);
    }
    // This file names the types it forbids.
    hits.retain(|h| !h.contains("no_random_state.rs"));
    assert!(
        hits.is_empty(),
        "RandomState collections:\n{}",
        hits.join("\n")
    );
}

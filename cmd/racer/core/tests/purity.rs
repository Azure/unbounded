//! Fails if racer-core sources reach for clocks, threads, the runtime, or OS randomness.

use std::path::Path;

const FORBIDDEN: &[&str] = &[
    "Instant::now",
    "SystemTime::now",
    "uring_runtime",
    "environment::now",
    "getrandom",
    "thread::spawn",
];

fn scan(dir: &Path, hits: &mut Vec<String>, files: &mut usize) {
    for entry in std::fs::read_dir(dir).unwrap() {
        let path = entry.unwrap().path();
        if path.is_dir() {
            scan(&path, hits, files);
        } else if path.extension().is_some_and(|e| e == "rs") {
            *files += 1;
            let text = std::fs::read_to_string(&path).unwrap();
            // Unit tests may build an `Instant` to feed the API; only scan non-test code.
            let body = text.split("#[cfg(test)]").next().unwrap_or("");
            hits.extend(
                find(body)
                    .into_iter()
                    .map(|(line, word)| format!("{}:{line}: {word}", path.display())),
            );
        }
    }
}

fn find(text: &str) -> Vec<(usize, &'static str)> {
    let mut out = Vec::new();
    for (i, line) in text.lines().enumerate() {
        for word in FORBIDDEN {
            if line.contains(word) {
                out.push((i + 1, *word));
            }
        }
    }
    out
}

#[test]
fn sources_are_pure() {
    let (mut hits, mut files) = (Vec::new(), 0);
    scan(
        &Path::new(env!("CARGO_MANIFEST_DIR")).join("src"),
        &mut hits,
        &mut files,
    );
    assert!(files > 0, "no sources scanned");
    assert!(
        hits.is_empty(),
        "impure calls in racer-core:\n{}",
        hits.join("\n")
    );
}

#[test]
fn scanner_detects_each_forbidden_call() {
    for word in FORBIDDEN {
        let text = format!("fn f() {{\n    let _ = {word}();\n}}");
        assert_eq!(find(&text), vec![(2, *word)]);
    }
    assert!(find("let instant = now.instant;").is_empty());
}

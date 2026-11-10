//! Fails if racer-core sources reach for clocks, threads, the runtime, or OS randomness.

use std::collections::BTreeSet;
use std::path::{Path, PathBuf};

const FORBIDDEN: &[&str] = &[
    "Instant::now",
    "SystemTime::now",
    "racer_io",
    "environment::now",
    "getrandom",
    "thread::spawn",
];

fn scan(dir: &Path, hits: &mut Vec<String>, files: &mut usize) {
    let mut skip = BTreeSet::new();
    let mut paths = Vec::new();
    collect(dir, &mut paths);
    paths.sort();
    let mut texts = Vec::new();
    for path in &paths {
        let text = std::fs::read_to_string(path).unwrap();
        let (_, modules) = production(&text);
        // Out-of-line test modules (`#[cfg(test)] mod tests;`) live beside the declaring file.
        let base = match path.file_name().and_then(|n| n.to_str()) {
            Some("lib.rs" | "mod.rs" | "main.rs") => path.parent().unwrap().to_path_buf(),
            _ => path.with_extension(""),
        };
        for module in modules {
            skip.insert(base.join(format!("{module}.rs")));
            skip.insert(base.join(&module).join("mod.rs"));
        }
        texts.push((path.clone(), text));
    }
    for (path, text) in texts {
        if skip.contains(&path) {
            continue;
        }
        *files += 1;
        let (body, _) = production(&text);
        hits.extend(
            find(&body)
                .into_iter()
                .map(|(line, word)| format!("{}:{line}: {word}", path.display())),
        );
    }
}

fn collect(dir: &Path, out: &mut Vec<PathBuf>) {
    for entry in std::fs::read_dir(dir).unwrap() {
        let path = entry.unwrap().path();
        if path.is_dir() {
            collect(&path, out);
        } else if path.extension().is_some_and(|e| e == "rs") {
            out.push(path);
        }
    }
}

/// Blanks every `#[cfg(test)]` item (and comment lines) while keeping line numbers, and
/// returns the names of out-of-line modules declared under `#[cfg(test)]`.
fn production(text: &str) -> (String, Vec<String>) {
    let mut out = String::new();
    let mut modules = Vec::new();
    // `None`: production code. `Some(None)`: waiting for the gated item. `Some(Some(d))`: inside it.
    let mut gated: Option<Option<i64>> = None;
    for line in text.lines() {
        let trimmed = line.trim();
        let keep = match gated {
            None if trimmed.starts_with("#[cfg(test)]") => {
                gated = Some(None);
                false
            }
            None => !trimmed.starts_with("//"),
            Some(None)
                if trimmed.is_empty() || trimmed.starts_with("#[") || trimmed.starts_with("//") =>
            {
                false
            }
            Some(depth) => {
                let mut depth = depth.unwrap_or(0);
                if depth == 0 && trimmed.ends_with(';') && !trimmed.contains('{') {
                    if let Some(name) = trimmed
                        .strip_prefix("mod ")
                        .or_else(|| trimmed.strip_prefix("pub(crate) mod "))
                        .and_then(|rest| rest.strip_suffix(';'))
                    {
                        modules.push(name.trim().to_owned());
                    }
                    gated = None;
                    false
                } else {
                    depth += line.matches('{').count() as i64 - line.matches('}').count() as i64;
                    gated = if depth <= 0 && line.contains('}') {
                        None
                    } else {
                        Some(Some(depth))
                    };
                    false
                }
            }
        };
        if keep {
            out.push_str(line);
        }
        out.push('\n');
    }
    (out, modules)
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
    // `racer-flow-core` is re-exported as `racer_core::flow` and held to the same rules.
    let root = Path::new(env!("CARGO_MANIFEST_DIR"));
    for dir in [root.join("src"), root.join("../flow-core/src")] {
        scan(&dir, &mut hits, &mut files);
    }
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

#[test]
fn production_skips_only_test_items() {
    let text = "fn a() { Instant::now(); }\n#[cfg(test)]\nmod tests {\n    fn b() { Instant::now(); }\n}\nfn c() { getrandom(); }\n#[cfg(test)]\n#[allow(x)]\nmod more;\n#[cfg(test)]\nuse std::time::Instant;\n// thread::spawn in a comment\nfn d() { thread::spawn(); }\n";
    let (body, modules) = production(text);
    assert_eq!(
        find(&body),
        vec![(1, "Instant::now"), (6, "getrandom"), (13, "thread::spawn")]
    );
    assert_eq!(modules, vec!["more".to_owned()]);
}

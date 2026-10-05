---
name: rust-refactor
description: Refactor Rust code while preserving behavior and using idiomatic ownership and APIs. Use when restructuring Rust functions, modules, traits, or types in this project.
---

- Bound commands with `timeout --signal=TERM --kill-after=10s 300s ...`. Investigate timeouts rather than retrying unchanged.
- Use clippy checks and cargo fmt on all code
- Leave exactly one empty line between consecutive declared struct fields and between consecutive trait associated items (types, constants, and methods, including methods with default bodies).
  - Keep each field or item's doc comments and attributes attached to it; put the separating empty line before those comments and attributes.
  - Do not add an empty line after the final field or item before the closing delimiter; rustfmt removes it. Empty and single-field structs or single-item traits need no separator. For tuple structs, apply the rule when fields are laid out on separate lines by rustfmt.
  - This is a manual review requirement, not an automated lint: Clippy has no built-in rule for it, and rustfmt preserves inter-field spacing but does not require it. Check the spacing in changed declarations after running cargo fmt; do not use `#[rustfmt::skip]` to force it.
- Minimize the public API surface area. Assume subcrates are only used by their parent crate - no external users to worry about (i.e. breaking subcrate changes are fine).
- Provide short, human-readable, plain English docs for all modules, types, and functions. The goal is human readable Cargo generated docs.
- Remove unused code, avoid duplication, and avoid unnecessary or non-useful abstraction.
- Prefer fewer, higher-level integration style tests over unit tests, except cases where the logic under test has a large state space that could be effectively covered by pure unit tests.
  - unit tests in the src files, integration in the tests tree
- No module-scoped README.md files.
- Minimize file count. All files should be 1000-3000 lines.
- Organize declarations in files in an order that makes sense to human readers.
  - important, public types/traits at the top.
  - more trivial helper functions at the bottom
  - unit tests below that
- Prefer designs that allow for strict semantic enforcement of the application's business logic by the Rust compiler. ALWAYS consider how to structure code to better utilize the compiler.
- Carefully consider concurrency. Pad structures to align with CPU cache lines whenever possible, prefer explicit scoping: code that should be pinned to a specific cpu should not implement clone/copy, etc.
- MAKE AGGRESSIVE CHANGES WHEN REFACTORING. It's okay to fundamentally re-write code.
- Use an iterative approach: review changes against these standards when done, iterate until you're confident the standard has been achieved.

## Field and trait item spacing example

```rust
struct Limits {
    max_bytes: usize,

    /// Maximum number of pending requests.
    max_requests: usize,
}

trait Store {
    type Value;

    const CAPACITY: usize;

    fn get(&self) -> Option<Self::Value>;

    fn is_empty(&self) -> bool {
        self.get().is_none()
    }
}
```

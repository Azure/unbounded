---
name: rust-refactor
description: Refactor Rust code while preserving behavior and using idiomatic ownership and APIs. Use when restructuring Rust functions, modules, traits, or types in this project.
---

- Bound commands with `timeout --signal=TERM --kill-after=10s 300s ...`. Investigate timeouts rather than retrying unchanged.
- Use clippy checks and cargo fmt on all code
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

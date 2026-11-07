//! Allocation probe binary: the production dataplane fixture with a counting
//! global allocator. Run only the ignored `warm_page_allocations` probe:
//! `cargo test --release --test alloc_probe -- --ignored --exact production_dataplane::hotpath::warm_page_allocations`
// Reuses the whole dataplane suite; only its allocation probe runs here.
#[path = "production_dataplane.rs"]
#[allow(dead_code)]
mod production_dataplane;

#[global_allocator]
static GLOBAL: production_dataplane::counting_alloc::Counting =
    production_dataplane::counting_alloc::Counting;

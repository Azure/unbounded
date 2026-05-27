// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! End-to-end HTTP test that exercises the full stack with a
//! MemoryObjectSource. Does not require a real BlockStore.

use std::collections::HashMap;
use std::sync::Arc;

use bytes::Bytes;
use futures::StreamExt;

use crate::catalog::{Catalog, YamlCatalog};
use crate::object::memory_source::MemoryObjectSource;
use crate::object::ObjectSource;

const CATALOG_YAML: &str = r#"
objects:
  - bucket: demo
    key: small.bin
    stripe: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    size: 5
    content_type: text/plain
  - bucket: demo
    key: empty.bin
    stripe: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    size: 0
"#;

#[tokio::test]
async fn full_read_full_content() {
    let catalog = Arc::new(YamlCatalog::from_str(CATALOG_YAML).unwrap());
    let meta = catalog.lookup("demo", "small.bin").unwrap();
    let mut source = MemoryObjectSource::new(HashMap::new());
    source.insert(&meta, Bytes::from_static(b"hello"));
    let source: Arc<dyn ObjectSource> = Arc::new(source);

    let stream = source.read_range(&meta, 0, 5);
    let chunks: Vec<Result<Bytes, _>> = stream.collect().await;
    assert_eq!(chunks.len(), 1);
    assert_eq!(chunks[0].as_ref().unwrap().as_ref(), b"hello");
}

#[tokio::test]
async fn partial_range() {
    let catalog = Arc::new(YamlCatalog::from_str(CATALOG_YAML).unwrap());
    let meta = catalog.lookup("demo", "small.bin").unwrap();
    let mut source = MemoryObjectSource::new(HashMap::new());
    source.insert(&meta, Bytes::from_static(b"hello"));
    let source: Arc<dyn ObjectSource> = Arc::new(source);

    let stream = source.read_range(&meta, 1, 3);
    let chunks: Vec<Result<Bytes, _>> = stream.collect().await;
    assert_eq!(chunks.len(), 1);
    assert_eq!(chunks[0].as_ref().unwrap().as_ref(), b"ell");
}

#[tokio::test]
async fn empty_object() {
    let catalog = Arc::new(YamlCatalog::from_str(CATALOG_YAML).unwrap());
    let meta = catalog.lookup("demo", "empty.bin").unwrap();
    let mut source = MemoryObjectSource::new(HashMap::new());
    source.insert(&meta, Bytes::new());
    let source: Arc<dyn ObjectSource> = Arc::new(source);

    let stream = source.read_range(&meta, 0, 0);
    let chunks: Vec<Result<Bytes, _>> = stream.collect().await;
    assert!(chunks.is_empty());
}

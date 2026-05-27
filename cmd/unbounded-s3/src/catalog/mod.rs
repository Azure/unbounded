// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

mod types;
mod yaml;

pub use types::ObjectMeta;
pub use yaml::YamlCatalog;

pub trait Catalog: Send + Sync + 'static {
    fn lookup(&self, bucket: &str, key: &str) -> Option<ObjectMeta>;
}

impl Catalog for YamlCatalog {
    fn lookup(&self, bucket: &str, key: &str) -> Option<ObjectMeta> {
        self.get(bucket, key).cloned()
    }
}

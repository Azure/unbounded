// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::sync::Arc;

use axum::routing::get;
use axum::Router;

use super::handlers::{bucket_root, get_object, head_object, AppState};
use super::response;
use crate::catalog::Catalog;
use crate::object::ObjectSource;

pub(crate) fn build_router(
    catalog: Arc<dyn Catalog>,
    source: Arc<dyn ObjectSource>,
) -> Router {
    let state = AppState { catalog, source };
    Router::new()
        // Object-level: `/<bucket>/<key>` (the `*key` glob requires at
        // least one path segment, so this never matches `/<bucket>/`).
        .route(
            "/:bucket/*key",
            get(get_object).head(head_object).fallback(fallback),
        )
        // Bucket-level: `/<bucket>/` and `/<bucket>`. Today only
        // `?location` is recognized; everything else falls through to
        // 405 inside `bucket_root`. Both GET and HEAD are wired so
        // that S3 clients which HEAD-probe the bucket get the same
        // headers as GET; hyper strips the body on HEAD automatically.
        // Defining both paths explicitly avoids axum's trailing-slash
        // strip surprising callers that do or don't include it.
        .route(
            "/:bucket/",
            get(bucket_root).head(bucket_root).fallback(fallback),
        )
        .route(
            "/:bucket",
            get(bucket_root).head(bucket_root).fallback(fallback),
        )
        .with_state(state)
}

async fn fallback() -> axum::response::Response {
    response::method_not_allowed()
}

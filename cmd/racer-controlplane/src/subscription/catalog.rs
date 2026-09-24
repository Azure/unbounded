// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Read-only management projection of one immutable published generation.
use super::*;
use serde::Serialize;

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Catalog<'a> {
    schema: u32,
    universe: &'a str,
    revision: u64,
    routing_algorithm: u32,
    left_factor: u32,
    right_factor: u32,
    members: &'a [String],
    roles: &'a [u32],
    #[serde(rename = "selectedPodUIDs")]
    selected_pod_uids: Vec<&'a str>,
    volumes: Vec<&'a str>,
}

#[derive(Serialize)]
struct ResponseCatalog<'a> {
    digest: String,
    catalog: Catalog<'a>,
}

impl Subscriptions {
    fn catalog_authority(&self) -> Result<String, StatusCode> {
        let context = (self.security)().ok_or(StatusCode::SERVICE_UNAVAILABLE)?;
        if self.fence.read().unwrap().as_deref() != Some(context.fence.as_str())
            || context.state.fence() != context.fence
            || !self
                .authority_deadline
                .read()
                .unwrap()
                .is_some_and(|deadline| Instant::now() < deadline)
        {
            return Err(StatusCode::SERVICE_UNAVAILABLE);
        }
        Ok(context.fence)
    }

    /// No subscription, observation, snapshot-cache, or Kubernetes side effects.
    /// The digest is SHA-256 of serde's compact JSON encoding of `catalog`, in
    /// the declared field order. Arrays retain canonical member and volume order.
    /// This describes publication, not a fleet-wide application acknowledgment.
    pub(crate) fn product_catalog(&self, universe: &str) -> Result<Vec<u8>, StatusCode> {
        let fence = self.catalog_authority()?;
        if universe.len() != 64
            || !universe
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        {
            return Err(StatusCode::BAD_REQUEST);
        }
        let publication = self
            .universes
            .read()
            .unwrap()
            .get(universe)
            .cloned()
            .ok_or(StatusCode::NOT_FOUND)?;
        let body = encode_catalog(universe, &publication)?;
        // Serialization can overlap publication or leadership replacement. Do
        // not return an old term's catalog or a superseded generation as current.
        if self.catalog_authority()? != fence
            || !self
                .universes
                .read()
                .unwrap()
                .get(universe)
                .is_some_and(|current| Arc::ptr_eq(current, &publication))
        {
            return Err(StatusCode::SERVICE_UNAVAILABLE);
        }
        Ok(body)
    }
}

fn encode_catalog(universe: &str, publication: &Published) -> Result<Vec<u8>, StatusCode> {
    let generation = publication.topology.generation();
    let product = generation.product.as_ref().ok_or(StatusCode::NOT_FOUND)?;
    let selected_pod_uids = product
        .members
        .iter()
        .map(|id| {
            publication
                .selections
                .get(id)
                .map(|selection| selection.pod_uid.as_str())
                .ok_or(StatusCode::SERVICE_UNAVAILABLE)
        })
        .collect::<Result<Vec<_>, _>>()?;
    let mut volumes: Vec<_> = generation
        .volumes
        .iter()
        .filter(|v| v.routing_algorithm == crate::model::PRODUCT_ROUTING_ALGORITHM)
        .map(|v| v.id.as_str())
        .collect();
    volumes.sort_unstable();
    let catalog = Catalog {
        schema: 1,
        universe,
        revision: generation.revision,
        routing_algorithm: crate::model::PRODUCT_ROUTING_ALGORITHM,
        left_factor: product.left_factor,
        right_factor: product.right_factor,
        members: &product.members,
        roles: &product.roles,
        selected_pod_uids,
        volumes,
    };
    let bytes = serde_json::to_vec(&catalog).map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;
    let digest = hex::encode(Sha256::digest(bytes));
    serde_json::to_vec(&ResponseCatalog { digest, catalog })
        .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)
}

#[cfg(test)]
#[path = "../../tests/runtime/catalog.rs"]
mod tests;

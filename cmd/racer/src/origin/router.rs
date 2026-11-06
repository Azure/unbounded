//! Per-cache origin dispatch.
//!
//! Caches published with `CacheDefinition::origin` are read in process through
//! [`AzureOrigin`]; every other cache keeps the adapter socket client. Without
//! an Azure origin configured, every cache uses the adapter socket, exactly as
//! before native origins existed.

use super::azure::{AzureOrigin, AzureTokenSource, AzureTransport};
use super::{MetadataReply, Origin, OriginPage};
use crate::admission::AdmissionPolicy;
use crate::control::Snapshot;
use crate::error::{Error, Operation};
use crate::model::{MetadataSelector, ObjectId, PageId, PageNumber};
use crate::read::candidates::OriginAuthority;
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use controlplane::Published;
use std::sync::Arc;

pub struct OriginRouter<U, T, K> {
    snapshots: Arc<Published<Snapshot>>,
    adapter: U,
    azure: Option<AzureOrigin<T, K>>,
}

impl<U: Origin, T: AzureTransport, K: AzureTokenSource> OriginRouter<U, T, K> {
    pub fn new(
        snapshots: Arc<Published<Snapshot>>,
        adapter: U,
        azure: Option<AzureOrigin<T, K>>,
    ) -> Self {
        Self {
            snapshots,
            adapter,
            azure,
        }
    }

    /// The current snapshot and cache index when `object` belongs to a cache
    /// with a native origin, looked up by cache UID. Any lookup failure falls
    /// through to the adapter, which reports it as before.
    fn native(&self, object: &ObjectId) -> Option<(&AzureOrigin<T, K>, Arc<Snapshot>, usize)> {
        let azure = self.azure.as_ref()?;
        let snapshot = self.snapshots.current().ok()??;
        azure.observe(&snapshot);
        let index = snapshot
            .caches
            .iter()
            .position(|cache| cache.id == object.cache && cache.origin.is_some())?;
        Some((azure, snapshot, index))
    }
}

impl<U: Origin, T: AzureTransport, K: AzureTokenSource> Origin for OriginRouter<U, T, K> {
    fn bootstrap_reserved<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        let Some((azure, snapshot, index)) = self.native(&context.object) else {
            return self
                .adapter
                .bootstrap_reserved(authority, context, reservation, scope);
        };
        Box::pin(async move {
            scope.check()?;
            authority.validate(&context.object, PageNumber(0))?;
            let definition = snapshot.caches[index]
                .origin
                .as_ref()
                .ok_or(Error::Unavailable)?;
            azure
                .bootstrap_with(definition, context, reservation, scope)
                .await
        })
    }

    fn page_reserved<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &'a RequestScope,
    ) -> Operation<'a, OriginPage> {
        let Some((azure, snapshot, index)) = self.native(&context.object) else {
            return self
                .adapter
                .page_reserved(authority, context, page, reservation, scope);
        };
        Box::pin(async move {
            scope.check()?;
            if context.object != page.version.object {
                return Err(Error::Unauthorized);
            }
            authority.validate(&page.version.object, page.number)?;
            let definition = snapshot.caches[index]
                .origin
                .as_ref()
                .ok_or(Error::Unavailable)?;
            azure
                .page_with(definition, context, page, reservation, scope)
                .await
        })
    }

    fn metadata<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        selector: MetadataSelector,
        scope: &'a RequestScope,
    ) -> Operation<'a, MetadataReply> {
        let Some((azure, snapshot, index)) = self.native(&context.object) else {
            return self.adapter.metadata(authority, context, selector, scope);
        };
        Box::pin(async move {
            scope.check()?;
            authority.validate(&context.object, PageNumber(0))?;
            let definition = snapshot.caches[index]
                .origin
                .as_ref()
                .ok_or(Error::Unavailable)?;
            azure
                .metadata_with(definition, context, selector, scope)
                .await
        })
    }
}

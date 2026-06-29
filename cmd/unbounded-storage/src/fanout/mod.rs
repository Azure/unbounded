// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Cross-shard stripe fan-out.
//!
//! Spreads the per-stripe work of serving one inbound connection across
//! every shard, routing each stripe to the shard that owns its
//! content-addressed key so the coordinator core does only dispatch and
//! zero-copy `SEND_ZC`. See [`channel`] for the wire-free Send channel
//! that carries fetch/release commands between shards.

mod channel;
mod router;
mod service;

pub use channel::{
    EventSlot, FetchChannel, FetchChannelReceiver, FetchCommand, FetchEvent, FetchPage,
    FetchStream, PageLoc,
};
pub use router::{
    FanoutPeer, FanoutTable, NumaShardTable, Owner, ShardPick, numa_local_shard, owner_shard,
};
pub use service::FetchService;

//! Kubernetes names and annotation keys shared with manifests and clients.

/// Prefix for every Racer annotation and label.
pub const PREFIX: &str = "racer.unbounded-cloud.io/";

/// Seat Lease names are `racer-seat-0` through `racer-seat-15`.
pub const SEAT_LEASE_PREFIX: &str = "racer-seat-";

/// Lease held by the single writer of durable state.
pub const PRIMARY_LEASE: &str = "racer-primary";

/// Secret created empty by the operator. Its UID is the cluster ID.
pub const STATE_SECRET: &str = "racer-state";

/// Data key in [`STATE_SECRET`] holding the JSON state record.
pub const STATE_KEY: &str = "state";

/// Highest seat count.
pub const MAX_SEATS: usize = 16;

/// Webhook configuration whose `caBundle` the primary manages.
pub const WEBHOOK_CONFIGURATION: &str = "racer-object-sidecar-injector";

/// EndpointSlice listing ready seat webhook endpoints.
pub const WEBHOOK_ENDPOINT_SLICE: &str = "racer-webhook";

/// Lease annotation: control API URL, `https://<pod-ip>:<port>`.
pub const ENDPOINT: &str = "racer.unbounded-cloud.io/endpoint";

/// Lease annotation: base64 DER self-signed control serving certificate.
pub const SERVING_CERTIFICATE: &str = "racer.unbounded-cloud.io/serving-certificate";

/// Lease annotation: base64 DER self-signed webhook serving certificate.
pub const WEBHOOK_CERTIFICATE: &str = "racer.unbounded-cloud.io/webhook-certificate";

/// Lease annotation: base64 DER issuer root of this seat.
pub const ISSUER_CERTIFICATE: &str = "racer.unbounded-cloud.io/issuer-certificate";

/// Lease annotation: decimal publication sequence served, present only when ready.
pub const READY: &str = "racer.unbounded-cloud.io/ready";

/// Lease annotation: per-process nonce of the holder. Lease
/// `holderIdentity` is the Pod name only, so this tells a restarted process
/// apart from its predecessor.
pub const HOLDER_NONCE: &str = "racer.unbounded-cloud.io/holder-nonce";

/// Data key in [`STATE_SECRET`] holding the cluster ID, the Secret UID.
pub const CLUSTER_KEY: &str = "cluster";

/// Lease annotation: UID of the holder Pod.
pub const HOLDER_UID: &str = "racer.unbounded-cloud.io/holder-uid";

/// Lease annotation: Node name of the holder Pod.
pub const HOLDER_NODE: &str = "racer.unbounded-cloud.io/holder-node";

/// Lease and state Secret annotation: cluster ID, the UID of [`STATE_SECRET`].
pub const CLUSTER_ID: &str = "racer.unbounded-cloud.io/cluster-id";

/// Primary Lease annotation: desired seat count.
pub const SEATS: &str = "racer.unbounded-cloud.io/seats";

/// Primary Lease annotation: JSON list of ready endpoints for all Pods.
///
/// Each item is `{"url": "...", "certificate": "<base64 DER>", "generation": "..."}`.
pub const ENDPOINTS: &str = "racer.unbounded-cloud.io/endpoints";

/// Pod annotation: JSON `{"shares": N, "rdma_nics": [...]}` reported by the Pod itself.
pub const REPORT: &str = "racer.unbounded-cloud.io/report";

/// Node annotation set by an administrator. Overrides the Pod report.
pub const SHARES: &str = "racer.unbounded-cloud.io/shares";

/// Node annotation set by an administrator. Overrides the Pod report.
pub const RDMA_NICS: &str = "racer.unbounded-cloud.io/rdma-nics";

/// Node annotation: block device pattern returned by bootstrap.
pub const BLOCK_DEVICES: &str = "racer.unbounded-cloud.io/block-devices";

/// Node label that removes a Node from membership.
pub const EXCLUDE: &str = "racer.unbounded-cloud.io/exclude";

/// Node label that names the site of a Node.
pub const SITE: &str = "unbounded-cloud.io/site";

/// Name of a seat Lease.
pub fn seat_lease(seat: usize) -> String {
    format!("{SEAT_LEASE_PREFIX}{seat}")
}

/// Primary Lease annotation: JSON array of the Node of each seat, `""` when vacant.
pub const SEAT_NODES: &str = "racer.unbounded-cloud.io/seat-nodes";

/// Primary Lease annotation: seconds over which Pods spread vacant seat claims.
pub const CLAIM_WINDOW: &str = "racer.unbounded-cloud.io/claim-window";

/// State Secret annotation set to the Secret UID by the first commit, with
/// [`CLUSTER_ID`]. Data missing with this set means the state was lost, and
/// the runtime fails closed.
pub const INITIALIZED: &str = "racer.unbounded-cloud.io/initialized";

/* Thin C adapter between rdma-verbs (Rust) and libibverbs.
 *
 * Rust loads this as librdma_verbs.so.1 and only sees the small structs and
 * functions below. libibverbs structs and inline helpers stay on this side,
 * built against the installed headers.
 *
 * Bump rdma_verbs_abi() whenever a struct or signature here changes.
 *
 * Unless noted, int results are 0 on success and pointer results are NULL
 * on failure. A failed free leaves the resource in place. */
#include <infiniband/verbs.h>
#include <errno.h>
#include <stdint.h>
#include <stddef.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

/* One active port. Mirrors `Port` in ffi.rs. */
struct rdma_verbs_port {
    char name[64];
    uint8_t gid[16];
    uint32_t mtu;
    uint16_t lid;
    uint8_t port;
    uint8_t link_layer;
};
/* What a peer needs to connect to a QP. Mirrors `Endpoint` in ffi.rs. */
struct rdma_verbs_endpoint {
    uint8_t gid[16];
    uint32_t qpn, psn, mtu;
    uint16_t lid;
    uint8_t port, link_layer;
};
/* One completion. Mirrors `Completion` in ffi.rs. */
struct rdma_verbs_wc { uint64_t id; uint32_t status, opcode; };
/* Catch layout drift from the Rust side at compile time. */
_Static_assert(sizeof(struct rdma_verbs_port) == 88, "port ABI size");
_Static_assert(offsetof(struct rdma_verbs_port, mtu) == 80, "port ABI offset");
_Static_assert(sizeof(struct rdma_verbs_endpoint) == 32, "endpoint ABI size");
_Static_assert(offsetof(struct rdma_verbs_endpoint, qpn) == 16, "endpoint ABI offset");
_Static_assert(sizeof(struct rdma_verbs_wc) == 16, "completion ABI size");
_Static_assert(IBV_WC_RDMA_WRITE == 1 && IBV_WC_BIND_MW == 5 && IBV_WC_LOCAL_INV == 6,
               "completion opcode ABI");
/* Private handles. Rust only holds pointers to these. */
struct rdma_verbs_device { struct ibv_context *ctx; struct ibv_pd *pd; };
struct rdma_verbs_qp { struct ibv_qp *qp; struct ibv_cq *cq; };
struct rdma_verbs_mr { struct ibv_mr *mr; void *bytes; size_t length; };

/* ABI version. Rust refuses to load any other value. */
uint32_t rdma_verbs_abi(void) { return 2; }

/* List active ports on devices that support type 2B memory windows.
 * Uses GID index 0. Fills up to `capacity` entries but returns the full count,
 * so a result above `capacity` means some were left out. */
int rdma_verbs_discover(struct rdma_verbs_port *out, uint32_t capacity) {
    int count = 0, n = 0;
    struct ibv_device **list = ibv_get_device_list(&n);
    if (!list) return -errno;
    for (int i = 0; i < n; ++i) {
        struct ibv_context *ctx = ibv_open_device(list[i]);
        if (!ctx) continue;
        struct ibv_device_attr attr;
        if (ibv_query_device(ctx, &attr) || !(attr.device_cap_flags & IBV_DEVICE_MEM_WINDOW_TYPE_2B)) {
            ibv_close_device(ctx); continue;
        }
        for (unsigned p = 1; p <= attr.phys_port_cnt; ++p) {
            struct ibv_port_attr port;
            union ibv_gid gid;
            if (ibv_query_port(ctx, p, &port) || port.state != IBV_PORT_ACTIVE ||
                ibv_query_gid(ctx, p, 0, &gid)) continue;
            if ((uint32_t)count < capacity) {
                struct rdma_verbs_port *v = &out[count];
                memset(v, 0, sizeof(*v));
                const char *name = ibv_get_device_name(list[i]);
                if (strlen(name) >= sizeof(v->name)) continue;
                memcpy(v->name, name, strlen(name));
                memcpy(v->gid, gid.raw, 16);
                v->mtu = port.active_mtu; v->lid = port.lid;
                v->port = p; v->link_layer = port.link_layer;
            }
            ++count;
        }
        ibv_close_device(ctx);
    }
    ibv_free_device_list(list);
    return count;
}

/* Open a device by name and allocate its protection domain. */
void *rdma_verbs_open(const char *name) {
    int n = 0;
    struct ibv_device **list = ibv_get_device_list(&n);
    if (!list) return NULL;
    struct rdma_verbs_device *d = calloc(1, sizeof(*d));
    if (d) {
        for (int i = 0; i < n; ++i) {
            if (!strcmp(name, ibv_get_device_name(list[i]))) {
                d->ctx = ibv_open_device(list[i]); break;
            }
        }
        if (d->ctx) d->pd = ibv_alloc_pd(d->ctx);
        if (!d->pd) {
            if (d->ctx) ibv_close_device(d->ctx);
            free(d); d = NULL;
        }
    }
    ibv_free_device_list(list);
    return d;
}
/* Free the protection domain and close. On failure, the handle is kept. */
int rdma_verbs_close(struct rdma_verbs_device *d) {
    if (d->pd) { int rc = ibv_dealloc_pd(d->pd); if (rc) return rc; d->pd = NULL; }
    int rc = ibv_close_device(d->ctx);
    if (!rc) free(d);
    return rc;
}

/* Create a reliable-connected QP with its own CQ, in the INIT state.
 * On failure, *qpn = UINT32_MAX means cleanup failed and something leaked;
 * Rust then leaks the device too. */
void *rdma_verbs_qp(struct rdma_verbs_device *d, uint8_t port, uint32_t depth, uint32_t *qpn) {
    struct rdma_verbs_qp *q = calloc(1, sizeof(*q));
    if (!q) return NULL;
    q->cq = ibv_create_cq(d->ctx, depth, NULL, NULL, 0);
    if (!q->cq) { free(q); return NULL; }
    struct ibv_qp_init_attr init = {0};
    init.send_cq = q->cq; init.recv_cq = q->cq; init.qp_type = IBV_QPT_RC;
    init.cap.max_send_wr = depth; init.cap.max_recv_wr = 1;
    init.cap.max_send_sge = 1; init.cap.max_recv_sge = 1;
    q->qp = ibv_create_qp(d->pd, &init);
    if (!q->qp) {
        if (ibv_destroy_cq(q->cq)) *qpn = UINT32_MAX;
        free(q); return NULL;
    }
    struct ibv_qp_attr attr = {0};
    attr.qp_state = IBV_QPS_INIT; attr.port_num = port; attr.pkey_index = 0;
    attr.qp_access_flags = IBV_ACCESS_REMOTE_WRITE;
    if (ibv_modify_qp(q->qp, &attr, IBV_QP_STATE | IBV_QP_PKEY_INDEX |
                      IBV_QP_PORT | IBV_QP_ACCESS_FLAGS)) {
        /* If destroy fails, keep the CQ: the QP may still use it. */
        if (ibv_destroy_qp(q->qp)) { *qpn = UINT32_MAX; }
        else {
            if (ibv_destroy_cq(q->cq)) *qpn = UINT32_MAX;
            free(q);
        }
        return NULL;
    }
    *qpn = q->qp->qp_num;
    return q;
}
/* Move the QP to RTR then RTS, pointed at `remote`. Uses the smaller MTU. */
int rdma_verbs_connect(struct rdma_verbs_qp *q, const struct rdma_verbs_endpoint *local,
                       const struct rdma_verbs_endpoint *remote) {
    struct ibv_qp_attr a = {0};
    a.qp_state = IBV_QPS_RTR; a.path_mtu = local->mtu < remote->mtu ? local->mtu : remote->mtu;
    a.dest_qp_num = remote->qpn; a.rq_psn = remote->psn;
    a.max_dest_rd_atomic = 0; a.min_rnr_timer = 12;
    a.ah_attr.dlid = remote->lid; a.ah_attr.port_num = local->port;
    a.ah_attr.is_global = 1;
    memcpy(a.ah_attr.grh.dgid.raw, remote->gid, 16);
    a.ah_attr.grh.sgid_index = 0; a.ah_attr.grh.hop_limit = 64;
    int rc = ibv_modify_qp(q->qp, &a, IBV_QP_STATE | IBV_QP_AV | IBV_QP_PATH_MTU |
                           IBV_QP_DEST_QPN | IBV_QP_RQ_PSN | IBV_QP_MAX_DEST_RD_ATOMIC |
                           IBV_QP_MIN_RNR_TIMER);
    if (rc) return rc;
    memset(&a, 0, sizeof(a));
    a.qp_state = IBV_QPS_RTS; a.sq_psn = local->psn;
    a.timeout = 14; a.retry_cnt = 3; a.rnr_retry = 3; a.max_rd_atomic = 0;
    return ibv_modify_qp(q->qp, &a, IBV_QP_STATE | IBV_QP_TIMEOUT | IBV_QP_RETRY_CNT |
                         IBV_QP_RNR_RETRY | IBV_QP_SQ_PSN | IBV_QP_MAX_QP_RD_ATOMIC);
}
/* Move the QP to the error state and destroy it. Once this succeeds, no more
 * DMA can touch local or remote memory through it. Safe to call twice. */
int rdma_verbs_stop(struct rdma_verbs_qp *q) {
    if (q->qp) {
        struct ibv_qp_attr a = { .qp_state = IBV_QPS_ERR };
        (void)ibv_modify_qp(q->qp, &a, IBV_QP_STATE);
        int rc = ibv_destroy_qp(q->qp);
        if (rc) return rc;
        q->qp = NULL;
    }
    return 0;
}
/* Stop the QP if needed, then destroy its CQ and free the handle. */
int rdma_verbs_qp_free(struct rdma_verbs_qp *q) {
    int rc = rdma_verbs_stop(q);
    if (rc) return rc;
    rc = ibv_destroy_cq(q->cq);
    if (!rc) free(q);
    return rc;
}
/* Allocate a zeroed, page-aligned buffer and register it for remote writes
 * and window binding. */
void *rdma_verbs_register(struct rdma_verbs_device *d, uint32_t length) {
    struct rdma_verbs_mr *m = calloc(1, sizeof(*m));
    if (!m) return NULL;
    if (posix_memalign(&m->bytes, 4096, length)) { free(m); return NULL; }
    memset(m->bytes, 0, length); m->length = length;
    m->mr = ibv_reg_mr(d->pd, m->bytes, length,
                       IBV_ACCESS_LOCAL_WRITE | IBV_ACCESS_REMOTE_WRITE | IBV_ACCESS_MW_BIND);
    if (!m->mr) { free(m->bytes); free(m); return NULL; }
    return m;
}
/* Deregister and free the buffer. */
int rdma_verbs_deregister(struct rdma_verbs_mr *m) {
    int rc = ibv_dereg_mr(m->mr);
    if (!rc) { free(m->bytes); free(m); }
    return rc;
}
/* The buffer behind a registered region. */
void *rdma_verbs_bytes(struct rdma_verbs_mr *m) { return m->bytes; }
/* Allocate a type 2 memory window. Writes the key to use for its next bind. */
void *rdma_verbs_window(struct rdma_verbs_device *d, uint32_t *key) {
    struct ibv_mw *mw = ibv_alloc_mw(d->pd, IBV_MW_TYPE_2);
    if (mw) *key = ibv_inc_rkey(mw->rkey);
    return mw;
}
/* Free a memory window. */
int rdma_verbs_window_free(struct ibv_mw *mw) { return ibv_dealloc_mw(mw); }
/* Post a bind: let the peer write the first `length` bytes of `m` via `key`. */
int rdma_verbs_bind(struct rdma_verbs_qp *q, struct ibv_mw *mw, struct rdma_verbs_mr *m,
                    uint32_t key, uint64_t id, uint32_t length) {
    if (!length || length > m->length) return EINVAL;
    struct ibv_send_wr wr = {0}, *bad = NULL;
    wr.wr_id = id; wr.opcode = IBV_WR_BIND_MW; wr.send_flags = IBV_SEND_SIGNALED;
    wr.bind_mw.mw = mw; wr.bind_mw.rkey = key;
    wr.bind_mw.bind_info.mr = m->mr;
    wr.bind_mw.bind_info.addr = (uintptr_t)m->bytes;
    wr.bind_mw.bind_info.length = length;
    wr.bind_mw.bind_info.mw_access_flags = IBV_ACCESS_REMOTE_WRITE;
    return ibv_post_send(q->qp, &wr, &bad);
}
/* Post an invalidate of `key`, with IBV_SEND_FENCE. */
int rdma_verbs_invalidate(struct rdma_verbs_qp *q, uint32_t key, uint64_t id) {
    struct ibv_send_wr wr = {0}, *bad = NULL;
    wr.wr_id = id; wr.opcode = IBV_WR_LOCAL_INV;
    wr.send_flags = IBV_SEND_SIGNALED | IBV_SEND_FENCE; wr.invalidate_rkey = key;
    return ibv_post_send(q->qp, &wr, &bad);
}
/* Post an RDMA write of the first `length` bytes of `m` to the peer's
 * `address`, using its window `key`. */
int rdma_verbs_write(struct rdma_verbs_qp *q, struct rdma_verbs_mr *m, uint64_t address,
                     uint32_t key, uint64_t id, uint32_t length) {
    if (!length || length > m->length) return EINVAL;
    struct ibv_sge sge = { .addr = (uintptr_t)m->bytes, .length = length, .lkey = m->mr->lkey };
    struct ibv_send_wr wr = {0}, *bad = NULL;
    wr.wr_id = id; wr.opcode = IBV_WR_RDMA_WRITE;
    wr.send_flags = IBV_SEND_SIGNALED; wr.sg_list = &sge; wr.num_sge = 1;
    wr.wr.rdma.remote_addr = address; wr.wr.rdma.rkey = key;
    return ibv_post_send(q->qp, &wr, &bad);
}
/* Read up to 32 completions. Returns the count, or negative on error. */
int rdma_verbs_poll(struct rdma_verbs_qp *q, struct rdma_verbs_wc *out, uint32_t capacity) {
    struct ibv_wc wc[32];
    if (capacity > 32) capacity = 32;
    int n = ibv_poll_cq(q->cq, capacity, wc);
    if (n < 0) return n;
    for (int i = 0; i < n; ++i) {
        out[i].id = wc[i].wr_id; out[i].status = wc[i].status;
        /* Opcode is only valid on success. */
        out[i].opcode = wc[i].status == IBV_WC_SUCCESS ? (uint32_t)wc[i].opcode : UINT32_MAX;
    }
    return n;
}

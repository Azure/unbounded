/* Thin C adapter between rdma-verbs (Rust) and libibverbs/librdmacm.
 *
 * Rust loads this as librdma_verbs.so.1 and only sees the small structs and
 * functions below. libibverbs and librdmacm structs and inline helpers stay
 * on this side, built against the installed headers.
 *
 * Bump rdma_verbs_abi() whenever a struct, signature, or result contract changes.
 *
 * Every function is a thin wrapper over one verbs or connection manager
 * call (or a short fixed sequence of them). Policy lives in Rust: which
 * device mode to use, how grants and writes map to memory windows or
 * registrations, the work request ledger, and the connection manager state
 * machine. This file spawns no threads and never blocks on the network:
 * completion and connection manager channels are nonblocking.
 *
 * Unless noted, int results are 0 on success and pointer results are NULL
 * on failure. A failed free leaves the resource in place. */
#include <infiniband/verbs.h>
#include <rdma/rdma_cma.h>
#include <arpa/inet.h>
#include <sys/mman.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdint.h>
#include <stdio.h>
#include <stddef.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

/* One active port. Mirrors `Port` in ffi.rs. `gid` is GID index 0;
 * `ipv4_gid` is the port's IPv4-mapped RoCE GID (::ffff:a.b.c.d) at
 * `ipv4_index`, or zero with index -1. `flags` reports facts, not a mode. */
struct rdma_verbs_port {
    char name[64];
    uint8_t gid[16];
    uint32_t mtu;
    uint16_t lid;
    uint8_t port;
    uint8_t link_layer;
    uint8_t ipv4_gid[16];
    int32_t ipv4_index;
    uint32_t flags;
};
#define RDMA_VERBS_PORT_MW 1u /* type 2B memory windows */
#define RDMA_VERBS_PORT_RC 2u /* an RC QP could be created */
/* What a peer needs to connect to a QP. Mirrors `Endpoint` in ffi.rs. */
struct rdma_verbs_endpoint {
    uint8_t gid[16];
    uint32_t qpn, psn, mtu;
    uint16_t lid;
    uint8_t port, link_layer;
};
/* One completion. Mirrors `Completion` in ffi.rs. Unless status is
 * IBV_WC_SUCCESS, opcode is UINT32_MAX and byte_len, imm, and flags are 0.
 * imm is in host byte order and valid only if flags & RDMA_VERBS_WC_IMM. */
struct rdma_verbs_wc {
    uint64_t id;
    uint32_t status, opcode;
    uint32_t byte_len, imm;
    uint32_t flags, vendor_err;
};
#define RDMA_VERBS_WC_IMM 1u
/* Device capabilities for one port. Mirrors `Caps` in ffi.rs. */
struct rdma_verbs_caps {
    uint32_t flags;
    uint32_t max_msg_sz;
    int32_t ipv4_index;
    uint8_t ipv4[4];
    /* Device limits: work requests per queue, CQ entries, memory windows. */
    uint32_t max_qp_wr, max_cqe, max_mw;
};
#define RDMA_VERBS_CAP_MW 1u  /* type 2B memory windows */
#define RDMA_VERBS_CAP_ODP 2u /* implicit on-demand paging for RC writes */
/* One scatter-gather entry. Mirrors `Sge` in ffi.rs. */
struct rdma_verbs_sge {
    uint64_t addr;
    uint32_t length, lkey;
};
/* rdma_verbs_post_write flags. */
#define RDMA_VERBS_POST_IMM 1u
#define RDMA_VERBS_POST_SIGNALED 2u
/* One connection manager event, copied out of librdmacm. Mirrors `CmEvent`
 * in ffi.rs. `handle` must be passed to rdma_verbs_cm_ack. */
struct rdma_verbs_cm_event {
    void *handle;
    void *id;
    void *listen_id;
    int32_t type;
    int32_t status;
    uint32_t private_len;
    uint8_t private_data[56];
    uint32_t reserved;
};
/* Region access sets for rdma_verbs_register and rdma_verbs_reg_mr. */
enum {
    RDMA_VERBS_MR_PAGES = 1,        /* LOCAL_WRITE | MW_BIND */
    RDMA_VERBS_MR_CONTROL = 2,      /* LOCAL_WRITE */
    RDMA_VERBS_MR_REMOTE_WRITE = 4, /* adds REMOTE_WRITE */
};
/* Catch layout drift from the Rust side at compile time. */
_Static_assert(sizeof(struct rdma_verbs_port) == 112, "port ABI size");
_Static_assert(offsetof(struct rdma_verbs_port, mtu) == 80, "port ABI offset");
_Static_assert(offsetof(struct rdma_verbs_port, ipv4_gid) == 88, "port ABI offset");
_Static_assert(offsetof(struct rdma_verbs_port, flags) == 108, "port ABI offset");
_Static_assert(sizeof(struct rdma_verbs_endpoint) == 32, "endpoint ABI size");
_Static_assert(offsetof(struct rdma_verbs_endpoint, qpn) == 16, "endpoint ABI offset");
_Static_assert(sizeof(struct rdma_verbs_wc) == 32, "completion ABI size");
_Static_assert(offsetof(struct rdma_verbs_wc, byte_len) == 16, "completion ABI offset");
_Static_assert(offsetof(struct rdma_verbs_wc, flags) == 24, "completion ABI offset");
_Static_assert(sizeof(struct rdma_verbs_caps) == 28, "caps ABI size");
_Static_assert(offsetof(struct rdma_verbs_caps, max_qp_wr) == 16, "caps ABI offset");
_Static_assert(offsetof(struct rdma_verbs_caps, ipv4) == 12, "caps ABI offset");
_Static_assert(sizeof(struct rdma_verbs_sge) == 16, "sge ABI size");
_Static_assert(offsetof(struct rdma_verbs_sge, lkey) == 12, "sge ABI offset");
_Static_assert(sizeof(struct rdma_verbs_cm_event) == 96, "cm event ABI size");
_Static_assert(offsetof(struct rdma_verbs_cm_event, type) == 24, "cm event ABI offset");
_Static_assert(offsetof(struct rdma_verbs_cm_event, private_len) == 32, "cm event ABI offset");
_Static_assert(offsetof(struct rdma_verbs_cm_event, private_data) == 36, "cm event ABI offset");
_Static_assert(IBV_WC_SEND == 0 && IBV_WC_RDMA_WRITE == 1 && IBV_WC_BIND_MW == 5 &&
               IBV_WC_LOCAL_INV == 6 && IBV_WC_RECV == 128 &&
               IBV_WC_RECV_RDMA_WITH_IMM == 129, "completion opcode ABI");
_Static_assert(RDMA_CM_EVENT_ADDR_RESOLVED == 0 && RDMA_CM_EVENT_ADDR_ERROR == 1 &&
               RDMA_CM_EVENT_ROUTE_RESOLVED == 2 && RDMA_CM_EVENT_ROUTE_ERROR == 3 &&
               RDMA_CM_EVENT_CONNECT_REQUEST == 4 && RDMA_CM_EVENT_CONNECT_ERROR == 6 &&
               RDMA_CM_EVENT_UNREACHABLE == 7 && RDMA_CM_EVENT_REJECTED == 8 &&
               RDMA_CM_EVENT_ESTABLISHED == 9 && RDMA_CM_EVENT_DISCONNECTED == 10 &&
               RDMA_CM_EVENT_DEVICE_REMOVAL == 11, "cm event type ABI");

/* Private handles. Rust only holds pointers to these. `cm_list` is set when
 * the context came from rdma_get_devices, so connection manager QPs can
 * share the PD, CQs, and MRs. */
struct rdma_verbs_device { struct ibv_context *ctx; struct ibv_pd *pd; struct ibv_context **cm_list; };
/* A QP. `id` is set when the connection manager created it. */
struct rdma_verbs_qp { struct ibv_qp *qp; struct rdma_cm_id *id; };
/* `bytes` is NULL for an implicit or dma-buf region. `addr` is the address
 * windows bind from: the buffer pointer, or the dma-buf iova. `device` is set
 * for a dma-buf region. `mapped` is the mmap length when the buffer came
 * from mmap. */
struct rdma_verbs_mr {
    struct ibv_mr *mr; void *bytes; size_t length; uint64_t addr; int device; size_t mapped;
};

/* ABI version. Rust refuses to load any other value.
 * Version 5 added rdma_verbs_register_dmabuf. Version 6 added registration
 * mode in C. Version 7 moved registration mode, the connection manager state
 * machine, and the work request ledger to Rust; this file now only wraps
 * verbs and librdmacm calls. Version 8 added rdma_verbs_prefetch. */
uint32_t rdma_verbs_abi(void) { return 9; }

/* Distinct from negative errno: cleanup leaked a resource. Rust must keep
 * the admission owner and adapter loaded. Stop before another resource leaks. */
#define RDMA_VERBS_CLEANUP_LEAK INT_MIN
#define RDMA_VERBS_OPEN_CLEANUP_LEAK ((void *)UINTPTR_MAX)

/* errno, or EIO if a failed call left it unset. */
static int err(void) { return errno ? errno : EIO; }

/* Index of the port's IPv4-mapped RoCE GID (::ffff:a.b.c.d), or -1. */
static int ipv4_gid_index(struct ibv_context *ctx, uint8_t port, int table, union ibv_gid *out) {
    for (int i = 0; i < table && i < 256; ++i) {
        union ibv_gid gid;
        if (ibv_query_gid(ctx, port, i, &gid)) continue;
        static const uint8_t prefix[12] = {0,0,0,0,0,0,0,0,0,0,0xff,0xff};
        if (!memcmp(gid.raw, prefix, sizeof(prefix))) { *out = gid; return i; }
    }
    return -1;
}

/* True if `ctx` can create an RC QP. The QP is destroyed at once. */
static int supports_rc(struct ibv_context *ctx) {
    struct ibv_pd *pd = ibv_alloc_pd(ctx);
    if (!pd) return 0;
    struct ibv_cq *cq = ibv_create_cq(ctx, 2, NULL, NULL, 0);
    int ok = 0;
    if (cq) {
        struct ibv_qp_init_attr init = {0};
        init.send_cq = cq; init.recv_cq = cq; init.qp_type = IBV_QPT_RC;
        init.cap.max_send_wr = 1; init.cap.max_recv_wr = 1;
        init.cap.max_send_sge = 1; init.cap.max_recv_sge = 1;
        struct ibv_qp *qp = ibv_create_qp(pd, &init);
        if (qp) { ok = 1; if (ibv_destroy_qp(qp)) ok = 0; }
        if (ibv_destroy_cq(cq)) ok = 0;
    }
    if (ibv_dealloc_pd(pd)) ok = 0;
    return ok;
}

/* List active ports on devices that support type 2B memory windows or can
 * create RC QPs, with both their GID index 0 and IPv4-mapped GID. Rust picks
 * the mode. Fills up to `capacity` entries but returns the full count, so a
 * result above `capacity` means some were left out.
 * Returns RDMA_VERBS_CLEANUP_LEAK if an internal context cannot close. */
int rdma_verbs_discover(struct rdma_verbs_port *out, uint32_t capacity) {
    int count = 0, n = 0;
    struct ibv_device **list = ibv_get_device_list(&n);
    if (!list) return -errno;
    fprintf(stderr, "racer: stage=rdma-discover devices=%d\n", n);
    for (int i = 0; i < n; ++i) {
        struct ibv_context *ctx = ibv_open_device(list[i]);
        if (!ctx) {
            fprintf(stderr, "racer: stage=rdma-discover device=%s skipped=open errno=%d\n",
                    ibv_get_device_name(list[i]), errno);
            continue;
        }
        struct ibv_device_attr attr;
        uint32_t flags = 0;
        errno = 0;
        int queried = ibv_query_device(ctx, &attr);
        if (!queried) {
            if (attr.device_cap_flags & IBV_DEVICE_MEM_WINDOW_TYPE_2B) flags |= RDMA_VERBS_PORT_MW;
            else if (supports_rc(ctx)) flags |= RDMA_VERBS_PORT_RC;
        }
        if (!flags)
            fprintf(stderr, "racer: stage=rdma-discover device=%s skipped=no_capability query=%d errno=%d\n",
                    ibv_get_device_name(list[i]), queried, errno);
        if (!flags) {
            if (ibv_close_device(ctx)) {
                count = RDMA_VERBS_CLEANUP_LEAK;
                break;
            }
            continue;
        }
        for (unsigned p = 1; p <= attr.phys_port_cnt; ++p) {
            struct ibv_port_attr port;
            union ibv_gid gid, ipv4;
            memset(&ipv4, 0, sizeof(ipv4));
            if (ibv_query_port(ctx, p, &port) || port.state != IBV_PORT_ACTIVE) {
                fprintf(stderr, "racer: stage=rdma-discover device=%s port=%u skipped=inactive errno=%d\n",
                        ibv_get_device_name(list[i]), p, errno);
                continue;
            }
            if (ibv_query_gid(ctx, p, 0, &gid)) {
                fprintf(stderr, "racer: stage=rdma-discover device=%s port=%u skipped=gid errno=%d\n",
                        ibv_get_device_name(list[i]), p, errno);
                continue;
            }
            int index = ipv4_gid_index(ctx, p, port.gid_tbl_len, &ipv4);
            if ((uint32_t)count < capacity) {
                struct rdma_verbs_port *v = &out[count];
                memset(v, 0, sizeof(*v));
                const char *name = ibv_get_device_name(list[i]);
                if (strlen(name) >= sizeof(v->name)) continue;
                memcpy(v->name, name, strlen(name));
                memcpy(v->gid, gid.raw, 16);
                v->mtu = port.active_mtu; v->lid = port.lid;
                v->port = p; v->link_layer = port.link_layer;
                if (index >= 0) memcpy(v->ipv4_gid, ipv4.raw, 16);
                v->ipv4_index = index;
                v->flags = flags;
            }
            ++count;
        }
        if (ibv_close_device(ctx)) {
            count = RDMA_VERBS_CLEANUP_LEAK;
            break;
        }
    }
    ibv_free_device_list(list);
    return count;
}

/* Open a device by name and allocate its protection domain. NULL means clean
 * failure. RDMA_VERBS_OPEN_CLEANUP_LEAK means a context leaked; it is not a
 * handle and must not be passed to close or any other adapter function.
 * With `cm` nonzero, use the connection manager's context for the device, so
 * QPs the connection manager creates can share the PD, CQs, and MRs. */
void *rdma_verbs_open(const char *name, int cm) {
    if (cm) {
        int m = 0;
        struct ibv_context **list = rdma_get_devices(&m);
        if (!list) {
            fprintf(stderr, "racer: stage=rdma-open device=%s cm=1 step=get_devices errno=%d\n", name, errno);
            return NULL;
        }
        for (int i = 0; i < m; ++i) {
            if (strcmp(name, ibv_get_device_name(list[i]->device))) continue;
            struct rdma_verbs_device *d = calloc(1, sizeof(*d));
            if (!d) break;
            d->ctx = list[i]; d->cm_list = list;
            d->pd = ibv_alloc_pd(d->ctx);
            if (d->pd) return d;
            fprintf(stderr, "racer: stage=rdma-open device=%s cm=1 step=alloc_pd errno=%d\n", name, errno);
            free(d);
            rdma_free_devices(list);
            return NULL;
        }
        fprintf(stderr, "racer: stage=rdma-open device=%s cm=1 step=lookup devices=%d\n", name, m);
        rdma_free_devices(list);
        return NULL;
    }
    int n = 0;
    int leaked = 0;
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
            if (d->ctx && ibv_close_device(d->ctx)) leaked = 1;
            free(d); d = NULL;
        }
    }
    ibv_free_device_list(list);
    return leaked ? RDMA_VERBS_OPEN_CLEANUP_LEAK : d;
}
/* Free the protection domain and close. On failure, the handle is kept. */
int rdma_verbs_close(struct rdma_verbs_device *d) {
    if (d->pd) { int rc = ibv_dealloc_pd(d->pd); if (rc) return rc; d->pd = NULL; }
    if (d->cm_list) {
        rdma_free_devices(d->cm_list);
        free(d);
        return 0;
    }
    int rc = ibv_close_device(d->ctx);
    if (!rc) free(d);
    return rc;
}

/* Report memory window and implicit ODP support, the device's queue, CQ, and
 * memory window limits, `port`'s maximum message size, and its IPv4-mapped
 * GID, if any. */
int rdma_verbs_query_caps(struct rdma_verbs_device *d, uint8_t port, struct rdma_verbs_caps *out) {
    memset(out, 0, sizeof(*out));
    out->ipv4_index = -1;
    struct ibv_device_attr_ex attr;
    memset(&attr, 0, sizeof(attr));
    if (ibv_query_device_ex(d->ctx, NULL, &attr)) return err();
    if (attr.orig_attr.device_cap_flags & IBV_DEVICE_MEM_WINDOW_TYPE_2B) out->flags |= RDMA_VERBS_CAP_MW;
    if ((attr.odp_caps.general_caps & IBV_ODP_SUPPORT) &&
        (attr.odp_caps.general_caps & IBV_ODP_SUPPORT_IMPLICIT) &&
        (attr.odp_caps.per_transport_caps.rc_odp_caps & IBV_ODP_SUPPORT_WRITE))
        out->flags |= RDMA_VERBS_CAP_ODP;
    out->max_qp_wr = attr.orig_attr.max_qp_wr > 0 ? (uint32_t)attr.orig_attr.max_qp_wr : 0;
    out->max_cqe = attr.orig_attr.max_cqe > 0 ? (uint32_t)attr.orig_attr.max_cqe : 0;
    out->max_mw = attr.orig_attr.max_mw > 0 ? (uint32_t)attr.orig_attr.max_mw : 0;
    struct ibv_port_attr pa;
    if (ibv_query_port(d->ctx, port, &pa)) return err();
    out->max_msg_sz = pa.max_msg_sz;
    union ibv_gid gid;
    int index = ipv4_gid_index(d->ctx, port, pa.gid_tbl_len, &gid);
    if (index >= 0) { out->ipv4_index = index; memcpy(out->ipv4, gid.raw + 12, 4); }
    return 0;
}

/* Set O_NONBLOCK on `fd`. */
static int nonblocking(int fd) {
    int flags = fcntl(fd, F_GETFL);
    return flags < 0 || fcntl(fd, F_SETFL, flags | O_NONBLOCK) < 0 ? -1 : 0;
}

/* Create a completion channel whose fd is nonblocking. */
void *rdma_verbs_comp_channel(struct rdma_verbs_device *d) {
    struct ibv_comp_channel *ch = ibv_create_comp_channel(d->ctx);
    if (!ch) return NULL;
    if (nonblocking(ch->fd)) { (void)ibv_destroy_comp_channel(ch); return NULL; }
    return ch;
}
/* The channel's fd: readable when a CQ armed with req_notify has an event. */
int rdma_verbs_comp_channel_fd(struct ibv_comp_channel *ch) { return ch->fd; }
/* Destroy a completion channel. Every CQ on it must be gone. */
int rdma_verbs_comp_channel_free(struct ibv_comp_channel *ch) { return ibv_destroy_comp_channel(ch); }
/* Create a CQ of `depth` entries reporting to `ch` (may be NULL). */
void *rdma_verbs_cq_create(struct rdma_verbs_device *d, uint32_t depth, struct ibv_comp_channel *ch) {
    if (!depth || depth > INT_MAX) return NULL;
    return ibv_create_cq(d->ctx, (int)depth, NULL, ch, 0);
}
/* Destroy a CQ. Every QP on it must be gone and every event acknowledged. */
int rdma_verbs_cq_free(struct ibv_cq *cq) { return ibv_destroy_cq(cq); }
/* Arm `cq`: its channel gets one event at the next completion. */
int rdma_verbs_req_notify(struct ibv_cq *cq) { return ibv_req_notify_cq(cq, 0); }
/* Take one event from `ch` without blocking. Returns 0 and the CQ, -EAGAIN
 * if there is none, or another negative errno. Acknowledge each event. */
int rdma_verbs_get_cq_event(struct ibv_comp_channel *ch, void **cq) {
    struct ibv_cq *c = NULL;
    void *context = NULL;
    errno = 0;
    if (ibv_get_cq_event(ch, &c, &context)) return -err();
    *cq = c;
    return 0;
}
/* Acknowledge `n` events of `cq`. */
void rdma_verbs_ack_cq_events(struct ibv_cq *cq, uint32_t n) { ibv_ack_cq_events(cq, n); }

/* Create a reliable-connected QP on `cq` for both queues, in the INIT state.
 * Both depths must be nonzero. On failure, *qpn = UINT32_MAX means cleanup
 * failed and something leaked; Rust then leaks the device too. */
void *rdma_verbs_qp(struct rdma_verbs_device *d, struct ibv_cq *cq, uint8_t port,
                    uint32_t send_depth, uint32_t recv_depth, uint32_t *qpn) {
    if (!send_depth || !recv_depth || send_depth > INT_MAX || recv_depth > INT_MAX) return NULL;
    struct rdma_verbs_qp *q = calloc(1, sizeof(*q));
    if (!q) return NULL;
    struct ibv_qp_init_attr init = {0};
    init.send_cq = cq; init.recv_cq = cq; init.qp_type = IBV_QPT_RC;
    init.cap.max_send_wr = send_depth; init.cap.max_recv_wr = recv_depth;
    init.cap.max_send_sge = 1; init.cap.max_recv_sge = 1;
    q->qp = ibv_create_qp(d->pd, &init);
    if (!q->qp) { free(q); return NULL; }
    struct ibv_qp_attr attr = {0};
    attr.qp_state = IBV_QPS_INIT; attr.port_num = port; attr.pkey_index = 0;
    attr.qp_access_flags = IBV_ACCESS_REMOTE_WRITE;
    if (ibv_modify_qp(q->qp, &attr, IBV_QP_STATE | IBV_QP_PKEY_INDEX |
                      IBV_QP_PORT | IBV_QP_ACCESS_FLAGS)) {
        if (ibv_destroy_qp(q->qp)) *qpn = UINT32_MAX;
        else free(q);
        return NULL;
    }
    *qpn = q->qp->qp_num;
    return q;
}

/* Move the QP to RTR then RTS, pointed at `remote`. Uses the smaller MTU.
 * rnr_retry is 0: a send or write with immediate that finds no posted
 * receive fails with IBV_WC_RNR_RETRY_EXC_ERR instead of stalling. */
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
    a.timeout = 14; a.retry_cnt = 3; a.rnr_retry = 0; a.max_rd_atomic = 0;
    return ibv_modify_qp(q->qp, &a, IBV_QP_STATE | IBV_QP_TIMEOUT | IBV_QP_RETRY_CNT |
                         IBV_QP_RNR_RETRY | IBV_QP_SQ_PSN | IBV_QP_MAX_QP_RD_ATOMIC);
}

/* Move the QP to the error state: posted work flushes. */
int rdma_verbs_qp_error(struct rdma_verbs_qp *q) {
    struct ibv_qp_attr a = { .qp_state = IBV_QPS_ERR };
    return ibv_modify_qp(q->qp, &a, IBV_QP_STATE);
}
/* Destroy the QP and free the handle. Once this succeeds, no more DMA can
 * touch local or remote memory through it. A connection manager QP is
 * destroyed through its id, which stays for rdma_verbs_cm_destroy_id. */
int rdma_verbs_qp_free(struct rdma_verbs_qp *q) {
    if (q->id) rdma_destroy_qp(q->id);
    else { int rc = ibv_destroy_qp(q->qp); if (rc) return rc; }
    free(q);
    return 0;
}

#define RDMA_VERBS_HUGE (2u << 20)
/* Allocate zeroed, page-aligned memory. Buffers of 2 MiB or more are 2 MiB
 * aligned and advised for transparent huge pages, which keeps registration
 * cheap. *mapped is the mmap length, or 0 for heap memory. */
static void *alloc_buffer(size_t length, size_t *mapped) {
    *mapped = 0;
    if (length >= RDMA_VERBS_HUGE) {
        size_t rounded = (length + RDMA_VERBS_HUGE - 1) & ~(size_t)(RDMA_VERBS_HUGE - 1);
        size_t reserve = rounded + RDMA_VERBS_HUGE;
        char *base = mmap(NULL, reserve, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
        if (base == MAP_FAILED) return NULL;
        uintptr_t start = ((uintptr_t)base + RDMA_VERBS_HUGE - 1) & ~(uintptr_t)(RDMA_VERBS_HUGE - 1);
        size_t prefix = start - (uintptr_t)base, suffix = reserve - prefix - rounded;
        if (prefix) munmap(base, prefix);
        if (suffix) munmap((char *)start + rounded, suffix);
        (void)madvise((void *)start, rounded, MADV_HUGEPAGE);
        *mapped = rounded;
        return (void *)start;
    }
    void *bytes = NULL;
    if (posix_memalign(&bytes, 4096, length)) return NULL;
    memset(bytes, 0, length);
    return bytes;
}
static void free_buffer(void *bytes, size_t mapped) {
    if (mapped) munmap(bytes, mapped); else free(bytes);
}

/* Verbs access flags for an RDMA_VERBS_MR_* set, or -1 if invalid. */
static int access_flags(uint32_t access) {
    int flags = IBV_ACCESS_LOCAL_WRITE;
    switch (access & ~(uint32_t)RDMA_VERBS_MR_REMOTE_WRITE) {
    case RDMA_VERBS_MR_PAGES: flags |= IBV_ACCESS_MW_BIND; break;
    case RDMA_VERBS_MR_CONTROL: break;
    default: return -1;
    }
    if (access & RDMA_VERBS_MR_REMOTE_WRITE) flags |= IBV_ACCESS_REMOTE_WRITE;
    return flags;
}

/* Allocate a zeroed, page-aligned buffer and register it. `access` is exactly
 * one of RDMA_VERBS_MR_PAGES or RDMA_VERBS_MR_CONTROL, optionally with
 * RDMA_VERBS_MR_REMOTE_WRITE. */
void *rdma_verbs_register(struct rdma_verbs_device *d, uint64_t length, uint32_t access) {
    int flags = access_flags(access);
    if (flags < 0 || !length || length > SIZE_MAX) return NULL;
    struct rdma_verbs_mr *m = calloc(1, sizeof(*m));
    if (!m) return NULL;
    m->bytes = alloc_buffer((size_t)length, &m->mapped);
    if (!m->bytes) { free(m); return NULL; }
    m->length = (size_t)length;
    m->addr = (uintptr_t)m->bytes;
    m->mr = ibv_reg_mr(d->pd, m->bytes, (size_t)length, flags);
    if (!m->mr) { free_buffer(m->bytes, m->mapped); free(m); return NULL; }
    return m;
}
/* Register `length` bytes of a dma-buf, starting at `offset` in it, as page
 * buffers (LOCAL_WRITE | MW_BIND, so peers write only through windows).
 * Windows over it bind from `iova`. The caller keeps the memory behind the
 * dma-buf alive until deregister succeeds. The kernel holds its own reference
 * to the dma-buf, so `fd` may be closed after this returns. The CPU cannot
 * reach the memory: rdma_verbs_bytes returns NULL. Needs rdma-core 34 or later. */
void *rdma_verbs_register_dmabuf(struct rdma_verbs_device *d, uint64_t offset, uint64_t length,
                                 uint64_t iova, int fd) {
    if (!length || length > SIZE_MAX || fd < 0) return NULL;
    struct rdma_verbs_mr *m = calloc(1, sizeof(*m));
    if (!m) return NULL;
    m->length = (size_t)length; m->addr = iova; m->device = 1;
    m->mr = ibv_reg_dmabuf_mr(d->pd, offset, (size_t)length, iova, fd,
                              IBV_ACCESS_LOCAL_WRITE | IBV_ACCESS_MW_BIND);
    if (!m->mr) { free(m); return NULL; }
    return m;
}
/* Deregister and free the buffer, if this side allocated one. */
int rdma_verbs_deregister(struct rdma_verbs_mr *m) {
    int rc = m->mr ? ibv_dereg_mr(m->mr) : 0;
    if (!rc) {
        if (m->bytes) free_buffer(m->bytes, m->mapped);
        free(m);
    }
    return rc;
}
/* Register the whole address space of this process as one implicit
 * on-demand-paging (ODP) MR, for zero-copy write sources. The NIC resolves
 * pages on use and the kernel keeps the mapping coherent with munmap and
 * reuse, so any live process memory can be a local SGE without a per-buffer
 * registration. Rust checks RDMA_VERBS_CAP_ODP first. `bytes` is NULL and
 * `length` SIZE_MAX. Local read only. */
void *rdma_verbs_register_implicit(struct rdma_verbs_device *d) {
    struct rdma_verbs_mr *m = calloc(1, sizeof(*m));
    if (!m) return NULL;
    m->mr = ibv_reg_mr(d->pd, NULL, SIZE_MAX, IBV_ACCESS_ON_DEMAND);
    /* Some providers require local write on any ODP MR. */
    if (!m->mr) m->mr = ibv_reg_mr(d->pd, NULL, SIZE_MAX, IBV_ACCESS_ON_DEMAND | IBV_ACCESS_LOCAL_WRITE);
    if (!m->mr) { free(m); return NULL; }
    m->bytes = NULL; m->length = SIZE_MAX;
    return m;
}
/* Fault [addr, addr + length) into the NIC's page tables for the implicit
 * ODP region `m` and wait (IBV_ADVISE_MR_FLAG_FLUSH) until it is mapped, so a
 * write posted from it next does not take NIC page faults. Thread safe: it
 * only reads `m`, which must stay registered until it returns. Returns 0 or
 * a positive errno (EOPNOTSUPP if the provider has no prefetch). */
int rdma_verbs_prefetch(struct rdma_verbs_mr *m, uint64_t addr, uint32_t length) {
    if (!m || !m->mr || !length) return EINVAL;
    struct ibv_sge sge = { .addr = addr, .length = length, .lkey = m->mr->lkey };
    return ibv_advise_mr(m->mr->pd, IBV_ADVISE_MR_ADVICE_PREFETCH, IBV_ADVISE_MR_FLAG_FLUSH,
                         &sge, 1);
}
/* The buffer behind a registered region. NULL for an implicit or dma-buf region. */
void *rdma_verbs_bytes(struct rdma_verbs_mr *m) { return m->bytes; }
/* The local key of a registered region. */
uint32_t rdma_verbs_lkey(struct rdma_verbs_mr *m) { return m->mr->lkey; }

/* Register caller memory [addr, addr + length) with an RDMA_VERBS_MR_CONTROL
 * access set (optionally with remote write). Writes its keys. */
void *rdma_verbs_reg_mr(struct rdma_verbs_device *d, uint64_t addr, uint64_t length,
                        uint32_t access, uint32_t *lkey, uint32_t *rkey) {
    int flags = access_flags(access);
    if (flags < 0 || (flags & IBV_ACCESS_MW_BIND) || !length || length > SIZE_MAX) return NULL;
    struct ibv_mr *mr = ibv_reg_mr(d->pd, (void *)(uintptr_t)addr, (size_t)length, flags);
    if (!mr) return NULL;
    *lkey = mr->lkey; *rkey = mr->rkey;
    return mr;
}
/* Deregister an MR from rdma_verbs_reg_mr. */
int rdma_verbs_dereg_mr(struct ibv_mr *mr) { return ibv_dereg_mr(mr); }

/* Allocate a type 2 memory window; writes the key to use for its next bind. */
void *rdma_verbs_window(struct rdma_verbs_device *d, uint32_t *key) {
    struct ibv_mw *mw = ibv_alloc_mw(d->pd, IBV_MW_TYPE_2);
    if (!mw) return NULL;
    *key = ibv_inc_rkey(mw->rkey);
    return mw;
}
/* Free a window. */
int rdma_verbs_window_free(struct ibv_mw *mw) { return ibv_dealloc_mw(mw); }

/* True if [offset, offset + length) is a nonempty range inside `m`. */
static int in_region(const struct rdma_verbs_mr *m, uint64_t offset, uint32_t length) {
    return length && offset <= m->length && length <= m->length - offset;
}

/* Post a bind: let the peer write `length` bytes of `m` at `offset` via `key`.
 * IBV_SEND_FENCE holds the bind until earlier work on the QP completes. On
 * mlx5, a bind posted behind an RDMA write whose implicit ODP source page
 * faults can fail with IBV_WC_MW_BIND_ERR (vendor_err 0x78) and break the
 * link; this happens when a link both writes zero-copy and grants. */
int rdma_verbs_bind(struct rdma_verbs_qp *q, struct ibv_mw *mw, struct rdma_verbs_mr *m,
                    uint32_t key, uint64_t id, uint64_t offset, uint32_t length) {
    if (!in_region(m, offset, length)) return EINVAL;
    struct ibv_send_wr wr = {0}, *bad = NULL;
    wr.wr_id = id; wr.opcode = IBV_WR_BIND_MW;
    wr.send_flags = IBV_SEND_SIGNALED | IBV_SEND_FENCE;
    wr.bind_mw.mw = mw; wr.bind_mw.rkey = key;
    wr.bind_mw.bind_info.mr = m->mr;
    wr.bind_mw.bind_info.addr = m->addr + offset;
    wr.bind_mw.bind_info.length = length;
    wr.bind_mw.bind_info.mw_access_flags = IBV_ACCESS_REMOTE_WRITE;
    return ibv_post_send(q->qp, &wr, &bad);
}
/* Post a signaled LOCAL_INV of `key`. Not fenced. */
int rdma_verbs_invalidate(struct rdma_verbs_qp *q, uint32_t key, uint64_t id) {
    struct ibv_send_wr wr = {0}, *bad = NULL;
    wr.wr_id = id; wr.opcode = IBV_WR_LOCAL_INV;
    wr.send_flags = IBV_SEND_SIGNALED; wr.invalidate_rkey = key;
    return ibv_post_send(q->qp, &wr, &bad);
}

/* Copy `n` (1 to 4) entries into verbs SGEs. Returns 0 if `n` is out of range. */
static int sges(struct ibv_sge *out, const struct rdma_verbs_sge *in, uint32_t n) {
    if (!n || n > 4) return 0;
    for (uint32_t i = 0; i < n; ++i) {
        out[i].addr = in[i].addr; out[i].length = in[i].length; out[i].lkey = in[i].lkey;
    }
    return 1;
}

/* Post an RDMA write of the `n` gathered entries to the peer's `raddr`
 * through `rkey`. With RDMA_VERBS_POST_IMM it carries `imm` (host byte
 * order) and consumes one of the peer's posted receives; with
 * RDMA_VERBS_POST_SIGNALED it completes on this side's CQ. */
int rdma_verbs_post_write(struct rdma_verbs_qp *q, const struct rdma_verbs_sge *sge, uint32_t n,
                          uint64_t raddr, uint32_t rkey, uint32_t imm, uint32_t flags,
                          uint64_t id) {
    struct ibv_sge list[4];
    if (!sges(list, sge, n)) return EINVAL;
    struct ibv_send_wr wr = {0}, *bad = NULL;
    wr.wr_id = id;
    wr.opcode = flags & RDMA_VERBS_POST_IMM ? IBV_WR_RDMA_WRITE_WITH_IMM : IBV_WR_RDMA_WRITE;
    if (flags & RDMA_VERBS_POST_IMM) wr.imm_data = htonl(imm);
    if (flags & RDMA_VERBS_POST_SIGNALED) wr.send_flags = IBV_SEND_SIGNALED;
    wr.sg_list = list; wr.num_sge = (int)n;
    wr.wr.rdma.remote_addr = raddr; wr.wr.rdma.rkey = rkey;
    return ibv_post_send(q->qp, &wr, &bad);
}
/* Post a signaled send of the `n` gathered entries. Consumes one of the
 * peer's posted receives. */
int rdma_verbs_post_send(struct rdma_verbs_qp *q, const struct rdma_verbs_sge *sge, uint32_t n,
                         uint64_t id) {
    struct ibv_sge list[4];
    if (!sges(list, sge, n)) return EINVAL;
    struct ibv_send_wr wr = {0}, *bad = NULL;
    wr.wr_id = id; wr.opcode = IBV_WR_SEND;
    wr.send_flags = IBV_SEND_SIGNALED; wr.sg_list = list; wr.num_sge = (int)n;
    return ibv_post_send(q->qp, &wr, &bad);
}
/* Post a receive into the `n` scattered entries. */
int rdma_verbs_post_recv(struct rdma_verbs_qp *q, const struct rdma_verbs_sge *sge, uint32_t n,
                         uint64_t id) {
    struct ibv_sge list[4];
    if (!sges(list, sge, n)) return EINVAL;
    struct ibv_recv_wr wr = {0}, *bad = NULL;
    wr.wr_id = id; wr.sg_list = list; wr.num_sge = (int)n;
    return ibv_post_recv(q->qp, &wr, &bad);
}
/* Read up to 32 completions from `cq` without blocking. Returns the count,
 * or negative on error. */
int rdma_verbs_poll(struct ibv_cq *cq, struct rdma_verbs_wc *out, uint32_t capacity) {
    struct ibv_wc wc[32];
    if (capacity > 32) capacity = 32;
    int got = ibv_poll_cq(cq, (int)capacity, wc);
    if (got < 0) return got;
    for (int i = 0; i < got; ++i) {
        struct rdma_verbs_wc *o = &out[i];
        memset(o, 0, sizeof(*o));
        o->id = wc[i].wr_id; o->status = wc[i].status;
        o->vendor_err = wc[i].vendor_err;
        /* Opcode, length, and immediate are only valid on success. */
        if (wc[i].status != IBV_WC_SUCCESS) { o->opcode = UINT32_MAX; continue; }
        o->opcode = (uint32_t)wc[i].opcode;
        o->byte_len = wc[i].byte_len;
        if (wc[i].wc_flags & IBV_WC_WITH_IMM) {
            o->imm = ntohl(wc[i].imm_data);
            o->flags = RDMA_VERBS_WC_IMM;
        }
    }
    return got;
}

/* Connection manager wrappers. Every id lives on a nonblocking event
 * channel; Rust reads events with rdma_verbs_cm_get_event and acknowledges
 * each one before destroying any id (rdma_destroy_id waits for that). */

/* Create an event channel whose fd is nonblocking. `d` is unused. */
void *rdma_verbs_cm_channel(struct rdma_verbs_device *d) {
    (void)d;
    struct rdma_event_channel *ch = rdma_create_event_channel();
    if (!ch) return NULL;
    if (nonblocking(ch->fd)) { rdma_destroy_event_channel(ch); return NULL; }
    return ch;
}
/* The channel's fd: readable when an event is waiting. */
int rdma_verbs_cm_channel_fd(struct rdma_event_channel *ch) { return ch->fd; }
/* Destroy an event channel. Every id on it must be gone. */
int rdma_verbs_cm_channel_free(struct rdma_event_channel *ch) {
    rdma_destroy_event_channel(ch);
    return 0;
}
/* Fill an IPv4 socket address. */
static void inet(struct sockaddr_in *a, const uint8_t addr[4], uint16_t port) {
    memset(a, 0, sizeof(*a));
    a->sin_family = AF_INET; a->sin_port = htons(port);
    memcpy(&a->sin_addr, addr, 4);
}
/* Listen on `addr`:`port`, which must belong to `d`. */
int rdma_verbs_cm_listen(struct rdma_event_channel *ch, struct rdma_verbs_device *d,
                         const uint8_t addr[4], uint16_t port, void **out) {
    struct rdma_cm_id *id = NULL;
    struct sockaddr_in local;
    inet(&local, addr, port);
    if (rdma_create_id(ch, &id, NULL, RDMA_PS_TCP)) return err();
    int rc = 0;
    if (rdma_bind_addr(id, (struct sockaddr *)&local)) rc = err();
    else if (id->verbs != d->ctx) rc = EINVAL;
    else if (rdma_listen(id, 128)) rc = err();
    if (rc) { (void)rdma_destroy_id(id); return rc; }
    *out = id;
    return 0;
}
/* Create an id on `ch` for an outgoing connection. */
int rdma_verbs_cm_id(struct rdma_event_channel *ch, void **out) {
    struct rdma_cm_id *id = NULL;
    if (rdma_create_id(ch, &id, NULL, RDMA_PS_TCP)) return err();
    *out = id;
    return 0;
}
/* Start resolving `dst`:`port` from `src`. Completes with an
 * ADDR_RESOLVED or ADDR_ERROR event. */
int rdma_verbs_cm_resolve_addr(struct rdma_cm_id *id, const uint8_t src[4], const uint8_t dst[4],
                               uint16_t port, int timeout_ms) {
    struct sockaddr_in s, t;
    inet(&s, src, 0); inet(&t, dst, port);
    return rdma_resolve_addr(id, (struct sockaddr *)&s, (struct sockaddr *)&t, timeout_ms) ? err() : 0;
}
/* Start resolving the route. Completes with ROUTE_RESOLVED or ROUTE_ERROR. */
int rdma_verbs_cm_resolve_route(struct rdma_cm_id *id, int timeout_ms) {
    return rdma_resolve_route(id, timeout_ms) ? err() : 0;
}
/* Create `id`'s RC QP on `cq` for both queues, with two send SGEs. `id` must
 * be bound to `d`'s context. Writes the QP number. */
void *rdma_verbs_cm_create_qp(struct rdma_cm_id *id, struct rdma_verbs_device *d, struct ibv_cq *cq,
                              uint32_t send_depth, uint32_t recv_depth, uint32_t *qpn) {
    if (id->verbs != d->ctx || !send_depth || !recv_depth || send_depth > INT_MAX ||
        recv_depth > INT_MAX) return NULL;
    struct rdma_verbs_qp *q = calloc(1, sizeof(*q));
    if (!q) return NULL;
    struct ibv_qp_init_attr init = {0};
    init.send_cq = cq; init.recv_cq = cq; init.qp_type = IBV_QPT_RC;
    init.cap.max_send_wr = send_depth; init.cap.max_recv_wr = recv_depth;
    init.cap.max_send_sge = 2; init.cap.max_recv_sge = 1;
    if (rdma_create_qp(id, d->pd, &init)) { free(q); return NULL; }
    q->qp = id->qp; q->id = id;
    *qpn = id->qp->qp_num;
    return q;
}
/* Connection parameters with `len` bytes of private data. */
static void params(struct rdma_conn_param *p, const uint8_t *data, uint32_t len) {
    memset(p, 0, sizeof(*p));
    p->private_data = data; p->private_data_len = (uint8_t)len;
    /* No RDMA reads or atomics are posted; MANA requires a nonzero depth. */
    p->responder_resources = 1; p->initiator_depth = 1;
    /* A missing receive fails rather than stalls, like window mode. Transport
     * retries are high because under fan-in a lost packet is congestion, and a
     * link broken by retry exhaustion falls back to HTTP for a while. Not 7:
     * MANA treats 7 as retry forever, so a write to a peer that went away
     * would never complete. */
    p->retry_count = 6; p->rnr_retry_count = 0;
}
/* Start connecting with `len` (at most 56) bytes of private data. Completes
 * with ESTABLISHED, REJECTED, UNREACHABLE, or CONNECT_ERROR. */
int rdma_verbs_cm_connect(struct rdma_cm_id *id, const uint8_t *data, uint32_t len) {
    if (len > 56) return EINVAL;
    struct rdma_conn_param p;
    params(&p, data, len);
    return rdma_connect(id, &p) ? err() : 0;
}
/* Accept a connection request on its id, with `len` bytes of private data. */
int rdma_verbs_cm_accept(struct rdma_cm_id *id, const uint8_t *data, uint32_t len) {
    if (len > 56) return EINVAL;
    struct rdma_conn_param p;
    params(&p, data, len);
    return rdma_accept(id, &p) ? err() : 0;
}
/* Reject a connection request. The id must still be destroyed. */
int rdma_verbs_cm_reject(struct rdma_cm_id *id) { return rdma_reject(id, NULL, 0) ? err() : 0; }
/* Take one event from `ch` without blocking and copy it out. Returns 0,
 * -EAGAIN if there is none, or another negative errno. */
int rdma_verbs_cm_get_event(struct rdma_event_channel *ch, struct rdma_verbs_cm_event *out) {
    struct rdma_cm_event *ev = NULL;
    errno = 0;
    if (rdma_get_cm_event(ch, &ev)) return -err();
    memset(out, 0, sizeof(*out));
    out->handle = ev; out->id = ev->id; out->listen_id = ev->listen_id;
    out->type = ev->event; out->status = ev->status;
    const void *data = ev->param.conn.private_data;
    uint32_t len = ev->param.conn.private_data_len;
    if (data && (ev->event == RDMA_CM_EVENT_CONNECT_REQUEST ||
                 ev->event == RDMA_CM_EVENT_ESTABLISHED)) {
        if (len > sizeof(out->private_data)) len = sizeof(out->private_data);
        memcpy(out->private_data, data, len);
        out->private_len = len;
    }
    return 0;
}
/* Acknowledge an event from rdma_verbs_cm_get_event. */
void rdma_verbs_cm_ack(void *handle) { (void)rdma_ack_cm_event(handle); }
/* Destroy an id. Its QP must be gone and its events acknowledged. */
int rdma_verbs_cm_destroy_id(struct rdma_cm_id *id) { return rdma_destroy_id(id) ? err() : 0; }

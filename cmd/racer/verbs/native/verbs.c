/* Thin C adapter between rdma-verbs (Rust) and libibverbs.
 *
 * Rust loads this as librdma_verbs.so.1 and only sees the small structs and
 * functions below. libibverbs structs and inline helpers stay on this side,
 * built against the installed headers.
 *
 * Bump rdma_verbs_abi() whenever a struct, signature, or result contract changes.
 *
 * Two device modes:
 * - Window mode (devices with type 2B memory windows, such as mlx5): grants are
 *   window binds, QPs connect with plain verbs, and writes use an implicit ODP
 *   region. This is the original design.
 * - Registration mode (devices without memory windows or implicit ODP, such as
 *   Azure MANA): see "Registration mode" below.
 *
 * Unless noted, int results are 0 on success and pointer results are NULL
 * on failure. A failed free leaves the resource in place. */
#include <infiniband/verbs.h>
#include <rdma/rdma_cma.h>
#include <arpa/inet.h>
#include <pthread.h>
#include <sys/mman.h>
#include <errno.h>
#include <limits.h>
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
/* Region access sets for rdma_verbs_register. */
enum {
    RDMA_VERBS_MR_PAGES = 1,        /* LOCAL_WRITE | MW_BIND */
    RDMA_VERBS_MR_CONTROL = 2,      /* LOCAL_WRITE */
    RDMA_VERBS_MR_REMOTE_WRITE = 4, /* adds REMOTE_WRITE */
};
/* Catch layout drift from the Rust side at compile time. */
_Static_assert(sizeof(struct rdma_verbs_port) == 88, "port ABI size");
_Static_assert(offsetof(struct rdma_verbs_port, mtu) == 80, "port ABI offset");
_Static_assert(sizeof(struct rdma_verbs_endpoint) == 32, "endpoint ABI size");
_Static_assert(offsetof(struct rdma_verbs_endpoint, qpn) == 16, "endpoint ABI offset");
_Static_assert(sizeof(struct rdma_verbs_wc) == 32, "completion ABI size");
_Static_assert(offsetof(struct rdma_verbs_wc, byte_len) == 16, "completion ABI offset");
_Static_assert(offsetof(struct rdma_verbs_wc, flags) == 24, "completion ABI offset");
_Static_assert(IBV_WC_SEND == 0 && IBV_WC_RDMA_WRITE == 1 && IBV_WC_BIND_MW == 5 &&
               IBV_WC_LOCAL_INV == 6 && IBV_WC_RECV == 128 &&
               IBV_WC_RECV_RDMA_WITH_IMM == 129, "completion opcode ABI");
/* Registration mode
 *
 * Some devices (Azure MANA) support RC QPs, RDMA write with immediate, and
 * plain MRs, but no memory windows and no on-demand paging, and only connect
 * QPs established through the RDMA connection manager. Their data path stays
 * zero copy:
 * - A grant registers the granted page buffer itself as a remote-write MR and
 *   hands the peer that MR's rkey. The registration covers only that buffer
 *   (rounded up to whole 2 MiB pages inside it, see bind). Invalidation
 *   deregisters it, so the rkey stops working, exactly where a window would
 *   be invalidated. One grant, one fresh key, one buffer.
 * - A write registers the caller's source memory for local access, posts the
 *   RDMA writes straight from it, and deregisters it when the write
 *   completes. Racer page memory is 2 MiB aligned and huge-page backed, which
 *   makes these registrations cheap (about 0.2 ms for a 16 MiB page).
 * - QPs connect through librdmacm: the initiator resolves the responder's
 *   address and connects, the responder accepts on a per-device listener,
 *   matching a token both sides exchanged in the authenticated setup.
 * No bounce buffer or extra copy is involved: the NIC reads the sender's page
 * memory and writes the receiver's page buffer, as in window mode. */
#define RDMA_VERBS_HUGE (2u << 20)
#define RDMA_VERBS_CM_MAGIC 0x52435231u /* "RCR1" */
struct rdma_verbs_cm_req { uint32_t magic, initiator, responder; };
/* Receives posted before a registration-mode QP exists. */
struct rdma_verbs_queued_recv { uint64_t id, addr; uint32_t length, lkey; };
/* Local-access MRs a registration-mode write posted from, by WR id. */
struct rdma_verbs_dyn { uint64_t id; struct ibv_mr *mr[2]; struct rdma_verbs_dyn *next; };
/* A completion the adapter produced itself (bind or invalidate). */
struct rdma_verbs_synth { uint64_t id; uint32_t opcode; struct rdma_verbs_synth *next; };
struct rdma_verbs_qp;
/* Per-device connection manager state (registration mode only). */
struct rdma_verbs_cm {
    struct rdma_event_channel *channel;
    struct rdma_cm_id *listener;
    pthread_t thread;
    int running;
    pthread_mutex_t lock;
    struct rdma_verbs_qp **qps; size_t nqps, cap;
    struct sockaddr_in local;
};
/* Private handles. Rust only holds pointers to these. */
struct rdma_verbs_device {
    struct ibv_context *ctx; struct ibv_pd *pd;
    int registration;              /* 1 in registration mode */
    uint32_t max_msg;              /* port max message size */
    struct ibv_context **cm_list;  /* rdma_get_devices list owning ctx */
    struct rdma_verbs_cm *cm;
};
struct rdma_verbs_qp {
    struct ibv_qp *qp; struct ibv_cq *cq;
    /* Registration mode only, under `lock`. */
    struct rdma_verbs_device *dev;
    struct rdma_cm_id *id;
    pthread_mutex_t lock;
    uint32_t token, expected, send_depth, recv_depth;
    int stopped;
    struct rdma_verbs_queued_recv *recvs; uint32_t nrecvs;
    struct rdma_verbs_dyn *dyn;
    struct rdma_verbs_synth *synth, *synth_tail;
};
/* `bytes` is NULL for an implicit or dma-buf region. `addr` is the address
 * windows bind from: the buffer pointer, or the dma-buf iova. `device` is set
 * for a dma-buf region, which only receives through windows. `dynamic` marks
 * a registration-mode implicit region: writes register their source. `mapped`
 * is the mmap length when the buffer came from mmap. */
struct rdma_verbs_mr {
    struct ibv_mr *mr; void *bytes; size_t length; uint64_t addr; int device;
    int dynamic; size_t mapped;
};
/* A window. In window mode it wraps an ibv_mw. In registration mode it holds
 * the MR of its current grant, if any. */
struct rdma_verbs_window { struct ibv_mw *mw; struct ibv_mr *mr; struct rdma_verbs_device *dev; };

/* ABI version. Rust refuses to load any other value.
 * Version 5 added rdma_verbs_register_dmabuf. Version 6 added registration
 * mode: connect takes the role, bind reports the key it used, and
 * invalidate takes the window. */
uint32_t rdma_verbs_abi(void) { return 6; }

/* Distinct from negative errno: cleanup leaked a resource. Rust must keep
 * the admission owner and adapter loaded. Stop before another resource leaks. */
#define RDMA_VERBS_CLEANUP_LEAK INT_MIN
#define RDMA_VERBS_OPEN_CLEANUP_LEAK ((void *)UINTPTR_MAX)

#ifndef RDMA_VERBS_NO_REGISTRATION
/* Index of the port's IPv4-mapped RoCE GID (::ffff:a.b.c.d), or -1. The
 * connection manager addresses peers by IPv4, so registration mode needs it. */
static int ipv4_gid_index(struct ibv_context *ctx, uint8_t port, int table) {
    for (int i = 0; i < table && i < 256; ++i) {
        union ibv_gid gid;
        if (ibv_query_gid(ctx, port, i, &gid)) continue;
        static const uint8_t prefix[12] = {0,0,0,0,0,0,0,0,0,0,0xff,0xff};
        if (!memcmp(gid.raw, prefix, sizeof(prefix))) return i;
    }
    return -1;
}

/* True if `ctx` can create an RC QP: registration mode needs RC and plain
 * MRs; the QP is destroyed at once. */
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
#endif

/* List active ports on devices that support type 2B memory windows, or, in
 * registration mode, devices without windows that support RC QPs and have an
 * IPv4-mapped RoCE GID. Window-mode ports report GID index 0; registration
 * mode ports report their IPv4-mapped GID. Fills up to `capacity` entries but
 * returns the full count, so a result above `capacity` means some were left out.
 * Returns RDMA_VERBS_CLEANUP_LEAK if an internal context cannot close. */
int rdma_verbs_discover(struct rdma_verbs_port *out, uint32_t capacity) {
    int count = 0, n = 0;
    struct ibv_device **list = ibv_get_device_list(&n);
    if (!list) return -errno;
    for (int i = 0; i < n; ++i) {
        struct ibv_context *ctx = ibv_open_device(list[i]);
        if (!ctx) continue;
        struct ibv_device_attr attr;
        int windows = 0, registration = 0;
        if (!ibv_query_device(ctx, &attr)) {
            windows = (attr.device_cap_flags & IBV_DEVICE_MEM_WINDOW_TYPE_2B) != 0;
#ifndef RDMA_VERBS_NO_REGISTRATION
            registration = !windows && supports_rc(ctx);
#endif
        }
        if (!windows && !registration) {
            if (ibv_close_device(ctx)) {
                count = RDMA_VERBS_CLEANUP_LEAK;
                break;
            }
            continue;
        }
        for (unsigned p = 1; p <= attr.phys_port_cnt; ++p) {
            struct ibv_port_attr port;
            union ibv_gid gid;
            int index = 0;
            if (ibv_query_port(ctx, p, &port) || port.state != IBV_PORT_ACTIVE) continue;
#ifndef RDMA_VERBS_NO_REGISTRATION
            if (registration && (index = ipv4_gid_index(ctx, p, port.gid_tbl_len)) < 0) continue;
#endif
            if (ibv_query_gid(ctx, p, index, &gid)) continue;
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
        if (ibv_close_device(ctx)) {
            count = RDMA_VERBS_CLEANUP_LEAK;
            break;
        }
    }
    ibv_free_device_list(list);
    return count;
}

#ifndef RDMA_VERBS_NO_REGISTRATION
static void *cm_thread(void *arg);
static void synth_push(struct rdma_verbs_qp *q, uint64_t id, uint32_t opcode);

/* The connection manager port: RACER_RDMA_CM_PORT, else 18515. */
static uint16_t cm_port(void) {
    const char *v = getenv("RACER_RDMA_CM_PORT");
    long p = v ? strtol(v, NULL, 10) : 18515;
    return p > 0 && p < 65536 ? (uint16_t)p : 18515;
}

/* Start the device's listener on its IPv4 address and the event thread. */
static int cm_start(struct rdma_verbs_device *d, uint8_t port) {
    struct ibv_port_attr pa;
    if (ibv_query_port(d->ctx, port, &pa)) return -1;
    int index = ipv4_gid_index(d->ctx, port, pa.gid_tbl_len);
    union ibv_gid gid;
    if (index < 0 || ibv_query_gid(d->ctx, port, index, &gid)) return -1;
    d->max_msg = pa.max_msg_sz ? pa.max_msg_sz : (1u << 30);
    struct rdma_verbs_cm *cm = calloc(1, sizeof(*cm));
    if (!cm) return -1;
    pthread_mutex_init(&cm->lock, NULL);
    cm->local.sin_family = AF_INET;
    memcpy(&cm->local.sin_addr, gid.raw + 12, 4);
    cm->local.sin_port = htons(cm_port());
    cm->channel = rdma_create_event_channel();
    if (!cm->channel) goto fail;
    if (rdma_create_id(cm->channel, &cm->listener, NULL, RDMA_PS_TCP)) goto fail;
    if (rdma_bind_addr(cm->listener, (struct sockaddr *)&cm->local) ||
        cm->listener->verbs != d->ctx || rdma_listen(cm->listener, 128)) goto fail;
    cm->running = 1;
    d->cm = cm;
    if (pthread_create(&cm->thread, NULL, cm_thread, d)) { d->cm = NULL; cm->running = 0; goto fail; }
    return 0;
fail:
    if (cm->listener) rdma_destroy_id(cm->listener);
    if (cm->channel) rdma_destroy_event_channel(cm->channel);
    free(cm);
    return -1;
}

/* Stop the listener and event thread. Every QP must already be freed. */
static void cm_stop(struct rdma_verbs_device *d) {
    struct rdma_verbs_cm *cm = d->cm;
    if (!cm) return;
    pthread_mutex_lock(&cm->lock);
    cm->running = 0;
    pthread_mutex_unlock(&cm->lock);
    /* Destroying the listener wakes nothing by itself; close the channel's
     * fd side by destroying the listener, then cancel the blocked read. */
    rdma_destroy_id(cm->listener);
    pthread_cancel(cm->thread);
    pthread_join(cm->thread, NULL);
    rdma_destroy_event_channel(cm->channel);
    free(cm->qps);
    free(cm);
    d->cm = NULL;
}

/* Register `q` under its token. */
static int cm_add(struct rdma_verbs_cm *cm, struct rdma_verbs_qp *q) {
    pthread_mutex_lock(&cm->lock);
    if (cm->nqps == cm->cap) {
        size_t cap = cm->cap ? cm->cap * 2 : 64;
        struct rdma_verbs_qp **qps = realloc(cm->qps, cap * sizeof(*qps));
        if (!qps) { pthread_mutex_unlock(&cm->lock); return -1; }
        cm->qps = qps; cm->cap = cap;
    }
    cm->qps[cm->nqps++] = q;
    pthread_mutex_unlock(&cm->lock);
    return 0;
}

static void cm_remove(struct rdma_verbs_cm *cm, struct rdma_verbs_qp *q) {
    pthread_mutex_lock(&cm->lock);
    for (size_t i = 0; i < cm->nqps; ++i)
        if (cm->qps[i] == q) { cm->qps[i] = cm->qps[--cm->nqps]; break; }
    pthread_mutex_unlock(&cm->lock);
}

/* Find the QP holding `token`. Caller holds cm->lock. */
static struct rdma_verbs_qp *cm_find(struct rdma_verbs_cm *cm, uint32_t token) {
    for (size_t i = 0; i < cm->nqps; ++i)
        if (cm->qps[i]->token == token) return cm->qps[i];
    return NULL;
}

/* Create `q`'s QP on `id` and replay its queued receives. Caller holds q->lock. */
static int cm_create_qp(struct rdma_verbs_qp *q, struct rdma_cm_id *id) {
    struct ibv_qp_init_attr init = {0};
    init.send_cq = q->cq; init.recv_cq = q->cq; init.qp_type = IBV_QPT_RC;
    init.cap.max_send_wr = q->send_depth; init.cap.max_recv_wr = q->recv_depth;
    init.cap.max_send_sge = 2; init.cap.max_recv_sge = 1;
    if (rdma_create_qp(id, q->dev->pd, &init)) return -1;
    q->id = id; q->qp = id->qp;
    for (uint32_t i = 0; i < q->nrecvs; ++i) {
        struct rdma_verbs_queued_recv *r = &q->recvs[i];
        struct ibv_sge sge = { .addr = r->addr, .length = r->length, .lkey = r->lkey };
        struct ibv_recv_wr wr = {0}, *bad = NULL;
        wr.wr_id = r->id; wr.sg_list = &sge; wr.num_sge = 1;
        if (ibv_post_recv(q->qp, &wr, &bad)) return -1;
    }
    q->nrecvs = 0;
    return 0;
}

static void cm_params(struct rdma_conn_param *p, struct rdma_verbs_cm_req *req) {
    memset(p, 0, sizeof(*p));
    p->private_data = req; p->private_data_len = sizeof(*req);
    /* No RDMA reads or atomics are posted; MANA requires a nonzero depth. */
    p->responder_resources = 1; p->initiator_depth = 1;
    /* Like window mode: a missing receive fails rather than stalls. */
    p->retry_count = 3; p->rnr_retry_count = 0;
}

/* Accept a connection request for a registered responder QP whose expected
 * initiator token and address match, else reject it. */
static void cm_request(struct rdma_verbs_device *d, struct rdma_cm_event *ev) {
    struct rdma_verbs_cm *cm = d->cm;
    struct rdma_cm_id *id = ev->id;
    const struct rdma_verbs_cm_req *req = ev->param.conn.private_data;
    if (ev->param.conn.private_data_len < sizeof(*req) || !req ||
        req->magic != RDMA_VERBS_CM_MAGIC || id->verbs != d->ctx) {
        rdma_reject(id, NULL, 0);
        return;
    }
    pthread_mutex_lock(&cm->lock);
    struct rdma_verbs_qp *q = cm_find(cm, req->responder);
    if (q) pthread_mutex_lock(&q->lock);
    pthread_mutex_unlock(&cm->lock);
    if (!q) { rdma_reject(id, NULL, 0); return; }
    int ok = !q->stopped && !q->qp && q->expected && q->expected == req->initiator;
    if (ok) ok = !cm_create_qp(q, id);
    if (ok) {
        struct rdma_verbs_cm_req rep = { RDMA_VERBS_CM_MAGIC, req->initiator, req->responder };
        struct rdma_conn_param p;
        cm_params(&p, &rep);
        if (rdma_accept(id, &p)) {
            rdma_destroy_qp(id);
            q->qp = NULL; q->id = NULL;
            ok = 0;
        }
    }
    pthread_mutex_unlock(&q->lock);
    if (!ok) rdma_reject(id, NULL, 0);
}

/* Event thread: accept requests; flush QPs whose peer disconnected. */
static void *cm_thread(void *arg) {
    struct rdma_verbs_device *d = arg;
    for (;;) {
        struct rdma_cm_event *ev;
        if (rdma_get_cm_event(d->cm->channel, &ev)) {
            if (errno == EINTR) continue;
            return NULL;
        }
        switch (ev->event) {
        case RDMA_CM_EVENT_CONNECT_REQUEST:
            cm_request(d, ev);
            break;
        case RDMA_CM_EVENT_DISCONNECTED: {
            /* Flush the responder QP so its link breaks promptly. Look it up
             * under the locks: the I/O thread may be stopping it. */
            struct rdma_verbs_cm *cm = d->cm;
            pthread_mutex_lock(&cm->lock);
            for (size_t i = 0; i < cm->nqps; ++i) {
                struct rdma_verbs_qp *q = cm->qps[i];
                pthread_mutex_lock(&q->lock);
                if (q->id == ev->id && q->qp) {
                    struct ibv_qp_attr a = { .qp_state = IBV_QPS_ERR };
                    (void)ibv_modify_qp(q->qp, &a, IBV_QP_STATE);
                }
                pthread_mutex_unlock(&q->lock);
            }
            pthread_mutex_unlock(&cm->lock);
            break;
        }
        default:
            break;
        }
        rdma_ack_cm_event(ev);
    }
}
#endif

/* Open a device by name and allocate its protection domain. NULL means clean
 * failure. RDMA_VERBS_OPEN_CLEANUP_LEAK means a context leaked; it is not a
 * handle and must not be passed to close or any other adapter function.
 * Registration-mode devices use the connection manager's context, so its
 * QPs can share the PD, CQs, and MRs. */
void *rdma_verbs_open(const char *name) {
    int n = 0;
    int leaked = 0;
#ifndef RDMA_VERBS_NO_REGISTRATION
    {
        int m = 0;
        struct ibv_context **cm_list = rdma_get_devices(&m);
        for (int i = 0; cm_list && i < m; ++i) {
            if (strcmp(name, ibv_get_device_name(cm_list[i]->device))) continue;
            struct ibv_device_attr attr;
            if (ibv_query_device(cm_list[i], &attr) ||
                (attr.device_cap_flags & IBV_DEVICE_MEM_WINDOW_TYPE_2B)) break;
            struct rdma_verbs_device *d = calloc(1, sizeof(*d));
            if (!d) break;
            d->ctx = cm_list[i]; d->cm_list = cm_list; d->registration = 1;
            d->pd = ibv_alloc_pd(d->ctx);
            if (d->pd && !cm_start(d, 1)) return d;
            if (d->pd && ibv_dealloc_pd(d->pd)) return RDMA_VERBS_OPEN_CLEANUP_LEAK;
            free(d);
            rdma_free_devices(cm_list);
            return NULL;
        }
        if (cm_list) rdma_free_devices(cm_list);
    }
#endif
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
#ifndef RDMA_VERBS_NO_REGISTRATION
    if (d->registration) {
        cm_stop(d);
        if (d->pd) { int rc = ibv_dealloc_pd(d->pd); if (rc) return rc; d->pd = NULL; }
        rdma_free_devices(d->cm_list);
        free(d);
        return 0;
    }
#endif
    if (d->pd) { int rc = ibv_dealloc_pd(d->pd); if (rc) return rc; d->pd = NULL; }
    int rc = ibv_close_device(d->ctx);
    if (!rc) free(d);
    return rc;
}

/* Create a reliable-connected QP with its own CQ, in the INIT state. The CQ
 * holds send_depth + recv_depth entries; both depths must be nonzero.
 * On failure, *qpn = UINT32_MAX means cleanup failed and something leaked;
 * Rust then leaks the device too.
 * In registration mode only the CQ exists until the connection manager
 * connects; *qpn is a random 24-bit token that names this QP in the setup
 * exchange and the connection request. */
void *rdma_verbs_qp(struct rdma_verbs_device *d, uint8_t port, uint32_t send_depth,
                    uint32_t recv_depth, uint32_t *qpn) {
    if (!send_depth || !recv_depth || send_depth > INT_MAX - recv_depth) return NULL;
    struct rdma_verbs_qp *q = calloc(1, sizeof(*q));
    if (!q) return NULL;
    q->cq = ibv_create_cq(d->ctx, (int)(send_depth + recv_depth), NULL, NULL, 0);
    if (!q->cq) { free(q); return NULL; }
#ifndef RDMA_VERBS_NO_REGISTRATION
    if (d->registration) {
        pthread_mutex_init(&q->lock, NULL);
        q->dev = d; q->send_depth = send_depth; q->recv_depth = recv_depth;
        q->recvs = calloc(recv_depth, sizeof(*q->recvs));
        if (!q->recvs) goto token_fail;
        pthread_mutex_lock(&d->cm->lock);
        do {
            uint32_t t = 0;
            if (getentropy(&t, sizeof(t))) { pthread_mutex_unlock(&d->cm->lock); goto token_fail; }
            q->token = t & 0xffffff;
        } while (!q->token || cm_find(d->cm, q->token));
        pthread_mutex_unlock(&d->cm->lock);
        if (cm_add(d->cm, q)) goto token_fail;
        *qpn = q->token;
        return q;
    token_fail:
        free(q->recvs);
        if (ibv_destroy_cq(q->cq)) *qpn = UINT32_MAX;
        free(q);
        return NULL;
    }
#endif
    struct ibv_qp_init_attr init = {0};
    init.send_cq = q->cq; init.recv_cq = q->cq; init.qp_type = IBV_QPT_RC;
    init.cap.max_send_wr = send_depth; init.cap.max_recv_wr = recv_depth;
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

#ifndef RDMA_VERBS_NO_REGISTRATION
/* Initiator side of registration mode: resolve the responder's IPv4 address
 * (from its IPv4-mapped GID), create the QP, and connect with both tokens.
 * Blocks until the connection is established or fails; the responder's
 * event thread answers without waiting on its I/O thread. */
static int cm_connect(struct rdma_verbs_qp *q, const struct rdma_verbs_endpoint *remote) {
    static const uint8_t prefix[12] = {0,0,0,0,0,0,0,0,0,0,0xff,0xff};
    if (memcmp(remote->gid, prefix, sizeof(prefix))) return EINVAL;
    struct sockaddr_in dst = {0};
    dst.sin_family = AF_INET; dst.sin_port = htons(cm_port());
    memcpy(&dst.sin_addr, remote->gid + 12, 4);
    struct rdma_cm_id *id = NULL;
    /* A NULL channel makes every call below synchronous. */
    if (rdma_create_id(NULL, &id, NULL, RDMA_PS_TCP)) return errno ? errno : EIO;
    struct sockaddr_in src = q->dev->cm->local;
    src.sin_port = 0;
    int rc = rdma_resolve_addr(id, (struct sockaddr *)&src, (struct sockaddr *)&dst, 2000);
    if (!rc) rc = rdma_resolve_route(id, 2000);
    if (!rc && id->verbs != q->dev->ctx) rc = EINVAL;
    pthread_mutex_lock(&q->lock);
    if (!rc && (q->stopped || q->qp)) rc = EINVAL;
    if (!rc) rc = cm_create_qp(q, id) ? EIO : 0;
    pthread_mutex_unlock(&q->lock);
    if (!rc) {
        struct rdma_verbs_cm_req req = { RDMA_VERBS_CM_MAGIC, q->token, remote->qpn };
        struct rdma_conn_param p;
        cm_params(&p, &req);
        rc = rdma_connect(id, &p) ? (errno ? errno : EIO) : 0;
    }
    if (rc) {
        pthread_mutex_lock(&q->lock);
        if (q->id == id) { rdma_destroy_qp(id); q->qp = NULL; q->id = NULL; }
        pthread_mutex_unlock(&q->lock);
        rdma_destroy_id(id);
        return rc;
    }
    return 0;
}
#endif

/* Move the QP to RTR then RTS, pointed at `remote`. Uses the smaller MTU.
 * rnr_retry is 0: a send or write with immediate that finds no posted
 * receive fails with IBV_WC_RNR_RETRY_EXC_ERR instead of stalling.
 * In registration mode the initiator (`initiator` nonzero) connects through
 * the connection manager and returns once connected; the responder records
 * the initiator's token and returns at once, and its event thread accepts
 * the initiator's request. */
int rdma_verbs_connect(struct rdma_verbs_qp *q, const struct rdma_verbs_endpoint *local,
                       const struct rdma_verbs_endpoint *remote, int initiator) {
#ifndef RDMA_VERBS_NO_REGISTRATION
    if (q->dev) {
        if (!remote->qpn || remote->qpn > 0xffffff) return EINVAL;
        if (initiator) return cm_connect(q, remote);
        pthread_mutex_lock(&q->lock);
        int rc = q->stopped || q->expected ? EINVAL : 0;
        if (!rc) q->expected = remote->qpn;
        pthread_mutex_unlock(&q->lock);
        return rc;
    }
#endif
    (void)initiator;
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

#ifndef RDMA_VERBS_NO_REGISTRATION
/* Deregister every write-source MR of `q`. Only once the QP is destroyed. */
static int dyn_release_all(struct rdma_verbs_qp *q) {
    while (q->dyn) {
        struct rdma_verbs_dyn *e = q->dyn;
        for (int i = 0; i < 2; ++i)
            if (e->mr[i]) { if (ibv_dereg_mr(e->mr[i])) return EBUSY; e->mr[i] = NULL; }
        q->dyn = e->next;
        free(e);
    }
    return 0;
}
#endif

/* Move the QP to the error state and destroy it. Once this succeeds, no more
 * DMA can touch local or remote memory through it. Safe to call twice. */
int rdma_verbs_stop(struct rdma_verbs_qp *q) {
#ifndef RDMA_VERBS_NO_REGISTRATION
    if (q->dev) {
        pthread_mutex_lock(&q->lock);
        q->stopped = 1;
        struct rdma_cm_id *id = q->id;
        if (q->qp) {
            struct ibv_qp_attr a = { .qp_state = IBV_QPS_ERR };
            (void)ibv_modify_qp(q->qp, &a, IBV_QP_STATE);
            rdma_destroy_qp(id);
            q->qp = NULL;
        }
        q->id = NULL;
        int rc = dyn_release_all(q);
        pthread_mutex_unlock(&q->lock);
        if (id) {
            /* Initiator ids are synchronous; responder ids belong to the
             * listener's channel and are destroyed by the event thread's
             * acknowledgment order. Either way the QP is gone. */
            rdma_destroy_id(id);
        }
        return rc;
    }
#endif
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
#ifndef RDMA_VERBS_NO_REGISTRATION
    if (q->dev) {
        cm_remove(q->dev->cm, q);
        while (q->synth) { struct rdma_verbs_synth *s = q->synth; q->synth = s->next; free(s); }
    }
#endif
    rc = ibv_destroy_cq(q->cq);
    if (!rc) {
#ifndef RDMA_VERBS_NO_REGISTRATION
        if (q->dev) { free(q->recvs); pthread_mutex_destroy(&q->lock); }
#endif
        free(q);
    }
    return rc;
}

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

/* Allocate a zeroed, page-aligned buffer and register it. `access` is exactly
 * one of RDMA_VERBS_MR_PAGES or RDMA_VERBS_MR_CONTROL, optionally with
 * RDMA_VERBS_MR_REMOTE_WRITE. In registration mode page buffers get no
 * MW_BIND (there are no windows); grants register them for remote write. */
void *rdma_verbs_register(struct rdma_verbs_device *d, uint64_t length, uint32_t access) {
    int flags = IBV_ACCESS_LOCAL_WRITE;
    switch (access & ~(uint32_t)RDMA_VERBS_MR_REMOTE_WRITE) {
    case RDMA_VERBS_MR_PAGES: if (!d->registration) flags |= IBV_ACCESS_MW_BIND; break;
    case RDMA_VERBS_MR_CONTROL: break;
    default: return NULL;
    }
    if (access & RDMA_VERBS_MR_REMOTE_WRITE) flags |= IBV_ACCESS_REMOTE_WRITE;
    if (!length || length > SIZE_MAX) return NULL;
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
 * reach the memory: rdma_verbs_bytes returns NULL, and send, write, and
 * receive posts reject the region. Needs rdma-core 34 or later.
 * Unsupported in registration mode. */
void *rdma_verbs_register_dmabuf(struct rdma_verbs_device *d, uint64_t offset, uint64_t length,
                                 uint64_t iova, int fd) {
    if (!length || length > SIZE_MAX || fd < 0 || d->registration) return NULL;
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
 * registration. Returns NULL if the device lacks implicit ODP for RC writes.
 * `bytes` is NULL and `length` SIZE_MAX, so an `offset` is a virtual address.
 * Local read only; the region is never a receive target or window.
 * In registration mode there is no MR: writes through this region register
 * their source memory for the duration of the write instead (still zero copy). */
void *rdma_verbs_register_implicit(struct rdma_verbs_device *d) {
    if (d->registration) {
        struct rdma_verbs_mr *m = calloc(1, sizeof(*m));
        if (!m) return NULL;
        m->length = SIZE_MAX; m->dynamic = 1;
        return m;
    }
    struct ibv_device_attr_ex attr;
    memset(&attr, 0, sizeof(attr));
    if (ibv_query_device_ex(d->ctx, NULL, &attr)) return NULL;
    if (!(attr.odp_caps.general_caps & IBV_ODP_SUPPORT) ||
        !(attr.odp_caps.general_caps & IBV_ODP_SUPPORT_IMPLICIT) ||
        !(attr.odp_caps.per_transport_caps.rc_odp_caps & IBV_ODP_SUPPORT_WRITE)) return NULL;
    struct rdma_verbs_mr *m = calloc(1, sizeof(*m));
    if (!m) return NULL;
    m->mr = ibv_reg_mr(d->pd, NULL, SIZE_MAX, IBV_ACCESS_ON_DEMAND);
    /* Some providers require local write on any ODP MR. */
    if (!m->mr) m->mr = ibv_reg_mr(d->pd, NULL, SIZE_MAX, IBV_ACCESS_ON_DEMAND | IBV_ACCESS_LOCAL_WRITE);
    if (!m->mr) { free(m); return NULL; }
    m->bytes = NULL; m->length = SIZE_MAX;
    return m;
}
/* The buffer behind a registered region. NULL for an implicit or dma-buf region. */
void *rdma_verbs_bytes(struct rdma_verbs_mr *m) { return m->bytes; }
/* Allocate a window. Window mode: a type 2 memory window; writes the key to
 * use for its next bind. Registration mode: a holder for one grant's MR;
 * *key is 0 because the key comes from each bind's registration. */
void *rdma_verbs_window(struct rdma_verbs_device *d, uint32_t *key) {
    struct rdma_verbs_window *w = calloc(1, sizeof(*w));
    if (!w) return NULL;
    w->dev = d;
    if (d->registration) { *key = 0; return w; }
    w->mw = ibv_alloc_mw(d->pd, IBV_MW_TYPE_2);
    if (!w->mw) { free(w); return NULL; }
    *key = ibv_inc_rkey(w->mw->rkey);
    return w;
}
/* Free a window, deregistering a live grant's MR in registration mode. */
int rdma_verbs_window_free(struct rdma_verbs_window *w) {
    int rc = 0;
    if (w->mw) rc = ibv_dealloc_mw(w->mw);
    else if (w->mr) { rc = ibv_dereg_mr(w->mr); if (!rc) w->mr = NULL; }
    if (!rc) free(w);
    return rc;
}
/* True if `m` has CPU-reachable or implicit memory a post may use as an SGE. */
static int host_sge(const struct rdma_verbs_mr *m) { return !m->device; }
/* True if [offset, offset + length) is a nonempty range inside `m`. */
static int in_region(const struct rdma_verbs_mr *m, uint64_t offset, uint32_t length) {
    return length && offset <= m->length && length <= m->length - offset;
}

#ifndef RDMA_VERBS_NO_REGISTRATION
/* Queue a completion the adapter produced itself; poll returns it first. */
static void synth_push(struct rdma_verbs_qp *q, uint64_t id, uint32_t opcode) {
    struct rdma_verbs_synth *s = calloc(1, sizeof(*s));
    if (!s) return;
    s->id = id; s->opcode = opcode;
    pthread_mutex_lock(&q->lock);
    if (q->synth_tail) q->synth_tail->next = s; else q->synth = s;
    q->synth_tail = s;
    pthread_mutex_unlock(&q->lock);
}
#endif

/* Post a bind: let the peer write `length` bytes of `m` at `offset` via `key`.
 * IBV_SEND_FENCE holds the bind until earlier work on the QP completes. On
 * mlx5, a bind posted behind an RDMA write whose implicit ODP source page
 * faults can fail with IBV_WC_MW_BIND_ERR (vendor_err 0x78) and break the
 * link; this happens when a link both writes zero-copy and grants.
 * `*key` is the key to use in window mode and is left unchanged; in
 * registration mode the grant registers the buffer for remote write and
 * `*key` becomes that registration's rkey. The registration starts at the
 * buffer and covers `length` rounded up to whole 2 MiB pages, never past
 * the region, so callers must space buffers at 2 MiB multiples (Racer's
 * BUFFER_LEN). It completes at once with opcode IBV_WC_BIND_MW. */
int rdma_verbs_bind(struct rdma_verbs_qp *q, struct rdma_verbs_window *w, struct rdma_verbs_mr *m,
                    uint32_t *key, uint64_t id, uint64_t offset, uint32_t length) {
    if (!in_region(m, offset, length)) return EINVAL;
#ifndef RDMA_VERBS_NO_REGISTRATION
    if (q->dev) {
        if (!m->bytes || w->mw) return EINVAL;
        if (w->mr) { if (ibv_dereg_mr(w->mr)) return EBUSY; w->mr = NULL; }
        uint64_t span = length;
        if ((m->addr + offset) % RDMA_VERBS_HUGE == 0) {
            span = ((uint64_t)length + RDMA_VERBS_HUGE - 1) & ~(uint64_t)(RDMA_VERBS_HUGE - 1);
            if (span > m->length - offset) span = length;
        }
        w->mr = ibv_reg_mr(q->dev->pd, (char *)m->bytes + offset, (size_t)span,
                           IBV_ACCESS_LOCAL_WRITE | IBV_ACCESS_REMOTE_WRITE);
        if (!w->mr) return errno ? errno : ENOMEM;
        *key = w->mr->rkey;
        synth_push(q, id, IBV_WC_BIND_MW);
        return 0;
    }
#endif
    struct ibv_send_wr wr = {0}, *bad = NULL;
    wr.wr_id = id; wr.opcode = IBV_WR_BIND_MW;
    wr.send_flags = IBV_SEND_SIGNALED | IBV_SEND_FENCE;
    wr.bind_mw.mw = w->mw; wr.bind_mw.rkey = *key;
    wr.bind_mw.bind_info.mr = m->mr;
    wr.bind_mw.bind_info.addr = m->addr + offset;
    wr.bind_mw.bind_info.length = length;
    wr.bind_mw.bind_info.mw_access_flags = IBV_ACCESS_REMOTE_WRITE;
    return ibv_post_send(q->qp, &wr, &bad);
}
/* Post a signaled LOCAL_INV of `key` on window `w`. Not fenced. In
 * registration mode, deregister the grant's MR so the key stops working, and
 * complete at once with opcode IBV_WC_LOCAL_INV. */
int rdma_verbs_invalidate(struct rdma_verbs_qp *q, struct rdma_verbs_window *w, uint32_t key,
                          uint64_t id) {
#ifndef RDMA_VERBS_NO_REGISTRATION
    if (q->dev) {
        if (!w->mr || w->mr->rkey != key) return EINVAL;
        if (ibv_dereg_mr(w->mr)) return EBUSY;
        w->mr = NULL;
        synth_push(q, id, IBV_WC_LOCAL_INV);
        return 0;
    }
#endif
    (void)w;
    struct ibv_send_wr wr = {0}, *bad = NULL;
    wr.wr_id = id; wr.opcode = IBV_WR_LOCAL_INV;
    wr.send_flags = IBV_SEND_SIGNALED; wr.invalidate_rkey = key;
    return ibv_post_send(q->qp, &wr, &bad);
}

#ifndef RDMA_VERBS_NO_REGISTRATION
/* Registration-mode write: register [addr, addr + length) for local access
 * (a 2 MiB aligned head and a small tail, so the huge-page part registers
 * fast), post it as RDMA writes of at most the port's message size straight
 * from that memory, the last with immediate, and deregister when the write
 * completes. */
static int dyn_write_imm(struct rdma_verbs_qp *q, uint64_t addr, uint32_t length,
                         uint64_t address, uint32_t key, uint32_t imm, uint64_t id) {
    struct rdma_verbs_dyn *e = calloc(1, sizeof(*e));
    if (!e) return ENOMEM;
    e->id = id;
    uint64_t head = (addr % RDMA_VERBS_HUGE == 0) ? (length & ~(uint64_t)(RDMA_VERBS_HUGE - 1)) : 0;
    uint64_t tail = length - head;
    int access = IBV_ACCESS_LOCAL_WRITE;
    if (head && !(e->mr[0] = ibv_reg_mr(q->dev->pd, (void *)(uintptr_t)addr, head, access))) goto fail;
    if (tail && !(e->mr[1] = ibv_reg_mr(q->dev->pd, (void *)(uintptr_t)(addr + head), tail, access)))
        goto fail;
    uint64_t max = q->dev->max_msg ? q->dev->max_msg : length;
    pthread_mutex_lock(&q->lock);
    if (!q->qp) { pthread_mutex_unlock(&q->lock); goto fail; }
    for (uint64_t off = 0; off < length;) {
        uint64_t chunk = length - off < max ? length - off : max;
        int last = off + chunk == length;
        struct ibv_sge sge[2];
        int n = 0;
        uint64_t s = off, end = off + chunk;
        while (s < end) {
            int part = s >= head;
            uint64_t stop = part ? end : (end < head ? end : head);
            struct ibv_mr *mr = e->mr[part];
            sge[n].addr = addr + s; sge[n].length = (uint32_t)(stop - s); sge[n].lkey = mr->lkey;
            ++n; s = stop;
        }
        struct ibv_send_wr wr = {0}, *bad = NULL;
        wr.wr_id = last ? id : 0;
        wr.opcode = last ? IBV_WR_RDMA_WRITE_WITH_IMM : IBV_WR_RDMA_WRITE;
        if (last) { wr.imm_data = htonl(imm); wr.send_flags = IBV_SEND_SIGNALED; }
        wr.sg_list = sge; wr.num_sge = n;
        wr.wr.rdma.remote_addr = address + off; wr.wr.rdma.rkey = key;
        int rc = ibv_post_send(q->qp, &wr, &bad);
        if (rc) {
            /* Earlier chunks may already be posted: keep the MRs until stop. */
            e->id = UINT64_MAX; e->next = q->dyn; q->dyn = e;
            pthread_mutex_unlock(&q->lock);
            return rc;
        }
        off = end;
    }
    e->next = q->dyn; q->dyn = e;
    pthread_mutex_unlock(&q->lock);
    return 0;
fail:
    for (int i = 0; i < 2; ++i) if (e->mr[i]) (void)ibv_dereg_mr(e->mr[i]);
    free(e);
    return errno ? errno : EIO;
}

/* Release a completed write's source registrations. Caller holds q->lock. */
static void dyn_complete(struct rdma_verbs_qp *q, uint64_t id) {
    for (struct rdma_verbs_dyn **p = &q->dyn; *p; p = &(*p)->next) {
        if ((*p)->id != id) continue;
        struct rdma_verbs_dyn *e = *p;
        int held = 0;
        for (int i = 0; i < 2; ++i)
            if (e->mr[i]) { if (ibv_dereg_mr(e->mr[i])) held = 1; else e->mr[i] = NULL; }
        if (held) { e->id = UINT64_MAX; return; } /* retried at stop */
        *p = e->next;
        free(e);
        return;
    }
}
#endif

/* Post a signaled RDMA write with immediate of `length` bytes of `m` at
 * `offset` to the peer's `address`, using its window `key`. `imm` is in host
 * byte order; the peer's completion reports the same value. Consumes one of
 * the peer's posted receives. */
int rdma_verbs_write_imm(struct rdma_verbs_qp *q, struct rdma_verbs_mr *m, uint64_t offset,
                         uint32_t length, uint64_t address, uint32_t key, uint32_t imm,
                         uint64_t id) {
    if (!host_sge(m) || !in_region(m, offset, length)) return EINVAL;
#ifndef RDMA_VERBS_NO_REGISTRATION
    if (m->dynamic) {
        if (!q->dev) return EINVAL;
        return dyn_write_imm(q, offset, length, address, key, imm, id);
    }
    if (q->dev && !q->qp) return EAGAIN;
#endif
    struct ibv_sge sge = { .addr = (uintptr_t)m->bytes + offset, .length = length,
                           .lkey = m->mr->lkey };
    struct ibv_send_wr wr = {0}, *bad = NULL;
    wr.wr_id = id; wr.opcode = IBV_WR_RDMA_WRITE_WITH_IMM; wr.imm_data = htonl(imm);
    wr.send_flags = IBV_SEND_SIGNALED; wr.sg_list = &sge; wr.num_sge = 1;
    wr.wr.rdma.remote_addr = address; wr.wr.rdma.rkey = key;
    return ibv_post_send(q->qp, &wr, &bad);
}
/* Post a signaled send of `length` bytes of `m` at `offset`. Consumes one of
 * the peer's posted receives. */
int rdma_verbs_send(struct rdma_verbs_qp *q, struct rdma_verbs_mr *m, uint64_t offset,
                    uint32_t length, uint64_t id) {
    if (!host_sge(m) || !in_region(m, offset, length) || m->dynamic) return EINVAL;
#ifndef RDMA_VERBS_NO_REGISTRATION
    if (q->dev && !q->qp) return EAGAIN;
#endif
    struct ibv_sge sge = { .addr = (uintptr_t)m->bytes + offset, .length = length,
                           .lkey = m->mr->lkey };
    struct ibv_send_wr wr = {0}, *bad = NULL;
    wr.wr_id = id; wr.opcode = IBV_WR_SEND;
    wr.send_flags = IBV_SEND_SIGNALED; wr.sg_list = &sge; wr.num_sge = 1;
    return ibv_post_send(q->qp, &wr, &bad);
}
/* Post a receive into `length` bytes of `m` at `offset`. In registration
 * mode before the connection exists, queue it; it is posted when the QP is
 * created, before the connection is accepted or requested. */
int rdma_verbs_post_recv(struct rdma_verbs_qp *q, struct rdma_verbs_mr *m, uint64_t offset,
                         uint32_t length, uint64_t id) {
    if (!host_sge(m) || !in_region(m, offset, length) || m->dynamic) return EINVAL;
#ifndef RDMA_VERBS_NO_REGISTRATION
    if (q->dev) {
        pthread_mutex_lock(&q->lock);
        int rc = 0;
        if (q->stopped) rc = EINVAL;
        else if (!q->qp) {
            if (q->nrecvs >= q->recv_depth) rc = ENOMEM;
            else q->recvs[q->nrecvs++] = (struct rdma_verbs_queued_recv){
                id, (uintptr_t)m->bytes + offset, length, m->mr->lkey };
        } else {
            struct ibv_sge sge = { .addr = (uintptr_t)m->bytes + offset, .length = length,
                                   .lkey = m->mr->lkey };
            struct ibv_recv_wr wr = {0}, *bad = NULL;
            wr.wr_id = id; wr.sg_list = &sge; wr.num_sge = 1;
            rc = ibv_post_recv(q->qp, &wr, &bad);
        }
        pthread_mutex_unlock(&q->lock);
        return rc;
    }
#endif
    struct ibv_sge sge = { .addr = (uintptr_t)m->bytes + offset, .length = length,
                           .lkey = m->mr->lkey };
    struct ibv_recv_wr wr = {0}, *bad = NULL;
    wr.wr_id = id; wr.sg_list = &sge; wr.num_sge = 1;
    return ibv_post_recv(q->qp, &wr, &bad);
}
/* Read up to 32 completions. Returns the count, or negative on error.
 * Registration mode returns the adapter's own bind and invalidate
 * completions first, hides unsignaled write chunks, and releases a write's
 * source registrations when its completion arrives. */
int rdma_verbs_poll(struct rdma_verbs_qp *q, struct rdma_verbs_wc *out, uint32_t capacity) {
    struct ibv_wc wc[32];
    if (capacity > 32) capacity = 32;
    int n = 0;
#ifndef RDMA_VERBS_NO_REGISTRATION
    if (q->dev) {
        pthread_mutex_lock(&q->lock);
        while (q->synth && (uint32_t)n < capacity) {
            struct rdma_verbs_synth *s = q->synth;
            q->synth = s->next;
            if (!q->synth) q->synth_tail = NULL;
            memset(&out[n], 0, sizeof(out[n]));
            out[n].id = s->id; out[n].status = IBV_WC_SUCCESS; out[n].opcode = s->opcode;
            ++n;
            free(s);
        }
        pthread_mutex_unlock(&q->lock);
        if ((uint32_t)n == capacity) return n;
    }
#endif
    int got = ibv_poll_cq(q->cq, (int)(capacity - (uint32_t)n), wc);
    if (got < 0) return n ? n : got;
    for (int i = 0; i < got; ++i) {
#ifndef RDMA_VERBS_NO_REGISTRATION
        if (q->dev) {
            pthread_mutex_lock(&q->lock);
            if (wc[i].opcode == IBV_WC_RDMA_WRITE || wc[i].status != IBV_WC_SUCCESS)
                dyn_complete(q, wc[i].wr_id);
            pthread_mutex_unlock(&q->lock);
            /* A successful unsignaled chunk never reports; one that failed
             * reports wr_id 0 and breaks the link like any failure. */
        }
#endif
        struct rdma_verbs_wc *o = &out[n++];
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
    return n;
}

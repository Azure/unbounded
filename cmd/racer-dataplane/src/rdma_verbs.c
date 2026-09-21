/* ABI firewall for rdma-core's provider-dispatched inline verbs. All storage
 * ownership, protocol, scheduling and error policy lives in rdma.rs. */
#define _GNU_SOURCE
#include <infiniband/verbs.h>
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>
#include <stdatomic.h>

struct racer_rail {
    char name[64];
    uint8_t gid[16];
    uint32_t max_read;
    uint16_t lid;
    uint8_t port, gid_index, mtu, ethernet, windows, pad;
};
struct racer_endpoint {
    uint8_t gid[16];
    uint32_t qpn, psn;
    uint16_t lid;
    uint8_t mtu, ethernet;
};
struct racer_wc { uint64_t id; uint32_t status, opcode, len, qpn; };
struct destroy_job;
struct racer_device {
    struct ibv_context *ctx;
    struct ibv_pd *pd;
    struct ibv_comp_channel *channel;
    struct ibv_cq *cq;
    struct ibv_mr *pool, *control;
    struct destroy_job *job;
    int closing;
    int async_ready, channel_ready;
};
static int destroy_object(struct racer_device *d, void *object, int operation, size_t count);

static int nonblock(int fd) {
    int flags = fcntl(fd, F_GETFL);
    if (flags < 0 || fcntl(fd, F_SETFL, flags | O_NONBLOCK) < 0) return errno;
    return 0;
}

int racer_discover(struct racer_rail *out, int capacity) {
    int n, used = 0;
    struct ibv_device **list = ibv_get_device_list(&n);
    if (!list) return -errno;
    for (int i = 0; i < n; ++i) {
        struct ibv_context *ctx = ibv_open_device(list[i]);
        if (!ctx) continue;
        struct ibv_device_attr dev;
        if (ibv_query_device(ctx, &dev)) { ibv_close_device(ctx); continue; }
        if (!(dev.device_cap_flags & IBV_DEVICE_MEM_WINDOW_TYPE_2B) ||
            !dev.max_qp_rd_atom || !dev.max_qp_init_rd_atom) { ibv_close_device(ctx); continue; }
        for (unsigned p = 1; p <= dev.phys_port_cnt; ++p) {
            struct ibv_port_attr port;
            if (ibv_query_port(ctx, p, &port) || port.state != IBV_PORT_ACTIVE ||
                port.max_msg_sz < 4194304) continue;
            struct racer_rail rail = {0};
            strncpy(rail.name, ibv_get_device_name(list[i]), sizeof(rail.name)-1);
            rail.port = p;
            rail.lid = port.lid;
            rail.mtu = port.active_mtu;
            rail.ethernet = port.link_layer == IBV_LINK_LAYER_ETHERNET;
            rail.windows = !!(dev.device_cap_flags & IBV_DEVICE_MEM_WINDOW_TYPE_2B);
            rail.max_read = dev.max_qp_rd_atom < dev.max_qp_init_rd_atom ?
                dev.max_qp_rd_atom : dev.max_qp_init_rd_atom;
            for (int g = 0; g < port.gid_tbl_len && g < 256; ++g) {
                struct ibv_gid_entry entry;
                if (ibv_query_gid_ex(ctx, p, g, &entry, 0)) continue;
                uint8_t zero[16] = {0};
                if (!memcmp(entry.gid.raw, zero, 16)) continue;
                if (rail.ethernet && entry.gid_type != IBV_GID_TYPE_ROCE_V2) continue;
                memcpy(rail.gid, entry.gid.raw, 16);
                rail.gid_index = g;
                if (used == capacity) { ibv_close_device(ctx); ibv_free_device_list(list); return -ENOSPC; }
                out[used++] = rail;
            }
        }
        ibv_close_device(ctx);
    }
    ibv_free_device_list(list);
    return used;
}

/* Retryable, dependency-ordered destruction. On failure Rust keeps both memory
 * allocations and this object alive. */
int racer_close(struct racer_device *d) {
    int e;
    if (!d->closing) {
        if ((e = destroy_object(d, d, 1, 0))) return e;
        /* CQ destruction has completed and its helper has been joined. Stop
         * consuming events BEFORE a new helper may close/free their context. */
        d->closing = 1;
    }
    if ((e = destroy_object(d, d, 2, 0))) return e;
    free(d);
    return 0;
}

/* Return a partial owner even on failure; Rust must run racer_close. */
int racer_open(const char *name, void *pool, size_t pool_len, void *control,
               size_t control_len, int cqe, struct racer_device **out) {
    struct racer_device *d = calloc(1, sizeof(*d));
    if (!d) return ENOMEM;
    *out = d;
    int n;
    struct ibv_device **list = ibv_get_device_list(&n);
    if (!list) return errno;
    for (int i = 0; i < n; ++i) {
        if (!strcmp(name, ibv_get_device_name(list[i]))) { d->ctx = ibv_open_device(list[i]); break; }
    }
    ibv_free_device_list(list);
    if (!d->ctx) return ENODEV;
    struct ibv_device_attr attr;
    int e = ibv_query_device(d->ctx, &attr);
    if (e) return e;
    if (!(attr.device_cap_flags & IBV_DEVICE_MEM_WINDOW_TYPE_2B)) return EOPNOTSUPP;
    if ((e = nonblock(d->ctx->async_fd))) return e;
    d->async_ready = 1;
    if (!(d->pd = ibv_alloc_pd(d->ctx))) return errno;
    if (!(d->channel = ibv_create_comp_channel(d->ctx))) return errno;
    if ((e = nonblock(d->channel->fd))) return e;
    d->channel_ready = 1;
    if (!(d->cq = ibv_create_cq(d->ctx, cqe, NULL, d->channel, 0))) return errno;
    /* The backing rkey is never exported. Remote writes are never permitted. */
    if (!(d->pool = ibv_reg_mr(d->pd, pool, pool_len,
          IBV_ACCESS_LOCAL_WRITE | IBV_ACCESS_REMOTE_READ | IBV_ACCESS_MW_BIND))) return errno;
    if (!(d->control = ibv_reg_mr(d->pd, control, control_len, IBV_ACCESS_LOCAL_WRITE))) return errno;
    return 0;
}
int racer_fd(struct racer_device *d, int async) { return async ? d->ctx->async_fd : d->channel->fd; }
int racer_notify(struct racer_device *d) { return ibv_req_notify_cq(d->cq, 0); }

/* One event per call; acknowledge immediately, including unexpected events. */
int racer_event(struct racer_device *d, int async, uint32_t *qpn) {
    *qpn = 0;
    if (d->closing || (async ? !d->async_ready : !d->channel_ready)) return -EAGAIN;
    if (!async) {
        struct ibv_cq *cq; void *context;
        if (ibv_get_cq_event(d->channel, &cq, &context)) return -errno;
        int match = cq == d->cq;
        ibv_ack_cq_events(cq, 1);
        return match ? 1 : 2;
    }
    struct ibv_async_event event;
    if (ibv_get_async_event(d->ctx, &event)) return -errno;
    int fatal = 1;
    switch (event.event_type) {
    case IBV_EVENT_QP_FATAL: case IBV_EVENT_QP_REQ_ERR: case IBV_EVENT_QP_ACCESS_ERR:
    case IBV_EVENT_PATH_MIG_ERR: *qpn = event.element.qp->qp_num; break;
    case IBV_EVENT_COMM_EST: case IBV_EVENT_SQ_DRAINED: case IBV_EVENT_QP_LAST_WQE_REACHED:
    case IBV_EVENT_PORT_ACTIVE: fatal = 0; break;
    default: break;
    }
    ibv_ack_async_event(&event);
    return fatal ? 2 : 1;
}
int racer_poll(struct racer_device *d, struct racer_wc *out, int capacity) {
    struct ibv_wc wc[32];
    if (capacity > 32) capacity = 32;
    int n = ibv_poll_cq(d->cq, capacity, wc);
    for (int i = 0; i < n; ++i) {
        out[i] = (struct racer_wc){ .id = wc[i].wr_id, .status = wc[i].status, .qpn = wc[i].qp_num };
        /* Only wr_id, status, qp_num, vendor_err are defined on errors. */
        if (wc[i].status == IBV_WC_SUCCESS) {
            out[i].len = wc[i].byte_len;
            switch (wc[i].opcode) {
            case IBV_WC_SEND: out[i].opcode = 1; break;
            case IBV_WC_RECV: out[i].opcode = 2; break;
            case IBV_WC_RDMA_READ: out[i].opcode = 3; break;
            case IBV_WC_BIND_MW: out[i].opcode = 4; break;
            case IBV_WC_LOCAL_INV: out[i].opcode = 5; break;
            default: out[i].opcode = 0;
            }
        }
    }
    return n;
}
struct ibv_qp *racer_qp(struct racer_device *d, uint32_t depth, const struct racer_rail *rail,
                        uint32_t psn, struct racer_endpoint *out) {
    struct ibv_qp_init_attr init = { .send_cq = d->cq, .recv_cq = d->cq,
        .qp_type = IBV_QPT_RC, .cap = { .max_send_wr = depth, .max_recv_wr = depth,
        .max_send_sge = 1, .max_recv_sge = 1 } };
    struct ibv_qp *qp = ibv_create_qp(d->pd, &init);
    if (!qp) return NULL;
    *out = (struct racer_endpoint){ .qpn = qp->qp_num, .psn = psn,
        .lid = rail->lid, .mtu = rail->mtu, .ethernet = rail->ethernet };
    memcpy(out->gid, rail->gid, 16);
    return qp;
}
int racer_init(struct ibv_qp *qp, uint8_t port) {
    struct ibv_qp_attr a = { .qp_state = IBV_QPS_INIT, .port_num = port,
        .pkey_index = 0, .qp_access_flags = IBV_ACCESS_REMOTE_READ };
    return ibv_modify_qp(qp, &a, IBV_QP_STATE | IBV_QP_PORT | IBV_QP_PKEY_INDEX | IBV_QP_ACCESS_FLAGS);
}
int racer_connect(struct ibv_qp *qp, const struct racer_rail *rail,
                  const struct racer_endpoint *peer, uint32_t psn, uint8_t reads) {
    struct ibv_qp_attr a = { .qp_state = IBV_QPS_RTR,
        .path_mtu = peer->mtu < rail->mtu ? peer->mtu : rail->mtu,
        .dest_qp_num = peer->qpn, .rq_psn = peer->psn,
        .max_dest_rd_atomic = reads, .min_rnr_timer = 12,
        .ah_attr = { .dlid = peer->lid, .port_num = rail->port, .is_global = 1,
            .grh = { .sgid_index = rail->gid_index, .hop_limit = 64 } } };
    memcpy(a.ah_attr.grh.dgid.raw, peer->gid, 16);
    int e = ibv_modify_qp(qp, &a, IBV_QP_STATE | IBV_QP_AV | IBV_QP_PATH_MTU |
        IBV_QP_DEST_QPN | IBV_QP_RQ_PSN | IBV_QP_MAX_DEST_RD_ATOMIC | IBV_QP_MIN_RNR_TIMER);
    if (e) return e;
    a = (struct ibv_qp_attr){ .qp_state = IBV_QPS_RTS, .sq_psn = psn,
        .timeout = 14, .retry_cnt = 3, .rnr_retry = 3, .max_rd_atomic = reads };
    return ibv_modify_qp(qp, &a, IBV_QP_STATE | IBV_QP_SQ_PSN | IBV_QP_TIMEOUT |
        IBV_QP_RETRY_CNT | IBV_QP_RNR_RETRY | IBV_QP_MAX_QP_RD_ATOMIC);
}
int racer_error(struct ibv_qp *qp) {
    struct ibv_qp_attr a = { .qp_state = IBV_QPS_ERR };
    return ibv_modify_qp(qp, &a, IBV_QP_STATE);
}
/* One job per device and at most 32 process-wide helper stacks (256 KiB each).
 * A blocked job holds its permit forever; admission never grows a retired list.
 * Only the reactor consumes/ACKs events. The helper never polls channels. */
static atomic_uint destroy_jobs;
struct destroy_job { void *object; size_t count; int operation; pthread_t thread; int result; };
static int close_stage(struct racer_device *d, int operation) {
    int e;
    if (operation == 1) {
        if (d->control) { if ((e = ibv_dereg_mr(d->control))) return e; d->control = NULL; }
        if (d->pool) { if ((e = ibv_dereg_mr(d->pool))) return e; d->pool = NULL; }
        /* The reactor only reads cq while this job runs. Do not clear it here. */
        if (d->cq && (e = ibv_destroy_cq(d->cq))) return e;
    } else {
        if (d->channel) { if ((e = ibv_destroy_comp_channel(d->channel))) return e; d->channel = NULL; }
        if (d->pd) { if ((e = ibv_dealloc_pd(d->pd))) return e; d->pd = NULL; }
        if (d->ctx) { if ((e = ibv_close_device(d->ctx))) return e; d->ctx = NULL; }
    }
    return 0;
}
static void *destroy(void *arg) {
    struct destroy_job *job = arg;
    if (!job->operation) {
        racer_error(job->object); /* ERR can block too; never run it on the reactor. */
        job->result = ibv_destroy_qp(job->object);
    } else if (job->operation == 3 || job->operation == 4) {
        /* The owner retains this fixed table and never reads/writes it until
         * join. Nulls preserve partial progress across provider errors. */
        void **objects = job->object;
        for (size_t i = 0; i < job->count; ++i) {
            if (!objects[i]) continue;
            if (job->operation == 4) {
                racer_error(objects[i]);
                job->result = ibv_destroy_qp(objects[i]);
            } else {
                job->result = ibv_dealloc_mw(objects[i]);
            }
            if (job->result) break;
            objects[i] = NULL;
        }
    } else {
        job->result = close_stage(job->object, job->operation);
    }
    return NULL;
}
static int destroy_object(struct racer_device *d, void *object, int operation, size_t count) {
    if (d->job) {
        struct destroy_job *job = d->job;
        if (job->object != object || job->operation != operation || job->count != count) return EAGAIN;
        int e = pthread_tryjoin_np(job->thread, NULL);
        if (e) return e == EBUSY ? EAGAIN : e;
        e = job->result;
        free(job);
        d->job = NULL;
        atomic_fetch_sub(&destroy_jobs, 1);
        if (!e && operation == 1) d->cq = NULL;
        return e;
    }
    unsigned n = atomic_load(&destroy_jobs);
    do {
        if (n >= 32) return EAGAIN;
    } while (!atomic_compare_exchange_weak(&destroy_jobs, &n, n + 1));
    struct destroy_job *job = calloc(1, sizeof(*job));
    int e = ENOMEM;
    if (job) {
        job->object = object;
        job->operation = operation;
        job->count = count;
        pthread_attr_t attr;
        e = pthread_attr_init(&attr);
        if (!e) {
            e = pthread_attr_setstacksize(&attr, 256 * 1024);
            if (!e) e = pthread_create(&job->thread, &attr, destroy, job);
            pthread_attr_destroy(&attr);
        }
        if (!e) { d->job = job; return EAGAIN; }
        free(job);
    }
    atomic_fetch_sub(&destroy_jobs, 1);
    return e;
}
int racer_destroy_qp(struct racer_device *d, struct ibv_qp *qp) {
    return destroy_object(d, qp, 0, 0);
}
/* Final shutdown can inherit a single-QP job from cancellation/event handling.
 * Join it before admitting the batch, without destroying that QP twice. */
int racer_destroy_qps(struct racer_device *d, void **qps, size_t count) {
    if (d->job && d->job->operation == 0) {
        void *qp = d->job->object;
        size_t i = 0;
        while (i < count && qps[i] != qp) ++i;
        if (i == count) return EINVAL;
        int e = destroy_object(d, qp, 0, 0);
        if (e) return e;
        qps[i] = NULL;
    }
    return destroy_object(d, qps, 4, count);
}
struct ibv_mw *racer_window(struct racer_device *d, uint32_t *key) {
    struct ibv_mw *mw = ibv_alloc_mw(d->pd, IBV_MW_TYPE_2);
    if (mw) *key = mw->rkey;
    return mw;
}
int racer_free_windows(struct racer_device *d, void **windows, size_t count) {
    return destroy_object(d, windows, 3, count);
}

/* A single WR per call: an error rejects the entire operation. */
int racer_post(struct racer_device *d, struct ibv_qp *qp, uint32_t op, uint64_t id,
               void *address, uint32_t len, uint64_t remote, uint32_t key,
               struct ibv_mw *mw) {
    struct ibv_sge sge = { .addr = (uintptr_t)address, .length = len,
        .lkey = op == 3 ? d->pool->lkey : d->control->lkey };
    if (op == 2) {
        struct ibv_recv_wr wr = { .wr_id = id, .sg_list = &sge, .num_sge = 1 }, *bad;
        return ibv_post_recv(qp, &wr, &bad);
    }
    struct ibv_send_wr wr = { .wr_id = id, .send_flags = IBV_SEND_SIGNALED }, *bad;
    switch (op) {
    case 1: wr.opcode = IBV_WR_SEND; wr.sg_list = &sge; wr.num_sge = 1; break;
    case 3: wr.opcode = IBV_WR_RDMA_READ; wr.sg_list = &sge; wr.num_sge = 1;
        wr.wr.rdma.remote_addr = remote; wr.wr.rdma.rkey = key; break;
    case 4: wr.opcode = IBV_WR_BIND_MW; wr.bind_mw.mw = mw; wr.bind_mw.rkey = key;
        wr.bind_mw.bind_info = (struct ibv_mw_bind_info){ .mr = d->pool,
            .addr = (uintptr_t)address, .length = len, .mw_access_flags = IBV_ACCESS_REMOTE_READ }; break;
    case 5: wr.opcode = IBV_WR_LOCAL_INV; wr.invalidate_rkey = key; break;
    default: return EINVAL;
    }
    return ibv_post_send(qp, &wr, &bad);
}

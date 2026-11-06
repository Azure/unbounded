/* Exercise production discovery and open with failing libibverbs calls. */
#include <infiniband/verbs.h>
#include <rdma/rdma_cma.h>
#include <arpa/inet.h>
#include <sys/eventfd.h>
#include <assert.h>
#include <fcntl.h>
#include <unistd.h>
#include <errno.h>
#include <stdlib.h>
#include <string.h>

static struct ibv_device devices[3];
static struct ibv_context contexts[3];
static struct ibv_pd pd;
static struct ibv_device *device_list[] = { &devices[0], &devices[1], &devices[2], NULL };
static unsigned mode, fail_close, opens, closes, lists_freed;
static unsigned allocations, deallocations, wrappers, channels;

/* Modes: query failure, unsupported, active, inactive, list error, open error,
 * empty list, capacity overflow, PD success, second PD failure, wrapper failure,
 * CQ creation succeeds. Close failure is a one-based call index. */
void rdma_test_reset(unsigned scenario, unsigned close_call) {
    mode = scenario; fail_close = close_call;
    opens = closes = lists_freed = 0;
    allocations = deallocations = wrappers = 0;
}

unsigned rdma_test_count(unsigned which) {
    switch (which) {
    case 0: return opens;
    case 1: return closes;
    case 2: return lists_freed;
    case 3: return allocations;
    case 4: return deallocations;
    case 6: return channels;
    default: return wrappers;
    }
}

static struct ibv_device **test_get_device_list(int *n) {
    if (mode == 4) { errno = EIO; return NULL; }
    *n = mode == 6 ? 0 : 3;
    return device_list;
}

static void test_free_device_list(struct ibv_device **list) {
    assert(list == device_list);
    ++lists_freed;
}

static struct ibv_context *test_open_device(struct ibv_device *device) {
    ++opens;
    return mode == 5 ? NULL : &contexts[device - devices];
}

static int test_close_device(struct ibv_context *ctx) {
    assert(ctx >= contexts && ctx < contexts + 3);
    return ++closes == fail_close ? EBUSY : 0;
}

static int test_query_device(struct ibv_context *ctx, struct ibv_device_attr *attr) {
    (void)ctx;
    memset(attr, 0, sizeof(*attr));
    attr->device_cap_flags = mode == 1 ? 0 : IBV_DEVICE_MEM_WINDOW_TYPE_2B;
    attr->phys_port_cnt = mode == 7 ? 65 : 1;
    return mode == 0 ? EIO : 0;
}

static int test_query_port(struct ibv_context *ctx, uint8_t port, struct ibv_port_attr *attr) {
    (void)ctx; (void)port;
    memset(attr, 0, sizeof(*attr));
    attr->state = mode == 3 ? IBV_PORT_DOWN : IBV_PORT_ACTIVE;
    attr->active_mtu = IBV_MTU_1024;
    attr->link_layer = IBV_LINK_LAYER_ETHERNET;
    return 0;
}

static int test_query_gid(struct ibv_context *ctx, uint8_t port, int index, union ibv_gid *gid) {
    (void)ctx; (void)port; (void)index;
    memset(gid, 1, sizeof(*gid));
    return 0;
}

static const char *test_get_device_name(struct ibv_device *device) {
    (void)device;
    return "discovery-test";
}

/* Original discovery scenarios still fail PD allocation on later opens. */
static struct ibv_pd *test_alloc_pd(struct ibv_context *ctx) {
    (void)ctx;
    ++allocations;
    return mode == 8 || (mode == 9 && allocations != 2) ? &pd : NULL;
}

static int test_dealloc_pd(struct ibv_pd *value) {
    assert(value == &pd);
    ++deallocations;
    return 0;
}

static void *test_calloc(size_t count, size_t size) {
    ++wrappers;
    return mode == 10 ? NULL : calloc(count, size);
}

/* Discovery's RC probe: CQ creation fails unless mode 11, so no QP. */
static struct ibv_cq fake_cq;
static struct ibv_cq *test_create_cq(struct ibv_context *ctx, int cqe, void *cq_context,
                                     struct ibv_comp_channel *ch, int vector) {
    (void)cq_context; (void)vector;
    if (mode != 11) return NULL;
    fake_cq.context = ctx; fake_cq.channel = ch; fake_cq.cqe = cqe;
    return &fake_cq;
}
static int test_destroy_cq(struct ibv_cq *cq) { return cq == &fake_cq ? 0 : EINVAL; }
static struct ibv_qp *test_create_qp(struct ibv_pd *value, struct ibv_qp_init_attr *init) {
    (void)value; (void)init;
    return NULL;
}
static int test_destroy_qp(struct ibv_qp *qp) { (void)qp; return 0; }

/* Capabilities: windows unless mode 1, implicit ODP for RC writes, and
 * fixed queue, CQ, and window limits. */
static int test_query_device_ex(struct ibv_context *ctx, const struct ibv_query_device_ex_input *in,
                                struct ibv_device_attr_ex *attr) {
    (void)ctx; (void)in;
    memset(attr, 0, sizeof(*attr));
    attr->orig_attr.device_cap_flags = mode == 1 ? 0 : IBV_DEVICE_MEM_WINDOW_TYPE_2B;
    attr->odp_caps.general_caps = IBV_ODP_SUPPORT | IBV_ODP_SUPPORT_IMPLICIT;
    attr->odp_caps.per_transport_caps.rc_odp_caps = IBV_ODP_SUPPORT_WRITE;
    attr->orig_attr.max_qp_wr = 2048;
    attr->orig_attr.max_cqe = 4096;
    attr->orig_attr.max_mw = 512;
    return 0;
}

/* Completion channels over a real eventfd, counted while live. */
static struct ibv_comp_channel *test_create_comp_channel(struct ibv_context *ctx) {
    struct ibv_comp_channel *ch = calloc(1, sizeof(*ch));
    if (!ch) return NULL;
    ch->context = ctx;
    ch->fd = eventfd(0, 0);
    if (ch->fd < 0) { free(ch); return NULL; }
    ++channels;
    return ch;
}
static int test_destroy_comp_channel(struct ibv_comp_channel *ch) {
    close(ch->fd); free(ch); --channels;
    return 0;
}

/* Connection manager events: none, then one connection request with more
 * private data than the event holds. */
static unsigned cm_events, cm_acks;
static uint8_t cm_private[60];
static struct rdma_cm_event cm_event;
static struct rdma_cm_id cm_ids[2];
static int test_get_cm_event(struct rdma_event_channel *ch, struct rdma_cm_event **out) {
    (void)ch;
    if (cm_events++ == 0) { errno = EAGAIN; return -1; }
    memset(&cm_event, 0, sizeof(cm_event));
    for (unsigned i = 0; i < sizeof(cm_private); ++i) cm_private[i] = (uint8_t)i;
    cm_event.id = &cm_ids[0]; cm_event.listen_id = &cm_ids[1];
    cm_event.event = RDMA_CM_EVENT_CONNECT_REQUEST; cm_event.status = -7;
    cm_event.param.conn.private_data = cm_private;
    cm_event.param.conn.private_data_len = sizeof(cm_private);
    *out = &cm_event;
    return 0;
}
static int test_ack_cm_event(struct rdma_cm_event *ev) { assert(ev == &cm_event); ++cm_acks; return 0; }

#define ibv_create_cq test_create_cq
#define ibv_destroy_cq test_destroy_cq
#define ibv_create_qp test_create_qp
#define ibv_destroy_qp test_destroy_qp
#define ibv_query_device_ex test_query_device_ex
#define ibv_create_comp_channel test_create_comp_channel
#define ibv_destroy_comp_channel test_destroy_comp_channel
#define rdma_get_cm_event test_get_cm_event
#define rdma_ack_cm_event test_ack_cm_event
#define ibv_get_device_list test_get_device_list
#define ibv_free_device_list test_free_device_list
#define ibv_open_device test_open_device
#define ibv_close_device test_close_device
#define ibv_query_device test_query_device
#undef ibv_query_port
#define ibv_query_port test_query_port
#define ibv_query_gid test_query_gid
#define ibv_get_device_name test_get_device_name
#define ibv_alloc_pd test_alloc_pd
#define ibv_dealloc_pd test_dealloc_pd
#define calloc test_calloc
#include "verbs.c"

/* Post one bind (`which` 0) or invalidate (`which` 1) through a fake QP and
 * return its send flags, or UINT_MAX if the post failed or had the wrong
 * opcode. */
static struct ibv_send_wr posted;
static struct ibv_sge posted_sge[4];
static struct ibv_recv_wr received;

static int test_post_send(struct ibv_qp *qp, struct ibv_send_wr *wr, struct ibv_send_wr **bad) {
    (void)qp; (void)bad;
    posted = *wr;
    for (int i = 0; i < wr->num_sge && i < 4; ++i) posted_sge[i] = wr->sg_list[i];
    return 0;
}

static int test_post_recv(struct ibv_qp *qp, struct ibv_recv_wr *wr, struct ibv_recv_wr **bad) {
    (void)qp; (void)bad;
    received = *wr;
    return 0;
}

static struct ibv_context fake_context;
static struct ibv_qp fake_qp;

static void fake_ops(void) {
    fake_context.ops.post_send = test_post_send;
    fake_context.ops.post_recv = test_post_recv;
    fake_qp.context = &fake_context;
}

unsigned rdma_test_post_flags(unsigned which) {
    struct ibv_mr mr = {0};
    struct ibv_mw mw = {0};
    fake_ops();
    struct rdma_verbs_qp q = { .qp = &fake_qp };
    struct rdma_verbs_mr m = { .mr = &mr, .bytes = (void *)4096, .length = 8192 };
    posted.opcode = (enum ibv_wr_opcode)UINT_MAX;
    int rc = which ? rdma_verbs_invalidate(&q, 1, 2) : rdma_verbs_bind(&q, &mw, &m, 1, 2, 0, 4096);
    if (rc || posted.opcode != (which ? IBV_WR_LOCAL_INV : IBV_WR_BIND_MW))
        return UINT_MAX;
    return posted.send_flags;
}

static int test_poll_cq(struct ibv_cq *cq, int n, struct ibv_wc *wc) {
    (void)cq;
    if (n < 2) return -1;
    memset(wc, 0, 2 * sizeof(*wc));
    wc[0].wr_id = 9; wc[0].status = IBV_WC_SUCCESS; wc[0].opcode = IBV_WC_RECV_RDMA_WITH_IMM;
    wc[0].byte_len = 100; wc[0].wc_flags = IBV_WC_WITH_IMM; wc[0].imm_data = htonl(0x01000007);
    wc[1].wr_id = 10; wc[1].status = IBV_WC_REM_ACCESS_ERR; wc[1].opcode = IBV_WC_RDMA_WRITE;
    wc[1].byte_len = 5; wc[1].vendor_err = 0x88;
    return 2;
}

/* Exercise the thin wrappers over mocked verbs and connection manager
 * calls. Returns 0, or the number of the first failed check. */
unsigned rdma_test_wrappers(void) {
    rdma_test_reset(11, 0);
    fake_ops();
    struct rdma_verbs_device d = { .ctx = &contexts[0] };
    /* Completion channel: nonblocking fd, freed. */
    void *ch = rdma_verbs_comp_channel(&d);
    if (!ch || channels != 1) return 1;
    int fd = rdma_verbs_comp_channel_fd(ch);
    if (fd < 0 || !(fcntl(fd, F_GETFL) & O_NONBLOCK)) return 2;
    /* CQ on the channel; zero depth refused. */
    if (rdma_verbs_cq_create(&d, 0, ch)) return 3;
    struct ibv_cq *cq = rdma_verbs_cq_create(&d, 8, ch);
    if (cq != &fake_cq || cq->channel != ch || cq->cqe != 8) return 4;
    if (rdma_verbs_cq_free(cq) || rdma_verbs_comp_channel_free(ch) || channels) return 5;
    /* Capabilities. */
    struct rdma_verbs_caps caps;
    if (rdma_verbs_query_caps(&d, 1, &caps)) return 6;
    if (caps.flags != (RDMA_VERBS_CAP_MW | RDMA_VERBS_CAP_ODP) || caps.ipv4_index != -1) return 7;
    if (caps.max_qp_wr != 2048 || caps.max_cqe != 4096 || caps.max_mw != 512) return 7;
    /* Writes: SGE count bounds, then a two-entry write with immediate. */
    struct rdma_verbs_qp q = { .qp = &fake_qp };
    struct rdma_verbs_sge sge[5] = {
        { .addr = 4096, .length = 10, .lkey = 1 }, { .addr = 8192, .length = 20, .lkey = 2 },
    };
    if (rdma_verbs_post_write(&q, sge, 0, 1, 2, 3, 0, 4) != EINVAL) return 8;
    if (rdma_verbs_post_write(&q, sge, 5, 1, 2, 3, 0, 4) != EINVAL) return 9;
    if (rdma_verbs_post_write(&q, sge, 2, 0x1000, 77, 0x123, RDMA_VERBS_POST_IMM | RDMA_VERBS_POST_SIGNALED, 4))
        return 10;
    if (posted.opcode != IBV_WR_RDMA_WRITE_WITH_IMM || posted.send_flags != IBV_SEND_SIGNALED ||
        posted.imm_data != htonl(0x123) || posted.num_sge != 2 || posted.wr_id != 4 ||
        posted.wr.rdma.remote_addr != 0x1000 || posted.wr.rdma.rkey != 77 ||
        posted_sge[1].addr != 8192 || posted_sge[1].length != 20 || posted_sge[1].lkey != 2)
        return 11;
    /* An unsignaled piece without immediate. */
    if (rdma_verbs_post_write(&q, sge, 1, 0x2000, 77, 0x123, 0, 0)) return 12;
    if (posted.opcode != IBV_WR_RDMA_WRITE || posted.send_flags || posted.imm_data) return 13;
    if (rdma_verbs_post_send(&q, sge, 1, 5) || posted.opcode != IBV_WR_SEND ||
        posted.send_flags != IBV_SEND_SIGNALED) return 14;
    if (rdma_verbs_post_recv(&q, sge, 1, 6) || received.wr_id != 6 || received.num_sge != 1) return 15;
    /* Polling translates statuses and immediates, in host order. */
    struct ibv_cq poll_cq = { .context = &fake_context };
    fake_context.ops.poll_cq = test_poll_cq;
    struct rdma_verbs_wc wc[2];
    if (rdma_verbs_poll(&poll_cq, wc, 2) != 2) return 16;
    if (wc[0].id != 9 || wc[0].opcode != IBV_WC_RECV_RDMA_WITH_IMM || wc[0].imm != 0x01000007 ||
        wc[0].flags != RDMA_VERBS_WC_IMM || wc[0].byte_len != 100) return 17;
    if (wc[1].status != IBV_WC_REM_ACCESS_ERR || wc[1].opcode != UINT32_MAX || wc[1].byte_len ||
        wc[1].vendor_err != 0x88) return 18;
    /* Connection manager events: none yet, then a request with clipped data. */
    struct rdma_verbs_cm_event ev;
    cm_events = cm_acks = 0;
    if (rdma_verbs_cm_get_event(NULL, &ev) != -EAGAIN) return 19;
    if (rdma_verbs_cm_get_event(NULL, &ev)) return 20;
    if (ev.type != RDMA_CM_EVENT_CONNECT_REQUEST || ev.status != -7 || ev.id != &cm_ids[0] ||
        ev.listen_id != &cm_ids[1] || ev.private_len != 56 || ev.private_data[55] != 55 ||
        ev.handle != &cm_event) return 21;
    rdma_verbs_cm_ack(ev.handle);
    if (cm_acks != 1) return 22;
    /* Private data longer than a connection request holds is refused. */
    if (rdma_verbs_cm_connect(NULL, cm_private, 57) != EINVAL ||
        rdma_verbs_cm_accept(NULL, cm_private, 57) != EINVAL) return 23;
    return 0;
}

/* Exercise the production discovery loop with failing libibverbs calls. */
#include <infiniband/verbs.h>
#include <assert.h>
#include <errno.h>
#include <string.h>

static struct ibv_device devices[3];
static struct ibv_context contexts[3];
static struct ibv_device *device_list[] = { &devices[0], &devices[1], &devices[2], NULL };
static unsigned mode, fail_close, opens, closes, lists_freed;

/* Modes: query failure, unsupported, active, inactive, list error, open error,
 * empty list, and capacity overflow. Close failure is a one-based call index. */
void rdma_test_reset(unsigned scenario, unsigned close_call) {
    mode = scenario; fail_close = close_call;
    opens = closes = lists_freed = 0;
}

unsigned rdma_test_count(unsigned which) {
    return which == 0 ? opens : which == 1 ? closes : lists_freed;
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

/* Discovery never allocates a PD. A later open fails cleanly in this fixture. */
static struct ibv_pd *test_alloc_pd(struct ibv_context *ctx) {
    (void)ctx;
    return NULL;
}

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
#include "verbs.c"

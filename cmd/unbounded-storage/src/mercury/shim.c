/*
 * Copyright (c) Microsoft Corporation.
 * Licensed under the MIT License.
 *
 * Minimal C shim for the unbounded-storage Mercury transport.
 *
 * Mercury exposes its serialization primitives (`hg_proc_hg_uint32_t`,
 * `hg_proc_hg_uint64_t`, `hg_proc_bytes`, ...) as `static HG_INLINE`
 * functions in `mercury_proc.h`. They have no exported symbols, so
 * Rust FFI cannot call them directly. This file is the only piece of
 * C we ship; everything above it is plain Rust.
 *
 * Two callbacks are registered against the single
 * `ub.bufferpool.bulk_get.v1` RPC:
 *   - `ub_proc_bulk_get_in`  encodes/decodes the request envelope
 *     (stripe key, offsets, length, client-side bulk handle, plus a
 *     length-prefixed opaque byte slice carrying the caller's
 *     `R: Serialize`).
 *   - `ub_proc_bulk_get_out` encodes/decodes a single `int32_t`
 *     status code.
 *
 * The Rust side stages the input/output structs in heap allocations
 * and passes pointers to them; this file just walks the bytes.
 */

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include <mercury.h>
#include <mercury_bulk.h>
#include <mercury_proc.h>
#include <mercury_proc_bulk.h>

/*
 * Mirrors `BulkGetIn` in `rpc.rs`. Field types match the Rust side
 * byte-for-byte: `hg_uint32_t` = `u32`, `hg_uint64_t` = `u64`,
 * `hg_bulk_t` is a pointer.
 */
struct ub_bulk_get_in {
    uint8_t     stripe_key[32];
    uint64_t    stripe_off;
    uint64_t    dst_offset;
    uint32_t    len;
    /* `req_bytes_len` precedes the opaque `req_bytes` buffer on the
     * wire; on the host side we keep the length in a separate field so
     * the decoder can size the heap allocation before reading. */
    uint32_t    req_bytes_len;
    uint8_t    *req_bytes;
    hg_bulk_t   dst_bulk;
};

struct ub_bulk_get_out {
    int32_t status;
};

hg_return_t
ub_proc_bulk_get_in(hg_proc_t proc, void *data);
hg_return_t
ub_proc_bulk_get_out(hg_proc_t proc, void *data);

size_t
ub_sizeof_bulk_get_in(void);
size_t
ub_sizeof_bulk_get_out(void);

/* `HG_Get_info` is `static HG_INLINE` in `mercury.h`; expose it as a
 * real symbol so the Rust side can resolve the handle's class /
 * context / origin address without poking at struct internals. */
const struct hg_info *
ub_handle_info(hg_handle_t handle);

const struct hg_info *
ub_handle_info(hg_handle_t handle)
{
    return HG_Get_info(handle);
}

hg_return_t
ub_proc_bulk_get_in(hg_proc_t proc, void *data)
{
    struct ub_bulk_get_in *in = (struct ub_bulk_get_in *) data;
    hg_proc_op_t op = hg_proc_get_op(proc);
    hg_return_t ret;

    ret = hg_proc_bytes(proc, in->stripe_key, sizeof(in->stripe_key));
    if (ret != HG_SUCCESS)
        return ret;
    ret = hg_proc_hg_uint64_t(proc, &in->stripe_off);
    if (ret != HG_SUCCESS)
        return ret;
    ret = hg_proc_hg_uint64_t(proc, &in->dst_offset);
    if (ret != HG_SUCCESS)
        return ret;
    ret = hg_proc_hg_uint32_t(proc, &in->len);
    if (ret != HG_SUCCESS)
        return ret;
    ret = hg_proc_hg_uint32_t(proc, &in->req_bytes_len);
    if (ret != HG_SUCCESS)
        return ret;

    /* Variable-length opaque tail. On decode we own the allocation
     * and free it from HG_Free_input via the HG_FREE op below. */
    if (in->req_bytes_len > 0) {
        if (op == HG_DECODE) {
            in->req_bytes = (uint8_t *) malloc(in->req_bytes_len);
            if (in->req_bytes == NULL)
                return HG_NOMEM;
        }
        if (op == HG_ENCODE || op == HG_DECODE) {
            ret = hg_proc_bytes(proc, in->req_bytes, in->req_bytes_len);
            if (ret != HG_SUCCESS)
                return ret;
        }
        if (op == HG_FREE && in->req_bytes != NULL) {
            free(in->req_bytes);
            in->req_bytes = NULL;
        }
    } else if (op == HG_DECODE) {
        in->req_bytes = NULL;
    }

    ret = hg_proc_hg_bulk_t(proc, &in->dst_bulk);
    if (ret != HG_SUCCESS)
        return ret;

    return HG_SUCCESS;
}

hg_return_t
ub_proc_bulk_get_out(hg_proc_t proc, void *data)
{
    struct ub_bulk_get_out *out = (struct ub_bulk_get_out *) data;
    /* int32 has no platform-portable hg_proc helper exported, but
     * uint32 round-trips bit-identically for our purposes. */
    return hg_proc_hg_uint32_t(proc, (uint32_t *) &out->status);
}

size_t
ub_sizeof_bulk_get_in(void)
{
    return sizeof(struct ub_bulk_get_in);
}

size_t
ub_sizeof_bulk_get_out(void)
{
    return sizeof(struct ub_bulk_get_out);
}

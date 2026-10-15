/* Compile with cc -fsyntax-only against the target Linux headers. */
#include <linux/perf_event.h>
#include <stddef.h>
_Static_assert(PERF_ATTR_SIZE_VER5 == 112, "attr version");
_Static_assert(offsetof(struct perf_event_attr, sample_max_stack) == 108, "stack offset");
_Static_assert(offsetof(struct perf_event_mmap_page, data_head) == 1024, "head offset");
_Static_assert(offsetof(struct perf_event_mmap_page, data_tail) == 1032, "tail offset");
_Static_assert(offsetof(struct perf_event_mmap_page, data_offset) == 1040, "data offset");
_Static_assert(offsetof(struct perf_event_mmap_page, data_size) == 1048, "size offset");
_Static_assert(PERF_SAMPLE_IP == 1 && PERF_SAMPLE_TID == 2, "sample flags");
_Static_assert(PERF_SAMPLE_CALLCHAIN == 32 && PERF_SAMPLE_PERIOD == 256, "sample flags");
_Static_assert(PERF_COUNT_SW_CPU_CLOCK == 0 && PERF_TYPE_SOFTWARE == 1, "event type");
_Static_assert(PERF_EVENT_IOC_ENABLE == 0x2400 && PERF_EVENT_IOC_DISABLE == 0x2401, "ioctls");

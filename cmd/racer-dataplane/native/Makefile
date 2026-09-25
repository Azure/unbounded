CC ?= cc
CFLAGS ?= -O2 -g
CPPFLAGS += $(shell pkg-config --cflags libibverbs)
LDLIBS += $(shell pkg-config --libs libibverbs)

.PHONY: all clean
all: libracer_rdma.so.1

libracer_rdma.so.1: rdma.c
	$(CC) $(CPPFLAGS) $(CFLAGS) -std=gnu11 -Wall -Wextra -Werror -fPIC -shared -Wl,-soname,libracer_rdma.so.1 -Wl,-z,defs -o $@ $< $(LDLIBS)

clean:
	rm -f libracer_rdma.so.1

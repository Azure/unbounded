// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racersdk reads objects through a node-local Racer cache and serves
// cache misses from your own storage.
//
// Racer runs on each node and exposes every cache as two Unix sockets under
// /run/racer/<cache>/. A [Client] reads objects through the client socket.
// An [Origin], registered with [ServeOrigin] on the origin socket, supplies
// objects that no Racer in the cluster has cached yet. One process may do
// either or both.
//
// # Reading
//
// Create one [Client] per cache and share it:
//
//	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: "blobs"})
//	if err != nil {
//		return err
//	}
//	defer client.Close()
//
//	object, err := client.Get(ctx, racersdk.Request{Key: key, Metadata: "bucket/path"})
//	if err != nil {
//		return err
//	}
//	defer object.Close()
//
//	_, err = io.Copy(w, object)
//
// [Client.Get] returns once Racer has accepted the read, with the object's
// [Metadata] available from [Object.Metadata]. [Client.Stat] returns metadata
// without reading content. Use [ReadOptions] to read a byte range or to pin
// a version by ETag.
//
// # Copy or no copy
//
// An [Object] can be consumed in exactly two ways. Pick one per object.
//
// [Object.Read] copies. Each call copies bytes from the socket into your
// buffer, so your code sees and owns the data: use it to hash, parse,
// decompress, or transform the content. Copying costs CPU and memory
// bandwidth proportional to the object size. Pass buffers of 32 KiB or more.
//
// [Object.WriteTo] does not copy. It forwards the object to a writer. When the
// writer is backed by a file descriptor and implements [io.ReaderFrom], such
// as an [*os.File], a [net.Conn], or the [http.ResponseWriter] of a
// plain-text HTTP/1 server, the kernel moves the data from Racer's socket to the destination
// with splice(2) and it never enters process memory. Other writers receive
// data through a single reused 256 KiB buffer. Your code never sees the
// bytes, so this is the right choice for proxies and anything that only
// stores or forwards content. [io.Copy] uses WriteTo automatically when the
// source is an Object, so io.Copy(dst, object) is the no-copy path; wrapping
// the object in another reader, such as [io.TeeReader], switches to Read.
//
// Both paths withhold the last byte until Racer confirms the whole range was
// delivered intact. A read that is cut short therefore always ends in an
// error rather than a silently truncated result, even for a destination
// that has already received the rest of the data.
//
// # Serving cache misses
//
// Implement [Origin] and pass it to [ServeOrigin]. Racer calls the origin for
// metadata and for individual pages of up to [PageSize] bytes. Requests for
// later pages are pinned to the ETag the origin returned for the first one,
// so origins only need ranged reads of immutable versions. The SDK validates
// every response, so a short, long, or mismatched body is reported to Racer
// instead of being cached.
//
// # Errors
//
// Test errors with [errors.Is] against [ErrNotFound], [ErrUnauthorized],
// [ErrForbidden], [ErrVersionMismatch], [ErrRangeNotSatisfiable],
// [ErrUnavailable], and [ErrInvalidRequest]. Errors caused by an ending
// context wrap [context.Canceled] or [context.DeadlineExceeded], and calls
// after Close wrap [net.ErrClosed]. A failure of the writer passed to
// [Object.WriteTo] matches [ErrDestination] instead; Racer is not at fault,
// so do not retry or count it against Racer. A WriteTo timeout that cannot
// be attributed to either side wraps [os.ErrDeadlineExceeded]. Any other
// error means Racer or an origin misbehaved; a proxy should report it as a
// bad gateway. Origins report failures by wrapping the same errors.
//
// # Testing
//
// Package [github.com/Azure/unbounded/pkg/racersdk/racersdktest] runs an
// in-process Racer for tests, connecting a real Client to your Origin.
package racersdk

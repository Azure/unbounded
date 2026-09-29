# Stage38 body progress diagnostics

This change is based on f19e21a7 and does not alter candidate time shares,
signed deadlines, cancellation fencing, retry credits, or the peer wire protocol.

`PeerReceiveBody` failures retain one fixed-size record in the existing 128-entry
ring. Positive reads only update local counters/timestamps. A numeric TCP tuple
is sampled once before body receive because failed I/O consumes the connection.
No descriptor, payload, header, object key, ETag, or credential is retained.

Body records use `seq` (hex), `w` (worker), request/attempt IDs, and:

- `rx=received/expected` and `n=positive-read-count` (decimal).
- `ms=hex`: all six times are absolute Unix milliseconds rendered in hex using
  the stable monotonic-to-wall mapping used for signing, not relative timeouts.
- `f` and `l`: first and last positive read completion; zero means no progress.
- `now`: time of failure observation, after accepted I/O is fenced.
- `orig`: original local acquisition hard deadline before candidate division.
- `share`: candidate deadline before transport narrowing.
- `sig`: outbound signed route deadline.
- `remote`: immediate authenticated peer UUID, not final route destination.
- `tcp=local>remote`: numeric socket tuple, or `none` if unavailable.

Original/share metadata exists only inside this process. Zero means unknown for
a remotely originated request; it must not be interpreted as the remote caller's
original deadline. A downstream candidate records its own inherited acquisition
ceiling. Correlate request/attempt IDs and peer tuples, not unrelated ring events.
The normal non-body ring format is unchanged. Worst-case IPv6/numeric body records
are tested against the existing 64-KiB response buffer including header headroom.

The signed HTTP body regression exercises success, a regularly progressing body
that outlives its fixed share but not its original ceiling, cancellation, and EOF.
It asserts the current failure behavior, exact single-event count, recent progress,
correlation, no secret fields, unchanged metric export, and released quota/fences.
The existing candidate suite covers real share division, fallback and credit
conservation; diagnostic metadata assertions cover its local propagation.

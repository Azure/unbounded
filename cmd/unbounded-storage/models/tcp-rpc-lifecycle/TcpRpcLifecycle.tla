------------------------- MODULE TcpRpcLifecycle -------------------------
(***************************************************************************
  Lifecycle model for the custom TLS TCP RPC transport.

  This model is intentionally separate from FabricCompletion. The TCP path
  has persistent single-request lanes, direct fixed receives into caller
  destinations, and request identity scoped by a connection generation. It
  does not share libfabric completion correlation.

  Requests move New -> Queued -> Active -> Succeeded/Failed. Admission bounds
  Queued plus Active requests, and each lane owns at most one Active request.
  A request records its wire request id, lane, connection generation, and
  destination lease epoch. Wire ids and destinations may be reused by later
  requests, while a frame emitted by an earlier generation may remain delayed.

  Direct page delivery is enabled only when the complete association still
  matches and the destination lease is current. Cancel, disconnect, and
  deadline failures terminalize an active request once and retire its lane;
  queued cancel/deadline failures remove the waiter without claiming a lane.
  A later connect increments the lane generation.

  SEND_ZC is represented independently on the serving side. Its source stays
  held after the byte-count completion and is released only by the final
  notification, even if its request has already failed.
*)

EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
  Requests,
  Lanes,
  RequestIds,
  Destinations,
  Sends,
  MaxInflight,
  MaxGeneration,
  NONE

ASSUME MaxInflight \in Nat /\ MaxInflight >= 1
ASSUME MaxGeneration \in Nat /\ MaxGeneration >= 1
ASSUME NONE \notin Requests \cup Lanes \cup RequestIds \cup Destinations \cup Sends

RequestStatus == {"New", "Queued", "Active", "Succeeded", "Failed"}
TerminalStatus == {"Succeeded", "Failed"}
TerminalCause == {"None", "End", "Cancel", "Disconnect", "Deadline"}
SendStatus == {"Free", "Submitted", "BytesComplete", "Notified"}

LaneGeneration == [lane: Lanes, gen: 1..MaxGeneration]
FrameUniverse == [lane: Lanes, gen: 1..MaxGeneration, id: RequestIds]
DeliveryRecord ==
  [req: Requests,
   lane: Lanes,
   gen: 1..MaxGeneration,
   id: RequestIds,
   dest: Destinations,
   epoch: 1..Cardinality(Requests)]

VARIABLES
  status,          \* [Requests -> RequestStatus]
  reqId,           \* wire request id assigned while Queued
  reqLane,         \* lane assigned while Active, retained after terminal
  reqGen,          \* connection generation assigned while Active
  reqDest,         \* destination owned by this request lifetime
  reqDestEpoch,    \* destination lease epoch captured at admission
  leaseValid,      \* direct receive may target the destination only if TRUE
  terminalCount,   \* ghost count proving one terminal transition
  terminalCause,   \* the unique reason for the terminal transition
  wasActive,       \* distinguishes active failures from queued withdrawal
  connUp,          \* [Lanes -> BOOLEAN]
  connGen,         \* monotonically increasing generation per lane
  laneReq,         \* the one Active request on a lane, or NONE
  usedIds,         \* request ids already used in the current generation
  retired,         \* lane generations retired by an active failure
  destOwner,       \* current destination lease owner, or NONE
  destEpoch,       \* incremented whenever a destination is leased again
  frames,          \* page frames emitted but not yet consumed
  justDelivery,    \* ghost record for the immediately preceding page write
  sendStatus,      \* SEND_ZC completion phase
  sendReq,         \* request whose source is held by a SEND_ZC
  sourceHeld       \* source pin/release-token lifetime

vars ==
  <<status, reqId, reqLane, reqGen, reqDest, reqDestEpoch, leaseValid,
    terminalCount, terminalCause, wasActive, connUp, connGen, laneReq,
    usedIds, retired, destOwner, destEpoch, frames, justDelivery,
    sendStatus, sendReq, sourceHeld>>

LiveRequests == {r \in Requests : status[r] \in {"Queued", "Active"}}
ActiveRequests == {r \in Requests : status[r] = "Active"}

OutstandingSends(r) ==
  {s \in Sends :
     sendReq[s] = r /\ sendStatus[s] \in {"Submitted", "BytesComplete"}}

FrameMatches(f, r) ==
  /\ status[r] = "Active"
  /\ reqLane[r] = f.lane
  /\ reqGen[r] = f.gen
  /\ reqId[r] = f.id

CanDeliver(f, r) ==
  /\ FrameMatches(f, r)
  /\ connUp[f.lane]
  /\ connGen[f.lane] = f.gen
  /\ laneReq[f.lane] = r
  /\ leaseValid[r]
  /\ destOwner[reqDest[r]] = r
  /\ destEpoch[reqDest[r]] = reqDestEpoch[r]

ClearDelivery == justDelivery' = NONE

Init ==
  /\ status = [r \in Requests |-> "New"]
  /\ reqId = [r \in Requests |-> NONE]
  /\ reqLane = [r \in Requests |-> NONE]
  /\ reqGen = [r \in Requests |-> 0]
  /\ reqDest = [r \in Requests |-> NONE]
  /\ reqDestEpoch = [r \in Requests |-> 0]
  /\ leaseValid = [r \in Requests |-> FALSE]
  /\ terminalCount = [r \in Requests |-> 0]
  /\ terminalCause = [r \in Requests |-> "None"]
  /\ wasActive = [r \in Requests |-> FALSE]
  /\ connUp = [l \in Lanes |-> FALSE]
  /\ connGen = [l \in Lanes |-> 0]
  /\ laneReq = [l \in Lanes |-> NONE]
  /\ usedIds = [l \in Lanes |-> {}]
  /\ retired = {}
  /\ destOwner = [d \in Destinations |-> NONE]
  /\ destEpoch = [d \in Destinations |-> 0]
  /\ frames = {}
  /\ justDelivery = NONE
  /\ sendStatus = [s \in Sends |-> "Free"]
  /\ sendReq = [s \in Sends |-> NONE]
  /\ sourceHeld = [s \in Sends |-> FALSE]

(***************************************************************************
  Establish a fresh connection generation on an idle down lane. Resetting
  usedIds permits a wire id to be reused, but the generation remains part of
  every live association and delayed frame.
*)
Connect(l) ==
  /\ ~connUp[l]
  /\ laneReq[l] = NONE
  /\ connGen[l] < MaxGeneration
  /\ connUp' = [connUp EXCEPT ![l] = TRUE]
  /\ connGen' = [connGen EXCEPT ![l] = @ + 1]
  /\ usedIds' = [usedIds EXCEPT ![l] = {}]
  /\ ClearDelivery
  /\ UNCHANGED
       <<status, reqId, reqLane, reqGen, reqDest, reqDestEpoch,
         leaseValid, terminalCount, terminalCause, wasActive, laneReq,
         retired, destOwner, destEpoch, frames, sendStatus, sendReq,
         sourceHeld>>

(***************************************************************************
  Admission owns a destination before the request has a lane. Leasing bumps
  the destination epoch, so a later request can safely recycle the same slot.
*)
Admit(r, d, id) ==
  /\ status[r] = "New"
  /\ Cardinality(LiveRequests) < MaxInflight
  /\ destOwner[d] = NONE
  /\ status' = [status EXCEPT ![r] = "Queued"]
  /\ reqId' = [reqId EXCEPT ![r] = id]
  /\ reqDest' = [reqDest EXCEPT ![r] = d]
  /\ reqDestEpoch' = [reqDestEpoch EXCEPT ![r] = destEpoch[d] + 1]
  /\ leaseValid' = [leaseValid EXCEPT ![r] = TRUE]
  /\ destOwner' = [destOwner EXCEPT ![d] = r]
  /\ destEpoch' = [destEpoch EXCEPT ![d] = @ + 1]
  /\ ClearDelivery
  /\ UNCHANGED
       <<reqLane, reqGen, terminalCount, terminalCause, wasActive, connUp,
         connGen, laneReq, usedIds, retired, frames, sendStatus, sendReq,
         sourceHeld>>

(***************************************************************************
  A lane admits exactly one active request. A request id is not reused on the
  same persistent connection generation; reconnect resets that generation's
  used-id set.
*)
Activate(r, l) ==
  /\ status[r] = "Queued"
  /\ connUp[l]
  /\ laneReq[l] = NONE
  /\ reqId[r] \notin usedIds[l]
  /\ status' = [status EXCEPT ![r] = "Active"]
  /\ reqLane' = [reqLane EXCEPT ![r] = l]
  /\ reqGen' = [reqGen EXCEPT ![r] = connGen[l]]
  /\ wasActive' = [wasActive EXCEPT ![r] = TRUE]
  /\ laneReq' = [laneReq EXCEPT ![l] = r]
  /\ usedIds' = [usedIds EXCEPT ![l] = @ \cup {reqId[r]}]
  /\ ClearDelivery
  /\ UNCHANGED
       <<reqId, reqDest, reqDestEpoch, leaseValid, terminalCount,
         terminalCause, connUp, connGen, retired, destOwner, destEpoch,
         frames, sendStatus, sendReq, sourceHeld>>

(***************************************************************************
  The network may delay a page frame after its request has failed and its
  lane has reconnected. The frame retains its original lane generation and
  request id.
*)
EmitPage(r) ==
  /\ status[r] = "Active"
  /\ LET f == [lane |-> reqLane[r], gen |-> reqGen[r], id |-> reqId[r]]
     IN /\ f \notin frames
        /\ frames' = frames \cup {f}
  /\ ClearDelivery
  /\ UNCHANGED
       <<status, reqId, reqLane, reqGen, reqDest, reqDestEpoch, leaseValid,
         terminalCount, terminalCause, wasActive, connUp, connGen, laneReq,
         usedIds, retired, destOwner, destEpoch, sendStatus, sendReq,
         sourceHeld>>

(***************************************************************************
  Direct fixed receive. All association and lease checks happen before the
  page body can target the registered destination.
*)
DeliverFrame(f, r) ==
  /\ f \in frames
  /\ CanDeliver(f, r)
  /\ frames' = frames \ {f}
  /\ justDelivery' =
       [req |-> r,
        lane |-> f.lane,
        gen |-> f.gen,
        id |-> f.id,
        dest |-> reqDest[r],
        epoch |-> reqDestEpoch[r]]
  /\ UNCHANGED
       <<status, reqId, reqLane, reqGen, reqDest, reqDestEpoch, leaseValid,
         terminalCount, terminalCause, wasActive, connUp, connGen, laneReq,
         usedIds, retired, destOwner, destEpoch, sendStatus, sendReq,
         sourceHeld>>

DropFrame(f) ==
  /\ f \in frames
  /\ ~\E r \in Requests : CanDeliver(f, r)
  /\ frames' = frames \ {f}
  /\ ClearDelivery
  /\ UNCHANGED
       <<status, reqId, reqLane, reqGen, reqDest, reqDestEpoch, leaseValid,
         terminalCount, terminalCause, wasActive, connUp, connGen, laneReq,
         usedIds, retired, destOwner, destEpoch, sendStatus, sendReq,
         sourceHeld>>

(***************************************************************************
  Serving-side SEND_ZC lifetime. The byte-count completion does not release
  the source. Only the final notification changes sourceHeld to FALSE.
*)
StartSend(r, s) ==
  /\ status[r] = "Active"
  /\ sendStatus[s] = "Free"
  /\ sendStatus' = [sendStatus EXCEPT ![s] = "Submitted"]
  /\ sendReq' = [sendReq EXCEPT ![s] = r]
  /\ sourceHeld' = [sourceHeld EXCEPT ![s] = TRUE]
  /\ ClearDelivery
  /\ UNCHANGED
       <<status, reqId, reqLane, reqGen, reqDest, reqDestEpoch, leaseValid,
         terminalCount, terminalCause, wasActive, connUp, connGen, laneReq,
         usedIds, retired, destOwner, destEpoch, frames>>

SendBytesComplete(s) ==
  /\ sendStatus[s] = "Submitted"
  /\ sendStatus' = [sendStatus EXCEPT ![s] = "BytesComplete"]
  /\ ClearDelivery
  /\ UNCHANGED
       <<status, reqId, reqLane, reqGen, reqDest, reqDestEpoch, leaseValid,
         terminalCount, terminalCause, wasActive, connUp, connGen, laneReq,
         usedIds, retired, destOwner, destEpoch, frames, sendReq, sourceHeld>>

SendNotification(s) ==
  /\ sendStatus[s] = "BytesComplete"
  /\ sendStatus' = [sendStatus EXCEPT ![s] = "Notified"]
  /\ sourceHeld' = [sourceHeld EXCEPT ![s] = FALSE]
  /\ ClearDelivery
  /\ UNCHANGED
       <<status, reqId, reqLane, reqGen, reqDest, reqDestEpoch, leaseValid,
         terminalCount, terminalCause, wasActive, connUp, connGen, laneReq,
         usedIds, retired, destOwner, destEpoch, frames, sendReq>>

(***************************************************************************
  Normal END terminalizes once and preserves the connection. Every SEND_ZC
  associated with the request must already have received its notification.
*)
CompleteResponse(r) ==
  /\ status[r] = "Active"
  /\ OutstandingSends(r) = {}
  /\ status' = [status EXCEPT ![r] = "Succeeded"]
  /\ terminalCount' = [terminalCount EXCEPT ![r] = @ + 1]
  /\ terminalCause' = [terminalCause EXCEPT ![r] = "End"]
  /\ leaseValid' = [leaseValid EXCEPT ![r] = FALSE]
  /\ destOwner' = [destOwner EXCEPT ![reqDest[r]] = NONE]
  /\ laneReq' = [laneReq EXCEPT ![reqLane[r]] = NONE]
  /\ ClearDelivery
  /\ UNCHANGED
       <<reqId, reqLane, reqGen, reqDest, reqDestEpoch, wasActive, connUp,
         connGen, usedIds, retired, destEpoch, frames, sendStatus, sendReq,
         sourceHeld>>

(***************************************************************************
  An active cancel, disconnect, or deadline failure closes the connection and
  records its generation as retired. Pending SEND_ZC notifications keep their
  source pins and complete independently.
*)
FailActive(r, cause) ==
  /\ status[r] = "Active"
  /\ cause \in {"Cancel", "Disconnect", "Deadline"}
  /\ status' = [status EXCEPT ![r] = "Failed"]
  /\ terminalCount' = [terminalCount EXCEPT ![r] = @ + 1]
  /\ terminalCause' = [terminalCause EXCEPT ![r] = cause]
  /\ leaseValid' = [leaseValid EXCEPT ![r] = FALSE]
  /\ destOwner' = [destOwner EXCEPT ![reqDest[r]] = NONE]
  /\ connUp' = [connUp EXCEPT ![reqLane[r]] = FALSE]
  /\ laneReq' = [laneReq EXCEPT ![reqLane[r]] = NONE]
  /\ retired' = retired \cup {[lane |-> reqLane[r], gen |-> reqGen[r]]}
  /\ ClearDelivery
  /\ UNCHANGED
       <<reqId, reqLane, reqGen, reqDest, reqDestEpoch, wasActive, connGen,
         usedIds, destEpoch, frames, sendStatus, sendReq, sourceHeld>>

(***************************************************************************
  Dropping or timing out a queued waiter terminalizes it once and releases its
  destination. There is no lane to retire until Activate has succeeded.
*)
FailQueued(r, cause) ==
  /\ status[r] = "Queued"
  /\ cause \in {"Cancel", "Deadline"}
  /\ status' = [status EXCEPT ![r] = "Failed"]
  /\ terminalCount' = [terminalCount EXCEPT ![r] = @ + 1]
  /\ terminalCause' = [terminalCause EXCEPT ![r] = cause]
  /\ leaseValid' = [leaseValid EXCEPT ![r] = FALSE]
  /\ destOwner' = [destOwner EXCEPT ![reqDest[r]] = NONE]
  /\ ClearDelivery
  /\ UNCHANGED
       <<reqId, reqLane, reqGen, reqDest, reqDestEpoch, wasActive, connUp,
         connGen, laneReq, usedIds, retired, destEpoch, frames, sendStatus,
         sendReq, sourceHeld>>

Next ==
  \/ \E l \in Lanes : Connect(l)
  \/ \E r \in Requests, d \in Destinations, id \in RequestIds : Admit(r, d, id)
  \/ \E r \in Requests, l \in Lanes : Activate(r, l)
  \/ \E r \in Requests : EmitPage(r)
  \/ \E f \in FrameUniverse, r \in Requests : DeliverFrame(f, r)
  \/ \E f \in FrameUniverse : DropFrame(f)
  \/ \E r \in Requests, s \in Sends : StartSend(r, s)
  \/ \E s \in Sends : SendBytesComplete(s)
  \/ \E s \in Sends : SendNotification(s)
  \/ \E r \in Requests : CompleteResponse(r)
  \/ \E r \in Requests, cause \in {"Cancel", "Disconnect", "Deadline"} :
       FailActive(r, cause)
  \/ \E r \in Requests, cause \in {"Cancel", "Deadline"} :
       FailQueued(r, cause)

(***************************************************************************
  Fair network consumption and SEND_ZC completion prevent kernel/network work
  from remaining pending forever. Fair END delivery then terminalizes active
  work after its finite set of possible sends has reached notification.
*)
Fairness ==
  /\ \A f \in FrameUniverse, r \in Requests : WF_vars(DeliverFrame(f, r))
  /\ \A f \in FrameUniverse : WF_vars(DropFrame(f))
  /\ \A s \in Sends : WF_vars(SendBytesComplete(s))
  /\ \A s \in Sends : WF_vars(SendNotification(s))
  /\ \A r \in Requests : WF_vars(CompleteResponse(r))

Spec == Init /\ [][Next]_vars /\ Fairness

EventualActiveTermination ==
  \A r \in Requests :
    (status[r] = "Active") ~> (status[r] \in TerminalStatus)

(***************************************************************************
  Type and lifecycle association invariants.
*)
TypeOK ==
  /\ status \in [Requests -> RequestStatus]
  /\ reqId \in [Requests -> RequestIds \cup {NONE}]
  /\ reqLane \in [Requests -> Lanes \cup {NONE}]
  /\ reqGen \in [Requests -> 0..MaxGeneration]
  /\ reqDest \in [Requests -> Destinations \cup {NONE}]
  /\ reqDestEpoch \in [Requests -> 0..Cardinality(Requests)]
  /\ leaseValid \in [Requests -> BOOLEAN]
  /\ terminalCount \in [Requests -> 0..1]
  /\ terminalCause \in [Requests -> TerminalCause]
  /\ wasActive \in [Requests -> BOOLEAN]
  /\ connUp \in [Lanes -> BOOLEAN]
  /\ connGen \in [Lanes -> 0..MaxGeneration]
  /\ laneReq \in [Lanes -> Requests \cup {NONE}]
  /\ usedIds \in [Lanes -> SUBSET RequestIds]
  /\ retired \subseteq LaneGeneration
  /\ destOwner \in [Destinations -> Requests \cup {NONE}]
  /\ destEpoch \in [Destinations -> 0..Cardinality(Requests)]
  /\ frames \subseteq FrameUniverse
  /\ justDelivery \in DeliveryRecord \cup {NONE}
  /\ sendStatus \in [Sends -> SendStatus]
  /\ sendReq \in [Sends -> Requests \cup {NONE}]
  /\ sourceHeld \in [Sends -> BOOLEAN]

RequestAssociation ==
  /\ \A r \in Requests :
       status[r] = "New" =>
         /\ reqId[r] = NONE
         /\ reqLane[r] = NONE
         /\ reqDest[r] = NONE
         /\ ~leaseValid[r]
  /\ \A r \in Requests :
       status[r] = "Queued" =>
         /\ reqId[r] \in RequestIds
         /\ reqLane[r] = NONE
         /\ reqGen[r] = 0
         /\ leaseValid[r]
         /\ destOwner[reqDest[r]] = r
         /\ destEpoch[reqDest[r]] = reqDestEpoch[r]
  /\ \A r \in Requests :
       status[r] = "Active" =>
         /\ reqId[r] \in RequestIds
         /\ reqLane[r] \in Lanes
         /\ reqGen[r] = connGen[reqLane[r]]
         /\ connUp[reqLane[r]]
         /\ laneReq[reqLane[r]] = r
         /\ reqId[r] \in usedIds[reqLane[r]]
         /\ leaseValid[r]
         /\ destOwner[reqDest[r]] = r
         /\ destEpoch[reqDest[r]] = reqDestEpoch[r]
  /\ \A r \in Requests :
       status[r] \in TerminalStatus =>
         /\ ~leaseValid[r]
         /\ \A l \in Lanes : laneReq[l] /= r
  /\ \A l \in Lanes :
       laneReq[l] /= NONE =>
         /\ connUp[l]
         /\ status[laneReq[l]] = "Active"
         /\ reqLane[laneReq[l]] = l
  /\ \A l \in Lanes : ~connUp[l] => laneReq[l] = NONE
  /\ \A r \in Requests :
       leaseValid[r] =>
         /\ reqDest[r] \in Destinations
         /\ destOwner[reqDest[r]] = r
         /\ destEpoch[reqDest[r]] = reqDestEpoch[r]

(***************************************************************************
  BOUNDED ADMISSION. Queued plus Active requests never exceed MaxInflight,
  and Active requests are in one-to-one correspondence with occupied lanes.
*)
BoundedAdmission ==
  /\ Cardinality(LiveRequests) <= MaxInflight
  /\ Cardinality(ActiveRequests) <= Cardinality(Lanes)
  /\ Cardinality({l \in Lanes : laneReq[l] /= NONE}) =
       Cardinality(ActiveRequests)

(***************************************************************************
  TERMINAL EXACTLY ONCE. The ghost counter changes only on a guarded terminal
  transition. Its value and cause agree with every lifecycle state.
*)
TerminalExactlyOnce ==
  /\ \A r \in Requests :
       (status[r] \in TerminalStatus) <=> (terminalCount[r] = 1)
  /\ \A r \in Requests :
       (status[r] \notin TerminalStatus) =>
         /\ terminalCount[r] = 0
         /\ terminalCause[r] = "None"
  /\ \A r \in Requests :
       status[r] = "Succeeded" => terminalCause[r] = "End"
  /\ \A r \in Requests :
       status[r] = "Failed" =>
         terminalCause[r] \in {"Cancel", "Disconnect", "Deadline"}

(***************************************************************************
  Active cancel/disconnect/deadline failure records the exact lane generation
  as retired. A reconnect may make that lane up again only at a newer gen.
*)
FailureRetiresLane ==
  \A r \in Requests :
    (status[r] = "Failed" /\ wasActive[r]) =>
      [lane |-> reqLane[r], gen |-> reqGen[r]] \in retired

(***************************************************************************
  NO LATE WRITES INTO RECYCLED DESTINATIONS. Immediately after any direct
  receive, its lease owner and epoch are still current. Terminalization clears
  the lease before another admission can increment and recycle the epoch.
*)
NoLateWritesIntoRecycledDestinations ==
  justDelivery /= NONE =>
    /\ status[justDelivery.req] = "Active"
    /\ leaseValid[justDelivery.req]
    /\ reqDest[justDelivery.req] = justDelivery.dest
    /\ reqDestEpoch[justDelivery.req] = justDelivery.epoch
    /\ destOwner[justDelivery.dest] = justDelivery.req
    /\ destEpoch[justDelivery.dest] = justDelivery.epoch

(***************************************************************************
  GENERATION-SAFE DELIVERY. A page write is correlated by lane, connection
  generation, and request id. A frame retained in frames from a retired
  generation cannot satisfy these equalities after reconnect.
*)
GenerationSafeDelivery ==
  justDelivery /= NONE =>
    /\ connUp[justDelivery.lane]
    /\ connGen[justDelivery.lane] = justDelivery.gen
    /\ laneReq[justDelivery.lane] = justDelivery.req
    /\ reqLane[justDelivery.req] = justDelivery.lane
    /\ reqGen[justDelivery.req] = justDelivery.gen
    /\ reqId[justDelivery.req] = justDelivery.id

(***************************************************************************
  SEND_ZC RELEASE SAFETY. A started source is held through both submission and
  byte-count completion. It becomes released only in the Notified state.
*)
SendZcReleaseSafety ==
  /\ \A s \in Sends :
       sendStatus[s] = "Free" =>
         /\ sendReq[s] = NONE
         /\ ~sourceHeld[s]
  /\ \A s \in Sends :
       sendStatus[s] \in {"Submitted", "BytesComplete"} =>
         /\ sendReq[s] \in Requests
         /\ sourceHeld[s]
  /\ \A s \in Sends :
       sendStatus[s] = "Notified" =>
         /\ sendReq[s] \in Requests
         /\ ~sourceHeld[s]
  /\ \A s \in Sends :
       (sendReq[s] \in Requests /\ ~sourceHeld[s]) =>
         sendStatus[s] = "Notified"

=============================================================================

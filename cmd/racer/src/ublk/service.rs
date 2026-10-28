//! Per-queue ublk request serving.
//!
//! Each tag of a queue runs one loop: send FETCH, wait for a request, serve
//! it, then send COMMIT_AND_FETCH with the result. The loop ends when the
//! kernel aborts the tag with `UBLK_IO_RES_ABORT`. The kernel side and the
//! page source are traits so tests can drive the loop without a device or a
//! reactor.
//!
//! Once the kernel hands a request to a tag, only an answer from that tag
//! completes it, and STOP_DEV waits for it. So a tag never gives up on a
//! request: a command the reactor refuses is retried with a short pause, and
//! an abandoned queue answers every request with `EIO` at once instead of
//! dropping it.

use std::cell::{Cell, RefCell};
use std::future::Future;
use std::ops::Range;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};

use futures::stream::{FuturesUnordered, StreamExt};

use super::queue::{Action, Request, Span, io_cmd_bytes, spans};
use super::uapi::{
    IoDesc, U_IO_COMMIT_AND_FETCH_REQ, U_IO_FETCH_REQ, UBLK_IO_RES_ABORT, UBLK_IO_RES_OK, ublk_pos,
};
use crate::error::{Error, Result};

/// Boxed local future used by the ublk traits.
pub(crate) type Local<'a, T> = Pin<Box<dyn Future<Output = T> + 'a>>;

/// Immutable page bytes that can be written into a request.
///
/// The worker hands these bytes to the reactor as a `SendBuffer`, so every
/// implementation carries that trait's backing-storage contract.
///
/// # Safety
/// `bytes` must return the same initialized allocation, at the same address
/// and length, for the owner and every clone, across moves of any of them.
/// Inline storage such as `[u8; N]` cannot qualify. No alias may mutate,
/// resize, or free the bytes while any owner or clone is alive.
pub(crate) unsafe trait PageBytes: Clone + 'static {
    fn bytes(&self) -> &[u8];
}

// SAFETY: the payload lives behind an `Arc` shared by all clones; moving the
// handle moves only the pointer, and verified payloads are never mutated or
// recycled while a handle holds the `Arc`.
unsafe impl PageBytes for crate::memory::VerifiedPage {
    fn bytes(&self) -> &[u8] {
        crate::memory::VerifiedPage::bytes(self)
    }
}

/// Bytes past the object end. One static buffer serves every zero span.
#[derive(Clone, Copy)]
pub(crate) struct Zeros;

/// Size of the shared zero buffer. Longer zero spans are written in pieces.
pub(crate) const ZERO_BYTES: usize = 1 << 20;

static ZERO_BUFFER: [u8; ZERO_BYTES] = [0; ZERO_BYTES];

// SAFETY: every value returns the same immutable `static` buffer.
unsafe impl PageBytes for Zeros {
    fn bytes(&self) -> &[u8] {
        &ZERO_BUFFER
    }
}

/// Pages of the one pinned object version behind a device.
pub(crate) trait PageSource: 'static {
    type Page: PageBytes;

    /// Get cache page `number` of the pinned version.
    fn page(&self, number: u64) -> Local<'_, Result<Self::Page>>;
}

/// The kernel side of one queue.
pub(crate) trait QueueKernel: 'static {
    /// Send a queue command and return the raw CQE result. The command must
    /// be submitted by the first poll: an `Err` from that poll means the
    /// kernel never saw it, and the caller sends it again.
    fn command(&self, cmd_op: u32, cmd: [u8; 16]) -> Local<'_, Result<i32>>;

    /// Wait a little before retry number `attempt` of a refused command.
    fn pause(&self, attempt: u32) -> Local<'_, ()>;

    /// Read a fetched tag's descriptor.
    fn descriptor(&self, tag: u16) -> IoDesc;

    /// Write `page[range]` into the request at char device position `pos`.
    /// Returns the bytes written, which may be short.
    fn write<P: PageBytes>(
        &self,
        page: P,
        range: Range<usize>,
        pos: u64,
    ) -> Local<'_, Result<usize>>;
}

/// Fixed facts about one device queue.
#[derive(Clone, Copy, Debug)]
pub(crate) struct QueueShape {
    pub q: u16,
    pub depth: u16,
    pub max_io_bytes: u32,
    /// Object length in bytes. Device bytes past it read as zero.
    pub length: u64,
    pub page_bytes: u64,
}

impl QueueShape {
    fn pages(&self) -> u64 {
        self.length.div_ceil(self.page_bytes)
    }
}

/// Why a tag loop ended.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum TagExit {
    /// The kernel aborted the tag, normally because the device stopped.
    Aborted,
    /// A command could not be sent or the kernel rejected it.
    Failed(Error),
}

/// Pages a sequential reader is about to need.
#[derive(Default)]
pub(crate) struct Readahead {
    wanted: RefCell<Vec<u64>>,
    last: RefCell<Option<u64>>,
}

/// Start the next page once a read reaches this fraction of the current one.
const READAHEAD_NUMERATOR: u64 = 3;
const READAHEAD_DENOMINATOR: u64 = 4;

/// At most this many readahead pages are fetched at once per queue.
pub(crate) const READAHEAD_IN_FLIGHT: usize = 2;

impl Readahead {
    /// Note a read that ended at `page_offset + len` within page `number`.
    fn observe(&self, shape: &QueueShape, number: u64, end_in_page: u64) {
        if end_in_page * READAHEAD_DENOMINATOR < shape.page_bytes * READAHEAD_NUMERATOR {
            return;
        }
        let next = number + 1;
        if next >= shape.pages() || *self.last.borrow() == Some(next) {
            return;
        }
        *self.last.borrow_mut() = Some(next);
        let mut wanted = self.wanted.borrow_mut();
        if wanted.len() < READAHEAD_IN_FLIGHT * 2 {
            wanted.push(next);
        }
    }

    fn take(&self) -> Option<u64> {
        let mut wanted = self.wanted.borrow_mut();
        (!wanted.is_empty()).then(|| wanted.remove(0))
    }
}

/// Serve one read into the request buffer. Returns the byte count to commit.
async fn serve_read<K: QueueKernel, S: PageSource>(
    kernel: &K,
    source: &S,
    shape: &QueueShape,
    readahead: &Readahead,
    tag: u16,
    offset: u64,
    len: u32,
) -> Result<i32> {
    let parts = spans(offset, len, shape.length, shape.page_bytes);
    if let Some(Span::Page {
        number,
        page_offset,
        len,
        ..
    }) = parts
        .iter()
        .rev()
        .find(|span| matches!(span, Span::Page { .. }))
    {
        readahead.observe(shape, *number, page_offset + u64::from(*len));
    }
    for span in parts {
        match span {
            Span::Page {
                number,
                page_offset,
                len,
                request_offset,
            } => {
                let page = source.page(number).await?;
                let start = usize::try_from(page_offset).map_err(|_| Error::InvalidRange)?;
                let end = start + len as usize;
                if page.bytes().len() < end {
                    return Err(Error::InvalidRange);
                }
                write_all(kernel, shape, tag, page, start..end, request_offset).await?;
            }
            Span::Zero {
                request_offset,
                len,
            } => {
                let mut done = 0u32;
                while done < len {
                    let take = (len - done).min(ZERO_BYTES as u32);
                    write_all(
                        kernel,
                        shape,
                        tag,
                        Zeros,
                        0..take as usize,
                        request_offset + done,
                    )
                    .await?;
                    done += take;
                }
            }
        }
    }
    i32::try_from(len).map_err(|_| Error::InvalidRange)
}

/// Write the whole range, continuing after short writes.
async fn write_all<K: QueueKernel, P: PageBytes>(
    kernel: &K,
    shape: &QueueShape,
    tag: u16,
    page: P,
    range: Range<usize>,
    request_offset: u32,
) -> Result<()> {
    let mut written = 0usize;
    let length = range.len();
    while written < length {
        let at = request_offset
            .checked_add(u32::try_from(written).map_err(|_| Error::InvalidRange)?)
            .ok_or(Error::InvalidRange)?;
        let bytes = kernel
            .write(
                page.clone(),
                range.start + written..range.end,
                ublk_pos(shape.q, tag, at),
            )
            .await?;
        if bytes == 0 || bytes > length - written {
            return Err(Error::Io);
        }
        written += bytes;
    }
    Ok(())
}

/// After this many refused attempts in a row, a command's error is
/// reported as a queue failure so the device is torn down. The tag keeps
/// retrying.
pub(crate) const REPORT_AFTER: u32 = 32;

/// Once the queue is abandoned, a command is given up after this many
/// refused attempts in a row. The request it answers is then left to the
/// kernel, which aborts it when the character device is released.
pub(crate) const ABANDONED_ATTEMPTS: u32 = 64;

/// State shared by the tag loops of one queue.
#[derive(Default)]
struct TagState {
    /// Answer every request with `EIO` and stop reading pages.
    abandoned: Cell<bool>,
    /// Tags holding a request whose answer the kernel has not accepted yet.
    owned: Cell<usize>,
    /// First failure seen by any tag.
    failure: Cell<Option<Error>>,
    /// Wakers of tags waiting for a page, woken on abandon.
    wakers: RefCell<Vec<Option<Waker>>>,
}

impl TagState {
    fn fail(&self, error: Error) {
        if self.failure.get().is_none() {
            self.failure.set(Some(error));
        }
    }

    /// Ready once the queue is abandoned.
    fn abandoned(&self, tag: u16) -> impl Future<Output = ()> + '_ {
        std::future::poll_fn(move |cx| {
            if self.abandoned.get() {
                return Poll::Ready(());
            }
            let mut wakers = self.wakers.borrow_mut();
            let slot = usize::from(tag);
            if wakers.len() <= slot {
                wakers.resize(slot + 1, None);
            }
            wakers[slot] = Some(cx.waker().clone());
            Poll::Pending
        })
    }

    fn abandon(&self) {
        self.abandoned.set(true);
        for waker in self.wakers.borrow_mut().iter_mut().filter_map(Option::take) {
            waker.wake();
        }
    }
}

/// Send one command until the kernel has it. A command refused before
/// submission is sent again after a pause. `owns` says the tag holds a
/// request this command answers; it stops counting as owned once the
/// kernel accepts the command. Returns the CQE result, or the last error
/// if the command was given up.
async fn send<K: QueueKernel>(
    kernel: &K,
    state: &TagState,
    cmd_op: u32,
    cmd: [u8; 16],
    owns: bool,
) -> Result<i32> {
    let release = || {
        if owns {
            state.owned.set(state.owned.get() - 1);
        }
    };
    let mut attempt = 0_u32;
    loop {
        let mut command = kernel.command(cmd_op, cmd);
        let mut submitted = false;
        let result = std::future::poll_fn(|cx| {
            let poll = command.as_mut().poll(cx);
            if !submitted && !matches!(poll, Poll::Ready(Err(_))) {
                // Pending, or done at once: either way the kernel has it.
                submitted = true;
                release();
            }
            poll
        })
        .await;
        match result {
            Ok(res) => return Ok(res),
            Err(error) if submitted => return Err(error),
            Err(error) => {
                attempt += 1;
                if attempt == REPORT_AFTER {
                    state.fail(error);
                }
                if state.abandoned.get() && attempt >= ABANDONED_ATTEMPTS {
                    state.fail(error);
                    release();
                    return Err(error);
                }
                drop(command);
                kernel.pause(attempt).await;
            }
        }
    }
}

/// Run one tag until the kernel aborts it.
async fn serve_tag<K: QueueKernel, S: PageSource>(
    kernel: Rc<K>,
    source: Rc<S>,
    shape: QueueShape,
    readahead: Rc<Readahead>,
    state: Rc<TagState>,
    tag: u16,
) -> TagExit {
    let mut cmd_op = U_IO_FETCH_REQ;
    let mut result = 0;
    loop {
        let owns = cmd_op == U_IO_COMMIT_AND_FETCH_REQ;
        let cmd = io_cmd_bytes(shape.q, tag, result);
        let res = match send(&*kernel, &state, cmd_op, cmd, owns).await {
            Ok(res) => res,
            Err(error) => return TagExit::Failed(error),
        };
        if res == UBLK_IO_RES_ABORT {
            return TagExit::Aborted;
        }
        if res != UBLK_IO_RES_OK {
            // The kernel refused the command, so this tag owns no request.
            return TagExit::Failed(if res < 0 { Error::Os(-res) } else { Error::Io });
        }
        state.owned.set(state.owned.get() + 1);
        result = if state.abandoned.get() {
            -libc::EIO
        } else {
            match Request::decode(kernel.descriptor(tag), shape.max_io_bytes) {
                Action::Complete(code) => code,
                Action::Read { offset, len } => {
                    let read = serve_read(&*kernel, &*source, &shape, &readahead, tag, offset, len);
                    let abandoned = state.abandoned(tag);
                    futures::pin_mut!(read, abandoned);
                    match futures::future::select(read, abandoned).await {
                        futures::future::Either::Left((result, _)) => result.unwrap_or(-libc::EIO),
                        futures::future::Either::Right(((), _)) => -libc::EIO,
                    }
                }
            }
        };
        cmd_op = U_IO_COMMIT_AND_FETCH_REQ;
    }
}

/// State of a queue after a poll.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum QueueState {
    Running,
    /// Every tag ended. `failure` is the first tag failure, if any.
    Finished {
        failure: Option<Error>,
    },
}

/// All tag loops of one device queue plus its readahead.
pub(crate) struct QueueService<K: QueueKernel, S: PageSource> {
    source: Rc<S>,
    readahead: Rc<Readahead>,
    tags: FuturesUnordered<Local<'static, TagExit>>,
    prefetch: FuturesUnordered<Local<'static, ()>>,
    state: Rc<TagState>,
    _kernel: Rc<K>,
}

impl<K: QueueKernel, S: PageSource> QueueService<K, S> {
    /// Start a loop for every tag. Nothing is sent until the first poll.
    pub(crate) fn new(kernel: Rc<K>, source: Rc<S>, shape: QueueShape) -> Self {
        let readahead = Rc::new(Readahead::default());
        let state = Rc::new(TagState::default());
        let tags = FuturesUnordered::new();
        for tag in 0..shape.depth {
            tags.push(Box::pin(serve_tag(
                kernel.clone(),
                source.clone(),
                shape,
                readahead.clone(),
                state.clone(),
                tag,
            )) as Local<'static, TagExit>);
        }
        Self {
            source,
            readahead,
            tags,
            prefetch: FuturesUnordered::new(),
            state,
            _kernel: kernel,
        }
    }

    /// Stop serving reads: answer every request held now or handed out
    /// later with `EIO`. The tag loops still run until the kernel aborts
    /// them, which STOP_DEV does.
    pub(crate) fn abandon(&mut self) {
        self.state.abandon();
        self.prefetch.clear();
    }

    /// Tags holding a request whose answer the kernel has not accepted.
    pub(crate) fn owned(&self) -> usize {
        self.state.owned.get()
    }

    /// Advance tag loops and readahead.
    pub(crate) fn poll(&mut self, cx: &mut Context<'_>) -> QueueState {
        while let Poll::Ready(Some(exit)) = self.tags.poll_next_unpin(cx) {
            if let TagExit::Failed(error) = exit {
                self.state.fail(error);
            }
        }
        if self.tags.is_empty() {
            self.prefetch.clear();
            return QueueState::Finished {
                failure: self.state.failure.get(),
            };
        }
        if self.state.abandoned.get() {
            return QueueState::Running;
        }
        while self.prefetch.len() < READAHEAD_IN_FLIGHT {
            let Some(number) = self.readahead.take() else {
                break;
            };
            let source = self.source.clone();
            self.prefetch.push(Box::pin(async move {
                // Readahead is a hint. A failure here shows up on the real read.
                let _ = source.page(number).await;
            }));
        }
        while let Poll::Ready(Some(())) = self.prefetch.poll_next_unpin(cx) {}
        QueueState::Running
    }

    /// Whether any tag has failed so far.
    pub(crate) fn failure(&self) -> Option<Error> {
        self.state.failure.get()
    }
}

#[cfg(test)]
mod tests {
    //! Drive tag loops against an in-memory kernel and page source.

    use super::*;
    use crate::ublk::uapi::{
        IoCmd, UBLK_IO_OP_FLUSH, UBLK_IO_OP_READ, UBLK_IO_OP_WRITE, UBLK_QID_OFF, UBLK_TAG_OFF,
        UBLKSRV_IO_BUF_OFFSET,
    };
    use futures::channel::oneshot;
    use std::cell::{Cell, RefCell};
    use std::collections::HashMap;

    const PAGE: u64 = 4096;
    const LENGTH: u64 = 3 * PAGE + 100;

    fn byte(at: u64) -> u8 {
        (at % 251) as u8
    }

    #[derive(Clone)]
    struct TestPage(Rc<Vec<u8>>);

    // SAFETY: bytes live in a heap `Vec` behind an `Rc` that is never
    // mutated after construction, so moves and clones keep the address.
    unsafe impl PageBytes for TestPage {
        fn bytes(&self) -> &[u8] {
            &self.0
        }
    }

    #[test]
    fn page_bytes_stable_across_moves() {
        let page = TestPage(Rc::new(vec![7; 64]));
        let before = PageBytes::bytes(&page).as_ptr();
        let clone = page.clone();
        let moved = Box::new(page);
        assert_eq!(PageBytes::bytes(&*moved).as_ptr(), before);
        assert_eq!(PageBytes::bytes(&clone).as_ptr(), before);
        let zeros = Box::new(Zeros);
        assert_eq!(
            PageBytes::bytes(&*zeros).as_ptr(),
            PageBytes::bytes(&Zeros).as_ptr()
        );
    }

    #[derive(Default)]
    struct Source {
        calls: RefCell<Vec<u64>>,
        fail: Cell<bool>,
        /// Pages never arrive.
        hang: Cell<bool>,
    }

    impl PageSource for Source {
        type Page = TestPage;

        fn page(&self, number: u64) -> Local<'_, Result<TestPage>> {
            self.calls.borrow_mut().push(number);
            let fail = self.fail.get();
            if self.hang.get() {
                return Box::pin(futures::future::pending());
            }
            Box::pin(async move {
                if fail {
                    return Err(Error::Unavailable);
                }
                let start = number * PAGE;
                let end = (start + PAGE).min(LENGTH);
                Ok(TestPage(Rc::new((start..end).map(byte).collect())))
            })
        }
    }

    struct Command {
        op: u32,
        cmd: IoCmd,
        reply: oneshot::Sender<Result<i32>>,
    }

    #[derive(Default)]
    struct Kernel {
        commands: RefCell<Vec<Command>>,
        descs: RefCell<HashMap<u16, IoDesc>>,
        /// Request buffers by tag.
        buffers: RefCell<HashMap<u16, Vec<u8>>>,
        /// Largest write accepted at once, to exercise short writes.
        max_write: Cell<usize>,
        /// Refuse this many commands before submission, like a full reactor.
        refuse: Cell<u32>,
        /// Commands refused so far.
        refused: Cell<u32>,
        pauses: Cell<u32>,
    }

    impl QueueKernel for Kernel {
        fn command(&self, cmd_op: u32, cmd: [u8; 16]) -> Local<'_, Result<i32>> {
            if self.refuse.get() > 0 {
                self.refuse.set(self.refuse.get() - 1);
                self.refused.set(self.refused.get() + 1);
                return Box::pin(async { Err(Error::Overloaded) });
            }
            let (reply, done) = oneshot::channel();
            let cmd = IoCmd {
                q_id: u16::from_ne_bytes([cmd[0], cmd[1]]),
                tag: u16::from_ne_bytes([cmd[2], cmd[3]]),
                result: i32::from_ne_bytes([cmd[4], cmd[5], cmd[6], cmd[7]]),
                addr: u64::from_ne_bytes(cmd[8..16].try_into().unwrap()),
            };
            self.commands.borrow_mut().push(Command {
                op: cmd_op,
                cmd,
                reply,
            });
            Box::pin(async move { done.await.unwrap_or(Err(Error::Cancelled)) })
        }

        fn descriptor(&self, tag: u16) -> IoDesc {
            self.descs.borrow()[&tag]
        }

        fn pause(&self, _attempt: u32) -> Local<'_, ()> {
            self.pauses.set(self.pauses.get() + 1);
            Box::pin(async {})
        }

        fn write<P: PageBytes>(
            &self,
            page: P,
            range: Range<usize>,
            pos: u64,
        ) -> Local<'_, Result<usize>> {
            let rel = pos - UBLKSRV_IO_BUF_OFFSET;
            assert_eq!(rel >> UBLK_QID_OFF, 1, "queue id");
            let tag = ((rel >> UBLK_TAG_OFF) & 0xffff) as u16;
            let offset = (rel & ((1 << UBLK_TAG_OFF) - 1)) as usize;
            let take = range.len().min(self.max_write.get().max(1));
            let data = page.bytes()[range.start..range.start + take].to_vec();
            let mut buffers = self.buffers.borrow_mut();
            let buffer = buffers.entry(tag).or_default();
            if buffer.len() < offset + take {
                buffer.resize(offset + take, 0xaa);
            }
            buffer[offset..offset + take].copy_from_slice(&data);
            Box::pin(async move { Ok(take) })
        }
    }

    impl Kernel {
        fn take(&self, tag: u16) -> Command {
            let mut commands = self.commands.borrow_mut();
            let at = commands
                .iter()
                .position(|c| c.cmd.tag == tag)
                .expect("tag has a pending command");
            commands.remove(at)
        }

        fn request(&self, tag: u16, op: u8, offset: u64, len: u32) {
            self.descs.borrow_mut().insert(
                tag,
                IoDesc {
                    op_flags: u32::from(op),
                    nr_sectors: len / 512,
                    start_sector: offset / 512,
                    addr: 0,
                },
            );
            self.buffers.borrow_mut().remove(&tag);
            let command = self.take(tag);
            command.reply.send(Ok(UBLK_IO_RES_OK)).ok().unwrap();
        }
    }

    struct Harness {
        kernel: Rc<Kernel>,
        source: Rc<Source>,
        queue: QueueService<Kernel, Source>,
    }

    impl Harness {
        fn new(depth: u16) -> Self {
            let kernel = Rc::new(Kernel::default());
            kernel.max_write.set(usize::MAX);
            let source = Rc::new(Source::default());
            let shape = QueueShape {
                q: 1,
                depth,
                max_io_bytes: 8 * PAGE as u32,
                length: LENGTH,
                page_bytes: PAGE,
            };
            let queue = QueueService::new(kernel.clone(), source.clone(), shape);
            Self {
                kernel,
                source,
                queue,
            }
        }

        fn poll(&mut self) -> QueueState {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let mut state = self.queue.poll(&mut cx);
            for _ in 0..8 {
                state = self.queue.poll(&mut cx);
            }
            state
        }

        /// Serve one request on `tag` and return the committed result.
        fn serve(&mut self, tag: u16, op: u8, offset: u64, len: u32) -> i32 {
            self.kernel.request(tag, op, offset, len);
            assert_eq!(self.poll(), QueueState::Running);
            let commands = self.kernel.commands.borrow();
            let next = commands.iter().find(|c| c.cmd.tag == tag).unwrap();
            assert_eq!(next.op, U_IO_COMMIT_AND_FETCH_REQ);
            assert_eq!(next.cmd.addr, 0);
            next.cmd.result
        }

        fn buffer(&self, tag: u16) -> Vec<u8> {
            self.kernel
                .buffers
                .borrow()
                .get(&tag)
                .cloned()
                .unwrap_or_default()
        }
    }

    fn expected(offset: u64, len: u64) -> Vec<u8> {
        (offset..offset + len)
            .map(|at| if at < LENGTH { byte(at) } else { 0 })
            .collect()
    }

    #[test]
    fn every_tag_fetches_on_first_poll() {
        let mut harness = Harness::new(3);
        assert_eq!(harness.poll(), QueueState::Running);
        let commands = harness.kernel.commands.borrow();
        assert_eq!(commands.len(), 3);
        for (tag, command) in commands.iter().enumerate() {
            assert_eq!(command.op, U_IO_FETCH_REQ);
            assert_eq!(command.cmd.q_id, 1);
            assert_eq!(command.cmd.tag, tag as u16);
        }
    }

    #[test]
    fn reads_cross_pages_and_zero_fill_past_the_end() {
        let mut harness = Harness::new(2);
        harness.poll();
        // Crosses page 0 into page 1.
        let len = 2 * PAGE as u32;
        assert_eq!(harness.serve(0, UBLK_IO_OP_READ, 2048, len), len as i32);
        assert_eq!(harness.buffer(0), expected(2048, u64::from(len)));
        // The last page is short; the rest of the device sector reads as zero.
        let offset = 3 * PAGE;
        let len = 1024;
        assert_eq!(harness.serve(1, UBLK_IO_OP_READ, offset, len), len as i32);
        assert_eq!(harness.buffer(1), expected(offset, u64::from(len)));
        // Entirely past the object end.
        assert_eq!(harness.serve(0, UBLK_IO_OP_READ, 4 * PAGE, 512), 512);
        assert_eq!(harness.buffer(0), vec![0; 512]);
    }

    #[test]
    fn short_writes_continue() {
        let mut harness = Harness::new(1);
        harness.kernel.max_write.set(700);
        harness.poll();
        let len = PAGE as u32 + 1536;
        assert_eq!(harness.serve(0, UBLK_IO_OP_READ, 512, len), len as i32);
        assert_eq!(harness.buffer(0), expected(512, u64::from(len)));
    }

    #[test]
    fn non_reads_and_failures_map_to_errnos() {
        let mut harness = Harness::new(1);
        harness.poll();
        assert_eq!(harness.serve(0, UBLK_IO_OP_FLUSH, 0, 0), 0);
        assert_eq!(harness.serve(0, UBLK_IO_OP_WRITE, 0, 512), -libc::EROFS);
        assert_eq!(harness.serve(0, 0x7f, 0, 512), -libc::EOPNOTSUPP);
        // Larger than max_io_bytes.
        assert_eq!(
            harness.serve(0, UBLK_IO_OP_READ, 0, 16 * PAGE as u32),
            -libc::EIO
        );
        harness.source.fail.set(true);
        assert_eq!(harness.serve(0, UBLK_IO_OP_READ, 0, 512), -libc::EIO);
        harness.source.fail.set(false);
        assert_eq!(harness.serve(0, UBLK_IO_OP_READ, 0, 512), 512);
    }

    #[test]
    fn abort_finishes_the_queue() {
        let mut harness = Harness::new(2);
        harness.poll();
        harness.serve(0, UBLK_IO_OP_READ, 0, 512);
        for tag in 0..2 {
            let command = harness.kernel.take(tag);
            command.reply.send(Ok(UBLK_IO_RES_ABORT)).ok().unwrap();
        }
        assert_eq!(harness.poll(), QueueState::Finished { failure: None });
    }

    #[test]
    fn refused_commit_is_retried_and_the_request_answered() {
        let mut harness = Harness::new(1);
        harness.poll();
        harness.kernel.refuse.set(3);
        harness.kernel.request(0, UBLK_IO_OP_READ, 0, 512);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert_eq!(harness.queue.poll(&mut cx), QueueState::Running);
        assert_eq!(harness.kernel.refused.get(), 3);
        assert_eq!(harness.kernel.pauses.get(), 3);
        let commands = harness.kernel.commands.borrow();
        assert_eq!(commands.len(), 1, "the tag was not dropped");
        assert_eq!(commands[0].op, U_IO_COMMIT_AND_FETCH_REQ);
        assert_eq!(commands[0].cmd.result, 512);
        drop(commands);
        assert_eq!(harness.queue.owned(), 0);
        assert_eq!(
            harness.queue.failure(),
            None,
            "a retried refusal is no failure"
        );
    }

    #[test]
    fn long_refusal_is_reported_but_still_retried() {
        let mut harness = Harness::new(1);
        harness.poll();
        harness.kernel.refuse.set(REPORT_AFTER + 5);
        harness.kernel.request(0, UBLK_IO_OP_FLUSH, 0, 0);
        assert_eq!(harness.poll(), QueueState::Running);
        assert_eq!(harness.queue.failure(), Some(Error::Overloaded));
        assert_eq!(harness.kernel.commands.borrow().len(), 1);
        assert_eq!(harness.queue.owned(), 0);
    }

    #[test]
    fn abandoned_queue_gives_up_a_refused_command_eventually() {
        let mut harness = Harness::new(1);
        harness.poll();
        harness.queue.abandon();
        harness.kernel.refuse.set(u32::MAX);
        harness.kernel.request(0, UBLK_IO_OP_READ, 0, 512);
        assert_eq!(
            harness.poll(),
            QueueState::Finished {
                failure: Some(Error::Overloaded)
            }
        );
        assert_eq!(harness.kernel.refused.get(), ABANDONED_ATTEMPTS);
        assert_eq!(harness.queue.owned(), 0);
    }

    #[test]
    fn abandon_answers_a_pending_read_with_eio() {
        let mut harness = Harness::new(2);
        harness.poll();
        harness.source.hang.set(true);
        harness.kernel.request(0, UBLK_IO_OP_READ, 0, 512);
        assert_eq!(harness.poll(), QueueState::Running);
        assert_eq!(harness.queue.owned(), 1, "the read is waiting for a page");
        assert!(
            harness
                .kernel
                .commands
                .borrow()
                .iter()
                .all(|c| c.cmd.tag != 0)
        );
        harness.queue.abandon();
        assert_eq!(harness.poll(), QueueState::Running);
        assert_eq!(harness.queue.owned(), 0);
        let command = harness.kernel.take(0);
        assert_eq!(command.op, U_IO_COMMIT_AND_FETCH_REQ);
        assert_eq!(command.cmd.result, -libc::EIO);
        // Later requests are answered with EIO without reading pages.
        command.reply.send(Ok(UBLK_IO_RES_ABORT)).ok().unwrap();
        harness.source.hang.set(false);
        let calls = harness.source.calls.borrow().len();
        assert_eq!(harness.serve(1, UBLK_IO_OP_READ, 0, 512), -libc::EIO);
        assert_eq!(harness.source.calls.borrow().len(), calls);
        // The tags still finish only when the kernel aborts them.
        harness
            .kernel
            .take(1)
            .reply
            .send(Ok(UBLK_IO_RES_ABORT))
            .ok()
            .unwrap();
        assert_eq!(harness.poll(), QueueState::Finished { failure: None });
    }

    #[test]
    fn command_failures_finish_with_the_first_error() {
        let mut harness = Harness::new(2);
        harness.poll();
        harness
            .kernel
            .take(0)
            .reply
            .send(Err(Error::Overloaded))
            .ok()
            .unwrap();
        assert_eq!(harness.poll(), QueueState::Running);
        assert_eq!(harness.queue.failure(), Some(Error::Overloaded));
        harness
            .kernel
            .take(1)
            .reply
            .send(Ok(-libc::EINVAL))
            .ok()
            .unwrap();
        assert_eq!(
            harness.poll(),
            QueueState::Finished {
                failure: Some(Error::Overloaded)
            }
        );
    }

    #[test]
    fn sequential_reads_prefetch_the_next_page_once() {
        let mut harness = Harness::new(1);
        harness.poll();
        // Ends in the first quarter of page 0: no readahead.
        harness.serve(0, UBLK_IO_OP_READ, 0, 1024);
        assert_eq!(*harness.source.calls.borrow(), vec![0]);
        // Ends in the last quarter of page 0: page 1 is fetched early.
        harness.serve(0, UBLK_IO_OP_READ, 2048, 1024);
        assert_eq!(*harness.source.calls.borrow(), vec![0, 0, 1]);
        // The same trigger does not fetch page 1 again.
        harness.serve(0, UBLK_IO_OP_READ, 3072, 512);
        assert_eq!(*harness.source.calls.borrow(), vec![0, 0, 1, 0]);
        // The last page has no successor.
        harness.serve(0, UBLK_IO_OP_READ, 3 * PAGE, 512);
        assert_eq!(*harness.source.calls.borrow(), vec![0, 0, 1, 0, 3]);
    }
}

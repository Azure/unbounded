//! Opt-in aliasing diagnostic, not a production test or proposed fix.
use std::ops::{DerefMut, Range};

struct Buffer<S>(S);
struct BufferRange<S> {
    buffer: Buffer<S>,
    range: Range<usize>,
}
struct InFlight<S> {
    buffer: BufferRange<S>,
}

impl<S: DerefMut<Target = [u8]>> BufferRange<S> {
    fn bytes_mut(&mut self) -> &mut [u8] {
        &mut self.buffer.0[self.range.clone()]
    }
}

// Match the by-value completion handoff and nested boxed closure in the reactor.
fn submit<S: 'static>(
    finish: impl FnOnce() -> InFlight<S> + 'static,
) -> Box<dyn FnOnce() -> InFlight<S>> {
    Box::new(move || finish())
}

fn exercise<S: DerefMut<Target = [u8]> + 'static>(storage: S, before: bool, read: bool) {
    let mut owned = InFlight {
        buffer: BufferRange {
            buffer: Buffer(storage),
            range: 1..2,
        },
    };
    let ptr = owned.buffer.bytes_mut().as_mut_ptr();
    if before {
        // Control: access before any subsequent owner move.
        unsafe { ptr.write(7) };
    }
    let completion = submit(move || {
        let InFlight { buffer } = owned;
        InFlight { buffer }
    });
    if !before {
        // Deliberately test the disputed pointer use, while allocation is alive.
        // No kernel, concurrency, deallocation, or pointer/integer conversion.
        if read {
            assert_eq!(unsafe { ptr.read() }, 0);
        } else {
            unsafe { ptr.write(7) };
        }
    }
    let owned = completion();
    assert_eq!(&*owned.buffer.buffer.0, &[0, if read { 0 } else { 7 }, 0]);
}

fn main() {
    let case = std::env::args().nth(1).expect("case required");
    match case.as_str() {
        "box-after-write" => exercise(vec![0; 3].into_boxed_slice(), false, false),
        "box-after-read" => exercise(vec![0; 3].into_boxed_slice(), false, true),
        "box-before-write" => exercise(vec![0; 3].into_boxed_slice(), true, false),
        "vec-after-write" => exercise(vec![0; 3], false, false),
        "vec-after-read" => exercise(vec![0; 3], false, true),
        _ => panic!("unknown case: {case}"),
    }
    println!("PASS {case}");
}

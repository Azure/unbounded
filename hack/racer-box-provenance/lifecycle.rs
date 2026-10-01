//! Production-shaped fixed Vec owner and completion/error/cancel handoff proof.
use std::{cell::Cell, rc::Rc};

struct Buffer {
    bytes: Vec<u8>,
    drops: Rc<Cell<usize>>,
}
impl Drop for Buffer {
    fn drop(&mut self) {
        // Production uses zeroize or reservation.recycle; this checks the shape,
        // not the cryptographic guarantees of this ordinary fill.
        self.bytes.as_mut_slice().fill(0);
        assert_eq!(self.bytes.len(), 3);
        self.drops.set(self.drops.get() + 1);
    }
}
struct InFlight(Buffer);
fn submit(
    finish: impl FnOnce(bool) -> Option<Buffer> + 'static,
) -> Box<dyn FnOnce(bool) -> Option<Buffer>> {
    Box::new(move |ok| finish(ok))
}
fn main() {
    // Success, I/O failure, canceled completion, and abandoned waiter after CQE.
    for outcome in 0..4 {
        let drops = Rc::new(Cell::new(0));
        let mut owned = InFlight(Buffer {
            bytes: vec![0; 3],
            drops: drops.clone(),
        });
        let ptr = owned.0.bytes[1..2].as_mut_ptr();
        let finish = submit(move |ok| {
            let InFlight(buffer) = owned;
            if ok { Some(buffer) } else { None }
        });
        // A receive can write before an error/cancellation is observed.
        unsafe { ptr.write(7) };
        assert_eq!(drops.get(), 0);
        // In production original/cancel CQE fences determine when this is called.
        let reply = finish(outcome == 0 || outcome == 3);
        if outcome == 0 || outcome == 3 {
            assert_eq!(reply.as_ref().unwrap().bytes, [0, 7, 0]);
            assert_eq!(drops.get(), 0);
        } else {
            assert!(reply.is_none());
            assert_eq!(drops.get(), 1);
        }
        drop(reply);
        assert_eq!(drops.get(), 1);
    }
    println!("PASS lifecycle");
}

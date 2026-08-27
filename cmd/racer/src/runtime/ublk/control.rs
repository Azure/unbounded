//! ublk control-thread device handling.

use std::io;

use crate::kernel;

use super::{
    CMD_ADD_DEV, CMD_DEL_DEV, CMD_DEL_DEV_ASYNC, CMD_GET_DEV_INFO, CMD_GET_FEATURES,
    CMD_GET_QUEUE_AFFINITY, CMD_SET_PARAMS, CMD_START_DEV, CMD_STOP_DEV, CtrlCmd, DevInfo, Params,
};

/// Synchronous owner of the ublk control device. Lives on the control thread only.
pub(in crate::runtime) struct Control {
    ctl: kernel::UblkControl,
    features: u64,
}

impl Control {
    pub(in crate::runtime) fn open() -> io::Result<Control> {
        let mut c = Control {
            ctl: kernel::ublk_control_open()?,
            features: 0,
        };
        c.features = c.get_features()?;
        Ok(c)
    }

    /// One command, submitted and waited for.
    ///
    /// The driver reads a `ublksrv_ctrl_cmd` out of the command area of the SQE, so that
    /// is what goes across: the struct's bytes, zero padded to the 80 the SQE carries.
    /// Anything larger travels by the `addr` the struct names, which the driver reads and
    /// writes in place.
    fn exec(&mut self, op: u32, cmd: &CtrlCmd) -> io::Result<i32> {
        let mut buf = [0u8; 80];
        // SAFETY: `CtrlCmd` is `repr(C)` and 32 bytes, which is less than the 80 available.
        unsafe {
            std::ptr::copy_nonoverlapping(
                cmd as *const CtrlCmd as *const u8,
                buf.as_mut_ptr(),
                size_of::<CtrlCmd>(),
            )
        };

        kernel::ublk_exec(&mut self.ctl, op, &buf)
    }

    fn get_features(&mut self) -> io::Result<u64> {
        let mut out: u64 = 0;
        let cmd = CtrlCmd {
            dev_id: u32::MAX,
            queue_id: u16::MAX,
            len: 8,
            addr: &mut out as *mut u64 as u64,
            ..Default::default()
        };
        self.exec(CMD_GET_FEATURES, &cmd)?;
        Ok(out)
    }

    pub(in crate::runtime) fn require(&self, want: u64) -> io::Result<()> {
        let missing = want & !self.features;
        if missing != 0 {
            return Err(io::Error::other(format!(
                "ublk driver missing required features {missing:#x} (have {:#x})",
                self.features
            )));
        }
        Ok(())
    }

    fn add_dev(&mut self, info: &mut DevInfo) -> io::Result<u32> {
        let cmd = CtrlCmd {
            dev_id: info.dev_id,
            queue_id: u16::MAX,
            len: size_of::<DevInfo>() as u16,
            addr: info as *mut DevInfo as u64,
            ..Default::default()
        };
        self.exec(CMD_ADD_DEV, &cmd)?;
        Ok(info.dev_id)
    }

    pub(in crate::runtime) fn set_params(&mut self, dev_id: u32, p: &Params) -> io::Result<()> {
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            len: size_of::<Params>() as u16,
            addr: p as *const Params as u64,
            ..Default::default()
        };
        self.exec(CMD_SET_PARAMS, &cmd).map(|_| ())
    }

    /// CPUs bound to queue `q`. Index in `data[0]`, `cpumask` of `len` bytes into `addr`.
    fn queue_affinity(&mut self, dev_id: u32, q: u16) -> io::Result<Vec<usize>> {
        let mut mask = [0u64; 16];
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            len: size_of_val(&mask) as u16,
            addr: mask.as_mut_ptr() as u64,
            data: [q as u64],
            ..Default::default()
        };
        self.exec(CMD_GET_QUEUE_AFFINITY, &cmd)?;
        Ok((0..mask.len() * 64)
            .filter(|b| mask[b / 64] & (1 << (b % 64)) != 0)
            .collect())
    }

    pub(in crate::runtime) fn start_dev(&mut self, dev_id: u32, pid: i32) -> io::Result<()> {
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            data: [pid as u64],
            ..Default::default()
        };
        self.exec(CMD_START_DEV, &cmd).map(|_| ())
    }

    pub(in crate::runtime) fn stop_dev(&mut self, dev_id: u32) -> io::Result<()> {
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            ..Default::default()
        };
        self.exec(CMD_STOP_DEV, &cmd).map(|_| ())
    }

    pub(in crate::runtime) fn del_dev(&mut self, dev_id: u32) -> io::Result<()> {
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            ..Default::default()
        };
        self.exec(CMD_DEL_DEV, &cmd).map(|_| ())
    }

    /// Ask for `dev_id` to go away without waiting for it.
    ///
    /// [`del_dev`](Self::del_dev) does not return until the minor is free, and a minor is
    /// only free once everyone who opened the block device behind it has closed it. A
    /// consumer that is still holding a dead export would therefore park this thread in
    /// the kernel with no way out. This asks for the same removal and comes straight
    /// back, leaving the minor to be freed whenever the last holder lets go.
    pub(in crate::runtime) fn del_dev_async(&mut self, dev_id: u32) -> io::Result<()> {
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            ..Default::default()
        };
        self.exec(CMD_DEL_DEV_ASYNC, &cmd).map(|_| ())
    }

    /// What the kernel holds at `dev_id`, whoever put it there. `ENODEV` if the minor is
    /// free.
    fn dev_info(&mut self, dev_id: u32) -> io::Result<DevInfo> {
        let mut info = DevInfo::default();
        let cmd = CtrlCmd {
            dev_id,
            queue_id: u16::MAX,
            len: size_of::<DevInfo>() as u16,
            addr: &mut info as *mut DevInfo as u64,
            ..Default::default()
        };
        self.exec(CMD_GET_DEV_INFO, &cmd)?;
        Ok(info)
    }
}

/// Create the export at the minor it was named. The minor is not ours to choose: the
/// control plane published `/dev/ublkb<minor>` to whoever consumes it, so a device left at
/// that number by an instance of us that died is reclaimed rather than worked around. One
/// still being served is not: some other program has the number, and stopping it to take
/// the number would be worse than not exporting.
///
/// Reclaiming is a request, not a guarantee. The kernel frees a minor once the last
/// consumer closes the block device that used to be there, so a peer still holding the
/// export our predecessor left behind keeps the number for as long as it likes. We ask,
/// wait [`RECLAIM`] for the holders to let go, and then say plainly that they have not,
/// rather than parking in the kernel until they do.
///
/// `held` is how many devices the caller already has, and only sharpens the message when
/// the kernel's device limit is what turned us away.
pub(in crate::runtime) fn add_dev(
    ctl: &mut Control,
    info: &mut DevInfo,
    held: usize,
) -> io::Result<()> {
    let minor = info.dev_id;
    let taken = match ctl.add_dev(info) {
        Ok(_) => return Ok(()),
        Err(e) if e.raw_os_error() == Some(libc::EEXIST) => e,
        Err(e) => return Err(ublks_max_hint(e, held)),
    };
    // A dead device reports the pid that used to serve it, or none at all.
    let pid = ctl.dev_info(minor).map(|d| d.ublksrv_pid).unwrap_or(0);
    if pid > 0 && kernel::process_alive(pid) {
        return Err(io::Error::other(format!(
            "device {minor} is already exported by pid {pid}: {taken}"
        )));
    }
    ctl.del_dev_async(minor).map_err(|e| {
        io::Error::other(format!(
            "device {minor} is held by a dead export that will not go away: {e}"
        ))
    })?;
    let start = crate::kernel::now();
    loop {
        match ctl.add_dev(info) {
            Err(e)
                if e.raw_os_error() == Some(libc::EEXIST)
                    && crate::kernel::now().saturating_duration_since(start) < RECLAIM =>
            {
                crate::kernel::sleep_blocking(std::time::Duration::from_millis(20));
            }
            Err(e) if e.raw_os_error() == Some(libc::EEXIST) => {
                return Err(io::Error::other(format!(
                    "device {minor} is still open by a consumer of the export that died with \
                     our predecessor; it cannot be exported again until that consumer lets go"
                )));
            }
            Err(e) => return Err(ublks_max_hint(e, held)),
            Ok(_) => return Ok(()),
        }
    }
}

/// How long a minor left behind by a dead export is waited for. Long enough for the
/// kernel to finish a removal nobody is holding up, short enough that a start which
/// cannot have the number says so while the control plane is still watching.
const RECLAIM: std::time::Duration = std::time::Duration::from_secs(5);

/// `ADD_DEV` fails once `ublk_drv.ublks_max` devices exist and the bare errno hides which
/// limit was hit; name the parameter, whose default of 64 is what the runtime sizes its
/// device table against.
fn ublks_max_hint(e: io::Error, held: usize) -> io::Error {
    const PARAM: &str = "/sys/module/ublk_drv/parameters/ublks_max";
    let max = std::fs::read_to_string(PARAM)
        .ok()
        .and_then(|s| s.trim().parse::<usize>().ok());
    match max {
        Some(m) => io::Error::other(format!(
            "ublk ADD_DEV failed with {held} devices already exported: {e}; {PARAM} is {m}, \
             raise it (ublk_drv.ublks_max=) to export more"
        )),
        None => e,
    }
}

/// Map each of `dev_id`'s `nq` hardware queues to a worker sharing its CPU, else to the
/// least loaded; `EINVAL` if every worker is already at `per_worker` queues.
///
/// `workers` lists the logical CPUs each worker owns, in worker order, and the result is
/// indexed the same way.
pub(in crate::runtime) fn assign_queues(
    ctl: &mut Control,
    dev_id: u32,
    nq: usize,
    workers: &[Vec<usize>],
    per_worker: usize,
) -> io::Result<Vec<Vec<u16>>> {
    let n = workers.len();
    let mut out: Vec<Vec<u16>> = vec![Vec::new(); n];
    let mut spill = Vec::new();
    for q in 0..nq as u16 {
        let mask = ctl.queue_affinity(dev_id, q)?;
        let home = workers
            .iter()
            .position(|cpus| mask.iter().any(|cpu| cpus.contains(cpu)));
        match home {
            Some(w) if out[w].len() < per_worker => out[w].push(q),
            _ => spill.push(q),
        }
    }
    for q in spill {
        let w = (0..n).min_by_key(|w| out[*w].len()).unwrap();
        if out[w].len() >= per_worker {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
        out[w].push(q);
    }
    Ok(out)
}

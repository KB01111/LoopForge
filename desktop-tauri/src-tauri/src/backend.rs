//! Lifecycle of the bundled Go backend process.

use std::fs::{self, File, OpenOptions};
use std::io::{Read, Seek, SeekFrom, Write};
use std::net::{SocketAddr, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use crate::config::{app_dir, Config};

const STARTUP_TIMEOUT: Duration = Duration::from_secs(90);

/// A backend process we launched, with the settings it was started with.
struct Running {
    child: Child,
    dsn: String,
    port: u16,
    secret: String,
    /// Owns the process tree; dropping it kills the backend and everything it spawned.
    _job: job::Job,
}

pub struct Backend {
    running: Mutex<Option<Running>>,
    /// Serialises start attempts so two never race for the port.
    lifecycle: Mutex<()>,
    /// Bumped by `stop`; an in-flight start that sees a change cancels itself.
    generation: AtomicU64,
}

fn log_path() -> PathBuf {
    app_dir().join("logs").join("backend.log")
}

fn exe_name() -> &'static str {
    if cfg!(windows) {
        "loopforge-backend.exe"
    } else {
        "loopforge-backend"
    }
}

/// Finds the backend binary: bundled resource first, then the dev build output.
fn find_binary(resource_dir: Option<&Path>) -> Option<PathBuf> {
    if let Some(p) = std::env::var_os("LOOPFORGE_BACKEND_BIN").map(PathBuf::from) {
        if p.exists() {
            return Some(p);
        }
    }
    let mut candidates = Vec::new();
    if let Some(r) = resource_dir {
        candidates.push(r.join("bin").join(exe_name()));
    }
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            candidates.push(dir.join("bin").join(exe_name()));
            candidates.push(dir.join(exe_name()));
        }
    }
    candidates.push(PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("..").join("bin").join(exe_name()));
    candidates.into_iter().find(|p| p.exists())
}

/// True if something already accepts connections on the loopback port.
fn port_in_use(port: u16) -> bool {
    let addr = SocketAddr::from(([127, 0, 0, 1], port));
    TcpStream::connect_timeout(&addr, Duration::from_millis(300)).is_ok()
}

/// GETs `/api/health`; returns the backend pid when the response is `200` with a JSON body.
fn health_pid(port: u16) -> Option<u32> {
    let addr = SocketAddr::from(([127, 0, 0, 1], port));
    let mut s = TcpStream::connect_timeout(&addr, Duration::from_millis(500)).ok()?;
    s.set_read_timeout(Some(Duration::from_secs(2))).ok()?;
    s.set_write_timeout(Some(Duration::from_secs(2))).ok()?;
    s.write_all(b"GET /api/health HTTP/1.0\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n").ok()?;
    let mut raw = Vec::new();
    s.take(4096).read_to_end(&mut raw).ok()?;
    let text = String::from_utf8_lossy(&raw);
    let status_line = text.lines().next()?;
    let mut parts = status_line.split_whitespace();
    if !parts.next()?.starts_with("HTTP/") || parts.next()? != "200" {
        return None;
    }
    let body = text.split("\r\n\r\n").nth(1)?;
    let after = body.split("\"pid\":").nth(1)?.trim_start();
    let digits: String = after.chars().take_while(|c| c.is_ascii_digit()).collect();
    digits.parse().ok()
}

fn terminate(mut r: Running) {
    let _ = r.child.kill();
    let _ = r.child.wait();
    // Dropping `r` closes the job object, which kills any agent processes left behind.
}

impl Backend {
    pub fn new() -> Self {
        Self { running: Mutex::new(None), lifecycle: Mutex::new(()), generation: AtomicU64::new(0) }
    }

    pub fn is_running(&self) -> bool {
        let mut g = self.running.lock().unwrap();
        match g.as_mut() {
            Some(r) => matches!(r.child.try_wait(), Ok(None)),
            None => false,
        }
    }

    /// Starts the backend if needed and blocks until it answers its health check.
    /// A healthy backend started with different settings is replaced.
    pub fn start(&self, cfg: &Config, resource_dir: Option<&Path>) -> Result<(), String> {
        let _life = self.lifecycle.lock().unwrap();
        let generation = self.generation.load(Ordering::SeqCst);
        let dsn = cfg.mysql_dsn.trim().to_string();

        {
            let mut g = self.running.lock().unwrap();
            if let Some(r) = g.as_mut() {
                let alive = matches!(r.child.try_wait(), Ok(None));
                let same = r.dsn == dsn && r.port == cfg.port && r.secret == cfg.session_secret;
                if alive && same && health_pid(cfg.port) == Some(r.child.id()) {
                    return Ok(());
                }
            }
            if let Some(r) = g.take() {
                terminate(r);
            }
        }

        if dsn.is_empty() {
            return Err("No MySQL database is configured yet.".into());
        }
        if port_in_use(cfg.port) {
            return Err(format!(
                "Port {} is already in use by another program. Choose a different port in Settings.",
                cfg.port
            ));
        }
        let bin = find_binary(resource_dir).ok_or_else(|| {
            format!(
                "The LoopForge backend ({}) was not found. Reinstall the app, or run scripts/build-backend.ps1 when developing.",
                exe_name()
            )
        })?;

        let data_dir = app_dir();
        fs::create_dir_all(data_dir.join("logs")).map_err(|e| format!("Cannot create data directory: {e}"))?;
        let log = OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(log_path())
            .map_err(|e| format!("Cannot open backend log: {e}"))?;
        let log_err: File = log.try_clone().map_err(|e| e.to_string())?;

        let mut cmd = Command::new(&bin);
        cmd.arg("-role")
            .arg("all") // never inherit a worker-only role from the parent environment
            .arg("-addr")
            // Loopback only: the desktop backend must not be reachable from the LAN.
            .arg(format!("127.0.0.1:{}", cfg.port))
            .current_dir(&data_dir)
            .env("AGENTFLOW_HTTP_PORT", cfg.port.to_string())
            .env("AGENTFLOW_MYSQL_DSN", &dsn)
            .env("AGENTFLOW_SESSION_SECRET", &cfg.session_secret)
            .stdin(Stdio::null())
            .stdout(Stdio::from(log))
            .stderr(Stdio::from(log_err));
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt;
            cmd.creation_flags(0x0800_0000); // CREATE_NO_WINDOW
        }
        let mut child = cmd
            .spawn()
            .map_err(|e| format!("Failed to start backend {}: {e}", bin.display()))?;
        let job = match job::Job::adopt(&child) {
            Ok(j) => j,
            Err(e) => {
                let _ = child.kill();
                let _ = child.wait();
                return Err(format!("Could not supervise the backend process: {e}"));
            }
        };
        let pid = child.id();
        let mut running = Running { child, dsn, port: cfg.port, secret: cfg.session_secret.clone(), _job: job };

        let started = Instant::now();
        while started.elapsed() < STARTUP_TIMEOUT {
            if self.generation.load(Ordering::SeqCst) != generation {
                terminate(running);
                return Err("Backend start was cancelled.".into());
            }
            if let Ok(Some(status)) = running.child.try_wait() {
                return Err(format!("The backend exited during startup ({status}).\n{}", self.log_tail(15)));
            }
            if health_pid(cfg.port) == Some(pid) {
                *self.running.lock().unwrap() = Some(running);
                return Ok(());
            }
            std::thread::sleep(Duration::from_millis(250));
        }
        terminate(running);
        Err(format!(
            "The backend did not become ready within {}s.\n{}",
            STARTUP_TIMEOUT.as_secs(),
            self.log_tail(15)
        ))
    }

    /// Stops the backend and cancels any start that is still waiting for readiness.
    pub fn stop(&self) {
        self.generation.fetch_add(1, Ordering::SeqCst);
        if let Some(r) = self.running.lock().unwrap().take() {
            terminate(r);
        }
    }

    pub fn log_tail(&self, lines: usize) -> String {
        let Ok(mut f) = File::open(log_path()) else {
            return "No backend log yet.".into();
        };
        let len = f.metadata().map(|m| m.len()).unwrap_or(0);
        let _ = f.seek(SeekFrom::Start(len.saturating_sub(64 * 1024)));
        let mut raw = Vec::new();
        let _ = f.read_to_end(&mut raw);
        let text = String::from_utf8_lossy(&raw);
        let all: Vec<&str> = text.lines().collect();
        all[all.len().saturating_sub(lines)..].join("\n")
    }
}

/// Windows job object, one per backend instance: closing it (on stop, or when the desktop
/// app exits or crashes) kills the backend and every CLI/agent process it spawned.
#[cfg(windows)]
mod job {
    use std::os::windows::io::AsRawHandle;
    use std::process::Child;

    use windows_sys::Win32::Foundation::{CloseHandle, HANDLE};
    use windows_sys::Win32::System::JobObjects::*;

    pub struct Job(HANDLE);

    // The handle is only used to close it; it is safe to move between threads.
    unsafe impl Send for Job {}

    impl Job {
        pub fn adopt(child: &Child) -> Result<Job, String> {
            unsafe {
                let h = CreateJobObjectW(std::ptr::null(), std::ptr::null());
                if h.is_null() {
                    return Err("CreateJobObjectW failed".into());
                }
                let job = Job(h);
                let mut info: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = std::mem::zeroed();
                info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
                if SetInformationJobObject(
                    h,
                    JobObjectExtendedLimitInformation,
                    &info as *const _ as *const _,
                    std::mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
                ) == 0
                {
                    return Err("SetInformationJobObject failed".into());
                }
                if AssignProcessToJobObject(h, child.as_raw_handle() as HANDLE) == 0 {
                    return Err("AssignProcessToJobObject failed".into());
                }
                Ok(job)
            }
        }
    }

    impl Drop for Job {
        fn drop(&mut self) {
            unsafe {
                CloseHandle(self.0);
            }
        }
    }
}

#[cfg(not(windows))]
mod job {
    use std::process::Child;

    pub struct Job;

    impl Job {
        pub fn adopt(_child: &Child) -> Result<Job, String> {
            Ok(Job)
        }
    }
}

//! Lifecycle of the bundled Go backend process.

use std::fs::{self, File, OpenOptions};
use std::io::{Read, Seek, SeekFrom, Write};
use std::net::{SocketAddr, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use crate::config::{app_dir, Config};

const STARTUP_TIMEOUT: Duration = Duration::from_secs(90);

pub struct Backend {
    child: Mutex<Option<Child>>,
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

/// Minimal HTTP GET against `/api/health`; true on a 200 response.
fn health_ok(port: u16) -> bool {
    let addr = SocketAddr::from(([127, 0, 0, 1], port));
    let Ok(mut s) = TcpStream::connect_timeout(&addr, Duration::from_millis(500)) else {
        return false;
    };
    let _ = s.set_read_timeout(Some(Duration::from_secs(2)));
    let _ = s.set_write_timeout(Some(Duration::from_secs(2)));
    if s.write_all(b"GET /api/health HTTP/1.0\r\nHost: 127.0.0.1\r\n\r\n").is_err() {
        return false;
    }
    let mut buf = [0u8; 32];
    let n = s.read(&mut buf).unwrap_or(0);
    std::str::from_utf8(&buf[..n]).map(|h| h.contains(" 200")).unwrap_or(false)
}

impl Backend {
    pub fn new() -> Self {
        Self { child: Mutex::new(None) }
    }

    pub fn is_running(&self) -> bool {
        let mut g = self.child.lock().unwrap();
        match g.as_mut() {
            Some(c) => matches!(c.try_wait(), Ok(None)),
            None => false,
        }
    }

    /// Starts the backend if needed and blocks until it answers its health check.
    pub fn start(&self, cfg: &Config, resource_dir: Option<&Path>) -> Result<(), String> {
        if self.is_running() && health_ok(cfg.port) {
            return Ok(());
        }
        self.stop();
        if cfg.mysql_dsn.trim().is_empty() {
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
        cmd.arg("-addr")
            // Loopback only: the desktop backend must not be reachable from the LAN.
            .arg(format!("127.0.0.1:{}", cfg.port))
            .current_dir(&data_dir)
            .env("AGENTFLOW_HTTP_PORT", cfg.port.to_string())
            .env("AGENTFLOW_MYSQL_DSN", cfg.mysql_dsn.trim())
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
        #[cfg(windows)]
        job::assign(&child);

        let started = Instant::now();
        while started.elapsed() < STARTUP_TIMEOUT {
            if let Ok(Some(status)) = child.try_wait() {
                return Err(format!("The backend exited during startup ({status}).\n{}", self.log_tail(15)));
            }
            if health_ok(cfg.port) {
                *self.child.lock().unwrap() = Some(child);
                return Ok(());
            }
            std::thread::sleep(Duration::from_millis(250));
        }
        let _ = child.kill();
        let _ = child.wait();
        Err(format!(
            "The backend did not become ready within {}s.\n{}",
            STARTUP_TIMEOUT.as_secs(),
            self.log_tail(15)
        ))
    }

    pub fn stop(&self) {
        if let Some(mut c) = self.child.lock().unwrap().take() {
            let _ = c.kill();
            let _ = c.wait();
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

/// Windows job object: the backend (and every CLI/agent process it spawns) is killed
/// when the desktop app exits for any reason, including a crash or End Task.
#[cfg(windows)]
mod job {
    use std::os::windows::io::AsRawHandle;
    use std::process::Child;
    use std::sync::OnceLock;

    use windows_sys::Win32::Foundation::HANDLE;
    use windows_sys::Win32::System::JobObjects::*;

    static JOB: OnceLock<usize> = OnceLock::new();

    fn job() -> Option<HANDLE> {
        let h = *JOB.get_or_init(|| unsafe {
            let h = CreateJobObjectW(std::ptr::null(), std::ptr::null());
            if h.is_null() {
                return 0;
            }
            let mut info: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = std::mem::zeroed();
            info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
            let ok = SetInformationJobObject(
                h,
                JobObjectExtendedLimitInformation,
                &info as *const _ as *const _,
                std::mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
            );
            if ok == 0 {
                0
            } else {
                h as usize
            }
        });
        if h == 0 {
            None
        } else {
            Some(h as HANDLE)
        }
    }

    pub fn assign(child: &Child) {
        if let Some(j) = job() {
            unsafe {
                AssignProcessToJobObject(j, child.as_raw_handle() as HANDLE);
            }
        }
    }
}

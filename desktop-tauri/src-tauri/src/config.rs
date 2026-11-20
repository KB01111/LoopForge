//! Persistent desktop settings, stored in `%APPDATA%\LoopForge\desktop.json`.

use std::fs;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum Mode {
    /// Run the bundled Go backend locally (needs a MySQL database).
    #[default]
    Local,
    /// Connect to an already-running LoopForge server.
    Remote,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct Config {
    pub mode: Mode,
    pub mysql_dsn: String,
    pub remote_url: String,
    pub port: u16,
    /// Signs login sessions; generated once and kept stable so logins survive restarts.
    pub session_secret: String,
    /// Set when settings could not be loaded or persisted; never written to disk.
    #[serde(skip_deserializing, skip_serializing_if = "String::is_empty")]
    pub load_error: String,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            mode: Mode::Local,
            mysql_dsn: String::new(),
            remote_url: String::new(),
            port: 8090,
            session_secret: String::new(),
            load_error: String::new(),
        }
    }
}

/// `%APPDATA%\LoopForge` (or the platform equivalent).
pub fn app_dir() -> PathBuf {
    std::env::var_os("LOOPFORGE_DESKTOP_HOME")
        .map(PathBuf::from)
        .or_else(|| std::env::var_os("APPDATA").map(|p| PathBuf::from(p).join("LoopForge")))
        .or_else(|| std::env::var_os("HOME").map(|p| PathBuf::from(p).join(".loopforge")))
        .unwrap_or_else(|| PathBuf::from("loopforge-data"))
}

fn config_path() -> PathBuf {
    app_dir().join("desktop.json")
}

fn random_hex(bytes: usize) -> String {
    let mut buf = vec![0u8; bytes];
    getrandom::getrandom(&mut buf).expect("system random source unavailable");
    buf.iter().map(|b| format!("{b:02x}")).collect()
}

impl Config {
    /// Loads settings. A missing file yields defaults. A corrupt file is moved aside (never
    /// overwritten) and reported through `load_error`, as is any failure to persist the signing key.
    pub fn load() -> Self {
        let path = config_path();
        let mut cfg = Config::default();
        let mut can_save = true;
        match fs::read(&path) {
            Ok(bytes) => match serde_json::from_slice::<Config>(&bytes) {
                Ok(c) => cfg = c,
                Err(e) => {
                    let backup = path.with_extension("json.invalid");
                    let moved = fs::rename(&path, &backup).is_ok();
                    cfg.load_error = format!(
                        "The saved settings could not be read ({e}). {} Please enter them again.",
                        if moved { format!("The old file was kept as {}.", backup.display()) } else { "The old file was left untouched.".into() }
                    );
                    can_save = moved;
                }
            },
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => {
                cfg.load_error = format!("Cannot read {}: {e}", path.display());
                can_save = false;
            }
        }
        // Environment override helps development and automated setups.
        if cfg.mysql_dsn.is_empty() {
            if let Ok(dsn) = std::env::var("AGENTFLOW_MYSQL_DSN") {
                cfg.mysql_dsn = dsn;
            }
        }
        if cfg.session_secret.len() < 32 {
            cfg.session_secret = random_hex(32);
            if can_save {
                if let Err(e) = cfg.save() {
                    cfg.load_error = format!("Cannot save settings to {}: {e}", path.display());
                }
            }
        }
        cfg
    }

    pub fn save(&self) -> std::io::Result<()> {
        let path = config_path();
        if let Some(dir) = path.parent() {
            fs::create_dir_all(dir)?;
        }
        let tmp = path.with_extension("json.tmp");
        fs::write(&tmp, serde_json::to_vec_pretty(self).map_err(std::io::Error::other)?)?;
        fs::rename(&tmp, &path)
    }

    /// Checks that the settings are usable for the selected mode.
    pub fn validate(&self) -> Result<(), String> {
        match self.mode {
            Mode::Local => {
                if self.port < 1024 {
                    return Err("Port must be between 1024 and 65535".into());
                }
                if self.mysql_dsn.trim().is_empty() {
                    return Err("A MySQL DSN is required, e.g. user:password@tcp(127.0.0.1:33315)/loopforge".into());
                }
            }
            Mode::Remote => {
                let u = tauri::Url::parse(self.remote_url.trim()).map_err(|e| format!("Invalid server URL: {e}"))?;
                if !matches!(u.scheme(), "http" | "https") || u.host_str().is_none() {
                    return Err("Server URL must start with http:// or https://".into());
                }
            }
        }
        Ok(())
    }

    /// The view handed to the UI: secrets are withheld, only their presence is reported.
    pub fn public_view(&self) -> Config {
        let mut c = self.clone();
        c.mysql_dsn = if self.mysql_dsn.is_empty() { String::new() } else { "(saved)".into() };
        c.session_secret.clear();
        c
    }
}

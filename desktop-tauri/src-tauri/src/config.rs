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
}

impl Default for Config {
    fn default() -> Self {
        Self {
            mode: Mode::Local,
            mysql_dsn: String::new(),
            remote_url: String::new(),
            port: 8090,
            session_secret: String::new(),
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
    pub fn load() -> Self {
        let mut cfg: Config = fs::read(config_path())
            .ok()
            .and_then(|b| serde_json::from_slice(&b).ok())
            .unwrap_or_default();
        // Environment override helps development and automated setups.
        if cfg.mysql_dsn.is_empty() {
            if let Ok(dsn) = std::env::var("AGENTFLOW_MYSQL_DSN") {
                cfg.mysql_dsn = dsn;
            }
        }
        if cfg.session_secret.len() < 32 {
            cfg.session_secret = random_hex(32);
            let _ = cfg.save();
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

    /// The view handed to the UI: secrets are withheld, only their presence is reported.
    pub fn public_view(&self) -> Config {
        let mut c = self.clone();
        c.mysql_dsn = if self.mysql_dsn.is_empty() { String::new() } else { "(saved)".into() };
        c.session_secret.clear();
        c
    }
}

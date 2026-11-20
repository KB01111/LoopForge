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
    /// True when the existing settings file could not be preserved; saving is refused.
    #[serde(skip)]
    pub save_blocked: bool,
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
            save_blocked: false,
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

/// A backup file name that does not exist yet, so an earlier backup is never replaced.
fn unused_backup_path(path: &std::path::Path) -> PathBuf {
    let stamp = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    let mut n = 0u32;
    loop {
        let ext = if n == 0 { format!("json.invalid-{stamp}") } else { format!("json.invalid-{stamp}-{n}") };
        let candidate = path.with_extension(ext);
        if !candidate.exists() {
            return candidate;
        }
        n += 1;
    }
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
                    let backup = unused_backup_path(&path);
                    let moved = fs::rename(&path, &backup).is_ok();
                    cfg.load_error = format!(
                        "The saved settings could not be read ({e}). {} Please enter them again.",
                        if moved { format!("The old file was kept as {}.", backup.display()) } else { "The old file was left untouched.".into() }
                    );
                    can_save = moved;
                    cfg.save_blocked = !moved;
                }
            },
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => {
                cfg.load_error = format!("Cannot read {}: {e}", path.display());
                can_save = false;
                cfg.save_blocked = true;
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
        if self.save_blocked {
            return Err(std::io::Error::other(
                "the existing settings file could not be read or backed up; fix it or delete it manually",
            ));
        }
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn corrupt_settings_are_preserved_and_never_overwritten() {
        let dir = std::env::temp_dir().join(format!("lf-cfg-test-{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        std::env::set_var("LOOPFORGE_DESKTOP_HOME", &dir);

        // Missing file: defaults, a persisted signing key, no error.
        let first = Config::load();
        assert!(first.load_error.is_empty());
        assert!(config_path().exists());

        // Two corrupt files in the same second must produce two distinct backups.
        for _ in 0..2 {
            fs::write(config_path(), b"{ not json").unwrap();
            let c = Config::load();
            assert!(c.load_error.contains("could not be read"));
            assert!(!c.save_blocked);
        }
        let backups = fs::read_dir(&dir)
            .unwrap()
            .filter(|e| e.as_ref().unwrap().file_name().to_string_lossy().contains("json.invalid"))
            .count();
        assert_eq!(backups, 2);

        // A blocked config refuses to save.
        let blocked = Config { save_blocked: true, ..Config::default() };
        assert!(blocked.save().is_err());
        let _ = fs::remove_dir_all(&dir);
    }
}

// Hide the console window in release builds on Windows.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod backend;
mod config;

use std::sync::Mutex;

use tauri::menu::{Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{AppHandle, Manager, RunEvent, Url, WebviewUrl, WebviewWindow, WebviewWindowBuilder};
use tauri_plugin_opener::OpenerExt;

use backend::Backend;
use config::{Config, Mode};

/// Shared application state.
pub struct AppState {
    pub config: Mutex<Config>,
    pub backend: Backend,
}

fn main_window(app: &AppHandle) -> Option<WebviewWindow> {
    app.get_webview_window("main")
}

fn show_main(app: &AppHandle) {
    if let Some(w) = main_window(app) {
        let _ = w.unminimize();
        let _ = w.show();
        let _ = w.set_focus();
    }
}

/// Is `url` part of the app itself (setup page or the local/remote platform)?
fn is_internal(app: &AppHandle, url: &Url) -> bool {
    match url.scheme() {
        "tauri" | "about" | "data" | "blob" => return true,
        _ => {}
    }
    let host = url.host_str().unwrap_or("");
    if host == "tauri.localhost" || host == "localhost" || host == "127.0.0.1" || host == "[::1]" {
        return true;
    }
    // Remote-server mode: the configured server is internal too.
    let state = app.state::<AppState>();
    let cfg = state.config.lock().unwrap();
    if cfg.mode == Mode::Remote {
        if let Ok(remote) = Url::parse(&cfg.remote_url) {
            return remote.host_str() == url.host_str() && remote.port_or_known_default() == url.port_or_known_default();
        }
    }
    false
}

fn create_main_window(app: &AppHandle) -> tauri::Result<WebviewWindow> {
    let nav_handle = app.clone();
    let popup_handle = app.clone();
    WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
        .title("LoopForge")
        .inner_size(1400.0, 900.0)
        .min_inner_size(1024.0, 700.0)
        .center()
        .visible(false)
        .background_color(tauri::window::Color(10, 14, 22, 255))
        .disable_drag_drop_handler()
        // External http(s) links open in the default browser, not in the app window.
        .on_navigation(move |url| {
            if is_internal(&nav_handle, url) {
                return true;
            }
            if matches!(url.scheme(), "http" | "https" | "mailto") {
                let _ = nav_handle.opener().open_url(url.as_str(), None::<&str>);
            }
            false
        })
        .on_new_window(move |url, _features| {
            let _ = popup_handle.opener().open_url(url.as_str(), None::<&str>);
            tauri::webview::NewWindowResponse::Deny
        })
        .build()
}

// ---------------------------------------------------------------- commands

#[tauri::command]
fn get_config(state: tauri::State<AppState>) -> Config {
    state.config.lock().unwrap().public_view()
}

#[tauri::command]
fn save_config(
    state: tauri::State<AppState>,
    mode: Mode,
    mysql_dsn: String,
    remote_url: String,
    port: u16,
) -> Result<(), String> {
    if port < 1024 {
        return Err("Port must be between 1024 and 65535".into());
    }
    if mode == Mode::Remote {
        let u = Url::parse(remote_url.trim()).map_err(|e| format!("Invalid server URL: {e}"))?;
        if !matches!(u.scheme(), "http" | "https") || u.host_str().is_none() {
            return Err("Server URL must start with http:// or https://".into());
        }
    } else if mysql_dsn.trim().is_empty() {
        return Err("A MySQL DSN is required, e.g. user:password@tcp(127.0.0.1:33315)/loopforge".into());
    }
    let mut cfg = state.config.lock().unwrap();
    cfg.mode = mode;
    // An empty DSN field means "keep the stored one" (the stored value is never sent to the UI).
    if !mysql_dsn.trim().is_empty() {
        cfg.mysql_dsn = mysql_dsn.trim().to_string();
    }
    cfg.remote_url = remote_url.trim().trim_end_matches('/').to_string();
    cfg.port = port;
    cfg.save().map_err(|e| format!("Could not save settings: {e}"))
}

/// Starts the local backend (or validates the remote one) and returns the URL to load.
#[tauri::command]
async fn launch(state: tauri::State<'_, AppState>, app: AppHandle) -> Result<String, String> {
    let cfg = state.config.lock().unwrap().clone();
    if cfg.mode == Mode::Remote {
        return Ok(cfg.remote_url);
    }
    let resource_dir = app.path().resource_dir().ok();
    let state_app = app.clone();
    tauri::async_runtime::spawn_blocking(move || {
        let st = state_app.state::<AppState>();
        st.backend.start(&cfg, resource_dir.as_deref())
    })
    .await
    .map_err(|e| e.to_string())??;
    Ok(format!("http://127.0.0.1:{}/", state.config.lock().unwrap().port))
}

#[tauri::command]
async fn restart_backend(app: AppHandle) -> Result<(), String> {
    tauri::async_runtime::spawn_blocking(move || {
        let st = app.state::<AppState>();
        st.backend.stop();
        let cfg = st.config.lock().unwrap().clone();
        let res = app.path().resource_dir().ok();
        st.backend.start(&cfg, res.as_deref())
    })
    .await
    .map_err(|e| e.to_string())?
}

#[tauri::command]
fn stop_backend(state: tauri::State<AppState>) {
    state.backend.stop();
}

#[tauri::command]
fn backend_status(state: tauri::State<AppState>) -> bool {
    state.backend.is_running()
}

#[tauri::command]
fn backend_logs(state: tauri::State<AppState>) -> String {
    state.backend.log_tail(200)
}

#[tauri::command]
fn app_info() -> String {
    format!(
        "LoopForge Desktop {} ({}-{})",
        env!("CARGO_PKG_VERSION"),
        std::env::consts::OS,
        std::env::consts::ARCH
    )
}

#[tauri::command]
fn open_url(app: AppHandle, url: String) -> Result<(), String> {
    let u = Url::parse(&url).map_err(|e| e.to_string())?;
    if !matches!(u.scheme(), "http" | "https") {
        return Err("Only http(s) URLs can be opened".into());
    }
    app.opener().open_url(url, None::<&str>).map_err(|e| e.to_string())
}

// -------------------------------------------------------------------- tray

fn go_to_setup(app: &AppHandle) {
    if let Some(w) = main_window(app) {
        // The setup page lives on the app origin; `settings=1` stops it auto-launching.
        let base = if cfg!(windows) { "http://tauri.localhost/index.html?settings=1" } else { "tauri://localhost/index.html?settings=1" };
        if let Ok(u) = Url::parse(base) {
            let _ = w.navigate(u);
        }
    }
    show_main(app);
}

fn setup_tray(app: &AppHandle) -> tauri::Result<()> {
    let show = MenuItem::with_id(app, "show", "Show LoopForge", true, None::<&str>)?;
    let settings = MenuItem::with_id(app, "settings", "Settings…", true, None::<&str>)?;
    let restart = MenuItem::with_id(app, "restart", "Restart backend", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "Quit", true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&show, &settings, &restart, &PredefinedMenuItem::separator(app)?, &quit])?;

    let mut builder = TrayIconBuilder::with_id("main")
        .tooltip("LoopForge")
        .menu(&menu)
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| match event.id.as_ref() {
            "show" => show_main(app),
            "settings" => go_to_setup(app),
            "restart" => {
                let app = app.clone();
                tauri::async_runtime::spawn_blocking(move || {
                    let st = app.state::<AppState>();
                    st.backend.stop();
                    let cfg = st.config.lock().unwrap().clone();
                    if cfg.mode == Mode::Local {
                        let res = app.path().resource_dir().ok();
                        if let Err(e) = st.backend.start(&cfg, res.as_deref()) {
                            log::error!("backend restart failed: {e}");
                        }
                    }
                });
            }
            "quit" => app.exit(0),
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            if let TrayIconEvent::Click { button: MouseButton::Left, button_state: MouseButtonState::Up, .. } = event {
                show_main(tray.app_handle());
            }
        });
    if let Some(icon) = app.default_window_icon() {
        builder = builder.icon(icon.clone());
    }
    builder.build(app)?;
    Ok(())
}

fn main() {
    env_logger::Builder::from_env(env_logger::Env::default().default_filter_or("info")).init();

    let app = tauri::Builder::default()
        // A second launch focuses the running instance instead of fighting for the port.
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| show_main(app)))
        .plugin(tauri_plugin_opener::init())
        .manage(AppState { config: Mutex::new(Config::load()), backend: Backend::new() })
        .setup(|app| {
            let handle = app.handle().clone();
            create_main_window(&handle)?;
            setup_tray(&handle)?;
            show_main(&handle);
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            get_config,
            save_config,
            launch,
            restart_backend,
            stop_backend,
            backend_status,
            backend_logs,
            app_info,
            open_url,
        ])
        .build(tauri::generate_context!())
        .expect("failed to build LoopForge Desktop");

    app.run(|app, event| {
        if let RunEvent::Exit = event {
            app.state::<AppState>().backend.stop();
        }
    });
}

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

/// Same scheme, host and effective port?
fn same_origin(a: &Url, b: &Url) -> bool {
    a.scheme() == b.scheme() && a.host_str() == b.host_str() && a.port_or_known_default() == b.port_or_known_default()
}

/// Is `url` the bundled setup page or the active platform server (and nothing else)?
fn is_internal(app: &AppHandle, url: &Url) -> bool {
    match url.scheme() {
        "tauri" | "about" | "data" | "blob" => return true,
        _ => {}
    }
    if url.host_str() == Some("tauri.localhost") {
        return true;
    }
    let state = app.state::<AppState>();
    let cfg = state.config.lock().unwrap();
    match cfg.mode {
        Mode::Local => {
            matches!(url.host_str(), Some("127.0.0.1") | Some("localhost") | Some("[::1]"))
                && url.scheme() == "http"
                && url.port_or_known_default() == Some(cfg.port)
        }
        Mode::Remote => Url::parse(&cfg.remote_url).map(|r| same_origin(&r, url)).unwrap_or(false),
    }
}

/// Opens a web or mail link in the system default handler; other schemes are ignored.
fn open_external(app: &AppHandle, url: &Url) {
    if matches!(url.scheme(), "http" | "https" | "mailto") {
        let _ = app.opener().open_url(url.as_str(), None::<&str>);
    }
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
            open_external(&nav_handle, url);
            false
        })
        .on_new_window(move |url, _features| {
            open_external(&popup_handle, &url);
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
    let mut cfg = state.config.lock().unwrap();
    // Edit a candidate and publish it only once it validates and is persisted.
    let mut next = cfg.clone();
    next.mode = mode;
    // An empty DSN field means "keep the stored one" (the stored value is never sent to the UI).
    if !mysql_dsn.trim().is_empty() {
        next.mysql_dsn = mysql_dsn.trim().to_string();
    }
    next.remote_url = remote_url.trim().trim_end_matches('/').to_string();
    next.port = port;
    next.load_error.clear();
    next.validate()?;
    next.save().map_err(|e| format!("Could not save settings: {e}"))?;
    *cfg = next;
    Ok(())
}

/// Starts the local backend (or validates the remote one) and returns the URL to load.
#[tauri::command]
async fn launch(state: tauri::State<'_, AppState>, app: AppHandle) -> Result<String, String> {
    let cfg = state.config.lock().unwrap().clone();
    if !cfg.load_error.is_empty() {
        return Err(cfg.load_error);
    }
    cfg.validate()?;
    if cfg.mode == Mode::Remote {
        return Ok(cfg.remote_url);
    }
    let resource_dir = app.path().resource_dir().ok();
    let state_app = app.clone();
    let start_cfg = cfg.clone();
    tauri::async_runtime::spawn_blocking(move || {
        let st = state_app.state::<AppState>();
        st.backend.start(&start_cfg, resource_dir.as_deref())
    })
    .await
    .map_err(|e| e.to_string())??;
    Ok(format!("http://127.0.0.1:{}/", cfg.port))
}

/// Single restart policy for the command and the tray: stop, then start only in Local mode.
fn restart_backend_blocking(app: &AppHandle) -> Result<(), String> {
    let st = app.state::<AppState>();
    st.backend.stop();
    let cfg = st.config.lock().unwrap().clone();
    if cfg.mode != Mode::Local {
        return Ok(());
    }
    let res = app.path().resource_dir().ok();
    st.backend.start(&cfg, res.as_deref())
}

#[tauri::command]
async fn restart_backend(app: AppHandle) -> Result<(), String> {
    tauri::async_runtime::spawn_blocking(move || restart_backend_blocking(&app))
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
                    if let Err(e) = restart_backend_blocking(&app) {
                        log::error!("backend restart failed: {e}");
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

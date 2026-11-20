# LoopForge Desktop (Windows, Tauri 2)

A native Windows shell around the LoopForge platform. The shell is a small Tauri 2 app that
runs the Go backend (`cmd/agentflow`) as a hidden child process and shows the platform's own
web UI in a WebView2 window. The UI is the backend's embedded `web/` bundle, unchanged; a few
backend features need Linux/macOS kernel facilities (see **Windows limitations** below).

## How it works

- **Local mode** (default): starts `loopforge-backend.exe` bound to `127.0.0.1:<port>` (default 8090),
  waits for `/api/health`, then loads the UI. Requires a MySQL database, as the platform always does.
- **Remote mode**: opens an existing LoopForge server URL instead of starting a backend.
- First launch shows a settings page (MySQL DSN, port, or server URL). Reopen it any time from the
  tray menu → **Settings…**. Settings live in `%APPDATA%\LoopForge\desktop.json`; a session secret is
  generated once and persisted there (the default folder is private to your Windows user; if you override it with `LOOPFORGE_DESKTOP_HOME`, pick a private directory). The backend log is `%APPDATA%\LoopForge\logs\backend.log`.
- Windows integration: single instance, system tray (show / settings / restart backend / quit),
  no console window, and a job object that kills the backend and every agent process it spawned
  when the app exits or crashes. External links open in the default browser.

The DSN format is the platform's: `user:password@tcp(127.0.0.1:33315)/loopforge`
(see `.env.example` and `docs/deployment.md`; `./start.sh` / compose can provide the database).

## Windows limitations

The backend is built for Windows via `internal/sysx` (process groups, file locks). Features that
depend on Linux/macOS kernel facilities are unavailable natively: the PTY workspace terminal,
bubblewrap/seatbelt sandboxes (use the Docker sandbox, Docker Desktop required), and `/proc`-based
git index-lock detection.

## Build

The installer targets 64-bit Windows (x64) only; the backend is built as amd64.

Prerequisites: Rust (MSVC toolchain), Node 20+, Go (version in `go.mod`), WebView2 runtime.

```powershell
cd desktop-tauri
npm install
npm run build:backend   # builds ..\cmd\agentflow -> bin\loopforge-backend.exe
npm run tauri dev       # or: npm run build  (NSIS + MSI installers in src-tauri\target\release\bundle)
```

The Rust crate bundles `bin\loopforge-backend.exe` as a resource, so build the backend first.
`LOOPFORGE_BACKEND_BIN` overrides the binary path and `LOOPFORGE_DESKTOP_HOME` overrides the data directory.

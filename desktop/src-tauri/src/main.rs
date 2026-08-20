// BearDrive Desktop: a menu-bar shell around the `bdrive desktop` sidecar.
//
// The shell does exactly three things — spawn the sidecar on loopback, show a
// tray menu of each mount's sync state (pause/resume/sync-now call the
// sidecar's /api/desktop/* control API), and open a window on the sidecar's
// URL, which serves the same React frontend a hub serves. All product logic
// lives in Go; anything smarter belongs in the sidecar, not here.

#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::process::{Child, Command};
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Mutex;
use std::time::Duration;

use tauri::menu::{AboutMetadata, Menu, MenuBuilder, MenuItemBuilder, SubmenuBuilder};
use tauri::tray::TrayIconBuilder;
use tauri::{Manager, RunEvent, WebviewUrl, WebviewWindowBuilder};

const ADDR: &str = "127.0.0.1:8990";

struct Sidecar(Mutex<Option<Child>>);

/// The sidecar binary: the bundled `bdrive` next to the app executable
/// (tauri externalBin lands in Contents/MacOS/), or PATH in dev.
fn bdrive_path() -> std::path::PathBuf {
    if let Ok(exe) = std::env::current_exe() {
        let bundled = exe.with_file_name("bdrive");
        if bundled.exists() {
            return bundled;
        }
    }
    "bdrive".into()
}

fn api(path: &str) -> String {
    format!("http://{ADDR}{path}")
}

/// Wait for the sidecar to answer. Also succeeds against a sidecar some other
/// instance already runs on the port — then our spawn failed and that's fine.
fn wait_ready() -> bool {
    for _ in 0..50 {
        if ureq::get(&api("/api/config"))
            .timeout(Duration::from_millis(500))
            .call()
            .is_ok()
        {
            return true;
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    false
}

#[derive(serde::Deserialize)]
struct MountStatus {
    project: String,
    name: String,
    running: bool,
    paused: bool,
}

#[derive(serde::Deserialize)]
struct Status {
    mounts: Vec<MountStatus>,
}

fn fetch_status() -> Vec<MountStatus> {
    ureq::get(&api("/api/desktop/status"))
        .timeout(Duration::from_secs(3))
        .call()
        .ok()
        .and_then(|r| r.into_json::<Status>().ok())
        .map(|s| s.mounts)
        .unwrap_or_default()
}

fn post(path: &str) {
    let _ = ureq::post(&api(path))
        .set("X-Bdrive-Desktop", "1")
        // Sync-now can take a while; login waits for the user to finish in
        // the browser (the sidecar bounds that at 5 minutes).
        .timeout(Duration::from_secs(360))
        .call();
}

#[derive(serde::Deserialize)]
struct Session {
    signed_in: bool,
    email: String,
}

fn fetch_session() -> Option<Session> {
    ureq::get(&api("/api/desktop/session"))
        .timeout(Duration::from_secs(3))
        .call()
        .ok()
        .and_then(|r| r.into_json::<Session>().ok())
}

fn refresh_menu<R: tauri::Runtime>(handle: &tauri::AppHandle<R>) {
    if let (Ok(menu), Some(tray)) = (build_menu(handle), handle.tray_by_id("main")) {
        let _ = tray.set_menu(Some(menu));
    }
}

/// The tray menu: one submenu per mount with its state in the title.
fn build_menu<R: tauri::Runtime>(app: &tauri::AppHandle<R>) -> tauri::Result<Menu<R>> {
    let mut b = MenuBuilder::new(app).text("open", "Open BearDrive").separator();
    let mounts = fetch_status();
    if mounts.is_empty() {
        b = b.text("none", "No synced folders yet");
    }
    for m in mounts {
        let state = if m.paused {
            "paused"
        } else if m.running {
            "syncing"
        } else {
            "stopped"
        };
        let sub = SubmenuBuilder::new(app, format!("{} — {}", m.name, state))
            .text(format!("sync:{}", m.project), "Sync now")
            .text(
                if m.paused {
                    format!("resume:{}", m.project)
                } else {
                    format!("pause:{}", m.project)
                },
                if m.paused { "Resume syncing" } else { "Pause syncing" },
            )
            .build()?;
        b = b.item(&sub);
    }
    b = b.separator();
    match fetch_session() {
        Some(s) if s.signed_in => {
            let who = MenuItemBuilder::with_id("acct", format!("Signed in as {}", s.email))
                .enabled(false)
                .build(app)?;
            b = b.item(&who).text("logout", "Sign out");
        }
        _ => {
            b = b.text("login", "Sign in…");
        }
    }
    b.separator().text("quit", "Quit BearDrive").build()
}

/// The window menu bar: the standard macOS App/Edit/Window menus (a custom
/// menu replaces Tauri's default wholesale, and without Edit the clipboard
/// shortcuts die in the webview) plus History → Back/Forward on the browser
/// shortcuts, since the window is a browser over the SPA's real history.
fn app_menu<R: tauri::Runtime>(app: &tauri::AppHandle<R>) -> tauri::Result<Menu<R>> {
    let bear = SubmenuBuilder::new(app, "BearDrive")
        .about(Some(AboutMetadata::default()))
        .separator()
        .hide()
        .hide_others()
        .show_all()
        .separator()
        .quit()
        .build()?;
    let edit = SubmenuBuilder::new(app, "Edit")
        .undo()
        .redo()
        .separator()
        .cut()
        .copy()
        .paste()
        .select_all()
        .build()?;
    let view = SubmenuBuilder::new(app, "View")
        .item(
            &MenuItemBuilder::with_id("view-reload", "Reload")
                .accelerator("CmdOrCtrl+R")
                .build(app)?,
        )
        .separator()
        .item(
            &MenuItemBuilder::with_id("view-zoom-reset", "Actual Size")
                .accelerator("CmdOrCtrl+0")
                .build(app)?,
        )
        .item(
            &MenuItemBuilder::with_id("view-zoom-in", "Zoom In")
                .accelerator("CmdOrCtrl+=")
                .build(app)?,
        )
        .item(
            &MenuItemBuilder::with_id("view-zoom-out", "Zoom Out")
                .accelerator("CmdOrCtrl+-")
                .build(app)?,
        )
        .build()?;
    let history = SubmenuBuilder::new(app, "History")
        .item(
            &MenuItemBuilder::with_id("nav-back", "Back")
                .accelerator("CmdOrCtrl+[")
                .build(app)?,
        )
        .item(
            &MenuItemBuilder::with_id("nav-forward", "Forward")
                .accelerator("CmdOrCtrl+]")
                .build(app)?,
        )
        .build()?;
    let window = SubmenuBuilder::new(app, "Window")
        .minimize()
        .separator()
        .close_window()
        .build()?;
    MenuBuilder::new(app)
        .items(&[&bear, &edit, &view, &history, &window])
        .build()
}

/// Webview zoom, percent. Survives window close/reopen (a fresh webview
/// resets to 1.0, so open_window re-applies it).
static ZOOM_PCT: AtomicU32 = AtomicU32::new(100);

fn apply_zoom<R: tauri::Runtime>(app: &tauri::AppHandle<R>, pct: u32) {
    let pct = pct.clamp(50, 300);
    ZOOM_PCT.store(pct, Ordering::Relaxed);
    if let Some(w) = app.get_webview_window("main") {
        let _ = w.set_zoom(pct as f64 / 100.0);
    }
}

fn navigate<R: tauri::Runtime>(app: &tauri::AppHandle<R>, js: &str) {
    if let Some(w) = app.get_webview_window("main") {
        let _ = w.eval(js);
    }
}

fn open_window<R: tauri::Runtime>(app: &tauri::AppHandle<R>) -> tauri::Result<()> {
    if let Some(w) = app.get_webview_window("main") {
        let _ = w.show();
        let _ = w.set_focus();
        return Ok(());
    }
    WebviewWindowBuilder::new(
        app,
        "main",
        WebviewUrl::External(api("/").parse().expect("static url")),
    )
    .title("BearDrive")
    .inner_size(1280.0, 850.0)
    // The window is OUR app's viewer; anything not served by the sidecar
    // (markdown links to the web, the hub's share pages) belongs in the
    // user's real browser. target=_blank links don't come through here —
    // known gap on the parity row.
    .on_navigation(|url| {
        let ours = url.host_str() == Some("127.0.0.1") && url.port() == Some(8990);
        if !ours {
            let _ = Command::new("open").arg(url.as_str()).spawn();
        }
        ours
    })
    // Downloads land in ~/Downloads under the served name; without this
    // handler the webview silently drops them.
    .on_download(|_wv, event| {
        if let tauri::webview::DownloadEvent::Requested { destination, .. } = event {
            let name = destination
                .file_name()
                .map(|s| s.to_os_string())
                .unwrap_or_else(|| "download".into());
            if let Ok(home) = std::env::var("HOME") {
                let dir = std::path::Path::new(&home).join("Downloads");
                let mut dest = dir.join(&name);
                let mut n = 1;
                while dest.exists() {
                    dest = dir.join(format!("{} ({n})", name.to_string_lossy()));
                    n += 1;
                }
                *destination = dest;
            }
        }
        true
    })
    .build()?;
    // A fresh webview starts at 100%; keep the user's chosen zoom.
    let pct = ZOOM_PCT.load(Ordering::Relaxed);
    if pct != 100 {
        apply_zoom(app, pct);
    }
    Ok(())
}

fn main() {
    let app = tauri::Builder::default()
        // Remembers the window's size/position across launches (and across
        // close-to-tray → Open), keyed by window label.
        .plugin(tauri_plugin_window_state::Builder::default().build())
        // App-menu events (the tray has its own handler; ids are disjoint).
        .on_menu_event(|app, e| match e.id().as_ref() {
            "nav-back" => navigate(app, "history.back()"),
            "nav-forward" => navigate(app, "history.forward()"),
            "view-reload" => navigate(app, "location.reload()"),
            "view-zoom-in" => apply_zoom(app, ZOOM_PCT.load(Ordering::Relaxed) + 10),
            "view-zoom-out" => apply_zoom(app, ZOOM_PCT.load(Ordering::Relaxed).saturating_sub(10)),
            "view-zoom-reset" => apply_zoom(app, 100),
            _ => {}
        })
        .setup(|app| {
            app.set_menu(app_menu(app.handle())?)?;
            let child = Command::new(bdrive_path())
                .args(["desktop", "--addr", ADDR])
                .spawn()
                .ok();
            app.manage(Sidecar(Mutex::new(child)));
            wait_ready();

            let menu = build_menu(app.handle())?;
            // The menu-bar glyph is its own asset: template mode renders only
            // the alpha channel, so the app icon's filled background would
            // show as a solid blob.
            TrayIconBuilder::with_id("main")
                .icon(tauri::image::Image::from_bytes(include_bytes!("../icons/tray.png"))?)
                .icon_as_template(true)
                .menu(&menu)
                .show_menu_on_left_click(true)
                .on_menu_event(|app, e| {
                    let id = e.id().as_ref();
                    match id {
                        "open" => {
                            let _ = open_window(app);
                        }
                        "quit" => app.exit(0),
                        _ => {
                            let path = if id == "login" {
                                Some("/api/desktop/login".to_string())
                            } else if id == "logout" {
                                Some("/api/desktop/logout".to_string())
                            } else if let Some(p) = id.strip_prefix("sync:") {
                                Some(format!("/api/desktop/p/{p}/sync"))
                            } else if let Some(p) = id.strip_prefix("pause:") {
                                Some(format!("/api/desktop/p/{p}/pause"))
                            } else if let Some(p) = id.strip_prefix("resume:") {
                                Some(format!("/api/desktop/p/{p}/resume"))
                            } else {
                                None
                            };
                            // Off the main thread: sync-now and login block
                            // until they finish, and the menu should show the
                            // outcome as soon as they do.
                            if let Some(path) = path {
                                let handle = app.clone();
                                std::thread::spawn(move || {
                                    post(&path);
                                    refresh_menu(&handle);
                                });
                            }
                        }
                    }
                })
                .build(app)?;

            // Keep the menu's state labels fresh.
            let handle = app.handle().clone();
            std::thread::spawn(move || loop {
                std::thread::sleep(Duration::from_secs(15));
                refresh_menu(&handle);
            });

            open_window(app.handle())?;
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("error while building BearDrive Desktop");

    app.run(|app, event| match event {
        // Closing the window keeps the tray app alive; only Quit exits.
        RunEvent::ExitRequested { api, code, .. } => {
            if code.is_none() {
                api.prevent_exit();
            }
        }
        // Clicking the Dock icon with the window closed reopens it.
        RunEvent::Reopen { .. } => {
            let _ = open_window(app);
        }
        RunEvent::Exit => {
            if let Some(sc) = app.try_state::<Sidecar>() {
                if let Some(mut c) = sc.0.lock().unwrap().take() {
                    let _ = c.kill();
                }
            }
        }
        _ => {}
    });
}

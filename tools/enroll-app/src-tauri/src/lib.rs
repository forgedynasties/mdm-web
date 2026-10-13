use enroll_core::adb::{Adb, Details, Owner};
use enroll_core::api::{self, ApiError, Session};
use enroll_core::enroll::enroll_device;
use serde::Serialize;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Mutex;
use std::time::{Duration, Instant};
use tauri::{AppHandle, Emitter, Manager};

const KEYRING_SERVICE: &str = "com.aioapp.enroll";

#[derive(Default)]
struct State {
    session: Mutex<Option<Session>>,
    adb: Mutex<Option<Adb>>,
    /// Per-handle adb probe results; owner/accounts are re-read every 10 s, not every poll.
    probes: Mutex<HashMap<String, (Instant, Probe)>>,
    busy: Mutex<Vec<String>>,
}

#[derive(Clone, Default)]
struct Probe {
    details: Details,
    owner: Owner,
    accounts: usize,
}

#[derive(Serialize)]
struct DeviceRow {
    handle: String,
    adb_state: String,
    name: String,
    serial: String,
    android: String,
    /// ready | enrolling | enrolled | blocked | unauthorized | offline
    status: String,
    note: String,
    class: String,
    agent_version: String,
}

fn entry() -> Result<keyring::Entry, String> {
    keyring::Entry::new(KEYRING_SERVICE, "session").map_err(|e| e.to_string())
}

fn config_dir(app: &AppHandle) -> Option<PathBuf> {
    let d = app.path().app_config_dir().ok()?;
    std::fs::create_dir_all(&d).ok()?;
    Some(d)
}

/// Writes a file only the owner can read (the session token lives here when the OS
/// keystore is unavailable, e.g. a Linux box with no Secret Service running).
fn write_private(path: &std::path::Path, data: &str) {
    #[cfg(unix)]
    {
        use std::io::Write;
        use std::os::unix::fs::OpenOptionsExt;
        if let Ok(mut f) = std::fs::OpenOptions::new().write(true).create(true).truncate(true).mode(0o600).open(path) {
            let _ = f.write_all(data.as_bytes());
        }
    }
    #[cfg(not(unix))]
    let _ = std::fs::write(path, data);
}

/// Keeps the session across restarts: OS keystore first, 0600 file as the fallback.
fn save_session(app: &AppHandle, s: &Session) {
    let Ok(j) = serde_json::to_string(s) else { return };
    let stored = entry().and_then(|e| e.set_password(&j).map_err(|e| e.to_string())).is_ok();
    if let Some(d) = config_dir(app) {
        if stored {
            let _ = std::fs::remove_file(d.join("session.json"));
        } else {
            write_private(&d.join("session.json"), &j);
        }
        // Not secret: lets the sign-in form come back pre-filled after a session expires.
        let last = serde_json::json!({ "server": s.server, "username": s.username });
        let _ = std::fs::write(d.join("last-login.json"), last.to_string());
    }
}

fn load_session(app: &AppHandle) -> Option<Session> {
    if let Some(s) = entry().ok().and_then(|e| e.get_password().ok()).and_then(|p| serde_json::from_str(&p).ok()) {
        return Some(s);
    }
    let raw = std::fs::read_to_string(config_dir(app)?.join("session.json")).ok()?;
    serde_json::from_str(&raw).ok()
}

fn clear_session(app: &AppHandle) {
    if let Ok(e) = entry() {
        let _ = e.delete_credential();
    }
    if let Some(d) = config_dir(app) {
        let _ = std::fs::remove_file(d.join("session.json"));
    }
}

fn adb_of(app: &AppHandle, st: &State) -> Result<Adb, String> {
    let mut g = st.adb.lock().unwrap();
    if g.is_none() {
        let res: Option<PathBuf> = app.path().resource_dir().ok();
        *g = Adb::find(res.as_deref());
        if let Some(a) = g.as_ref() {
            a.start_server();
        }
    }
    g.clone().ok_or_else(|| "adb was not found. Reinstall the app, or set AIO_ADB to your adb path.".to_string())
}

#[derive(Serialize)]
struct Me {
    username: String,
    role: String,
    server: String,
}

#[tauri::command]
fn me(app: AppHandle, state: tauri::State<State>) -> Option<Me> {
    let mut g = state.session.lock().unwrap();
    if g.is_none() {
        *g = load_session(&app);
    }
    g.as_ref().map(|s| Me { username: s.username.clone(), role: s.role.clone(), server: s.server.clone() })
}

#[tauri::command]
async fn sign_in(app: AppHandle, state: tauri::State<'_, State>, server: String, username: String, password: String) -> Result<Me, String> {
    let s = tauri::async_runtime::spawn_blocking(move || api::login(&server, &username, &password))
        .await
        .map_err(|e| e.to_string())?
        .map_err(|e| e.message().to_string())?;
    save_session(&app, &s);
    let me = Me { username: s.username.clone(), role: s.role.clone(), server: s.server.clone() };
    *state.session.lock().unwrap() = Some(s);
    Ok(me)
}

#[tauri::command]
async fn sign_out(app: AppHandle, state: tauri::State<'_, State>) -> Result<(), String> {
    let s = state.session.lock().unwrap().take();
    clear_session(&app);
    if let Some(s) = s {
        let _ = tauri::async_runtime::spawn_blocking(move || s.logout()).await;
    }
    Ok(())
}

fn session_of(state: &State) -> Result<Session, String> {
    state.session.lock().unwrap().clone().ok_or_else(|| "signed-out".to_string())
}

fn auth_failed(app: &AppHandle, state: &State, e: ApiError) -> String {
    if matches!(e, ApiError::Unauthorized(_)) {
        *state.session.lock().unwrap() = None;
        clear_session(app);
        return "signed-out".into();
    }
    e.message().to_string()
}

#[tauri::command]
async fn list_devices(app: AppHandle, state: tauri::State<'_, State>) -> Result<Vec<DeviceRow>, String> {
    let adb = adb_of(&app, &state)?;
    let session = session_of(&state)?;
    let app2 = app.clone();

    let (rows, serials) = tauri::async_runtime::spawn_blocking(move || -> Result<_, String> {
        let st = app2.state::<State>();
        let raw = adb.devices()?;
        let mut rows = Vec::new();
        let mut serials = Vec::new();
        for d in raw {
            let mut row = DeviceRow {
                handle: d.handle.clone(), adb_state: d.state.clone(), name: d.model.clone(),
                serial: String::new(), android: String::new(), status: "offline".into(),
                note: String::new(), class: String::new(), agent_version: String::new(),
            };
            match d.state.as_str() {
                "unauthorized" => {
                    row.status = "unauthorized".into();
                    row.note = "Accept the USB debugging prompt on the device".into();
                }
                "device" => {
                    let probe = {
                        let g = st.probes.lock().unwrap();
                        match g.get(&d.handle) {
                            Some((t, p)) if t.elapsed() < Duration::from_secs(10) => p.clone(),
                            _ => {
                                drop(g);
                                let p = Probe {
                                    details: adb.details(&d.handle),
                                    owner: adb.owner(&d.handle),
                                    accounts: adb.account_count(&d.handle),
                                };
                                st.probes.lock().unwrap().insert(d.handle.clone(), (Instant::now(), p.clone()));
                                p
                            }
                        }
                    };
                    row.serial = probe.details.serial.clone();
                    row.android = probe.details.android.clone();
                    let name = format!("{} {}", probe.details.manufacturer, probe.details.model).trim().to_string();
                    if !name.is_empty() {
                        row.name = name;
                    }
                    if !row.serial.is_empty() {
                        serials.push(row.serial.clone());
                    }
                    if probe.accounts > 0 {
                        row.status = "blocked".into();
                        row.note = "Has an account — factory reset, don't add an account".into();
                    } else if probe.owner.set && !probe.owner.ours {
                        row.status = "blocked".into();
                        row.note = format!("Owned by {} — factory reset", probe.owner.package);
                    } else {
                        row.status = "ready".into();
                    }
                }
                other => row.note = other.to_string(),
            }
            rows.push(row);
        }
        Ok((rows, serials))
    })
    .await
    .map_err(|e| e.to_string())??;

    let statuses = {
        let session = session.clone();
        tauri::async_runtime::spawn_blocking(move || session.statuses(&serials))
            .await
            .map_err(|e| e.to_string())?
            .map_err(|e| auth_failed(&app, &state, e))?
    };
    let busy = state.busy.lock().unwrap().clone();
    let mut rows = rows;
    for r in rows.iter_mut() {
        if busy.contains(&r.handle) {
            r.status = "enrolling".into();
        } else if let Some(s) = statuses.get(&r.serial) {
            if s.enrolled {
                r.status = "enrolled".into();
                r.class = s.class.clone();
                r.agent_version = s.agent_version.clone();
                r.note.clear();
            }
        }
    }
    Ok(rows)
}

#[tauri::command]
async fn enroll(app: AppHandle, state: tauri::State<'_, State>, handle: String, class: String) -> Result<(), String> {
    let adb = adb_of(&app, &state)?;
    let session = session_of(&state)?;
    {
        let mut b = state.busy.lock().unwrap();
        if b.contains(&handle) {
            return Err("already enrolling this device".into());
        }
        b.push(handle.clone());
    }
    let apk = std::env::temp_dir().join("aio-mdm-dpc.apk");
    let (h2, app2) = (handle.clone(), app.clone());
    let res = tauri::async_runtime::spawn_blocking(move || {
        let mut log = |step: usize, l: &str| {
            let _ = app2.emit("enroll-step", serde_json::json!({ "handle": h2, "step": step, "line": l }));
        };
        // A stale cached APK from an earlier version must not be installed.
        let _ = std::fs::remove_file(&apk);
        enroll_device(&adb, &session, &h2, &class, &apk, &mut log)
    })
    .await
    .map_err(|e| e.to_string());
    state.busy.lock().unwrap().retain(|h| h != &handle);
    state.probes.lock().unwrap().remove(&handle);
    res?
}

#[derive(Serialize)]
struct ProfileOut {
    name: String,
    /// `data:image/png;base64,…`, or null when the account has no picture.
    avatar: Option<String>,
}

#[tauri::command]
async fn profile(state: tauri::State<'_, State>) -> Result<ProfileOut, String> {
    use base64::Engine;
    let session = session_of(&state)?;
    tauri::async_runtime::spawn_blocking(move || {
        let p = session.profile().unwrap_or_default();
        let avatar = if p.has_avatar { session.avatar() } else { None }
            .map(|b| format!("data:image/png;base64,{}", base64::engine::general_purpose::STANDARD.encode(b)));
        ProfileOut { name: p.name, avatar }
    })
    .await
    .map_err(|e| e.to_string())
}

/// Server + username from the last sign-in, to pre-fill the form (never the password).
#[tauri::command]
fn last_login(app: AppHandle) -> Option<serde_json::Value> {
    let raw = std::fs::read_to_string(config_dir(&app)?.join("last-login.json")).ok()?;
    serde_json::from_str(&raw).ok()
}

#[tauri::command]
fn adb_version(app: AppHandle, state: tauri::State<State>) -> Result<String, String> {
    let adb = adb_of(&app, &state)?;
    let v = adb.run(&["version"])?;
    Ok(v.lines().next().unwrap_or("").to_string())
}

pub fn run() {
    tauri::Builder::default()
        .manage(State::default())
        .invoke_handler(tauri::generate_handler![me, sign_in, sign_out, list_devices, enroll, adb_version, profile, last_login])
        .run(tauri::generate_context!())
        .expect("error while running AIO Enroll");
}

use crate::adb::{valid_handle, Adb};
use crate::api::Session;
use crate::{CLASSES, DPC_ADMIN, DPC_PKG};
use std::path::Path;
use std::time::{Duration, Instant};

/// The seven steps the UI shows, in order.
pub const STEPS: [&str; 7] = [
    "Checking the device",
    "Getting a token",
    "Installing the agent",
    "Setting Device Owner",
    "Granting permissions",
    "Starting the agent",
    "Waiting for the server",
];

/// Enrolls one device over adb. Same steps as tools/enroll-adb.sh, with each failure
/// stated in terms of what the person should do. `progress(step, text)` is called as each
/// of the 7 steps in [`STEPS`] starts (a step may report more than once).
pub fn enroll_device(
    adb: &Adb,
    session: &Session,
    handle: &str,
    class: &str,
    apk: &Path,
    progress: &mut dyn FnMut(usize, &str),
) -> Result<(), String> {
    if !valid_handle(handle) {
        return Err("invalid device handle".into());
    }
    if !CLASSES.contains(&class) {
        return Err(format!("unknown class {class}"));
    }

    progress(0, "Checking the device…");
    if adb.account_count(handle) > 0 {
        return Err("The device has an account on it. Factory reset it and do not add an account.".into());
    }
    let owner = adb.owner(handle);
    if owner.set && !owner.ours {
        return Err(format!("Another Device Owner is set ({}). Factory reset required.", owner.package));
    }

    progress(1, "Getting an enrollment token…");
    let tok = session.enroll_token(class).map_err(|e| e.message().to_string())?;

    if !apk.is_file() {
        progress(2, "Downloading the agent…");
        session.download_apk(apk)?;
    }
    let apk_s = apk.to_str().ok_or("bad APK path")?;

    progress(2, "Installing the agent…");
    let out = adb.run_on(handle, &["install", "-r", apk_s]).map_err(|e| format!("Install failed: {e}"))?;
    if !out.contains("Success") {
        return Err(format!("Install failed: {}", out.trim()));
    }

    let component = format!("{DPC_PKG}/{DPC_ADMIN}");
    if owner.set {
        progress(3, "Already our Device Owner, skipping.");
    } else {
        progress(3, "Setting Device Owner…");
        let out = adb
            .shell(handle, &["dpm", "set-device-owner", &component])
            .map_err(|e| format!("set-device-owner failed: {e}"))?;
        if !out.contains("Success") {
            return Err(format!("set-device-owner failed: {}", out.trim()));
        }
    }

    progress(4, "Granting permissions…");
    let _ = adb.shell(handle, &["pm", "grant", DPC_PKG, "android.permission.READ_LOGS"]);
    let _ = adb.shell(handle, &["pm", "grant", DPC_PKG, "android.permission.WRITE_SECURE_SETTINGS"]);
    let _ = adb.shell(handle, &["appops", "set", DPC_PKG, "GET_USAGE_STATS", "allow"]);
    let _ = adb.shell(handle, &["appops", "set", DPC_PKG, "PROJECT_MEDIA", "allow"]);

    let serial = adb.prop(handle, "ro.serialno");
    progress(5, &format!("Starting the agent (class {class})…"));
    let main = format!("{DPC_PKG}/.ui.MainActivity");
    adb.shell(
        handle,
        &["am", "start", "-W", "-n", &main, "--es", "server_url", &tok.server_url, "--es", "enroll_token", &tok.token],
    )
    .map_err(|e| format!("Could not start the agent: {e}"))?;

    progress(6, "Waiting for the server to confirm…");
    let deadline = Instant::now() + Duration::from_secs(30);
    while Instant::now() < deadline {
        if let Ok(m) = session.statuses(&[serial.clone()]) {
            if m.get(&serial).map(|s| s.enrolled).unwrap_or(false) {
                return Ok(());
            }
        }
        std::thread::sleep(Duration::from_secs(1));
    }
    Err("The server did not confirm within 30 s. Check the device has internet.".into())
}

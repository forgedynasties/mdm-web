use serde::Serialize;
use std::path::{Path, PathBuf};
use std::process::Command;

#[derive(Clone, Debug)]
pub struct Adb {
    pub bin: PathBuf,
}

#[derive(Clone, Debug, Serialize, PartialEq)]
pub struct RawDevice {
    pub handle: String,
    pub state: String,
    pub model: String,
}

#[derive(Clone, Debug, Serialize, Default)]
pub struct Details {
    pub serial: String,
    pub manufacturer: String,
    pub model: String,
    pub android: String,
}

#[derive(Clone, Debug, Serialize, Default, PartialEq)]
pub struct Owner {
    pub set: bool,
    pub ours: bool,
    pub package: String,
}

/// adb handles end up as `-s <handle>` arguments (never in a shell string), but keep
/// them to the characters adb itself produces so a hostile value can't smuggle anything.
pub fn valid_handle(h: &str) -> bool {
    !h.is_empty()
        && h.len() <= 128
        && h.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, ':' | '.' | '_' | '-'))
}

impl Adb {
    /// Looks for adb in: `$AIO_ADB`, a bundled `platform-tools/` next to the app, then PATH
    /// and the usual SDK locations.
    pub fn find(bundled_dir: Option<&Path>) -> Option<Adb> {
        let exe = if cfg!(windows) { "adb.exe" } else { "adb" };
        let mut cands: Vec<PathBuf> = Vec::new();
        if let Ok(p) = std::env::var("AIO_ADB") {
            cands.push(PathBuf::from(p));
        }
        if let Some(d) = bundled_dir {
            cands.push(d.join("platform-tools").join(exe));
            cands.push(d.join(exe));
        }
        if let Some(path) = std::env::var_os("PATH") {
            for d in std::env::split_paths(&path) {
                cands.push(d.join(exe));
            }
        }
        if let Some(home) = std::env::var_os("HOME").or_else(|| std::env::var_os("USERPROFILE")) {
            let h = PathBuf::from(home);
            for rel in [
                "Library/Android/sdk/platform-tools",
                "Android/Sdk/platform-tools",
                "AppData/Local/Android/Sdk/platform-tools",
                "Downloads/platform-tools",
            ] {
                cands.push(h.join(rel).join(exe));
            }
        }
        cands.push(PathBuf::from("/opt/homebrew/bin").join(exe));
        cands.push(PathBuf::from("/usr/local/bin").join(exe));
        cands
            .into_iter()
            .find(|p| p.is_file())
            .map(|bin| Adb { bin })
    }

    fn cmd(&self) -> Command {
        #[allow(unused_mut)] // only mutated on Windows
        let mut c = Command::new(&self.bin);
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt;
            c.creation_flags(0x0800_0000); // CREATE_NO_WINDOW
        }
        c
    }

    /// Runs adb with the given args; returns stdout. Errors carry stderr.
    pub fn run(&self, args: &[&str]) -> Result<String, String> {
        let out = self.cmd().args(args).output().map_err(|e| format!("cannot run adb: {e}"))?;
        let stdout = String::from_utf8_lossy(&out.stdout).into_owned();
        if out.status.success() {
            Ok(stdout)
        } else {
            let err = String::from_utf8_lossy(&out.stderr);
            Err(format!("{}{}", stdout.trim(), err.trim()).trim().to_string())
        }
    }

    /// `adb -s <handle> <args...>`; the handle is validated first.
    pub fn run_on(&self, handle: &str, args: &[&str]) -> Result<String, String> {
        if !valid_handle(handle) {
            return Err("invalid device handle".into());
        }
        let mut full = vec!["-s", handle];
        full.extend_from_slice(args);
        self.run(&full)
    }

    pub fn shell(&self, handle: &str, args: &[&str]) -> Result<String, String> {
        let mut full = vec!["shell"];
        full.extend_from_slice(args);
        self.run_on(handle, &full)
    }

    pub fn start_server(&self) {
        let _ = self.run(&["start-server"]);
    }

    pub fn devices(&self) -> Result<Vec<RawDevice>, String> {
        Ok(parse_devices(&self.run(&["devices", "-l"])?))
    }

    pub fn prop(&self, handle: &str, name: &str) -> String {
        self.shell(handle, &["getprop", name]).unwrap_or_default().trim().to_string()
    }

    pub fn details(&self, handle: &str) -> Details {
        Details {
            serial: self.prop(handle, "ro.serialno"),
            manufacturer: self.prop(handle, "ro.product.manufacturer"),
            model: self.prop(handle, "ro.product.model"),
            android: self.prop(handle, "ro.build.version.release"),
        }
    }

    pub fn owner(&self, handle: &str) -> Owner {
        let out = self.shell(handle, &["dumpsys", "device_policy"]).unwrap_or_default();
        parse_owner(&out)
    }

    pub fn account_count(&self, handle: &str) -> usize {
        let out = self.shell(handle, &["dumpsys", "account"]).unwrap_or_default();
        out.matches("Account {").count()
    }
}

pub fn parse_devices(out: &str) -> Vec<RawDevice> {
    out.lines()
        .skip_while(|l| !l.starts_with("List of devices"))
        .skip(1)
        .filter_map(|l| {
            let mut it = l.split_whitespace();
            let handle = it.next()?.to_string();
            let state = it.next().unwrap_or("?").to_string();
            let model = it
                .find_map(|t| t.strip_prefix("model:"))
                .unwrap_or("")
                .replace('_', " ");
            Some(RawDevice { handle, state, model })
        })
        .collect()
}

/// Mirrors `dumpsys device_policy | grep -A2 'Device Owner:'`: the block after the
/// heading names the admin component and package.
pub fn parse_owner(dump: &str) -> Owner {
    let mut lines = dump.lines();
    while let Some(l) = lines.next() {
        if l.contains("Device Owner:") {
            let block: String = lines.by_ref().take(3).collect::<Vec<_>>().join("\n");
            if !block.contains("admin=") {
                break;
            }
            let ours = block.contains(&format!("{}/{}", crate::DPC_PKG, crate::DPC_ADMIN));
            let package = block
                .split("package=")
                .nth(1)
                .and_then(|r| r.split_whitespace().next())
                .unwrap_or("")
                .to_string();
            return Owner { set: true, ours, package };
        }
    }
    Owner::default()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_devices_l() {
        let out = "List of devices attached\n\
            A1B2C3 device usb:1-2 product:rbox model:HK1_RBOX_D8 device:rbox transport_id:1\n\
            0123 unauthorized usb:1-3 transport_id:2\n\n";
        let d = parse_devices(out);
        assert_eq!(d.len(), 2);
        assert_eq!(d[0].model, "HK1 RBOX D8");
        assert_eq!(d[1].state, "unauthorized");
    }

    #[test]
    fn daemon_noise_is_skipped() {
        let out = "* daemon not running; starting now at tcp:5037\n* daemon started successfully\nList of devices attached\n\n";
        assert!(parse_devices(out).is_empty());
    }

    #[test]
    fn owner_ours_and_other() {
        let ours = "  Device Owner: \n    admin=ComponentInfo{aio.app.mdmclient.dpc/aio.app.mdmclient.dpc.MdmDeviceAdminReceiver}\n    name=x package=aio.app.mdmclient.dpc\n";
        let o = parse_owner(ours);
        assert!(o.set && o.ours);
        let other = "  Device Owner: \n    admin=ComponentInfo{com.other/com.other.Rx}\n    package=com.other\n";
        let o = parse_owner(other);
        assert!(o.set && !o.ours && o.package == "com.other");
        assert_eq!(parse_owner("nothing here"), Owner::default());
    }

    #[test]
    fn handle_validation() {
        assert!(valid_handle("192.168.1.5:5555"));
        assert!(valid_handle("A1B2C3"));
        assert!(!valid_handle("x; rm -rf /"));
        assert!(!valid_handle(""));
    }
}

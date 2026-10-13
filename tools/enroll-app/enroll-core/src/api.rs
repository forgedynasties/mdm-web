use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::io::Write;
use std::path::Path;
use std::time::Duration;

pub const DEFAULT_SERVER: &str = "https://mdm.dev.aioapp.com";

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Session {
    pub server: String,
    pub token: String,
    pub username: String,
    pub role: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct Status {
    #[serde(default)]
    pub enrolled: bool,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub class: String,
    #[serde(default)]
    pub agent_version: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct Profile {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub has_avatar: bool,
}

#[derive(Clone, Debug, Deserialize)]
pub struct EnrollToken {
    pub token: String,
    pub server_url: String,
}

/// Why a call failed; `Unauthorized` means the stored session is dead and the app
/// should go back to the sign-in screen.
#[derive(Debug)]
pub enum ApiError {
    Unauthorized(String),
    Other(String),
}

impl ApiError {
    pub fn message(&self) -> &str {
        match self {
            ApiError::Unauthorized(m) | ApiError::Other(m) => m,
        }
    }
}

pub fn normalize_server(s: &str) -> Result<String, String> {
    let s = s.trim().trim_end_matches('/');
    let s = if s.is_empty() { DEFAULT_SERVER } else { s };
    if s.starts_with("https://") || s.starts_with("http://localhost") || s.starts_with("http://127.0.0.1") {
        Ok(s.to_string())
    } else {
        Err("server must be an https:// address".into())
    }
}

fn agent() -> ureq::Agent {
    ureq::AgentBuilder::new()
        .timeout_connect(Duration::from_secs(8))
        .timeout(Duration::from_secs(30))
        .build()
}

fn map_err(e: ureq::Error) -> ApiError {
    match e {
        ureq::Error::Status(code, resp) => {
            let body: serde_json::Value = resp.into_json().unwrap_or_default();
            let msg = body["error"].as_str().map(str::to_string).unwrap_or_else(|| format!("HTTP {code}"));
            if code == 401 {
                ApiError::Unauthorized(msg)
            } else if code == 404 {
                ApiError::Other("This server doesn't support the Enroll app yet. Check the Server address.".into())
            } else {
                ApiError::Other(msg)
            }
        }
        other => ApiError::Other(format!("cannot reach server: {other}")),
    }
}

pub fn login(server: &str, username: &str, password: &str) -> Result<Session, ApiError> {
    let server = normalize_server(server).map_err(ApiError::Other)?;
    let v: serde_json::Value = agent()
        .post(&format!("{server}/api/v1/app/login"))
        .send_json(serde_json::json!({"username": username, "password": password}))
        .map_err(|e| match e {
            // A wrong password is a sign-in failure, not "session expired".
            ureq::Error::Status(401, r) => {
                let b: serde_json::Value = r.into_json().unwrap_or_default();
                ApiError::Other(b["error"].as_str().unwrap_or("invalid credentials").to_string())
            }
            other => map_err(other),
        })?
        .into_json()
        .map_err(|e| ApiError::Other(e.to_string()))?;
    Ok(Session {
        server,
        token: v["token"].as_str().unwrap_or_default().to_string(),
        username: v["username"].as_str().unwrap_or(username).to_string(),
        role: v["role"].as_str().unwrap_or_default().to_string(),
    })
}

impl Session {
    fn auth(&self) -> String {
        format!("Bearer {}", self.token)
    }

    pub fn logout(&self) {
        let _ = agent()
            .post(&format!("{}/api/v1/app/logout", self.server))
            .set("Authorization", &self.auth())
            .call();
    }

    /// Enrollment state for up to 100 serials; unknown serials come back not enrolled.
    pub fn statuses(&self, serials: &[String]) -> Result<HashMap<String, Status>, ApiError> {
        if serials.is_empty() {
            return Ok(HashMap::new());
        }
        let v: serde_json::Value = agent()
            .get(&format!("{}/api/v1/app/enroll-status", self.server))
            .set("Authorization", &self.auth())
            .query("serials", &serials.join(","))
            .call()
            .map_err(map_err)?
            .into_json()
            .map_err(|e| ApiError::Other(e.to_string()))?;
        serde_json::from_value(v["devices"].clone()).map_err(|e| ApiError::Other(e.to_string()))
    }

    /// The signed-in user's display name; `None` fields when the server has no data.
    pub fn profile(&self) -> Result<Profile, ApiError> {
        agent()
            .get(&format!("{}/api/v1/app/me", self.server))
            .set("Authorization", &self.auth())
            .call()
            .map_err(map_err)?
            .into_json()
            .map_err(|e| ApiError::Other(e.to_string()))
    }

    /// Profile picture PNG, or `None` when the user has none (or the server is older).
    pub fn avatar(&self) -> Option<Vec<u8>> {
        use std::io::Read;
        let resp = agent()
            .get(&format!("{}/api/v1/app/avatar", self.server))
            .set("Authorization", &self.auth())
            .call()
            .ok()?;
        let mut buf = Vec::new();
        resp.into_reader().take(2 << 20).read_to_end(&mut buf).ok()?;
        (buf.len() > 8 && buf.starts_with(&[0x89, b'P', b'N', b'G'])).then_some(buf)
    }

    pub fn enroll_token(&self, class: &str) -> Result<EnrollToken, ApiError> {
        agent()
            .post(&format!("{}/api/v1/app/enroll-token", self.server))
            .set("Authorization", &self.auth())
            .send_json(serde_json::json!({ "device_class": class }))
            .map_err(map_err)?
            .into_json()
            .map_err(|e| ApiError::Other(e.to_string()))
    }

    /// Downloads the agent APK (public on the server) to `dest`, via a temp file so a
    /// half download is never mistaken for a good one.
    pub fn download_apk(&self, dest: &Path) -> Result<(), String> {
        let resp = ureq::AgentBuilder::new()
            .timeout_connect(Duration::from_secs(8))
            .timeout(Duration::from_secs(180))
            .build()
            .get(&format!("{}/agent/aio-mdm-dpc.apk", self.server))
            .call()
            .map_err(|e| format!("APK download failed: {e}"))?;
        let tmp = dest.with_extension("part");
        let mut f = std::fs::File::create(&tmp).map_err(|e| e.to_string())?;
        let mut r = resp.into_reader();
        let n = std::io::copy(&mut r, &mut f).map_err(|e| format!("APK download failed: {e}"))?;
        f.flush().map_err(|e| e.to_string())?;
        if n < 100_000 {
            let _ = std::fs::remove_file(&tmp);
            return Err(format!("APK download too small ({n} bytes)"));
        }
        std::fs::rename(&tmp, dest).map_err(|e| e.to_string())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn server_urls() {
        assert_eq!(normalize_server("").unwrap(), DEFAULT_SERVER);
        assert_eq!(normalize_server("https://x.com/ ").unwrap(), "https://x.com");
        assert!(normalize_server("http://evil.com").is_err());
        assert!(normalize_server("http://localhost:8080").is_ok());
    }
}

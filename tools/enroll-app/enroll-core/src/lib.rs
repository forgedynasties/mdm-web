//! Everything the enroll app does that is not a window: talk to adb, talk to the MDM,
//! and run the enrollment steps. No GUI dependency, so it builds and tests anywhere.

pub mod adb;
pub mod api;
pub mod enroll;

/// Device class keys the MDM knows for the DPC agent (same list the server accepts).
pub const CLASSES: &[&str] = &["dongle", "pos", "kds", "kiosk"];

pub const DPC_PKG: &str = "aio.app.mdmclient.dpc";
pub const DPC_ADMIN: &str = "aio.app.mdmclient.dpc.MdmDeviceAdminReceiver";

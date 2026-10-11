//! Read-only check against whatever adb sees: `cargo run -p enroll-core --example probe`
use enroll_core::adb::Adb;

fn main() {
    let adb = Adb::find(None).expect("adb not found");
    println!("adb: {}", adb.bin.display());
    for d in adb.devices().expect("adb devices") {
        println!("{d:?}");
        if d.state == "device" {
            println!("  details  {:?}", adb.details(&d.handle));
            println!("  owner    {:?}", adb.owner(&d.handle));
            println!("  accounts {}", adb.account_count(&d.handle));
        }
    }
}

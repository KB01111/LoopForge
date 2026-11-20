fn main() {
    // The bundled backend is built for amd64; refuse other Windows targets instead of shipping
    // an installer whose backend cannot run.
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows")
        && std::env::var("CARGO_CFG_TARGET_ARCH").as_deref() != Ok("x86_64")
    {
        panic!("LoopForge Desktop supports 64-bit Windows (x86_64) only; the bundled backend is amd64");
    }
    tauri_build::build()
}

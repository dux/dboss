// The desktop shell dboss build tauri wraps every app in. It is the same file for every app and
// both modes: the app-specific values come from shell.json, bundled as a resource.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::collections::HashMap;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::Path;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use tauri::path::BaseDirectory;
use tauri::{Manager, RunEvent, Url, WebviewUrl, WebviewWindowBuilder};

// How long the server gets to answer its health path before the window shows an error.
const READY_TIMEOUT: Duration = Duration::from_secs(60);

#[derive(serde::Deserialize, Default)]
#[serde(default)]
struct Shell {
    app: String,
    title: String,
    width: f64,
    height: f64,
    health: String,
    args: Vec<String>,
    env: HashMap<String, String>,
}

// The running sidecar, killed when the app exits.
struct Child(Mutex<Option<std::process::Child>>);

fn main() {
    tauri::Builder::default()
        .setup(|app| {
            let path = app.path().resolve("shell.json", BaseDirectory::Resource)?;
            let shell: Shell = serde_json::from_str(&std::fs::read_to_string(path)?)?;
            let data = app.path().app_data_dir()?;
            std::fs::create_dir_all(&data)?;
            let port = free_port()?;
            start(app, &shell, port, &data)?;

            let window = WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
                .title(&shell.title)
                .inner_size(shell.width, shell.height)
                .build()?;
            let health = if shell.health.is_empty() { "/".to_string() } else { shell.health.clone() };
            std::thread::spawn(move || {
                if ready(port, &health) {
                    let url = Url::parse(&format!("http://127.0.0.1:{port}/")).expect("loopback url");
                    let _ = window.navigate(url);
                } else {
                    let _ = window.eval(&format!(
                        "document.getElementById('status').textContent = 'The server did not answer {health} within {}s.'",
                        READY_TIMEOUT.as_secs()
                    ));
                }
            });
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("build the tauri app")
        .run(|app, event| {
            if let RunEvent::Exit = event {
                if let Some(child) = app.try_state::<Child>() {
                    if let Some(mut child) = child.0.lock().unwrap().take() {
                        let _ = child.kill();
                        let _ = child.wait();
                    }
                }
            }
        });
}

// A sidecar runs next to the shell executable, where the bundler puts every externalBin, with the
// app data folder as its working directory.
#[cfg(not(feature = "server"))]
fn start(app: &tauri::App, shell: &Shell, port: u16, data: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let binary = std::env::current_exe()?.with_file_name("sidecar");
    let child = std::process::Command::new(binary)
        .args(&shell.args)
        .envs(&shell.env)
        .env("PORT", port.to_string())
        .env("APP_NAME", &shell.app)
        .env("PROC_TYPE", "sidecar")
        .current_dir(data)
        .spawn()?;
    app.manage(Child(Mutex::new(Some(child))));
    Ok(())
}

// A linked server runs on the shell's async runtime and shares its process, so it has nothing to
// kill on exit.
#[cfg(feature = "server")]
fn start(_app: &tauri::App, shell: &Shell, port: u16, data: &Path) -> Result<(), Box<dyn std::error::Error>> {
    for (key, value) in &shell.env {
        std::env::set_var(key, value);
    }
    std::env::set_var("PORT", port.to_string());
    std::env::set_var("APP_NAME", &shell.app);
    std::env::set_var("PROC_TYPE", "server");
    std::env::set_current_dir(data)?;
    tauri::async_runtime::spawn(async move {
        if let Err(err) = app_server::serve(port).await {
            eprintln!("server: {err}");
        }
    });
    Ok(())
}

fn free_port() -> std::io::Result<u16> {
    Ok(TcpListener::bind("127.0.0.1:0")?.local_addr()?.port())
}

// ready polls the health path until it answers 2xx or 3xx.
fn ready(port: u16, path: &str) -> bool {
    let deadline = Instant::now() + READY_TIMEOUT;
    while Instant::now() < deadline {
        if answers(port, path) {
            return true;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    false
}

fn answers(port: u16, path: &str) -> bool {
    let Ok(mut stream) = TcpStream::connect(("127.0.0.1", port)) else {
        return false;
    };
    let _ = stream.set_read_timeout(Some(Duration::from_secs(2)));
    let request = format!("GET {path} HTTP/1.0\r\nHost: 127.0.0.1:{port}\r\nConnection: close\r\n\r\n");
    if stream.write_all(request.as_bytes()).is_err() {
        return false;
    }
    let mut head = [0u8; 12];
    if stream.read_exact(&mut head).is_err() {
        return false;
    }
    // "HTTP/1.1 200"
    matches!(head[9], b'2' | b'3')
}

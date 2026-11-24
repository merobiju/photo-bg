<div align="center">

# Photo BG

**Fast, lightweight (~11MB), and completely offline desktop utility for extracting crisp signatures and removing backgrounds from scanned documents and photos.**

[![Release](https://img.shields.io/github/v/release/merobiju/photo-bg?style=flat-square&color=0969da)](https://github.com/merobiju/photo-bg/releases)
[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev)
[![Wails](https://img.shields.io/badge/Wails-v2-DF0000?style=flat-square)](https://wails.io)
[![License: MIT](https://img.shields.io/badge/License-MIT-2ea44f?style=flat-square)](LICENSE)
[![Network](https://img.shields.io/badge/Network-Zero%20Uploads-1f883d?style=flat-square)](#)

</div>

---
## Overview

**Photo BG** is a native desktop utility designed for administrative offices, legal teams, designers, and everyday users who need clean, transparent signatures or stamps without compromising document privacy.

Most background-removal websites require uploading sensitive IDs, contracts, or handwritten signatures to third-party servers, often compressing the output or adding watermarks. **Photo BG** executes the entire image-processing pipeline locally on your CPU in milliseconds—requiring zero internet access, zero accounts, and zero external APIs.

<!-- Future Demo Video / GIF Placeholder (Uncomment after uploading demo.gif) -->
<!-- <p align="center"><img src="demo.gif" alt="Photo BG Workflow" width="820" /></p> -->

## Why Photo BG?

- **Complete Document Privacy:** Files never leave your machine. No telemetry, analytics, or background network calls occur at any point.
- **Zero Compression Loss:** Retains full native resolution with optional high-definition upscaling for low-DPI scans or mobile camera photos.
- **Instant Local Execution:** Built in **Go** with multi-core worker pools—processing single signatures in milliseconds and batch folders in seconds.
- **Lightweight Footprint:** Ships as a compact ~11MB native binary using your operating system's built-in WebView rather than bundling a heavy browser engine.
## Core Capabilities

- **Adaptive Shadow Normalization:** Uses integral-image local background estimation to remove uneven paper lighting, yellow scanner tint, and mobile camera shadows without eroding thin pen strokes.
- **High-Definition Upscaling & Alpha Matting:** Employs Catmull-Rom bicubic interpolation and soft threshold matting to produce smooth, anti-aliased edges on transparent PNGs.
- **Ink Color & Boldness Presets:**
  - **Original Color:** Preserves natural fountain-pen, gel, or official stamp ink tones.
  - **Pure Black (`#000000`):** Converts faint pencil or gray scans into high-contrast black ink.
  - **Official Blue (`#0A369D`):** Standardizes signatures into crisp royal blue ink for digital forms.
- **Real-Time Side-by-Side Comparison:** Adjust threshold and stroke boldness sliders with instant visual feedback before exporting.
- **Concurrent Batch Folder Mode:** Processes entire directories of PNG, JPG, JPEG, and WEBP images simultaneously across all available CPU cores and exports a single structured ZIP archive.
## Download Packages

Pre-compiled standalone binaries and installers are published on the **[Releases](https://github.com/merobiju/photo-bg/releases)** page. Choose the package matching your operating system:

| Operating System | Architecture | Package Name | Format |
| :--- | :--- | :--- | :--- |
| **Windows 11 / 10** | x64 (64-bit) | `Photo_BG_Setup_Windows_x64.zip` | Official Installer (Recommended) |
| **Windows 11 / 10** | x64 (64-bit) | `Photo_BG_Portable_Windows_x64.zip` | Standalone Portable EXE |
| **Windows 11** | ARM64 (Surface / Snapdragon) | `Photo_BG_Portable_Windows_ARM64.zip` | Standalone Portable EXE |
| **Windows 10 / 8 / 7** | x86 (32-bit Legacy) | `Photo_BG_Windows_x86_32bit.zip` | Standalone Portable EXE |
| **macOS** | Apple Silicon (M1 / M2 / M3 / M4) | `Photo_BG_macOS_AppleSilicon.zip` | Native Darwin Binary |
| **macOS** | Intel (x86_64) | `Photo_BG_macOS_Intel.zip` | Native Darwin Binary |
| **Linux** | Ubuntu / Debian / Mint / Kali (x64) | `Photo_BG_Linux_Ubuntu_Debian_x64.zip` | Native GTK/WebKit Binary |
| **Linux** | Fedora / RHEL / Arch (x64) | `Photo_BG_Linux_Fedora_Arch_x64.tar.gz` | Native GTK/WebKit Binary |
## Installation Guide

### Windows (11 / 10 / Server)

**Option A: Official Installer (Recommended)**
1. Download `Photo_BG_Setup_Windows_x64.zip` from the latest release.
2. Right-click the downloaded ZIP file and select **Extract All...**.
3. Run `Photo_BG_Setup_Windows_x64.exe` and follow the setup wizard. It installs cleanly to your user directory without requiring administrator privileges and creates Start Menu and Desktop shortcuts.

**Option B: Portable Executable**
1. Download `Photo_BG_Portable_Windows_x64.zip` (or `ARM64` / `x86` depending on your hardware).
2. Extract the archive and double-click `Photo_BG_Portable_Windows_x64.exe` to launch immediately.

> **Note on Windows SmartScreen:** Because this open-source binary is not signed with a paid commercial Authenticode certificate, Windows Defender SmartScreen may display a *"Windows protected your PC"* prompt on first launch. Click **More info** and then **Run anyway** to proceed.
### macOS (Apple Silicon & Intel)

1. Download `Photo_BG_macOS_AppleSilicon.zip` (for M1/M2/M3/M4 Macs) or `Photo_BG_macOS_Intel.zip` (for Intel Macs).
2. Double-click the archive to extract the `Photo_BG` binary.
3. Open **Terminal** in the extracted folder, grant execute permission, and clear the macOS quarantine attribute:

```bash
chmod +x Photo_BG
xattr -cr Photo_BG
./Photo_BG
```

*(Alternatively, you can **Control-click (Right-click)** the `Photo_BG` binary in Finder, choose **Open**, and confirm **Open** in the dialog box.)*
### Linux (Ubuntu / Debian / Mint / Fedora / Arch)

1. Download `Photo_BG_Linux_Ubuntu_Debian_x64.zip` or `Photo_BG_Linux_Fedora_Arch_x64.tar.gz` from Releases.
2. Extract the archive and install the native GTK3 and WebKit2GTK runtime libraries if they are not already present on your distribution:

**Ubuntu / Debian / Linux Mint / Kali:**
```bash
sudo apt update && sudo apt install -y libgtk-3-0 libwebkit2gtk-4.1-0
```

**Fedora / RHEL / Rocky Linux:**
```bash
sudo dnf install -y gtk3 webkit2gtk4.1
```

**Arch Linux / Manjaro:**
```bash
sudo pacman -S --needed gtk3 webkit2gtk-4.1
```

3. Make the binary executable and launch it:
```bash
chmod +x Photo_BG
./Photo_BG
```
## How to Use

### 1. Single Image & Signature Extraction
1. **Load Image:** Click **Open Image** (or drag and drop a PNG, JPG, JPEG, or WEBP file onto the canvas).
2. **Select Ink Preset:**
   - Choose **Original** to keep the natural pen or stamp ink color.
   - Choose **Pure Black** for crisp black signatures on official documents.
   - Choose **Blue Ink** for standard royal-blue digital signatures.
3. **Fine-Tune Clarity:** Use the **Threshold / Sensitivity** and **Ink Boldness** sliders while inspecting the live transparent checkerboard preview. Toggle **HD Upscale** if the source scan has low resolution.
4. **Export:** Click **Save PNG** to export a clean, high-resolution image with a true transparent alpha channel.

### 2. Batch Folder Processing
1. Switch to **Batch Mode** from the top toolbar.
2. Select an input directory containing multiple scanned signatures, stamps, or documents.
3. Configure your preferred ink preset and threshold settings once.
4. Click **Process & Export ZIP**—Photo BG will process all supported files concurrently across your CPU cores and package the transparent PNGs into a single ready-to-use ZIP archive.
## Building from Source

To compile **Photo BG** locally from source, ensure you have **Go 1.23+** and the **Wails CLI v2** installed:

```bash
git clone https://github.com/merobiju/photo-bg.git
cd photo-bg

# Build for Windows (with native WebView2 loader)
wails build -clean -tags native_webview2loader -ldflags "-s -w"

# Build for macOS or Linux
wails build -clean -ldflags "-s -w"
```

The compiled executable will be generated inside `build/bin/`.

## Checksum Verification

Every release includes a `SHA256SUMS.txt` file so you can verify the integrity of your downloaded archive before running it:

**Windows (PowerShell):**
```powershell
Get-FileHash .\Photo_BG_Setup_Windows_x64.zip -Algorithm SHA256
```

**macOS / Linux:**
```bash
sha256sum -c SHA256SUMS.txt --ignore-missing
```
## Contributing & Support

Bug reports, feature suggestions, and pull requests are welcome. Please open an issue on the [Issue Tracker](https://github.com/merobiju/photo-bg/issues) before submitting major changes.

For security concerns or private inquiries, reach out directly via email.

## Author & License

**Bijaya Pokhrel**  
Email: [merobiju@proton.me](mailto:merobiju@proton.me)  
Crafted in Nepal.

Released under the [MIT License](LICENSE).
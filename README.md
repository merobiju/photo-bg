# Photo BG

**Fast, lightweight (~11MB), and completely offline desktop utility for extracting signatures and removing backgrounds from documents.**

## Overview & Capabilities

**Photo BG** is a lightweight, local desktop utility built in Go that processes images on your CPU in milliseconds for complete document privacy and zero compression loss. Key features include adaptive shadow normalization, high-definition upscaling, ink color presets, real-time comparison sliders, and concurrent batch folder processing.
## Download Packages

Pre-compiled binaries for Windows, macOS, and Linux are available on the **Releases** page:

* **Windows 11 / 10 (64-bit):** `Photo_BG_Windows_64bit_Most_PCs_Setup.exe` (Installer) or `Photo_BG_Windows_64bit_Most_PCs_Portable.zip` (Portable)
* **Windows 11 (ARM64):** `Photo_BG_Windows_ARM_Surface_Portable.zip`
* **Windows 10 / 8 / 7 (32-bit):** `Photo_BG_Windows_32bit_Old_PCs_Portable.zip`
* **macOS (Apple Silicon & Intel):** `Photo_BG_Mac_New_M1_M2_M3_M4.zip` or `Photo_BG_Mac_Old_Intel.zip` *(also available in `.tar.gz` format)*
* **Linux (x64 - Ubuntu/Debian/Fedora/Arch):** `Photo_BG_Linux_64bit.zip` / `Photo_BG_Linux_64bit.tar.gz`

## Installation Guide

### Windows (11 / 10 / Server)
* **Official Installer:** Run `Photo_BG_Windows_64bit_Most_PCs_Setup.exe` with Administrator rights and follow the setup wizard.
* **Portable Version:** Extract the respective ZIP archive for your architecture (`64-bit`, `ARM64`, or `32-bit`) and run the executable directly.
* *Note:* If Windows Defender SmartScreen triggers a warning, click **More info** followed by **Run anyway**.

### macOS (Apple Silicon & Intel)
1. Download and extract the appropriate package for your Mac (Apple Silicon or Intel).
2. Open Terminal in the extracted folder and run:
   ```bash
   chmod +x Photo_BG
   xattr -cr Photo_BG
   ./Photo_BG
   ```
   *(Or, right-click the binary in Finder, select **Open**, and confirm.)*

### Linux (Ubuntu / Debian / Fedora / Arch)
1. Extract the downloaded ZIP or TAR.GZ package.
2. Install necessary dependencies (such as GTK3 and WebKit2GTK) via your distribution's package manager (`apt`, `dnf`, or `pacman`).
3. Grant execution permissions and run the application:
   ```bash
   chmod +x Photo_BG
   ./Photo_BG
   ```
## How to Use

1. **Single Image & Signature Extraction:** Load an image, select an ink preset (Original, Pure Black, or Blue Ink), fine-tune clarity using threshold and boldness sliders, and export as a transparent PNG.
2. **Batch Folder Processing:** Switch to Batch Mode, select an input directory, configure settings, and process all files concurrently into a ZIP archive.

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

## Contributing & Support

Contributions are welcome. Please open an issue on the Issue Tracker before submitting major changes, or reach out via email for private inquiries.

## Author & License

* **Author:** Bijaya Pokhrel (Crafted in Nepal)
* **Email:** merobiju@proton.me
* **License:** Released under the MIT License

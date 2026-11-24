<div align="center">

# Photo BG

**Ultra-lightweight (~11MB), 100% Offline HD Background & Signature Remover Desktop Studio.**

[![Release](https://img.shields.io/github/v/release/merobiju/photo-bg?style=flat-square&color=blue)](https://github.com/merobiju/photo-bg/releases)
[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat-square&logo=go)](https://go.dev)
[![Wails](https://img.shields.io/badge/Wails-v2-DF0000?style=flat-square)](https://wails.io)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg?style=flat-square)](LICENSE)
[![Privacy](https://img.shields.io/badge/Privacy-100%25%20Offline-success?style=flat-square)](#)

</div>

---

## Overview

Online background removers upload your sensitive documents, IDs, and signatures to external cloud servers and degrade image clarity. **Photo BG** runs entirely on your local CPU in milliseconds—with zero internet connection required.

<!-- पछि तपाईंले demo.gif अपलोड गरेपछि तलको लाइनको कमेन्ट हटाउनुहोला: -->
<!-- <p align="center"><img src="demo.gif" alt="Photo BG Demo" width="820" /></p> -->

## Key Features

- **Zero Cloud Uploads:** 100% offline execution ensures complete document and signature privacy.
- **HD Extraction Engine:** Powered by Catmull-Rom upscaling, integral-image shadow normalization, and soft alpha matting.
- **Ink & Threshold Presets:** Switch between Original Color, Pure Black, and Blue Ink with live boldness sliders.
- **Concurrent Batch Processing:** Process entire folders of scanned files at once and export them as a single ZIP.
- **Native Cross-Platform:** Standalone builds for Windows, macOS (Apple Silicon & Intel), and Linux.

## Downloads

Download the latest pre-compiled package for your operating system from **[Releases](https://github.com/merobiju/photo-bg/releases)**:

| Operating System | Architecture | Package |
| :--- | :--- | :--- |
| **Windows 10 / 11** | x64 (Official Installer) | `Photo_BG_Setup_Windows_x64.zip` |
| **Windows 10 / 11** | x64 (Portable) | `Photo_BG_Portable_Windows_x64.zip` |
| **Windows 11** | ARM64 (Portable) | `Photo_BG_Portable_Windows_ARM64.zip` |
| **macOS** | Apple Silicon (M1–M4) | `Photo_BG_macOS_AppleSilicon.zip` |
| **macOS** | Intel (x64) | `Photo_BG_macOS_Intel.zip` |
| **Linux** | Ubuntu / Debian / Fedora (x64) | `Photo_BG_Linux_x64.zip` |

## Building from Source

Requires **Go 1.23+** and **Wails CLI v2**.

```bash
git clone https://github.com/merobiju/photo-bg.git
cd photo-bg
wails build -clean -tags native_webview2loader
```

## Author & License

Crafted in Nepal by **Bijaya Pokhrel** ([merobiju@proton.me](mailto:merobiju@proton.me)).  
Released under the [MIT License](LICENSE).

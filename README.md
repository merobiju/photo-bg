# Photo BG

Fast, lightweight (~11MB), and completely offline desktop utility for extracting crisp signatures and removing backgrounds from scanned documents and photos.

Built in **Go** and **Wails**. All image processing runs locally on your CPU with zero cloud uploads or third-party API dependencies.

## Features

- **100% Offline Processing:** Documents and signatures never leave your local computer.
- **HD Signature Extraction:** Uses Catmull-Rom resampling, integral-image shadow removal, and soft alpha matting.
- **Ink Presets & Thresholding:** Supports Original Color, Pure Black, and Blue Ink modes with real-time boldness adjustment.
- **Batch Folder Mode:** Process entire folders concurrently and export results into a single ZIP archive.
- **Native Cross-Platform:** Built natively for Windows, macOS, and Linux.

## Downloads

Grab the latest binaries from the [Releases](https://github.com/merobiju/photo-bg/releases) page:

| Platform | Architecture | Package |
| :--- | :--- | :--- |
| **Windows 10 / 11** | x64 (Installer) | `Photo_BG_Setup_Windows_x64.zip` |
| **Windows 10 / 11** | x64 (Portable) | `Photo_BG_Portable_Windows_x64.zip` |
| **macOS** | Apple Silicon (M1-M4) | `Photo_BG_macOS_AppleSilicon.zip` |
| **macOS** | Intel (x64) | `Photo_BG_macOS_Intel.zip` |
| **Linux** | Ubuntu / Debian / Fedora | `Photo_BG_Linux_x64.zip` |

## Building from Source

Requires **Go 1.23+** and **Wails CLI v2**.

```bash
git clone \https://github.com/merobiju/photo-bg.git
cd photo-bg
wails build -clean -tags native_webview2loader
```

## Author

**Bijaya Pokhrel**  
Email: [merobiju@proton.me](mailto:merobiju@proton.me)  
Crafted in Nepal.

## License

Licensed under the [MIT License](LICENSE).

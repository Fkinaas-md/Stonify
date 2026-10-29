# 🪨 stonify

[![Go Version](https://img.shields.io/badge/Go-1.20+-00ADD8?logo=go)](https://golang.org/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Platform](https://img.shields.io/badge/OS-AerynOS%20%7C%20SerpentOS-blueviolet)](#)

**stonify** is a fast, intelligent CLI utility designed to convert Debian/Ubuntu packages (`.deb`) into native `.stone` packages for **AerynOS** (formerly Serpent OS). 

It automates the repackaging of proprietary applications (like Google Chrome, Discord, or Telegram), strips Debian-specific cruft, adapts directory layouts to modern UsrMerge and stateless OS standards, and produces clean binary packages ready to be managed through **moss**.

Conceptually inspired by [xdeb](https://github.com/xdeb-org/xdeb), but built specifically for the `boulder` and `moss` ecosystem.

---

## ✨ Features

* 📦 **Zero `dpkg` Dependencies:** Directly parses Unix `ar` archives natively via Go. Fully supports modern GNU string tables (resolving long filenames like `/0` or `//` for `.tar.zst` payloads).
* 📂 **Stateless & UsrMerge Compliance:**
  * Automatically shifts `/opt/<app>` proprietary payloads into `/usr/lib/<pkg>`.
  * Generates clean wrapper launch scripts in `/usr/bin`.
  * Safely merges legacy root directories (`/bin`, `/sbin`, `/lib`, `/lib64`) into `/usr`.
* 🧹 **Debian Cruft Stripping:** Automatically deletes Debian/Ubuntu-specific system artifacts before packaging (e.g., `/etc/apt`, `/etc/cron.*`, systemd unit files, lintian data, and heavy documentation).
* 🔗 **Dependency Translation:** Scans the Debian `Depends:` field, strips version bounds (e.g., `>= 1.0`), maps known library names to native AerynOS equivalents, and populates the `rundeps:` array in `stone.yaml`.
* 🖥️ **Desktop & Wayland Integration:**
  * Detects Electron payloads (via `app.asar` scanning) and injects `--ozone-platform-hint=auto` and `--enable-features=WaylandWindowDecorations` flags to fix blurry fonts on Wayland.
  * Collects and redistributes icons to all standaard `hicolor` resolution directories and `/usr/share/pixmaps`.
  * Patches `.desktop` files with normalized executable paths and fixed icon references.
* 🛠️ **Declarative Overrides:** Allows overriding flags and dependencies per package through an optional `stonify-rules.json` file, without touching the source code.
* ⚡ **Automated Installation:** Optionally indexes the local package pool and installs the package via `moss` in a single command.

---

## 📋 Requirements

### Build-Time Dependencies (For Compiling)
To compile the `stonify` binary from source, ensure you have:
* **Go** (version 1.20 or newer)
* **Git** (to clone the repository)

### Runtime Dependencies (For Execution)
`stonify` delegates package building and system cache integration to local system tools. The following utilities must be installed on your AerynOS system:

**Core Tools:**
* **`boulder`** — Required to process the generated `stone.yaml` and emit the binary `.stone` packages.
* **`moss`** — Required for repository index generation (`moss index`) and package installation (`moss pkg add`).
* **`bsdtar`** or GNU **`tar`** — Required for payload extraction (`data.tar.*`). `bsdtar` is attempted first.

**Desktop Integration (Optional, but recommended for GUI apps):**
* **`update-desktop-database`** (from `desktop-file-utils`) — Refreshes the desktop entry cache.
* **`gtk-update-icon-cache`** — Updates GTK / hicolor icon theme caches.
* **`kbuildsycoca6`** (KDE Plasma 6 users) — Rebuilds the system configuration cache in KDE.

*You can verify core dependencies in your terminal:*
```bash
which boulder moss bsdtar update-desktop-database gtk-update-icon-cache
```

---

## 🚀 Installation

1. **Clone the repository:**
   ```bash
   git clone https://github.com/your-username/stonify.git
   cd stonify
   ```

2. **Compile the binary:**
   ```bash
   go build -o stonify main.go
   ```

3. **Install the executable globally:**
   ```bash
   sudo install -Dm755 stonify /usr/bin/stonify
   ```

---

## 📖 Usage

### 1. Interactive Conversion
Run `stonify` pointing to any valid Debian binary package. It will build the `.stone` package in your current directory:
```bash
stonify google-chrome-stable_current_amd64.deb
```
*Once `boulder` finishes emitting the archive, `stonify` will prompt you to install it locally.*

### 2. Unattended Auto-Install
Pass the `-i` (or `--install`) flag to automatically trigger `moss index`, register the local repo, and install the package without prompts:
```bash
stonify -i discord-0.0.60.deb
```

### 3. Manual Installation
If you choose not to install immediately, you can deploy the generated `.stone` package at any time:
```bash
moss index
sudo moss repo remove local 2>/dev/null
sudo moss repo add --comment "Local Packages" local file://$(pwd)/stone.index
sudo moss pkg add package-name
```

---

## ⚙️ Configuration (`stonify-rules.json`)

To override dependencies or provide custom execution flags without modifying the Go source code, create a `stonify-rules.json` file in your working directory. 

*Example:*
```json
{
  "google-chrome-stable": {
    "extra_deps": [
      "nss",
      "alsa-lib"
    ],
    "exec_flags": "--enable-features=UseOzonePlatform --ozone-platform=wayland"
  },
  "yandex-browser": {
    "extra_deps": [
      "ffmpeg"
    ],
    "exec_flags": "--no-sandbox"
  }
}
```
* **`extra_deps`**: An array of AerynOS package names injected directly into the `stone.yaml` `rundeps` block.
* **`exec_flags`**: CLI parameters inserted into the `.desktop` file's `Exec=` line (smartly placed before argument specifiers like `%U`).

---

## 🏗️ Architecture Pipeline

```text
[ .deb Archive ]
       │
       ├── 1. extractArArchive (Pure Go parser with GNU string table support)
       │      ├── control.tar.* ──> 2. parseDebControl (Metadata & depMap translation)
       │      └── data.tar.*    ──> 3. extractPayload (bsdtar/tar execution)
       │
       ├── 4. normalizeAndFix
       │      ├── removeDebianCruft (Removes /etc/apt, cron, docs, systemd)
       │      ├── UsrMerge (/opt -> /usr/lib, /bin -> /usr/bin)
       │      ├── Icon Harvester (Auto-distribute to hicolor & pixmaps)
       │      └── .desktop Patcher (Exec paths, Icon keys, Wayland flags)
       │
       ├── 5. generateStoneYaml (Generates recipe manifest with rundeps)
       ├── 6. boulder build (Builds isolated .stone package)
       └── 7. moss integration (Cleans workspace, creates stone.index, installs)
```

---

## 📝 License

This project is licensed under the [MIT License](LICENSE).

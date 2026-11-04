## Download and run (no Go or Docker needed)

| Your computer | Download |
|---|---|
| Windows | `recura_VERSION_windows_amd64.zip` |
| Mac with Apple chip (M1 to M4) | `recura_VERSION_darwin_arm64.tar.gz` |
| Mac with Intel chip | `recura_VERSION_darwin_amd64.tar.gz` |
| Linux | `recura_VERSION_linux_amd64.tar.gz` (or `_arm64` for ARM servers and Raspberry Pi) |

1. Extract the file.
2. Windows: double-click `start-recura.bat`. Mac and Linux: run `./start-recura.sh`.
3. Open http://localhost:8080 and ask a question, then ask it again in different words.

With no API key, two mock models answer so you can try caching and fallback straight away. To use a real model, set `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` or `GEMINI_API_KEY` before starting.

On a Mac, the first run may say the developer cannot be verified. Right-click `recura`, choose Open, then Open again; or run `xattr -d com.apple.quarantine recura` in Terminal.

Docker: `docker run -p 8080:8080 ghcr.io/prm01/recura:latest`

Verify downloads with `checksums.txt` (SHA-256). What changed: see [CHANGELOG.md](https://github.com/Prm01/recura/blob/main/CHANGELOG.md).

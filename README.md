# Swift Print

Swift Print is a small Windows utility that exposes a local HTTP API for sending
raw print data to installed printers. It runs in the system tray, listens only on
`127.0.0.1`, and protects print endpoints with a secret token.

It is designed for browser printing, POS printing, kiosk printing, receipt
printing, ESC/POS commands, thermal printers, and other raw printer workflows
that need a simple local print bridge.

## Features

- Windows system tray app
- Local-only HTTP server
- Raw print support for TCP, USB, LPT, COM, and spooler fallback paths
- Printer discovery from the Windows registry
- Simple settings page for port and token changes
- Standard Windows installer with uninstaller (Inno Setup)
- Useful for browser printing, POS printing, receipt printing, and local print bridge integrations

## Project Layout

```text
cmd/
  swift-print/             Main tray app and local print server
installer/swift-print.iss  Inno Setup installer script
examples/test.html         Browser smoke-test page
config.example.json        Example runtime config
```

## Security Notes

Swift Print controls local printers, so treat the token like a password.

- Keep the server bound to `127.0.0.1`.
- Do not publish your real `config.json`.
- The default `secret_token` is `change-me`; change it before using Swift Print.
- Send the token with API requests as the `X-API-KEY` header.
- Browser clients can call the API by sending the `X-API-KEY` header.
- The settings page accepts changes only from same-origin localhost requests.

If `config.json` is missing, Swift Print creates one beside the executable with
port `1818` and token `change-me`.

## Build

Swift Print is a Windows app. If you build from Linux or macOS, set
`GOOS=windows GOARCH=amd64` as shown below.

First install `goversioninfo` and generate the Windows resources (icon, version
info and manifest). Do this before building so the generated `resource.syso` is
included in the executable:

```bash
go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest
mkdir -p dist
cd cmd/swift-print && goversioninfo -64 -o resource.syso versioninfo.json && cd ../..
```

Then build the app:

```bash
go build -trimpath -ldflags "-H=windowsgui" -o "dist/Swift Print.exe" ./cmd/swift-print
```

If you are building from Linux or macOS, cross-build with:

```bash
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H=windowsgui" -o "dist/Swift Print.exe" ./cmd/swift-print
```

Do not pack or obfuscate the executable (UPX, garble, etc.). Packed binaries are
much more likely to be flagged by antivirus software.

## Installer

The installer is built with [Inno Setup 7.1.0](https://jrsoftware.org/isinfo.php)
on Windows. After building `dist/Swift Print.exe`, run from the repository root:

```bash
iscc installer/swift-print.iss
iscc /DAppVersion=1.2.3 installer/swift-print.iss
```

This produces `dist/Swift Print Setup.exe`.

The installer:

- installs per user to `%LOCALAPPDATA%\Programs\Swift Print` (no admin prompt)
- adds Swift Print to Windows startup and the Start Menu
- keeps the existing `config.json` on upgrade
- registers an uninstaller in Settings > Apps (and Control Panel > Programs)

If you installed an earlier version of Swift Print in `C:\Swift Print`, quit it
from the tray and delete that folder manually. Copy `config.json` to the new
install folder first if you want to keep your port and token.

## Uninstall

Open Settings > Apps > Installed apps (or Control Panel > Programs and
Features), select Swift Print, and choose Uninstall. This stops the app and
removes its files, its config, the startup entry and the Start Menu shortcuts.

## Antivirus and SmartScreen Warnings

Swift Print releases are not code signed yet, so Windows SmartScreen may show
"Windows protected your PC" when you run the installer. Click **More info** and
then **Run anyway**.

Download Swift Print only from this repository's GitHub Releases page. If
Microsoft Defender flags the installer, it is a false positive caused by the
missing signature; please open an issue so it can be reported.

## Runtime Config

Example:

```json
{
  "port": "1818",
  "secret_token": "change-me"
}
```

Copy `config.example.json` to `config.json` for local testing, then change the
token before use.

## API

Health check:

```http
GET / HTTP/1.1
Host: 127.0.0.1:1818
```

List printers:

```http
GET /printers HTTP/1.1
Host: 127.0.0.1:1818
X-API-KEY: your-secret-token
```

Print raw base64 data:

```http
POST /print HTTP/1.1
Host: 127.0.0.1:1818
Content-Type: application/json
X-API-KEY: your-secret-token

{
  "printer": "Printer Name",
  "data": "BASE64_ENCODED_RAW_PRINT_BYTES"
}
```

Smoke test:

The settings page (tray menu > Open Settings) has a built-in Test Print
section. It loads the installed printers and sends a small test print using the
saved port and token.

To test from a separate web page, as your own app would call the bridge, open
`examples/test.html` in a browser, enter the Swift Print port and secret token,
then load printers or send a small test print.

## License

Swift Print is released under the MIT License. See `LICENSE` for details.

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
- Optional single-file installer build
- Useful for browser printing, POS printing, receipt printing, and local print bridge integrations

## Project Layout

```text
cmd/
  swift-print/             Main tray app and local print server
  swift-print-installer/   Installer command
examples/test.html         Browser smoke-test page
config.example.json        Example runtime config
```

The old local project folders are ignored so this repository can be published
without generated binaries, debug builds, or private config files.

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

First install `rsrc` and generate the Windows icon resources. Do this before building so the generated `rsrc.syso` files are included in the executables:

```bash
go install github.com/akavel/rsrc@latest
mkdir -p dist
rsrc -ico cmd/swift-print/swiftprint.ico -arch amd64 -o cmd/swift-print/rsrc.syso
rsrc -ico cmd/swift-print-installer/swiftprint.ico -arch amd64 -o cmd/swift-print-installer/rsrc.syso
```

After the icon resources are generated, build from Windows with:

```bash
go build -ldflags "-H=windowsgui" -o "dist/Swift Print.exe" ./cmd/swift-print
go build -o "dist/Swift Print Installer.exe" ./cmd/swift-print-installer
```

If you are building from Linux or macOS, cross-build with:

```bash
GOOS=windows GOARCH=amd64 go build -ldflags "-H=windowsgui" -o "dist/Swift Print.exe" ./cmd/swift-print
GOOS=windows GOARCH=amd64 go build -o "dist/Swift Print Installer.exe" ./cmd/swift-print-installer
```

## Single-File Installer Release

To embed the app and config inside the installer, place release payload files in:

```text
cmd/swift-print-installer/payload/Swift Print.exe
cmd/swift-print-installer/payload/config.json
```

Then build the installer command. The payload files are ignored by Git, but Go
will embed them into the installer binary when they exist at build time.

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

Browser smoke test:

```text
examples/test.html
```

Open the file in a browser, enter the Swift Print port and secret token, then
load printers or send a small test print.

## License

Swift Print is released under the MIT License. See `LICENSE` for details.

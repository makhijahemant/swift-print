package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/systray"

	_ "embed"
)

const (
	defaultPort        = "1818"
	defaultSecretToken = "change-me"
	maxPrintBodyBytes  = 16 << 20

	appName    = "Swift Print"
	configName = "config.json"
	iconName   = "swiftprint.ico"

	printersKey = `SYSTEM\CurrentControlSet\Control\Print\Printers`
)

//go:embed swiftprint.ico
var trayIcon []byte

var config AppConfig

type AppConfig struct {
	Port        string `json:"port"`
	SecretToken string `json:"secret_token"`
}

type PrintRequest struct {
	Printer string `json:"printer"`
	Data    string `json:"data"`
}

//////////////////////////////////////////////////////////////
// WINDOWS DLLS
//////////////////////////////////////////////////////////////

var (
	winspool = syscall.NewLazyDLL("winspool.drv")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	openPrinter     = winspool.NewProc("OpenPrinterW")
	closePrinter    = winspool.NewProc("ClosePrinter")
	startDocPrinter = winspool.NewProc("StartDocPrinterW")
	writePrinter    = winspool.NewProc("WritePrinter")
	endDocPrinter   = winspool.NewProc("EndDocPrinter")

	regOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	regEnumKeyExW    = advapi32.NewProc("RegEnumKeyExW")
	regCloseKey      = advapi32.NewProc("RegCloseKey")
	regQueryValueExW = advapi32.NewProc("RegQueryValueExW")
)

//////////////////////////////////////////////////////////////
// WINDOWS CONSTANTS + STRUCTS
//////////////////////////////////////////////////////////////

const (
	hkeyCurrentUser  = 0x80000001
	hkeyLocalMachine = 0x80000002

	keyRead          = 0x20019
	errorSuccess     = 0
	errorNoMoreItems = 259
)

type DOC_INFO_1 struct {
	pDocName    *uint16
	pOutputFile *uint16
	pDatatype   *uint16
}

//////////////////////////////////////////////////////////////
// CONFIG
//////////////////////////////////////////////////////////////

func appDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func configPath() string {
	return filepath.Join(appDir(), configName)
}

func defaultConfig() AppConfig {
	return AppConfig{
		Port:        defaultPort,
		SecretToken: defaultSecretToken,
	}
}

func loadConfig() AppConfig {
	cfg := defaultConfig()
	file, err := os.ReadFile(configPath())
	if err == nil {
		json.Unmarshal(file, &cfg)
	}
	if err := validatePort(cfg.Port); err != nil {
		cfg.Port = defaultPort
	}
	if strings.TrimSpace(cfg.SecretToken) == "" {
		cfg.SecretToken = defaultSecretToken
	}
	saveConfig(cfg)
	return cfg
}

func saveConfig(cfg AppConfig) error {
	if err := os.MkdirAll(appDir(), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
}

func validatePort(port string) error {
	port = strings.TrimSpace(port)
	if port == "" {
		return fmt.Errorf("port number is required")
	}

	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return fmt.Errorf("port number must be between 1 and 65535")
	}

	return nil
}

//////////////////////////////////////////////////////////////
// WINDOWS PRINTING
//////////////////////////////////////////////////////////////

// getPrinterPort reads the port name for a printer from the registry,
// e.g. "127.0.0.1:9100" for a Standard TCP/IP port, or "USB001" for USB.
func getPrinterPort(printerName string) (string, error) {
	keyPath := printersKey + `\` + printerName
	var key uintptr
	pathPtr, _ := syscall.UTF16PtrFromString(keyPath)
	r1, _, err := regOpenKeyExW.Call(hkeyLocalMachine, uintptr(unsafe.Pointer(pathPtr)), 0, keyRead, uintptr(unsafe.Pointer(&key)))
	if r1 != errorSuccess {
		return "", fmt.Errorf("RegOpenKeyEx(%s): %w", keyPath, err)
	}
	defer regCloseKey.Call(key)

	valueName, _ := syscall.UTF16PtrFromString("Port")
	buf := make([]uint16, 256)
	bufSize := uint32(len(buf) * 2)
	r1, _, err = regQueryValueExW.Call(
		key,
		uintptr(unsafe.Pointer(valueName)),
		0, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bufSize)),
	)
	if r1 != errorSuccess {
		return "", fmt.Errorf("RegQueryValueEx(Port): %w", err)
	}
	return syscall.UTF16ToString(buf), nil
}

// printViaTCP connects directly to host:port and writes raw bytes,
// completely bypassing the Windows print spooler.
func printViaTCP(hostport string, data []byte) error {
	conn, err := net.DialTimeout("tcp", hostport, 5*time.Second)
	if err != nil {
		return fmt.Errorf("TCP connect to %s: %w", hostport, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	n, err := conn.Write(data)
	if err != nil {
		return fmt.Errorf("TCP write: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("TCP: only wrote %d of %d bytes", n, len(data))
	}
	fmt.Printf("[PRINT] TCP direct: sent %d bytes to %s\n", n, hostport)
	return nil
}

// printViaSpooler sends raw bytes through the Windows print spooler.
// Used for every port type other than a TCP host:port.
func printViaSpooler(printerName string, data []byte) error {
	var handle syscall.Handle
	namePtr, _ := syscall.UTF16PtrFromString(printerName)

	r1, _, err := openPrinter.Call(uintptr(unsafe.Pointer(namePtr)), uintptr(unsafe.Pointer(&handle)), 0)
	if r1 == 0 {
		return fmt.Errorf("OpenPrinter: %w", err)
	}
	defer closePrinter.Call(uintptr(handle))

	docName, _ := syscall.UTF16PtrFromString("Swift Print")
	rawDatatype, _ := syscall.UTF16PtrFromString("RAW")
	docInfo := DOC_INFO_1{pDocName: docName, pDatatype: rawDatatype}

	jobID, _, err := startDocPrinter.Call(uintptr(handle), 1, uintptr(unsafe.Pointer(&docInfo)))
	if jobID == 0 {
		return fmt.Errorf("StartDocPrinter: %w", err)
	}
	defer endDocPrinter.Call(uintptr(handle))

	var written uint32
	ok, _, err := writePrinter.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(unsafe.SliceData(data))),
		uintptr(len(data)),
		uintptr(unsafe.Pointer(&written)),
	)
	if ok == 0 {
		return fmt.Errorf("WritePrinter: %w", err)
	}
	if written != uint32(len(data)) {
		return fmt.Errorf("spooler: only wrote %d of %d bytes", written, len(data))
	}
	fmt.Printf("[PRINT] Spooler: sent %d bytes to %s\n", written, printerName)
	return nil
}

func printRawWindows(printerName string, data []byte) error {
	if strings.TrimSpace(printerName) == "" {
		return fmt.Errorf("printer name required")
	}
	if len(data) == 0 {
		return fmt.Errorf("empty print data")
	}

	port, err := getPrinterPort(printerName)
	if err != nil {
		fmt.Printf("[PRINT] Could not read port from registry (%v) — falling back to spooler\n", err)
		return printViaSpooler(printerName, data)
	}

	// Network printer (e.g. "127.0.0.1:9100"): send straight to the socket.
	if isTCPPort(port) {
		fmt.Printf("[PRINT] TCP port (%s) — direct socket\n", port)
		return printViaTCP(port, data)
	}

	// USB, LPT, COM, PDF/file ports and anything else go through the spooler.
	fmt.Printf("[PRINT] Port %s — spooler\n", port)
	return printViaSpooler(printerName, data)
}

// isTCPPort reports whether port is a network address like "192.168.1.50:9100".
// Ports like "PORTPROMPT:" (Print to PDF), "FILE:", "COM1:" or "C:\out.pdf" also
// contain a colon, so a non-empty host and a numeric port are both required.
func isTCPPort(port string) bool {
	host, portNumber, err := net.SplitHostPort(strings.TrimSpace(port))
	if err != nil || host == "" {
		return false
	}
	return validatePort(portNumber) == nil
}

func getPrintersFromRegistry() ([]string, error) {
	names, err := enumRegistrySubkeys(hkeyLocalMachine, printersKey)
	if err != nil {
		return nil, err
	}

	connectionNames, _ := enumRegistrySubkeys(hkeyCurrentUser, `Printers\Connections`)
	for _, name := range connectionNames {
		parts := strings.Split(strings.TrimLeft(name, ","), ",")
		if len(parts) >= 2 {
			names = append(names, `\\`+parts[0]+`\`+parts[1])
		}
	}

	sort.Strings(names)
	return uniqueStrings(names), nil
}

//////////////////////////////////////////////////////////////
// REGISTRY
//////////////////////////////////////////////////////////////

func enumRegistrySubkeys(root uintptr, path string) ([]string, error) {
	var key uintptr
	pathPtr, _ := syscall.UTF16PtrFromString(path)
	r1, _, err := regOpenKeyExW.Call(root, uintptr(unsafe.Pointer(pathPtr)), 0, keyRead, uintptr(unsafe.Pointer(&key)))
	if r1 != errorSuccess {
		return nil, err
	}
	defer regCloseKey.Call(key)

	var names []string
	for i := uint32(0); ; i++ {
		name := make([]uint16, 256)
		nameLen := uint32(len(name))
		r1, _, err := regEnumKeyExW.Call(
			key,
			uintptr(i),
			uintptr(unsafe.Pointer(&name[0])),
			uintptr(unsafe.Pointer(&nameLen)),
			0,
			0,
			0,
			0,
		)
		if r1 == errorNoMoreItems {
			break
		}
		if r1 != errorSuccess {
			return nil, err
		}
		names = append(names, syscall.UTF16ToString(name[:nameLen]))
	}
	return names, nil
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var unique []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	return unique
}

//////////////////////////////////////////////////////////////
// AUTH
//////////////////////////////////////////////////////////////

// tokenMatches reports whether given equals the configured secret token (constant time).
func tokenMatches(given string) bool {
	expected := []byte(config.SecretToken)
	actual := []byte(given)
	return len(expected) == len(actual) && subtle.ConstantTimeCompare(expected, actual) == 1
}

//////////////////////////////////////////////////////////////
// HTTP ENDPOINTS
//////////////////////////////////////////////////////////////

// apiHandler adds the checks every API endpoint shares: headers, CORS preflight,
// the allowed method (empty means any) and, if requireKey is set, the X-API-KEY header.
func apiHandler(method string, requireKey bool, handle http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		setAPIHeaders(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if method != "" && r.Method != method {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if requireKey && !tokenMatches(r.Header.Get("X-API-KEY")) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		handle(w, r)
	}
}

func printHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPrintBodyBytes)
	body, _ := io.ReadAll(r.Body)
	var req PrintRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	raw, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		http.Error(w, "Invalid base64 data", http.StatusBadRequest)
		return
	}

	err = printRawWindows(req.Printer, raw)

	json.NewEncoder(w).Encode(map[string]any{
		"success": err == nil,
		"error":   errorString(err),
	})
}

func printersHandler(w http.ResponseWriter, r *http.Request) {
	printers, err := getPrintersFromRegistry()
	json.NewEncoder(w).Encode(map[string]any{
		"success":  err == nil,
		"printers": printers,
		"error":    errorString(err),
	})
}

func testHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{
		"status": "running",
		"port":   config.Port,
	})
}

func settingsHandler(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	// The browser must send a real Origin on the form POST for the same-origin check.
	w.Header().Set("Referrer-Policy", "same-origin")

	if r.Method == http.MethodGet {
		settingsTemplate.Execute(w, settingsPage{AppConfig: config})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !isSameOriginSettingsRequest(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	if !tokenMatches(r.FormValue("current_secret_token")) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	next := AppConfig{
		Port:        strings.TrimSpace(r.FormValue("port")),
		SecretToken: strings.TrimSpace(r.FormValue("secret_token")),
	}
	err = validateSettings(next)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Open the new port before saving, so a busy port leaves everything unchanged.
	portChanged := next.Port != config.Port
	var newListener net.Listener
	if portChanged {
		newListener, err = net.Listen("tcp", "127.0.0.1:"+next.Port)
		if err != nil {
			http.Error(w, fmt.Sprintf("Port %s is not available: %v", next.Port, err), http.StatusBadRequest)
			return
		}
	}

	err = saveConfig(next)
	if err != nil {
		if newListener != nil {
			newListener.Close()
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	config = next

	if !portChanged {
		settingsTemplate.Execute(w, settingsPage{AppConfig: config})
		return
	}

	startServing(newListener)
	systray.SetTooltip(appName + " running on port " + config.Port)
	settingsTemplate.Execute(w, settingsPage{
		AppConfig:   config,
		RedirectURL: "http://127.0.0.1:" + config.Port + "/settings",
	})
}

func validateSettings(cfg AppConfig) error {
	err := validatePort(cfg.Port)
	if err != nil {
		return err
	}
	if cfg.SecretToken == "" {
		return fmt.Errorf("Secret token is required")
	}
	if len(cfg.SecretToken) > 256 {
		return fmt.Errorf("Secret token must be 256 characters or fewer")
	}
	return nil
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func setAPIHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-KEY")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.Header().Set("Content-Type", "application/json")
}

func isSameOriginSettingsRequest(r *http.Request) bool {
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		return requestHeaderMatchesHost(origin, r.Host)
	}
	if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" {
		return requestHeaderMatchesHost(referer, r.Host)
	}
	return true
}

func requestHeaderMatchesHost(rawURL, host string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, host)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// settingsPage is the data for settingsTemplate. When RedirectURL is set, the page
// shows "Port changed" and sends the browser to the new port instead of the form.
type settingsPage struct {
	AppConfig
	RedirectURL string
}

var settingsTemplate = template.Must(template.New("settings").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
{{if .RedirectURL}}<meta http-equiv="refresh" content="1;url={{.RedirectURL}}">{{end}}
<title>Swift Print Settings</title>
<style>
body{font-family:Arial,sans-serif;background:#f5f7fb;margin:0;padding:40px;color:#172033}
main{max-width:520px;margin:0 auto;background:#fff;border:1px solid #dde3ee;border-radius:8px;padding:24px;box-shadow:0 8px 24px rgba(20,30,50,.08)}
label{display:block;font-weight:700;margin-top:16px}
input,select,textarea{width:100%;box-sizing:border-box;margin-top:6px;padding:10px;border:1px solid #c8d1df;border-radius:6px;font-size:14px;font-family:inherit}
button{margin-top:20px;padding:10px 14px;border:0;border-radius:6px;background:#1769c2;color:#fff;font-weight:700;cursor:pointer}
button.secondary{margin-top:8px;background:#fff;color:#1769c2;border:1px solid #c8d1df}
button:disabled{background:#91a4bd;cursor:not-allowed}
hr{border:0;border-top:1px solid #dde3ee;margin:24px 0}
.hint{font-size:13px;color:#5d6b80;margin-top:12px;line-height:1.4}
.status{margin-top:12px;font-weight:700;min-height:1.2em}
a{color:#1769c2}
</style>
</head>
<body>
<main>
{{if .RedirectURL}}
<h2>Port changed to {{.Port}}</h2>
<p>Redirecting to <a href="{{.RedirectURL}}">{{.RedirectURL}}</a>&hellip;</p>
{{else}}
<h2>Swift Print Settings</h2>
<form method="post">
<input type="hidden" name="current_secret_token" value="{{.SecretToken}}">
<label>Secret Token</label>
<input name="secret_token" value="{{.SecretToken}}" required>
<label>Port Number</label>
<input name="port" value="{{.Port}}" required>
<button type="submit">Save Settings</button>
</form>
<p class="hint">Keep this token private and use it as the X-API-KEY header for print requests.</p>
<hr>
<h3>Test Print</h3>
<label for="printers">Printer</label>
<select id="printers"></select>
<button type="button" class="secondary" onclick="loadPrinters()">Refresh Printers</button>
<label for="printText">Text to Print</label>
<textarea id="printText" rows="4">Hello from Swift Print!</textarea>
<button type="button" id="printButton" onclick="printNow()">Print Test Page</button>
<div class="status" id="status"></div>
<p class="hint">Tests use the saved settings. Save first if you changed the token.</p>
<script>
function apiKey() {
	return document.querySelector("[name=current_secret_token]").value;
}

function setStatus(message, color) {
	const el = document.getElementById("status");
	el.textContent = message;
	el.style.color = color || "#172033";
}

async function request(path, options) {
	options = options || {};
	const response = await fetch(path, {
		...options,
		headers: {"X-API-KEY": apiKey(), ...(options.headers || {})}
	});
	if (!response.ok) {
		const text = (await response.text()).trim();
		throw new Error(text || response.status + " " + response.statusText);
	}
	return response.json();
}

function textToBase64(text) {
	let binary = "";
	new TextEncoder().encode(text).forEach(b => { binary += String.fromCharCode(b); });
	return btoa(binary);
}

async function loadPrinters() {
	const select = document.getElementById("printers");
	try {
		const result = await request("/printers");
		const printers = Array.isArray(result.printers) ? result.printers : [];
		const previous = select.value;
		select.innerHTML = "";
		printers.forEach(name => {
			const option = document.createElement("option");
			option.value = name;
			option.textContent = name;
			select.appendChild(option);
		});
		if (printers.includes(previous)) select.value = previous;
		if (!result.success) {
			setStatus("Failed to load printers: " + result.error, "red");
		} else {
			setStatus(printers.length ? "Loaded " + printers.length + " printer(s)" : "No printers found", printers.length ? "green" : "red");
		}
	} catch (error) {
		setStatus("Failed to load printers: " + error.message, "red");
	}
}

async function printNow() {
	const printer = document.getElementById("printers").value;
	const text = document.getElementById("printText").value;
	if (!printer) {
		setStatus("Select a printer first", "red");
		return;
	}
	const button = document.getElementById("printButton");
	button.disabled = true;
	setStatus("Printing...");
	try {
		const result = await request("/print", {
			method: "POST",
			headers: {"Content-Type": "application/json"},
			body: JSON.stringify({printer: printer, data: textToBase64(text + "\n")})
		});
		setStatus(result.success ? "Print sent to " + printer : "Print failed: " + result.error, result.success ? "green" : "red");
	} catch (error) {
		setStatus("Print failed: " + error.message, "red");
	} finally {
		button.disabled = false;
	}
}

loadPrinters();
</script>
{{end}}
</main>
</body>
</html>`))

//////////////////////////////////////////////////////////////
// SYSTEM TRAY
//////////////////////////////////////////////////////////////

func runTray() {
	systray.Run(onTrayReady, func() {})
}

func onTrayReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle(appName)
	systray.SetTooltip(appName + " running on port " + config.Port)

	settings := systray.AddMenuItem("Open Settings", "Open Swift Print settings")
	quit := systray.AddMenuItem("Quit", "Quit Swift Print")

	go func() {
		for {
			select {
			case <-settings.ClickedCh:
				openBrowser("http://127.0.0.1:" + config.Port + "/settings")
			case <-quit.ClickedCh:
				systray.Quit()
				os.Exit(0)
			}
		}
	}()
}

func openBrowser(url string) {
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

//////////////////////////////////////////////////////////////
// SERVER
//////////////////////////////////////////////////////////////

// currentServer is the HTTP server that is listening right now.
var currentServer *http.Server

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", apiHandler("", false, testHandler))
	mux.HandleFunc("/print", apiHandler(http.MethodPost, true, printHandler))
	mux.HandleFunc("/printers", apiHandler(http.MethodGet, true, printersHandler))
	mux.HandleFunc("/settings", settingsHandler)
	return mux
}

// startServing serves on listener and stops the server that was running before.
func startServing(listener net.Listener) {
	previousServer := currentServer

	newServer := &http.Server{Handler: newMux()}
	currentServer = newServer
	go func() {
		err := newServer.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			fmt.Println("[SERVER]", err)
		}
	}()

	if previousServer != nil {
		// Run in the background: Shutdown waits for the current request (the Save) to finish.
		go previousServer.Shutdown(context.Background())
	}
}

func startServer() {
	listener, err := net.Listen("tcp", "127.0.0.1:"+config.Port)
	if err != nil {
		fmt.Println("[SERVER]", err)
		return
	}
	startServing(listener)
}

//////////////////////////////////////////////////////////////
// MAIN
//////////////////////////////////////////////////////////////

func main() {
	config = loadConfig()
	go startServer()
	runTray()
}

package main

import (
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

// printViaFile opens a Windows device port (USB001, LPT1, COM3, etc.) directly
// using CreateFile and writes raw bytes, bypassing the print spooler entirely.
func printViaFile(portName string, data []byte) error {
	path := `\\.\` + portName
	pathPtr, _ := syscall.UTF16PtrFromString(path)

	handle, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_WRITE,
		0,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return fmt.Errorf("CreateFile(%s): %w", path, err)
	}
	defer syscall.CloseHandle(handle)

	var written uint32
	if err := syscall.WriteFile(handle, data, &written, nil); err != nil {
		return fmt.Errorf("WriteFile(%s): %w", path, err)
	}
	if written != uint32(len(data)) {
		return fmt.Errorf("%s: only wrote %d of %d bytes", path, written, len(data))
	}
	fmt.Printf("[PRINT] File direct: sent %d bytes to %s\n", written, path)
	return nil
}

// printViaSpooler sends raw bytes through the Windows print spooler.
// Last-resort fallback for port types we don't handle directly.
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

	upper := strings.ToUpper(strings.TrimSpace(port))

	switch {
	// Network printer: TCP direct socket (e.g. "127.0.0.1:9100")
	case func() bool { _, _, e := net.SplitHostPort(port); return e == nil }():
		fmt.Printf("[PRINT] TCP port (%s) — direct socket\n", port)
		return printViaTCP(port, data)

	// USB printer: direct device file (e.g. "USB001")
	case strings.HasPrefix(upper, "USB"):
		fmt.Printf("[PRINT] USB port (%s) — direct file\n", port)
		return printViaSpooler(printerName, data)
		// return printViaFile(port, data)

	// Parallel port: direct device file (e.g. "LPT1")
	case strings.HasPrefix(upper, "LPT"):
		fmt.Printf("[PRINT] LPT port (%s) — direct file\n", port)
		return printViaSpooler(printerName, data)
		// return printViaFile(port, data)

	// Serial port: direct device file (e.g. "COM3")
	case strings.HasPrefix(upper, "COM"):
		fmt.Printf("[PRINT] COM port (%s) — direct file\n", port)
		return printViaSpooler(printerName, data)
		// return printViaFile(port, data)

	// Unknown port type: let the spooler handle it
	default:
		fmt.Printf("[PRINT] Unknown port type (%s) — falling back to spooler\n", port)
		return printViaSpooler(printerName, data)
	}
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

func checkAPIKey(r *http.Request) bool {
	expected := []byte(config.SecretToken)
	actual := []byte(r.Header.Get("X-API-KEY"))
	return len(expected) == len(actual) && subtle.ConstantTimeCompare(expected, actual) == 1
}

//////////////////////////////////////////////////////////////
// HTTP ENDPOINTS
//////////////////////////////////////////////////////////////

func printHandler(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	setAPIHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !checkAPIKey(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

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
	setSecurityHeaders(w)
	setAPIHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !checkAPIKey(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	printers, err := getPrintersFromRegistry()
	json.NewEncoder(w).Encode(map[string]any{
		"success":  err == nil,
		"printers": printers,
		"error":    errorString(err),
	})
}

func testHandler(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	setAPIHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"status": "running",
		"port":   config.Port,
	})
}

func settingsHandler(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	switch r.Method {
	case http.MethodGet:
		settingsTemplate.Execute(w, config)
	case http.MethodPost:
		if !isSameOriginSettingsRequest(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid form", http.StatusBadRequest)
			return
		}
		if !checkCurrentSettingsToken(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next := config
		next.Port = strings.TrimSpace(r.FormValue("port"))
		next.SecretToken = strings.TrimSpace(r.FormValue("secret_token"))

		if err := validatePort(next.Port); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if next.SecretToken == "" {
			http.Error(w, "Secret token is required", http.StatusBadRequest)
			return
		}
		if len(next.SecretToken) > 256 {
			http.Error(w, "Secret token must be 256 characters or fewer", http.StatusBadRequest)
			return
		}
		if err := saveConfig(next); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		config = next
		settingsTemplate.Execute(w, config)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
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

func checkCurrentSettingsToken(r *http.Request) bool {
	expected := []byte(config.SecretToken)
	actual := []byte(r.FormValue("current_secret_token"))
	return len(expected) == len(actual) && subtle.ConstantTimeCompare(expected, actual) == 1
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var settingsTemplate = template.Must(template.New("settings").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>Swift Print Settings</title>
<style>
body{font-family:Arial,sans-serif;background:#f5f7fb;margin:0;padding:40px;color:#172033}
main{max-width:520px;margin:0 auto;background:#fff;border:1px solid #dde3ee;border-radius:8px;padding:24px;box-shadow:0 8px 24px rgba(20,30,50,.08)}
label{display:block;font-weight:700;margin-top:16px}
input{width:100%;box-sizing:border-box;margin-top:6px;padding:10px;border:1px solid #c8d1df;border-radius:6px;font-size:14px}
button{margin-top:20px;padding:10px 14px;border:0;border-radius:6px;background:#1769c2;color:#fff;font-weight:700;cursor:pointer}
.hint{font-size:13px;color:#5d6b80;margin-top:12px;line-height:1.4}
</style>
</head>
<body>
<main>
<h2>Swift Print Settings</h2>
<form method="post">
<input type="hidden" name="current_secret_token" value="{{.SecretToken}}">
<label>Secret Token</label>
<input name="secret_token" value="{{.SecretToken}}" required>
<label>Port Number</label>
<input name="port" value="{{.Port}}" required>
<button type="submit">Save Settings</button>
</form>
<p class="hint">Restart Swift Print after changing the port. Keep this token private and use it as the X-API-KEY header for print requests.</p>
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

func startServer() {
	http.HandleFunc("/", testHandler)
	http.HandleFunc("/print", printHandler)
	http.HandleFunc("/printers", printersHandler)
	http.HandleFunc("/settings", settingsHandler)

	if err := http.ListenAndServe("127.0.0.1:"+config.Port, nil); err != nil {
		fmt.Println("[SERVER]", err)
	}
}

//////////////////////////////////////////////////////////////
// MAIN
//////////////////////////////////////////////////////////////

func main() {
	config = loadConfig()
	go startServer()
	runTray()
}

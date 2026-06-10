package main

import (
	"embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

var installDir string

const (
	exeName    = `Swift Print.exe`
	configName = `config.json`
	payloadDir = `payload`
)

//go:embed payload
var embeddedPayload embed.FS

func main() {

	fmt.Println("===================================")
	fmt.Println("Swift Print Installer")
	fmt.Println("===================================")

	// Create install directory
	fmt.Println("[1/4] Creating installation directory...")

	err := os.MkdirAll(installDir, 0755)
	if err != nil {
		panic(err)
	}

	// Copy files
	fmt.Println("[2/4] Copying files...")

	err = copyInstallerFiles()
	if err != nil {
		panic(err)
	}

	// Add startup entry
	fmt.Println("[3/4] Configuring Windows startup...")

	err = addStartupEntry()
	if err != nil {
		panic(err)
	}

	// Launch application
	fmt.Println("[4/4] Starting Swift Print...")

	err = startApplication()
	if err != nil {
		panic(err)
	}

	fmt.Println()
	fmt.Println("Installation completed successfully.")
}

func init() {
	systemDrive := os.Getenv("SystemDrive")

	if systemDrive == "" {
		systemDrive = "C:"
	}

	installDir = filepath.Join(systemDrive, "\\Swift Print")
}

func copyInstallerFiles() error {

	files := []string{
		exeName,
		configName,
	}

	total := len(files)

	for i, file := range files {

		dst := filepath.Join(installDir, file)

		fmt.Printf("    [%d/%d] %s\n", i+1, total, file)

		if err := writeInstallerFile(file, dst); err != nil {
			return err
		}
	}

	return nil
}

func writeInstallerFile(name, dst string) error {

	err := os.MkdirAll(filepath.Dir(dst), 0755)
	if err != nil {
		return err
	}

	embeddedPath := filepath.ToSlash(filepath.Join(payloadDir, name))
	if data, err := embeddedPayload.ReadFile(embeddedPath); err == nil {
		return os.WriteFile(dst, data, 0644)
	}

	payloadPath := filepath.Join(payloadDir, name)
	if _, err := os.Stat(payloadPath); err == nil {
		return copyFile(payloadPath, dst)
	}

	if _, err := os.Stat(name); err == nil {
		return copyFile(name, dst)
	}

	return fmt.Errorf("%s not found in embedded payload, %s folder, or installer directory", name, payloadDir)
}

func copyFile(src, dst string) error {

	err := os.MkdirAll(filepath.Dir(dst), 0755)
	if err != nil {
		return err
	}

	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	target, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer target.Close()

	_, err = io.Copy(target, source)
	return err
}

func addStartupEntry() error {

	key, _, err := registry.CreateKey(
		registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`,
		registry.SET_VALUE,
	)

	if err != nil {
		return err
	}
	defer key.Close()

	exePath := filepath.Join(installDir, exeName)

	return key.SetStringValue("SwiftPrint", exePath)
}

func startApplication() error {

	exePath := filepath.Join(installDir, exeName)

	cmd := exec.Command(exePath)

	return cmd.Start()
}

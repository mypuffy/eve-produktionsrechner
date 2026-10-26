package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	AppName  = "EVE Produktionsrechner"
	Version  = "1.0.0"
	Port     = 52173
	Callback = "http://localhost:52173/callback"
)

//go:embed web
var webFS embed.FS

// Scopes the app asks for. The same list has to be ticked when registering the app.
var Scopes = []string{
	"esi-assets.read_assets.v1",
	"esi-characters.read_blueprints.v1",
	"esi-skills.read_skills.v1",
	"esi-universe.read_structures.v1",
}

type Character struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	RefreshToken string `json:"refreshToken"`
	accessToken  string
	accessExp    time.Time
}

type Config struct {
	ClientID   string       `json:"clientId"`
	Characters []*Character `json:"characters"`
}

var (
	dataDir string
	cfgMu   sync.Mutex
	cfg     Config
	server  *http.Server
)

func configPath() string { return filepath.Join(dataDir, "config.json") }

func loadConfig() {
	b, err := os.ReadFile(configPath())
	if err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
}

func saveConfig() error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(configPath(), b, 0600)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func main() {
	base, err := os.UserConfigDir()
	if err != nil {
		base, _ = os.UserHomeDir()
	}
	dataDir = filepath.Join(base, "EVE-Produktionsrechner")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		log.Fatal(err)
	}
	logFile, _ := os.OpenFile(filepath.Join(dataDir, "log.txt"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if logFile != nil {
		log.SetOutput(multiWriter{os.Stdout, logFile})
	}
	loadConfig()
	loadSDECache()

	url := fmt.Sprintf("http://localhost:%d/", Port)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", Port))
	if err != nil {
		// Läuft schon: einfach die Oberfläche öffnen.
		openBrowser(url)
		return
	}

	sub, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	registerAPI(mux)
	server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	fmt.Printf("%s %s\n", AppName, Version)
	fmt.Printf("Läuft auf %s – dieses Fenster offen lassen. Zum Beenden schließen oder in der Oberfläche auf „Beenden“ klicken.\n", url)
	go func() {
		time.Sleep(300 * time.Millisecond)
		openBrowser(url)
	}()
	if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func shutdown() {
	go func() {
		time.Sleep(200 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		os.Exit(0)
	}()
}

type multiWriter struct{ a, b *os.File }

func (m multiWriter) Write(p []byte) (int, error) {
	if m.a != nil {
		_, _ = m.a.Write(p)
	}
	return m.b.Write(p)
}

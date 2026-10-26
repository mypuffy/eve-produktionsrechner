package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Nur Anfragen von der eigenen Oberfläche zulassen (Schutz gegen fremde Webseiten).
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return r.Method == "GET"
	}
	return o == "http://localhost:52173" || o == "http://127.0.0.1:52173"
}

func api(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostOK := r.Host == "localhost:52173" || r.Host == "127.0.0.1:52173"
		if !hostOK || !sameOrigin(r) || (r.Method != "GET" && r.Header.Get("X-App") != "1") {
			writeErr(w, 403, "nicht erlaubt")
			return
		}
		fn(w, r)
	}
}

func charFromQuery(w http.ResponseWriter, r *http.Request) *Character {
	id, _ := strconv.ParseInt(r.URL.Query().Get("char"), 10, 64)
	ch := findChar(id)
	if ch == nil {
		writeErr(w, 404, "Charakter nicht gefunden – bitte neu anmelden")
	}
	return ch
}

func registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("/callback", handleCallback)

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		u, err := loginURL()
		if err != nil {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		http.Redirect(w, r, u, http.StatusFound)
	})

	mux.HandleFunc("/api/state", api(func(w http.ResponseWriter, r *http.Request) {
		cfgMu.Lock()
		chars := []map[string]any{}
		for _, c := range cfg.Characters {
			chars = append(chars, map[string]any{"id": c.ID, "name": c.Name})
		}
		clientID := cfg.ClientID
		cfgMu.Unlock()
		sdeMu.RLock()
		sdeInfo := map[string]any{"loaded": sde != nil}
		if sde != nil {
			sdeInfo["build"] = sde.Build
			sdeInfo["at"] = sde.At
			sdeInfo["blueprints"] = len(sde.Blueprints)
		}
		sdeMu.RUnlock()
		sdeStatus.Lock()
		sdeInfo["running"] = sdeStatus.Running
		sdeInfo["step"] = sdeStatus.Step
		sdeInfo["pct"] = sdeStatus.Pct
		sdeInfo["error"] = sdeStatus.Err
		sdeStatus.Unlock()
		writeJSON(w, map[string]any{
			"version": Version, "clientId": clientID, "characters": chars, "sde": sdeInfo,
			"callback": Callback, "scopes": Scopes, "hubs": Hubs, "dataDir": dataDir,
		})
	}))

	mux.HandleFunc("/api/config", api(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ClientID string `json:"clientId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, 400, "ungültige Eingabe")
			return
		}
		cfgMu.Lock()
		cfg.ClientID = strings.TrimSpace(body.ClientID)
		err := saveConfig()
		cfgMu.Unlock()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("/api/logout", api(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.URL.Query().Get("char"), 10, 64)
		cfgMu.Lock()
		out := cfg.Characters[:0]
		for _, c := range cfg.Characters {
			if c.ID != id {
				out = append(out, c)
			}
		}
		cfg.Characters = out
		_ = saveConfig()
		cfgMu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("/api/sde/update", api(func(w http.ResponseWriter, r *http.Request) {
		startSDEUpdate(r.URL.Query().Get("force") == "1")
		writeJSON(w, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("/api/search", api(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, searchProducts(r.URL.Query().Get("q"), 25))
	}))

	mux.HandleFunc("/api/blueprint", api(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.URL.Query().Get("product"), 10, 64)
		bp := blueprintForProduct(id)
		if bp == nil {
			writeErr(w, 404, "Keine Blaupause für dieses Item gefunden")
			return
		}
		type mat struct {
			TypeID    int64  `json:"typeId"`
			Name      string `json:"name"`
			Qty       int64  `json:"qty"`
			Buildable bool   `json:"buildable"`
		}
		mats := []mat{}
		for _, m := range bp.Materials {
			mats = append(mats, mat{m.TypeID, typeName(m.TypeID), m.Qty, blueprintForProduct(m.TypeID) != nil})
		}
		writeJSON(w, map[string]any{
			"blueprintId": bp.ID, "blueprint": typeName(bp.ID),
			"productId": bp.Product, "product": typeName(bp.Product), "perRun": bp.ProductQty,
			"maxRuns": bp.MaxRuns, "materials": mats,
		})
	}))

	mux.HandleFunc("/api/systems", api(func(w http.ResponseWriter, r *http.Request) {
		s, at, err := getSystems(r.URL.Query().Get("force") == "1")
		if err != nil {
			writeErr(w, 502, "Systemindizes nicht abrufbar: "+err.Error())
			return
		}
		writeJSON(w, map[string]any{"systems": s, "at": at})
	}))

	mux.HandleFunc("/api/prices", api(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Hub   string  `json:"hub"`
			Types []int64 `json:"types"`
			Force bool    `json:"force"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, 400, "ungültige Eingabe")
			return
		}
		if len(body.Types) > 500 {
			body.Types = body.Types[:500]
		}
		p, errs := getPrices(body.Hub, body.Types, body.Force)
		out := map[string]MarketPrice{}
		for k, v := range p {
			out[strconv.FormatInt(k, 10)] = v
		}
		writeJSON(w, map[string]any{"prices": out, "errors": errs, "at": time.Now()})
	}))

	mux.HandleFunc("/api/assets", api(func(w http.ResponseWriter, r *http.Request) {
		ch := charFromQuery(w, r)
		if ch == nil {
			return
		}
		a, err := getAssets(ch, r.URL.Query().Get("force") == "1")
		if err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		writeJSON(w, a)
	}))

	mux.HandleFunc("/api/blueprints", api(func(w http.ResponseWriter, r *http.Request) {
		ch := charFromQuery(w, r)
		if ch == nil {
			return
		}
		b, err := getBlueprints(ch, r.URL.Query().Get("force") == "1")
		if err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		writeJSON(w, b)
	}))

	mux.HandleFunc("/api/skills", api(func(w http.ResponseWriter, r *http.Request) {
		ch := charFromQuery(w, r)
		if ch == nil {
			return
		}
		s, err := getSkills(ch, r.URL.Query().Get("force") == "1")
		if err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		writeJSON(w, s)
	}))

	// Gespeicherte Rechnungen des Nutzers
	dataFile := func() string { return filepath.Join(dataDir, "rechnungen.json") }
	mux.HandleFunc("/api/data", api(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			b, err := os.ReadFile(dataFile())
			if err != nil {
				writeJSON(w, map[string]any{})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, 20<<20))
		if err != nil || !json.Valid(b) {
			writeErr(w, 400, "ungültige Daten")
			return
		}
		tmp := dataFile() + ".tmp"
		if err := os.WriteFile(tmp, b, 0600); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if err := os.Rename(tmp, dataFile()); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}))

	mux.HandleFunc("/api/quit", api(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		shutdown()
	}))
}

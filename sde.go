package main

// Statische Spieldaten (SDE) von CCP: Blaupausen-Rezepte und Itemnamen.
// Wird einmal heruntergeladen (ZIP mit JSON-Lines) und als kleine Cache-Datei abgelegt.

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sdeLatestInfo = "https://developers.eveonline.com/static-data/tranquility/latest.jsonl"
	sdeZipURL     = "https://developers.eveonline.com/static-data/eve-online-static-data-latest-jsonl.zip"
)

type Material struct {
	TypeID int64 `json:"t"`
	Qty    int64 `json:"q"`
}

type Blueprint struct {
	ID         int64      `json:"id"`
	Product    int64      `json:"p"`
	ProductQty int64      `json:"pq"`
	Materials  []Material `json:"m"`
	MaxRuns    int64      `json:"mr,omitempty"`
}

type TypeName struct {
	EN string `json:"en"`
	DE string `json:"de,omitempty"`
}

type SDECache struct {
	Build      string               `json:"build"`
	At         time.Time            `json:"at"`
	Blueprints map[int64]*Blueprint `json:"blueprints"`
	Names      map[int64]TypeName   `json:"names"`
}

var (
	sdeMu     sync.RWMutex
	sde       *SDECache
	byProduct map[int64]*Blueprint
	sdeStatus = struct {
		sync.Mutex
		Running bool
		Step    string
		Err     string
		Pct     float64
	}{}
)

func sdeCachePath() string { return filepath.Join(dataDir, "spieldaten.json") }

func loadSDECache() {
	b, err := os.ReadFile(sdeCachePath())
	if err != nil {
		return
	}
	var c SDECache
	if err := json.Unmarshal(b, &c); err != nil {
		log.Println("Spieldaten-Cache unlesbar:", err)
		return
	}
	setSDE(&c)
}

func setSDE(c *SDECache) {
	bp := make(map[int64]*Blueprint, len(c.Blueprints))
	for _, b := range c.Blueprints {
		// Bei mehreren Blaupausen für dasselbe Produkt die erste behalten
		if old, ok := bp[b.Product]; !ok || b.ID < old.ID {
			bp[b.Product] = b
		}
	}
	sdeMu.Lock()
	sde = c
	byProduct = bp
	sdeMu.Unlock()
}

func typeName(id int64) string {
	sdeMu.RLock()
	defer sdeMu.RUnlock()
	if sde != nil {
		if n, ok := sde.Names[id]; ok {
			if lang == "de" && n.DE != "" {
				return n.DE
			}
			return n.EN
		}
	}
	return fmt.Sprintf("Typ %d", id)
}

var lang = "de"

func blueprintByID(id int64) *Blueprint {
	sdeMu.RLock()
	defer sdeMu.RUnlock()
	if sde == nil {
		return nil
	}
	return sde.Blueprints[id]
}

func blueprintForProduct(id int64) *Blueprint {
	sdeMu.RLock()
	defer sdeMu.RUnlock()
	return byProduct[id]
}

type SearchHit struct {
	TypeID int64  `json:"typeId"`
	Name   string `json:"name"`
	Alt    string `json:"alt,omitempty"`
}

func searchProducts(q string, limit int) []SearchHit {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	sdeMu.RLock()
	defer sdeMu.RUnlock()
	if sde == nil {
		return nil
	}
	type scored struct {
		h SearchHit
		s int
	}
	var res []scored
	for pid := range byProduct {
		n := sde.Names[pid]
		en, de := strings.ToLower(n.EN), strings.ToLower(n.DE)
		s := -1
		switch {
		case en == q || de == q:
			s = 0
		case strings.HasPrefix(en, q) || strings.HasPrefix(de, q):
			s = 1
		case strings.Contains(en, q) || strings.Contains(de, q):
			s = 2
		}
		if s < 0 {
			continue
		}
		h := SearchHit{TypeID: pid, Name: n.EN}
		if lang == "de" && n.DE != "" {
			h.Name = n.DE
			if n.DE != n.EN {
				h.Alt = n.EN
			}
		}
		res = append(res, scored{h, s})
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].s != res[j].s {
			return res[i].s < res[j].s
		}
		return res[i].h.Name < res[j].h.Name
	})
	out := []SearchHit{}
	for i := 0; i < len(res) && i < limit; i++ {
		out = append(out, res[i].h)
	}
	return out
}

func setStep(step string, pct float64) {
	sdeStatus.Lock()
	sdeStatus.Step = step
	sdeStatus.Pct = pct
	sdeStatus.Unlock()
	log.Println("Spieldaten:", step)
}

// latestBuild asks CCP for the current SDE build number.
func latestBuild() (string, error) {
	req, _ := http.NewRequest("GET", sdeLatestInfo, nil)
	req.Header.Set("User-Agent", userAgent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m["_key"] == "sde" {
			for _, k := range []string{"buildNumber", "build", "_value"} {
				if v, ok := m[k]; ok {
					return fmt.Sprint(v), nil
				}
			}
			b, _ := json.Marshal(m)
			return string(b), nil
		}
	}
	return "", fmt.Errorf("Build-Nummer nicht gefunden")
}

func startSDEUpdate(force bool) {
	sdeStatus.Lock()
	if sdeStatus.Running {
		sdeStatus.Unlock()
		return
	}
	sdeStatus.Running, sdeStatus.Err = true, ""
	sdeStatus.Unlock()
	go func() {
		err := updateSDE(force)
		sdeStatus.Lock()
		sdeStatus.Running = false
		if err != nil {
			sdeStatus.Err = err.Error()
			log.Println("Spieldaten-Fehler:", err)
		}
		sdeStatus.Unlock()
	}()
}

func updateSDE(force bool) error {
	setStep("Prüfe Version", 0)
	build, err := latestBuild()
	if err != nil {
		build = "" // kein Abbruch, dann einfach laden
	}
	sdeMu.RLock()
	have := sde != nil && build != "" && sde.Build == build
	sdeMu.RUnlock()
	if have && !force {
		setStep("Spieldaten sind aktuell", 1)
		return nil
	}

	setStep("Lade Spieldaten von CCP herunter", 0)
	tmp := filepath.Join(dataDir, "sde-download.zip")
	defer os.Remove(tmp)
	req, _ := http.NewRequest("GET", sdeZipURL, nil)
	req.Header.Set("User-Agent", userAgent())
	cl := &http.Client{Timeout: 30 * time.Minute}
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("Download fehlgeschlagen: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Download fehlgeschlagen: HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 256*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return werr
			}
			done += int64(n)
			if total > 0 {
				setStepQuiet(fmt.Sprintf("Lade Spieldaten (%d von %d MB)", done>>20, total>>20), float64(done)/float64(total)*0.85)
			} else {
				setStepQuiet(fmt.Sprintf("Lade Spieldaten (%d MB)", done>>20), 0.4)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return fmt.Errorf("Download abgebrochen: %w", rerr)
		}
	}
	f.Close()

	setStep("Lese Blaupausen und Namen", 0.9)
	c, err := parseSDEZip(tmp)
	if err != nil {
		return err
	}
	if build == "" {
		build = time.Now().Format("2006-01-02")
	}
	c.Build, c.At = build, time.Now()
	b, _ := json.Marshal(c)
	if err := os.WriteFile(sdeCachePath(), b, 0600); err != nil {
		return err
	}
	setSDE(c)
	setStep(fmt.Sprintf("Fertig: %d Blaupausen geladen", len(c.Blueprints)), 1)
	return nil
}

func setStepQuiet(step string, pct float64) {
	sdeStatus.Lock()
	sdeStatus.Step, sdeStatus.Pct = step, pct
	sdeStatus.Unlock()
}

func parseSDEZip(file string) (*SDECache, error) {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return nil, fmt.Errorf("ZIP unlesbar: %w", err)
	}
	defer zr.Close()
	var bpFile, typeFile *zip.File
	for _, f := range zr.File {
		switch strings.ToLower(path.Base(f.Name)) {
		case "blueprints.jsonl":
			bpFile = f
		case "types.jsonl":
			typeFile = f
		}
	}
	if bpFile == nil || typeFile == nil {
		return nil, fmt.Errorf("blueprints.jsonl oder types.jsonl fehlt im Download")
	}
	c := &SDECache{Blueprints: map[int64]*Blueprint{}, Names: map[int64]TypeName{}}

	err = eachLine(bpFile, func(m map[string]any) {
		id := toInt(m["_key"])
		if id == 0 {
			id = toInt(m["blueprintTypeID"])
		}
		acts, _ := m["activities"].(map[string]any)
		man, _ := acts["manufacturing"].(map[string]any)
		if man == nil {
			return
		}
		prods := listOf(man["products"])
		if len(prods) == 0 {
			return
		}
		bp := &Blueprint{ID: id, Product: typeIDOf(prods[0]), ProductQty: toInt(prods[0]["quantity"]), MaxRuns: toInt(m["maxProductionLimit"])}
		for _, mat := range listOf(man["materials"]) {
			bp.Materials = append(bp.Materials, Material{TypeID: typeIDOf(mat), Qty: toInt(mat["quantity"])})
		}
		if bp.Product == 0 || len(bp.Materials) == 0 {
			return
		}
		if bp.ProductQty == 0 {
			bp.ProductQty = 1
		}
		c.Blueprints[id] = bp
	})
	if err != nil {
		return nil, err
	}

	err = eachLine(typeFile, func(m map[string]any) {
		id := toInt(m["_key"])
		if id == 0 {
			return
		}
		if pub, ok := m["published"].(bool); ok && !pub {
			return
		}
		var n TypeName
		switch v := m["name"].(type) {
		case string:
			n.EN = v
		case map[string]any:
			n.EN, _ = v["en"].(string)
			n.DE, _ = v["de"].(string)
		}
		if n.EN == "" {
			return
		}
		if n.DE == n.EN {
			n.DE = ""
		}
		c.Names[id] = n
	})
	if err != nil {
		return nil, err
	}
	if len(c.Blueprints) == 0 || len(c.Names) == 0 {
		return nil, fmt.Errorf("Spieldaten im unerwarteten Format (%d Blaupausen, %d Namen)", len(c.Blueprints), len(c.Names))
	}
	return c, nil
}

func eachLine(f *zip.File, fn func(map[string]any)) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			fn(m)
		}
	}
	return sc.Err()
}

func listOf(v any) []map[string]any {
	arr, _ := v.([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, x := range arr {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func typeIDOf(m map[string]any) int64 {
	for _, k := range []string{"typeID", "typeId", "type_id"} {
		if v, ok := m[k]; ok {
			return toInt(v)
		}
	}
	return 0
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}

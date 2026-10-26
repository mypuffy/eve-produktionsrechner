package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	esiBase       = "https://esi.evetech.net"
	esiCompatDate = "2025-09-30"
)

var httpClient = &http.Client{Timeout: 60 * time.Second}

func userAgent() string { return "EVE-Produktionsrechner/" + Version }

type esiError struct {
	Status int
	Msg    string
}

func (e *esiError) Error() string { return fmt.Sprintf("ESI HTTP %d: %s", e.Status, e.Msg) }

// esiDo sends one ESI request. Returns body and response headers.
func esiDo(method, path, token string, body any) ([]byte, http.Header, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	try := func(p string) ([]byte, http.Header, int, error) {
		if rdr != nil {
			if s, ok := rdr.(*bytes.Reader); ok {
				_, _ = s.Seek(0, io.SeekStart)
			}
		}
		req, _ := http.NewRequest(method, esiBase+p, rdr)
		req.Header.Set("User-Agent", userAgent())
		req.Header.Set("X-Compatibility-Date", esiCompatDate)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, nil, 0, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			var e struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(b, &e)
			if e.Error == "" {
				e.Error = strings.TrimSpace(string(b))
				if len(e.Error) > 200 {
					e.Error = e.Error[:200]
				}
			}
			return nil, resp.Header, resp.StatusCode, &esiError{resp.StatusCode, e.Error}
		}
		return b, resp.Header, resp.StatusCode, nil
	}
	b, h, st, err := try(path)
	if st == 404 && !strings.Contains(path, "?") && !strings.HasSuffix(path, "/") {
		// Fallback falls eine Route nur mit Schrägstrich am Ende existiert.
		if b2, h2, _, err2 := try(path + "/"); err2 == nil {
			return b2, h2, nil
		}
	}
	return b, h, err
}

// esiGetPaged fetches all pages of a list route and appends JSON arrays.
func esiGetPaged[T any](path, token string) ([]T, error) {
	var all []T
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	b, h, err := esiDo("GET", path, token, nil)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	pages, _ := strconv.Atoi(h.Get("X-Pages"))
	for p := 2; p <= pages; p++ {
		b, _, err := esiDo("GET", fmt.Sprintf("%s%spage=%d", path, sep, p), token, nil)
		if err != nil {
			return nil, err
		}
		var part []T
		if err := json.Unmarshal(b, &part); err != nil {
			return nil, err
		}
		all = append(all, part...)
	}
	return all, nil
}

// ---------- simple time-based cache ----------

type cacheEntry struct {
	at  time.Time
	val any
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cacheEntry{}
)

func cached[T any](key string, ttl time.Duration, force bool, fn func() (T, error)) (T, time.Time, error) {
	cacheMu.Lock()
	e, ok := cache[key]
	cacheMu.Unlock()
	if ok && !force && time.Since(e.at) < ttl {
		return e.val.(T), e.at, nil
	}
	v, err := fn()
	if err != nil {
		if ok { // alte Daten sind besser als keine
			return e.val.(T), e.at, nil
		}
		var zero T
		return zero, time.Time{}, err
	}
	now := time.Now()
	cacheMu.Lock()
	cache[key] = cacheEntry{now, v}
	cacheMu.Unlock()
	return v, now, nil
}

// ---------- public data ----------

type System struct {
	ID    int64   `json:"id"`
	Name  string  `json:"name"`
	Index float64 `json:"index"` // manufacturing cost index in %
}

func getSystems(force bool) ([]System, time.Time, error) {
	return cached("systems", time.Hour, force, func() ([]System, error) {
		b, _, err := esiDo("GET", "/industry/systems", "", nil)
		if err != nil {
			return nil, err
		}
		var raw []struct {
			SolarSystemID int64 `json:"solar_system_id"`
			CostIndices   []struct {
				Activity  string  `json:"activity"`
				CostIndex float64 `json:"cost_index"`
			} `json:"cost_indices"`
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			return nil, err
		}
		ids := make([]int64, 0, len(raw))
		out := make([]System, 0, len(raw))
		for _, s := range raw {
			idx := 0.0
			for _, c := range s.CostIndices {
				if c.Activity == "manufacturing" {
					idx = c.CostIndex * 100
				}
			}
			ids = append(ids, s.SolarSystemID)
			out = append(out, System{ID: s.SolarSystemID, Index: idx})
		}
		names, err := resolveNames(ids)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].Name = names[out[i].ID]
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	})
}

var (
	nameMu    sync.Mutex
	nameCache = map[int64]string{}
)

// resolveNames turns station/system/character IDs into names via POST /universe/names.
func resolveNames(ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	var todo []int64
	nameMu.Lock()
	for _, id := range ids {
		if n, ok := nameCache[id]; ok {
			out[id] = n
		} else {
			todo = append(todo, id)
		}
	}
	nameMu.Unlock()
	for len(todo) > 0 {
		n := len(todo)
		if n > 1000 {
			n = 1000
		}
		chunk := todo[:n]
		todo = todo[n:]
		b, _, err := esiDo("POST", "/universe/names", "", chunk)
		if err != nil {
			// eine ungültige ID lässt den ganzen Block scheitern: dann einzeln
			if len(chunk) == 1 {
				continue
			}
			for _, id := range chunk {
				if r, e := resolveNames([]int64{id}); e == nil {
					for k, v := range r {
						out[k] = v
					}
				}
			}
			continue
		}
		var res []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(b, &res)
		nameMu.Lock()
		for _, r := range res {
			nameCache[r.ID] = r.Name
			out[r.ID] = r.Name
		}
		nameMu.Unlock()
	}
	return out, nil
}

func getAdjustedPrices(force bool) (map[int64]float64, time.Time, error) {
	return cached("adjprices", time.Hour, force, func() (map[int64]float64, error) {
		b, _, err := esiDo("GET", "/markets/prices", "", nil)
		if err != nil {
			return nil, err
		}
		var raw []struct {
			TypeID   int64   `json:"type_id"`
			Adjusted float64 `json:"adjusted_price"`
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			return nil, err
		}
		m := make(map[int64]float64, len(raw))
		for _, r := range raw {
			m[r.TypeID] = r.Adjusted
		}
		return m, nil
	})
}

type Hub struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Region  int64  `json:"region"`
	Station int64  `json:"station"`
}

var Hubs = []Hub{
	{"jita", "Jita IV - Moon 4 - Caldari Navy Assembly Plant", 10000002, 60003760},
	{"amarr", "Amarr VIII (Oris) - Emperor Family Academy", 10000043, 60008494},
	{"dodixie", "Dodixie IX - Moon 20 - Federation Navy Assembly Plant", 10000032, 60011866},
	{"rens", "Rens VI - Moon 8 - Brutor Tribe Treasury", 10000030, 60004588},
	{"hek", "Hek VIII - Moon 12 - Boundless Creation Factory", 10000042, 60005686},
}

type MarketPrice struct {
	Sell     float64 `json:"sell"` // niedrigster Verkaufspreis an der Station
	Buy      float64 `json:"buy"`  // höchster Kaufpreis an der Station
	SellVol  int64   `json:"sellVol"`
	BuyVol   int64   `json:"buyVol"`
	Adjusted float64 `json:"adjusted"`
}

func hubPrice(h Hub, typeID int64, force bool) (MarketPrice, error) {
	key := fmt.Sprintf("mkt:%s:%d", h.Key, typeID)
	v, _, err := cached(key, 5*time.Minute, force, func() (MarketPrice, error) {
		orders, err := esiGetPaged[struct {
			LocationID   int64   `json:"location_id"`
			IsBuy        bool    `json:"is_buy_order"`
			Price        float64 `json:"price"`
			VolumeRemain int64   `json:"volume_remain"`
		}](fmt.Sprintf("/markets/%d/orders?order_type=all&type_id=%d", h.Region, typeID), "")
		if err != nil {
			return MarketPrice{}, err
		}
		var p MarketPrice
		for _, o := range orders {
			if o.LocationID != h.Station {
				continue
			}
			if o.IsBuy {
				p.BuyVol += o.VolumeRemain
				if o.Price > p.Buy {
					p.Buy = o.Price
				}
			} else {
				p.SellVol += o.VolumeRemain
				if p.Sell == 0 || o.Price < p.Sell {
					p.Sell = o.Price
				}
			}
		}
		return p, nil
	})
	return v, err
}

func getPrices(hubKey string, ids []int64, force bool) (map[int64]MarketPrice, []string) {
	h := Hubs[0]
	for _, x := range Hubs {
		if x.Key == hubKey {
			h = x
		}
	}
	adj, _, _ := getAdjustedPrices(false)
	out := map[int64]MarketPrice{}
	var mu sync.Mutex
	var errs []string
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id int64) {
			defer wg.Done()
			defer func() { <-sem }()
			p, err := hubPrice(h, id, force)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", typeName(id), err))
				return
			}
			p.Adjusted = adj[id]
			out[id] = p
		}(id)
	}
	wg.Wait()
	return out, errs
}

// ---------- character data ----------

type AssetType struct {
	TypeID int64            `json:"typeId"`
	Name   string           `json:"name"`
	Qty    int64            `json:"qty"`
	ByLoc  map[string]int64 `json:"byLoc"` // location id (string) -> qty
}

type AssetResult struct {
	Character string            `json:"character"`
	Types     []AssetType       `json:"types"`
	Locations map[string]string `json:"locations"` // id -> name
	At        time.Time         `json:"at"`
	parent    map[int64]int64
}

func (a *AssetResult) root(loc int64) int64 {
	for i := 0; i < 10; i++ {
		p, ok := a.parent[loc]
		if !ok {
			return loc
		}
		loc = p
	}
	return loc
}

func getAssets(ch *Character, force bool) (*AssetResult, error) {
	v, at, err := cached(fmt.Sprintf("assets:%d", ch.ID), 10*time.Minute, force, func() (*AssetResult, error) {
		tok, err := accessToken(ch)
		if err != nil {
			return nil, err
		}
		items, err := esiGetPaged[struct {
			ItemID       int64  `json:"item_id"`
			TypeID       int64  `json:"type_id"`
			Quantity     int64  `json:"quantity"`
			LocationID   int64  `json:"location_id"`
			LocationType string `json:"location_type"`
			LocationFlag string `json:"location_flag"`
		}](fmt.Sprintf("/characters/%d/assets", ch.ID), tok)
		if err != nil {
			return nil, err
		}
		parent := map[int64]int64{}
		for _, it := range items {
			parent[it.ItemID] = it.LocationID
		}
		root := func(loc int64) int64 {
			for i := 0; i < 10; i++ {
				p, ok := parent[loc]
				if !ok {
					return loc
				}
				loc = p
			}
			return loc
		}
		byType := map[int64]*AssetType{}
		locSet := map[int64]bool{}
		for _, it := range items {
			r := root(it.LocationID)
			locSet[r] = true
			a := byType[it.TypeID]
			if a == nil {
				a = &AssetType{TypeID: it.TypeID, Name: typeName(it.TypeID), ByLoc: map[string]int64{}}
				byType[it.TypeID] = a
			}
			a.Qty += it.Quantity
			a.ByLoc[strconv.FormatInt(r, 10)] += it.Quantity
		}
		res := &AssetResult{Character: ch.Name, Locations: map[string]string{}, parent: parent}
		for _, a := range byType {
			res.Types = append(res.Types, *a)
		}
		sort.Slice(res.Types, func(i, j int) bool { return res.Types[i].Name < res.Types[j].Name })
		res.Locations = locationNames(locSet, tok)
		return res, nil
	})
	if err != nil {
		return nil, err
	}
	cp := *v
	cp.At = at
	return &cp, nil
}

func locationNames(set map[int64]bool, tok string) map[string]string {
	out := map[string]string{}
	var npc []int64
	for id := range set {
		if id >= 1_000_000_000_000 { // Spielerstruktur
			name := fmt.Sprintf("Struktur %d", id)
			if b, _, err := esiDo("GET", fmt.Sprintf("/universe/structures/%d", id), tok, nil); err == nil {
				var s struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(b, &s) == nil && s.Name != "" {
					name = s.Name
				}
			}
			out[strconv.FormatInt(id, 10)] = name
		} else {
			npc = append(npc, id)
		}
	}
	names, _ := resolveNames(npc)
	for _, id := range npc {
		n := names[id]
		if n == "" {
			n = fmt.Sprintf("Ort %d", id)
		}
		out[strconv.FormatInt(id, 10)] = n
	}
	return out
}

type OwnedBlueprint struct {
	TypeID         int64  `json:"typeId"`
	Name           string `json:"name"`
	ProductID      int64  `json:"productId"`
	Product        string `json:"product"`
	ME             int    `json:"me"`
	TE             int    `json:"te"`
	Runs           int    `json:"runs"` // -1 = Original
	Location       string `json:"location"`
	Manufacturable bool   `json:"manufacturable"`
}

func getBlueprints(ch *Character, force bool) ([]OwnedBlueprint, error) {
	v, _, err := cached(fmt.Sprintf("bps:%d", ch.ID), 10*time.Minute, force, func() ([]OwnedBlueprint, error) {
		tok, err := accessToken(ch)
		if err != nil {
			return nil, err
		}
		raw, err := esiGetPaged[struct {
			TypeID     int64 `json:"type_id"`
			ME         int   `json:"material_efficiency"`
			TE         int   `json:"time_efficiency"`
			Runs       int   `json:"runs"`
			LocationID int64 `json:"location_id"`
		}](fmt.Sprintf("/characters/%d/blueprints", ch.ID), tok)
		if err != nil {
			return nil, err
		}
		// Blaupausen in Containern zeigen auf den Container: über die Assets zum Standort auflösen
		assets, _ := getAssets(ch, force)
		out := make([]OwnedBlueprint, 0, len(raw))
		for _, b := range raw {
			loc := ""
			if assets != nil {
				loc = assets.Locations[strconv.FormatInt(assets.root(b.LocationID), 10)]
			}
			ob := OwnedBlueprint{TypeID: b.TypeID, Name: typeName(b.TypeID), ME: b.ME, TE: b.TE, Runs: b.Runs,
				Location: loc}
			if bp := blueprintByID(b.TypeID); bp != nil {
				ob.ProductID = bp.Product
				ob.Product = typeName(bp.Product)
				ob.Manufacturable = true
			}
			out = append(out, ob)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Product != out[j].Product {
				return out[i].Product < out[j].Product
			}
			return out[i].ME > out[j].ME
		})
		return out, nil
	})
	return v, err
}

type Skills struct {
	Accounting      int `json:"accounting"`
	BrokerRelations int `json:"brokerRelations"`
}

func getSkills(ch *Character, force bool) (Skills, error) {
	v, _, err := cached(fmt.Sprintf("skills:%d", ch.ID), 30*time.Minute, force, func() (Skills, error) {
		tok, err := accessToken(ch)
		if err != nil {
			return Skills{}, err
		}
		b, _, err := esiDo("GET", fmt.Sprintf("/characters/%d/skills", ch.ID), tok, nil)
		if err != nil {
			return Skills{}, err
		}
		var raw struct {
			Skills []struct {
				SkillID int64 `json:"skill_id"`
				Level   int   `json:"active_skill_level"`
			} `json:"skills"`
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			return Skills{}, err
		}
		var s Skills
		for _, sk := range raw.Skills {
			switch sk.SkillID {
			case 16622:
				s.Accounting = sk.Level
			case 3446:
				s.BrokerRelations = sk.Level
			}
		}
		return s, nil
	})
	return v, err
}

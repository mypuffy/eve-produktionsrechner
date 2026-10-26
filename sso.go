package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ssoAuthorize = "https://login.eveonline.com/v2/oauth/authorize"
	ssoToken     = "https://login.eveonline.com/v2/oauth/token"
)

var (
	pendingMu sync.Mutex
	pending   = map[string]string{} // state -> code_verifier
)

func randString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func loginURL() (string, error) {
	cfgMu.Lock()
	clientID := cfg.ClientID
	cfgMu.Unlock()
	if clientID == "" {
		return "", errors.New("keine Client-ID eingetragen")
	}
	verifier := randString(32)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randString(16)
	pendingMu.Lock()
	pending[state] = verifier
	pendingMu.Unlock()

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("redirect_uri", Callback)
	q.Set("client_id", clientID)
	q.Set("scope", strings.Join(Scopes, " "))
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return ssoAuthorize + "?" + q.Encode(), nil
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func tokenRequest(form url.Values) (*tokenResp, error) {
	req, _ := http.NewRequest("POST", ssoToken, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var t tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("Antwort der EVE-Anmeldung unlesbar (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != 200 || t.AccessToken == "" {
		msg := t.ErrorDesc
		if msg == "" {
			msg = t.Error
		}
		return nil, fmt.Errorf("EVE-Anmeldung abgelehnt (HTTP %d): %s", resp.StatusCode, msg)
	}
	return &t, nil
}

// Reads character ID and name from the JWT payload. The token comes straight from
// login.eveonline.com over TLS, so the signature is not checked again here.
func parseJWT(tok string) (int64, string, error) {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return 0, "", errors.New("ungültiges Token")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return 0, "", err
	}
	var c struct {
		Sub  string `json:"sub"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return 0, "", err
	}
	idx := strings.LastIndex(c.Sub, ":")
	id, err := strconv.ParseInt(c.Sub[idx+1:], 10, 64)
	if err != nil {
		return 0, "", errors.New("Charakter-ID nicht im Token")
	}
	return id, c.Name, nil
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		callbackPage(w, false, "Anmeldung abgebrochen: "+e)
		return
	}
	pendingMu.Lock()
	verifier, ok := pending[q.Get("state")]
	delete(pending, q.Get("state"))
	pendingMu.Unlock()
	if !ok {
		callbackPage(w, false, "Die Anmeldung ist abgelaufen. Bitte noch einmal auf „Charakter hinzufügen“ klicken.")
		return
	}
	cfgMu.Lock()
	clientID := cfg.ClientID
	cfgMu.Unlock()
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", q.Get("code"))
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)
	t, err := tokenRequest(form)
	if err != nil {
		callbackPage(w, false, err.Error())
		return
	}
	id, name, err := parseJWT(t.AccessToken)
	if err != nil {
		callbackPage(w, false, err.Error())
		return
	}
	cfgMu.Lock()
	var ch *Character
	for _, c := range cfg.Characters {
		if c.ID == id {
			ch = c
		}
	}
	if ch == nil {
		ch = &Character{ID: id}
		cfg.Characters = append(cfg.Characters, ch)
	}
	ch.Name = name
	ch.RefreshToken = t.RefreshToken
	ch.accessToken = t.AccessToken
	ch.accessExp = time.Now().Add(time.Duration(t.ExpiresIn-30) * time.Second)
	err = saveConfig()
	cfgMu.Unlock()
	if err != nil {
		callbackPage(w, false, "Konnte die Anmeldung nicht speichern: "+err.Error())
		return
	}
	http.Redirect(w, r, "/?login="+url.QueryEscape(name), http.StatusFound)
}

func callbackPage(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !ok {
		w.WriteHeader(400)
	}
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title><body style="font-family:system-ui;padding:40px;max-width:600px">
<h2>Anmeldung fehlgeschlagen</h2><p>%s</p><p><a href="/">Zurück zum Rechner</a></p>`, AppName, htmlEscape(msg))
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func findChar(id int64) *Character {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	for _, c := range cfg.Characters {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// accessToken returns a valid access token, refreshing it if needed.
func accessToken(ch *Character) (string, error) {
	cfgMu.Lock()
	if ch.accessToken != "" && time.Now().Before(ch.accessExp) {
		t := ch.accessToken
		cfgMu.Unlock()
		return t, nil
	}
	clientID, refresh := cfg.ClientID, ch.RefreshToken
	cfgMu.Unlock()
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", clientID)
	t, err := tokenRequest(form)
	if err != nil {
		return "", fmt.Errorf("%s muss sich neu anmelden (%v)", ch.Name, err)
	}
	cfgMu.Lock()
	ch.accessToken = t.AccessToken
	ch.accessExp = time.Now().Add(time.Duration(t.ExpiresIn-30) * time.Second)
	if t.RefreshToken != "" && t.RefreshToken != ch.RefreshToken {
		ch.RefreshToken = t.RefreshToken
		_ = saveConfig()
	}
	cfgMu.Unlock()
	return t.AccessToken, nil
}

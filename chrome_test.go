package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Сквозной тест режима chrome: настоящий Chrome/Chromium и поддельный портал на
// локальном HTTPS с именами me.sap.com, accounts.sap.com, notesdownloads.sap.com,
// userapps.support.sap.com (через --host-resolver-rules). Запускается только с
// SAP_NOTES_E2E=1: нужен установленный браузер.

type fakePortal struct {
	srv       *httptest.Server
	logins    atomic.Int32
	password  string
	sarServed atomic.Int32
}

func newFakePortal(t *testing.T) *fakePortal {
	fp := &fakePortal{password: "secret"}
	mux := http.NewServeMux()
	page := func(w http.ResponseWriter, title, body string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!DOCTYPE html><html><head><title>%s</title></head><body>%s</body></html>", title, body)
	}
	cookie := func(r *http.Request, name string) bool { c, err := r.Cookie(name); return err == nil && c.Value == "1" }
	toLogin := func(w http.ResponseWriter, r *http.Request, back string) {
		http.Redirect(w, r, "https://accounts.sap.com/login?back="+back, http.StatusFound)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		host := strings.Split(r.Host, ":")[0]
		switch {
		case host == "accounts.sap.com" && r.URL.Path == "/login":
			back := r.FormValue("back")
			if cookie(r, "ias") { // сессия SAP ID уже есть — вход без формы
				http.Redirect(w, r, back, http.StatusFound)
				return
			}
			if r.Method == http.MethodPost && r.FormValue("j_password") != "" {
				if r.FormValue("j_password") != fp.password {
					page(w, "Sign In", `<div class="message-error">Invalid password</div><form method="post" action="/login"><input type="hidden" name="back" value="`+back+`"><input id="j_password" name="j_password" type="password"><button id="logOnFormSubmit" type="submit">Continue</button></form>`)
					return
				}
				fp.logins.Add(1)
				http.SetCookie(w, &http.Cookie{Name: "ias", Value: "1", Path: "/"})
				http.Redirect(w, r, back, http.StatusFound)
				return
			}
			if r.Method == http.MethodPost && r.FormValue("j_username") != "" { // второй шаг: пароль
				page(w, "Sign In", `<form method="post" action="/login"><input type="hidden" name="back" value="`+back+`"><input id="j_username" name="j_username" value="`+r.FormValue("j_username")+`" readonly><input id="j_password" name="j_password" type="password"><input type="checkbox" id="rememberMe"><button id="logOnFormSubmit" type="submit">Continue</button></form>`)
				return
			}
			page(w, "Sign In", `<form method="post" action="/login"><input type="hidden" name="back" value="`+back+`"><input id="j_username" name="j_username" type="email"><button id="logOnFormSubmit" type="submit">Continue</button></form>`)
		case host == "me.sap.com" && r.URL.Path == "/notes":
			if !cookie(r, "me") {
				// как настоящий портал: страница грузится, а на вход уводит её же скрипт
				page(w, "SAP for Me", `<script>setTimeout(() => location.replace("https://accounts.sap.com/login?back=https://me.sap.com/auth"), 400)</script>`)
				return
			}
			page(w, "SAP for Me", "<h1>SAP for Me</h1>")
		case host == "me.sap.com" && r.URL.Path == "/auth":
			http.SetCookie(w, &http.Cookie{Name: "me", Value: "1", Path: "/"})
			http.Redirect(w, r, "https://me.sap.com/notes", http.StatusFound)
		case host == "me.sap.com" && strings.HasPrefix(r.URL.Path, "/backend/"):
			if !cookie(r, "me") {
				toLogin(w, r, "https://me.sap.com/auth")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			key := "SAPNotes"
			if strings.Contains(r.URL.Path, "sapnotes/Detail") {
				key = "/backend/raw/sapnotes/Detail"
			}
			fx := fixtures[key]
			if key == "/backend/raw/sapnotes/Detail" {
				b, _ := json.Marshal(fx)
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				note := m["Response"].(map[string]any)["SAPNote"].(map[string]any)
				note["Actions"] = map[string]any{
					"Download": map[string]any{"url": "https://notesdownloads.sap.com/note/0040000000483702026"},
					"Print":    map[string]any{"url": "https://userapps.support.sap.com/print/0003771041?token=X"},
				}
				fx = m
			}
			_ = json.NewEncoder(w).Encode(fx)
		case host == "notesdownloads.sap.com" && r.URL.Path == "/auth":
			http.SetCookie(w, &http.Cookie{Name: "nd", Value: "1", Path: "/"})
			http.Redirect(w, r, "https://notesdownloads.sap.com/note/0040000000483702026", http.StatusFound)
		case host == "notesdownloads.sap.com":
			if !cookie(r, "nd") {
				toLogin(w, r, "https://notesdownloads.sap.com/auth")
				return
			}
			fp.sarServed.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="0003771041_00.SAR"`)
			_, _ = w.Write([]byte("CAR 2.01 fake sar content"))
		case host == "userapps.support.sap.com" && r.URL.Path == "/auth":
			http.SetCookie(w, &http.Cookie{Name: "ua", Value: "1", Path: "/"})
			http.Redirect(w, r, "https://userapps.support.sap.com/print/0003771041", http.StatusFound)
		case host == "userapps.support.sap.com":
			if !cookie(r, "ua") {
				toLogin(w, r, "https://userapps.support.sap.com/auth")
				return
			}
			page(w, "SAP Note 3771041", "<h1>3771041 - ITAB_DUPLICATE_KEY</h1><p>Symptom text.</p><script>window.print()</script>")
		default:
			http.NotFound(w, r)
		}
	})
	fp.srv = httptest.NewTLSServer(mux)
	t.Cleanup(fp.srv.Close)
	return fp
}

func e2eChrome(t *testing.T) (*fakePortal, *chromeSession, string) {
	t.Helper()
	if os.Getenv("SAP_NOTES_E2E") != "1" {
		t.Skip("set SAP_NOTES_E2E=1 to run the browser end-to-end test")
	}
	bin := os.Getenv("SAP_NOTES_BROWSER")
	if _, err := findBrowser(bin); err != nil {
		t.Skip(err)
	}
	fp := newFakePortal(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(fp.srv.URL, "https://"))
	var rules []string
	for _, h := range []string{"me.sap.com", "accounts.sap.com", "notesdownloads.sap.com", "userapps.support.sap.com"} {
		rules = append(rules, "MAP "+h+":443 127.0.0.1:"+port)
	}
	dir := t.TempDir()
	cs := newChromeSession(chromeConfig{
		Binary: bin, ProfileDir: filepath.Join(dir, "profile"), DownloadDir: filepath.Join(dir, "dl"),
		Username: "user@example.com", Password: "secret",
		ExtraArgs: []string{"--host-resolver-rules=" + strings.Join(rules, ","), "--ignore-certificate-errors", "--no-sandbox", "--no-proxy-server"},
	})
	t.Cleanup(cs.Close)
	return fp, cs, dir
}

func TestChromeEndToEnd(t *testing.T) {
	fp, cs, dir := e2eChrome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// поиск: вход в два шага, потом скрипт на вкладке me.sap.com
	raw, err := cs.run(ctx, searchJS(searchParams{Query: "F5A 194", Limit: 10}))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var res map[string]any
	_ = json.Unmarshal(raw, &res)
	if res["status"] != "ok" || res["returned"].(float64) != 2 {
		t.Fatalf("search result: %s", raw)
	}
	if fp.logins.Load() != 1 {
		t.Errorf("expected exactly one form sign-in, got %d", fp.logins.Load())
	}
	// второй вызов — без нового входа
	if _, err := cs.run(ctx, readJS("3771041", 1, "", "")); err != nil {
		t.Fatalf("read: %v", err)
	}
	if fp.logins.Load() != 1 {
		t.Errorf("the second call must reuse the session")
	}

	// загрузки: .SAR и PDF, вход на других хостах проходит по сессии SAP ID
	dl, err := cs.download(ctx, "3771041", "both")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if dl.Status != "ok" || len(dl.Files) != 2 {
		t.Fatalf("download result: %+v", dl)
	}
	sar, err := os.ReadFile(filepath.Join(dir, "dl", "0003771041_00.SAR"))
	if err != nil || !strings.HasPrefix(string(sar), "CAR 2.01") {
		t.Errorf("SAR file: %v %q", err, sar)
	}
	pdf, err := os.ReadFile(dl.Files[1])
	if err != nil || !strings.HasPrefix(string(pdf), "%PDF") {
		t.Errorf("PDF file: %v (%d bytes)", err, len(pdf))
	}
}

func TestChromeWrongPassword(t *testing.T) {
	fp, cs, _ := e2eChrome(t)
	fp.password = "other"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := cs.run(ctx, readJS("1", 1, "", ""))
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("expected a rejected sign-in, got %v", err)
	}
}

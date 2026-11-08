package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Режим chrome: сервер сам запускает Chrome или Edge с отдельным профилем и
// управляет им по протоколу отладки. Вход на accounts.sap.com по логину и
// паролю из конфига делает сервер (это программа пользователя, а не Claude).
// Двухфакторка или нет пароля — браузер перезапускается видимым, человек
// входит сам, дальше снова в фоне. Скрипты те же, что в режиме browser.

type chromeConfig struct {
	Binary      string // путь к chrome/msedge; пусто — искать
	ProfileDir  string
	DownloadDir string
	Username    string
	Password    string
	Visible     bool // всегда показывать окно
	ExtraArgs   []string
	PortalURL   string // для тестов: стартовая страница вместо https://me.sap.com/notes
	LoginHost   string // для тестов: хост формы входа
}

type chromeSession struct {
	cfg     chromeConfig
	mu      sync.Mutex
	cmd     *exec.Cmd
	cdp     *cdpClient
	headful bool
	page    string // sessionId постоянной вкладки me.sap.com
	pageTgt string
	ua      string
}

func newChromeSession(cfg chromeConfig) *chromeSession { return &chromeSession{cfg: cfg} }

func (s *chromeSession) portalStart() string {
	if s.cfg.PortalURL != "" {
		return s.cfg.PortalURL
	}
	return portalOrigin + "/notes"
}

func (s *chromeSession) loginHost() string {
	if s.cfg.LoginHost != "" {
		return s.cfg.LoginHost
	}
	return "accounts.sap.com"
}

// ---------------------------------------------------------------- поиск браузера

func findBrowser(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("SAP_NOTES_BROWSER=%s: %v", explicit, err)
		}
		return explicit, nil
	}
	var cands []string
	switch runtime.GOOS {
	case "windows":
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
			if d := os.Getenv(env); d != "" {
				cands = append(cands,
					filepath.Join(d, `Google\Chrome\Application\chrome.exe`),
					filepath.Join(d, `Microsoft\Edge\Application\msedge.exe`))
			}
		}
	case "darwin":
		cands = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	default:
		for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge"} {
			if p, err := exec.LookPath(n); err == nil {
				cands = append(cands, p)
			}
		}
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("no Chrome or Edge found; set SAP_NOTES_BROWSER to the browser executable")
}

// ---------------------------------------------------------------- запуск

func (s *chromeSession) start(ctx context.Context, headful bool) error {
	bin, err := findBrowser(s.cfg.Binary)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.cfg.ProfileDir, 0o700); err != nil {
		return fmt.Errorf("profile folder: %v", err)
	}
	portFile := filepath.Join(s.cfg.ProfileDir, "DevToolsActivePort")
	_ = os.Remove(portFile)
	args := []string{
		"--remote-debugging-port=0",
		"--remote-allow-origins=*",
		"--user-data-dir=" + s.cfg.ProfileDir,
		"--no-first-run", "--no-default-browser-check", "--disable-default-apps",
		"--disable-features=Translate,MediaRouter", "--password-store=basic",
	}
	if !headful {
		args = append(args, "--headless=new", "--window-size=1280,1000")
	}
	args = append(args, s.cfg.ExtraArgs...)
	args = append(args, "about:blank")
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %v", filepath.Base(bin), err)
	}
	s.cmd, s.headful = cmd, headful
	log.Printf("браузер запущен: %s (%s)", filepath.Base(bin), map[bool]string{true: "окно", false: "фон"}[headful])

	var wsURL string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(portFile); err == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) == 2 {
				wsURL = "ws://127.0.0.1:" + strings.TrimSpace(lines[0]) + strings.TrimSpace(lines[1])
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if wsURL == "" {
		s.kill()
		return errors.New("the browser did not open its DevTools port in 20 s (is another browser already using the profile folder?)")
	}
	c, err := cdpConnect(wsURL)
	if err != nil {
		s.kill()
		return fmt.Errorf("connect to the browser: %v", err)
	}
	s.cdp = c
	var v struct {
		UserAgent string `json:"userAgent"`
	}
	if err := c.call(ctx, "", "Browser.getVersion", nil, &v); err == nil {
		// фоновый Chrome называет себя HeadlessChrome — порталу это знать незачем
		s.ua = strings.Replace(v.UserAgent, "HeadlessChrome", "Chrome", 1)
	}
	s.page = ""
	return nil
}

func (s *chromeSession) kill() {
	if s.cdp != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.cdp.call(ctx, "", "Browser.close", nil, nil)
		cancel()
		s.cdp.close()
		s.cdp = nil
	}
	if s.cmd != nil && s.cmd.Process != nil {
		done := make(chan struct{})
		go func() { _, _ = s.cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = s.cmd.Process.Kill()
		}
	}
	s.cmd, s.page, s.pageTgt = nil, "", ""
}

func (s *chromeSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kill()
}

func (s *chromeSession) ensureBrowser(ctx context.Context) error {
	if s.cdp != nil && s.cdp.alive() {
		return nil
	}
	s.kill()
	return s.start(ctx, s.cfg.Visible)
}

// openTab — новая вкладка с плоской сессией; возвращает targetId и sessionId.
func (s *chromeSession) openTab(ctx context.Context, url string) (string, string, error) {
	var t struct {
		TargetID string `json:"targetId"`
	}
	if err := s.cdp.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &t); err != nil {
		return "", "", err
	}
	var a struct {
		SessionID string `json:"sessionId"`
	}
	if err := s.cdp.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": t.TargetID, "flatten": true}, &a); err != nil {
		return "", "", err
	}
	if s.ua != "" {
		_ = s.cdp.call(ctx, a.SessionID, "Network.setUserAgentOverride", map[string]any{"userAgent": s.ua}, nil)
	}
	_ = s.cdp.call(ctx, a.SessionID, "Page.enable", nil, nil)
	if url != "" {
		if err := s.cdp.call(ctx, a.SessionID, "Page.navigate", map[string]any{"url": url}, nil); err != nil {
			return "", "", err
		}
	}
	return t.TargetID, a.SessionID, nil
}

func (s *chromeSession) closeTab(targetID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.cdp.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, nil)
}

type pageState struct {
	Host  string `json:"host"`
	Href  string `json:"href"`
	Ready string `json:"ready"`
	Title string `json:"title"`
}

func (s *chromeSession) state(ctx context.Context, session string) (pageState, error) {
	var st pageState
	raw, err := s.cdp.evaluate(ctx, session, `({host: location.host, href: location.href.split('?')[0], ready: document.readyState, title: document.title})`)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(raw, &st)
	return st, err
}

// ---------------------------------------------------------------- вход

// loginJS — один шаг формы входа SAP ID (accounts.sap.com): форма бывает в один
// экран (почта и пароль) и в два (сначала почта, потом пароль).
func loginJS(user, pass string, passwordAllowed bool) string {
	return `(() => {
const USER = ` + jsLit(user) + `, PASS = ` + jsLit(pass) + `, ALLOW_PASSWORD = ` + jsLit(passwordAllowed) + `;
const vis = (el) => !!el && !el.disabled && (el.offsetParent !== null || el.getClientRects().length > 0);
const q = (sel) => [...document.querySelectorAll(sel)].find(vis);
const set = (el, v) => {
  const proto = Object.getPrototypeOf(el);
  const d = Object.getOwnPropertyDescriptor(proto, 'value');
  if (d && d.set) d.set.call(el, v); else el.value = v;
  el.dispatchEvent(new Event('input', {bubbles: true}));
  el.dispatchEvent(new Event('change', {bubbles: true}));
};
const text = (document.body && document.body.innerText || '').toLowerCase();
const otp = q('input[autocomplete="one-time-code"], input[name*="otp" i], input[name*="passcode" i], input[name*="token" i][type="text"]');
if (otp || /verification code|passcode|authenticator|код подтверждения|одноразов/.test(text)) return {step: 'mfa'};
const user = q('#j_username, input[name="j_username"], input[type="email"], input[autocomplete="username"]');
const pwd = q('#j_password, input[name="j_password"], input[type="password"]');
const btn = q('#logOnFormSubmit, button[type="submit"], input[type="submit"]');
const err = q('.message-error, .error, [role="alert"]');
if (pwd) {
  if (!PASS) return {step: 'need_password'};
  // Пароль отправляется один раз за вызов: форма пароля снова — значит отказ.
  // Повторять нельзя: после нескольких неверных паролей SAP блокирует учётку.
  if (!ALLOW_PASSWORD || (err && err.innerText && /password|пароль|invalid|неверн|incorrect/i.test(err.innerText)))
    return {step: 'rejected', message: ((err && err.innerText) || 'The password form came back after the password was sent.').trim().slice(0, 200)};
  if (user && !user.readOnly && user.value.trim().toLowerCase() !== USER.toLowerCase() && USER) set(user, USER);
  set(pwd, PASS);
  const keep = q('#rememberMe, input[name="rememberMe"], input[type="checkbox"][id*="remember" i]');
  if (keep && !keep.checked) keep.click();
  if (btn) btn.click(); else if (pwd.form) pwd.form.submit();
  return {step: 'password'};
}
if (user) {
  if (!USER) return {step: 'need_user'};
  set(user, USER);
  if (btn) btn.click(); else if (user.form) user.form.submit();
  return {step: 'user'};
}
return {step: 'unknown', title: document.title, snippet: text.slice(0, 160)};
})()`
}

type loginResult struct {
	Step    string `json:"step"`
	Message string `json:"message"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
}

var errNeedHuman = errors.New("need human")

// waitForPortal ведёт вкладку до целевого хоста, по пути проходя форму входа.
// Возвращает errNeedHuman, если нужен человек (двухфакторка, нет пароля).
func (s *chromeSession) waitForPortal(ctx context.Context, session, wantHost string) error {
	deadline := time.Now().Add(45 * time.Second)
	lastLoginStep := time.Time{}
	steps, passwordSent := 0, 0
	last := ""
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := s.state(ctx, session)
		if err != nil {
			time.Sleep(300 * time.Millisecond) // идёт переход: контекст страницы пересоздаётся
			continue
		}
		if cur := st.Host + " " + st.Ready; cur != last {
			log.Printf("страница: %s (%s) %q", st.Host, st.Ready, st.Title)
			last = cur
		}
		if st.Host == wantHost && st.Ready == "complete" {
			// Страница портала сама уводит на вход (BTP → SAP ID) уже после load,
			// поэтому «дошли до хоста» ещё не значит «вошли». Для me.sap.com —
			// проверка запросом к API, для прочих — хост держится 2 с.
			if s.ready(ctx, session, wantHost) {
				return nil
			}
		}
		if st.Host == s.loginHost() && st.Ready != "loading" && time.Since(lastLoginStep) > 2500*time.Millisecond {
			if steps >= 4 {
				return errors.New("the SAP sign-in did not finish after 4 attempts; check SAP_USERNAME and SAP_PASSWORD")
			}
			raw, err := s.cdp.evaluate(ctx, session, loginJS(s.cfg.Username, s.cfg.Password, passwordSent == 0))
			if err == nil {
				var r loginResult
				_ = json.Unmarshal(raw, &r)
				log.Printf("вход: шаг %s", r.Step)
				if r.Step == "password" {
					passwordSent++
				}
				switch r.Step {
				case "mfa", "need_password", "need_user":
					return errNeedHuman
				case "rejected":
					return fmt.Errorf("SAP rejected the sign-in: %s", r.Message)
				case "user", "password":
					steps++
					lastLoginStep = time.Now()
				}
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("the page did not reach %s in 45 s", wantHost)
}

// ensurePortal — живой браузер, постоянная вкладка на me.sap.com, вход выполнен.
func (s *chromeSession) ensurePortal(ctx context.Context) (string, error) {
	if err := s.ensureBrowser(ctx); err != nil {
		return "", err
	}
	host := hostOf(s.portalStart())
	if s.page != "" {
		if st, err := s.state(ctx, s.page); err == nil && st.Host == host && st.Ready == "complete" {
			return s.page, nil
		}
	}
	s.dropPage()
	tgt, sess, err := s.openTab(ctx, s.portalStart())
	if err != nil {
		return "", err
	}
	err = s.waitForPortal(ctx, sess, host)
	if errors.Is(err, errNeedHuman) {
		return "", s.handOver(ctx)
	}
	if err != nil {
		return "", err
	}
	s.page, s.pageTgt = sess, tgt
	// окно было открыто только ради входа — дальше снова в фоне
	if s.headful && !s.cfg.Visible {
		log.Printf("вход выполнен, браузер уходит в фон")
		s.kill()
		if err := s.start(ctx, false); err != nil {
			return "", err
		}
		return s.ensurePortal(ctx)
	}
	return s.page, nil
}

type loginPending struct{ msg string }

func (e loginPending) Error() string { return e.msg }

// handOver — открыть видимое окно на странице входа и отдать вход человеку.
func (s *chromeSession) handOver(ctx context.Context) error {
	if !s.headful {
		s.kill()
		if err := s.start(ctx, true); err != nil {
			return err
		}
		if _, _, err := s.openTab(ctx, s.portalStart()); err != nil {
			return err
		}
	}
	why := "two-factor authentication"
	if s.cfg.Password == "" {
		why = "no SAP_PASSWORD in the configuration"
	}
	return loginPending{msg: "A browser window with the SAP sign-in is open on the user's computer (" + why + "). Ask the user to sign in there and confirm the second factor, then call the same action again. Never ask for the password in chat."}
}

func (s *chromeSession) dropPage() {
	if s.pageTgt != "" && s.cdp != nil {
		s.closeTab(s.pageTgt)
	}
	s.page, s.pageTgt = "", ""
}

// portalProbe — вход выполнен, если API портала отвечает JSON со своего же хоста.
const portalProbe = `fetch('/backend/raw/core/User?login=true', {credentials: 'include', headers: {Accept: 'application/json'}})
  .then(r => r.ok && r.url.startsWith(location.origin) && /json/.test(r.headers.get('content-type') || ''))
  .catch(() => false)`

func (s *chromeSession) ready(ctx context.Context, session, host string) bool {
	if host == hostOf(s.portalStart()) {
		raw, err := s.cdp.evaluate(ctx, session, portalProbe)
		return err == nil && string(raw) == "true"
	}
	time.Sleep(2 * time.Second)
	st, err := s.state(ctx, session)
	return err == nil && st.Host == host && st.Ready == "complete"
}

func hostOf(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return strings.SplitN(u, "/", 2)[0]
}

// ---------------------------------------------------------------- выполнение скриптов

func (s *chromeSession) run(ctx context.Context, script string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runLocked(ctx, script)
}

func (s *chromeSession) runLocked(ctx context.Context, script string) (json.RawMessage, error) {
	expr := strings.TrimPrefix(script, "await ")
	for attempt := 0; attempt < 2; attempt++ {
		page, err := s.ensurePortal(ctx)
		if err != nil {
			return nil, err
		}
		raw, err := s.cdp.evaluate(ctx, page, expr)
		if err != nil {
			log.Printf("скрипт не выполнился: %v", firstLine(err.Error()))
			s.dropPage() // вкладка ушла с портала или умерла — в следующий раз новая
			if attempt == 0 {
				continue
			}
			return nil, err
		}
		var st struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &st)
		if (st.Status == "login_required" || st.Status == "timeout") && attempt == 0 {
			s.dropPage() // протухшая сессия или зависшая вкладка: открыть заново и повторить
			continue
		}
		return raw, nil
	}
	return nil, errors.New("unreachable")
}

// ---------------------------------------------------------------- загрузки

var reFileUnsafe = regexp.MustCompile(`[^A-Za-z0-9._ -]+`)

// downloadSAR — файл для SNOTE через загрузчик браузера, без окна «сохранить как».
func (s *chromeSession) downloadSAR(ctx context.Context, url string) (string, error) {
	if err := os.MkdirAll(s.cfg.DownloadDir, 0o755); err != nil {
		return "", fmt.Errorf("download folder: %v", err)
	}
	if err := s.cdp.call(ctx, "", "Browser.setDownloadBehavior", map[string]any{
		"behavior": "allowAndName", "downloadPath": s.cfg.DownloadDir, "eventsEnabled": true,
	}, nil); err != nil {
		return "", err
	}
	type dl struct{ guid, name, state string }
	var mu sync.Mutex
	cur := dl{}
	done := make(chan struct{}, 1)
	unsub := s.cdp.subscribe(func(m cdpMsg) {
		switch m.Method {
		case "Browser.downloadWillBegin":
			var p struct{ GUID, SuggestedFilename string }
			_ = json.Unmarshal(m.Params, &p)
			mu.Lock()
			cur.guid, cur.name = p.GUID, p.SuggestedFilename
			mu.Unlock()
		case "Browser.downloadProgress":
			var p struct{ GUID, State string }
			_ = json.Unmarshal(m.Params, &p)
			mu.Lock()
			if p.GUID == cur.guid && (p.State == "completed" || p.State == "canceled") {
				cur.state = p.State
				select {
				case done <- struct{}{}:
				default:
				}
			}
			mu.Unlock()
		}
	})
	defer unsub()
	target, sess, err := s.openTab(ctx, url)
	if err != nil {
		return "", err
	}
	defer s.closeTab(target)
	// по пути может быть вход: проходим его на этой вкладке, пока не началась загрузка
	go func() {
		for i := 0; i < 60; i++ {
			mu.Lock()
			started := cur.guid != ""
			mu.Unlock()
			if started || ctx.Err() != nil {
				return
			}
			if st, err := s.state(ctx, sess); err == nil && st.Host == s.loginHost() && st.Ready != "loading" {
				_, _ = s.cdp.evaluate(ctx, sess, loginJS(s.cfg.Username, s.cfg.Password, false))
				time.Sleep(2500 * time.Millisecond)
				continue
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		return "", errors.New("the download did not finish in 60 s")
	case <-ctx.Done():
		return "", ctx.Err()
	}
	mu.Lock()
	d := cur
	mu.Unlock()
	if d.state != "completed" {
		return "", errors.New("the download was cancelled by the browser")
	}
	name := reFileUnsafe.ReplaceAllString(d.name, "_")
	if name == "" {
		name = d.guid + ".SAR"
	}
	dst := filepath.Join(s.cfg.DownloadDir, name)
	_ = os.Remove(dst)
	if err := os.Rename(filepath.Join(s.cfg.DownloadDir, d.guid), dst); err != nil {
		return "", fmt.Errorf("rename the downloaded file: %v", err)
	}
	return dst, nil
}

// printPDF — страница ноты для печати → настоящий PDF средствами браузера.
func (s *chromeSession) printPDF(ctx context.Context, url, file string) (string, error) {
	if err := os.MkdirAll(s.cfg.DownloadDir, 0o755); err != nil {
		return "", fmt.Errorf("download folder: %v", err)
	}
	target, sess, err := s.openTab(ctx, "")
	if err != nil {
		return "", err
	}
	defer s.closeTab(target)
	// страница сама зовёт window.print(): в фоновом браузере это лишнее
	_ = s.cdp.call(ctx, sess, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.print = function () {};"}, nil)
	if err := s.cdp.call(ctx, sess, "Page.navigate", map[string]any{"url": url}, nil); err != nil {
		return "", err
	}
	if err := s.waitForPortal(ctx, sess, hostOf(url)); err != nil {
		if errors.Is(err, errNeedHuman) {
			return "", s.handOver(ctx)
		}
		return "", err
	}
	time.Sleep(1500 * time.Millisecond) // дорисовка после load
	var r struct {
		Data string `json:"data"`
	}
	if err := s.cdp.call(ctx, sess, "Page.printToPDF", map[string]any{"printBackground": true, "preferCSSPageSize": true}, &r); err != nil {
		return "", err
	}
	b, err := base64.StdEncoding.DecodeString(r.Data)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(s.cfg.DownloadDir, reFileUnsafe.ReplaceAllString(file, "_"))
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

type downloadResult struct {
	Status string   `json:"status"`
	Number string   `json:"number"`
	Files  []string `json:"files,omitempty"`
	Errors []string `json:"errors,omitempty"`
	Folder string   `json:"folder"`
}

// download — файл SNOTE и/или PDF ноты в папку загрузок.
func (s *chromeSession) download(ctx context.Context, number, kind string) (downloadResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := downloadResult{Number: number, Folder: s.cfg.DownloadDir}
	raw, err := s.runLocked(ctx, linksJS(number))
	if err != nil {
		return res, err
	}
	var l struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Version any    `json:"version"`
		SAR     string `json:"sar_url"`
		PDF     string `json:"pdf_print_url"`
	}
	_ = json.Unmarshal(raw, &l)
	if l.Status != "ok" {
		return res, fmt.Errorf("note %s: %s %s", number, l.Status, l.Message)
	}
	if kind == "sar" || kind == "both" {
		if l.SAR == "" {
			res.Errors = append(res.Errors, "sar: the note has no SNOTE download (a KBA or a note without corrections)")
		} else if f, err := s.downloadSAR(ctx, l.SAR); err != nil {
			res.Errors = append(res.Errors, "sar: "+err.Error())
		} else {
			res.Files = append(res.Files, f)
		}
	}
	if kind == "pdf" || kind == "both" {
		if l.PDF == "" {
			res.Errors = append(res.Errors, "pdf: the portal gave no print page for this note")
		} else if f, err := s.printPDF(ctx, l.PDF, fmt.Sprintf("SAP Note %s v%v.pdf", number, l.Version)); err != nil {
			var lp loginPending
			if errors.As(err, &lp) {
				return res, err
			}
			res.Errors = append(res.Errors, "pdf: "+err.Error())
		} else {
			res.Files = append(res.Files, f)
		}
	}
	res.Status = "ok"
	if len(res.Files) == 0 {
		res.Status = "error"
	}
	return res, nil
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }

// sap-notes — MCP-сервер для SAP Notes и KBA с портала me.sap.com.
//
// Программе портал после одного-двух запросов отдаёт заглушку, настоящему
// браузеру — данные. Два режима:
//   - browser (по умолчанию): сервер в SAP не ходит, каждое действие
//     возвращает скрипт, который Claude выполняет во встроенном браузере
//     Claude Desktop на вкладке me.sap.com, в сессии пользователя;
//   - chrome: сервер сам управляет своим Chrome/Edge в фоне (chrome.go) и
//     выполняет там те же скрипты.
//
// Один бинарник без зависимостей: только стандартная библиотека.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const (
	serverName    = "sap-notes"
	serverVersion = "0.1.2"
)

func main() {
	showVersion := flag.Bool("version", false, "напечатать версию и выйти")
	flag.Parse()
	if *showVersion {
		fmt.Println(serverName, serverVersion)
		return
	}

	log.SetOutput(os.Stderr) // stdout принадлежит протоколу
	log.SetFlags(log.LstdFlags)

	ts := newToolset()
	mode := strings.ToLower(strings.TrimSpace(env("SAP_NOTES_MODE")))
	switch mode {
	case "", "browser":
		log.Printf("старт %s %s, режим browser: скрипты для вкладки %s, в сеть сервер не ходит", serverName, serverVersion, portalOrigin)
	case "chrome":
		cfg := chromeConfigFromEnv()
		ts.chrome = newChromeSession(cfg)
		defer ts.chrome.Close()
		log.Printf("старт %s %s, режим chrome: пользователь %q, пароль %s, профиль %s, загрузки %s",
			serverName, serverVersion, cfg.Username, map[bool]string{true: "задан", false: "не задан"}[cfg.Password != ""], cfg.ProfileDir, cfg.DownloadDir)
	default:
		log.Fatalf("SAP_NOTES_MODE=%q: expected browser or chrome", mode)
	}
	serve(os.Stdin, os.Stdout, ts)
}

func chromeConfigFromEnv() chromeConfig {
	cfg := chromeConfig{
		Binary:      strings.TrimSpace(env("SAP_NOTES_BROWSER")),
		ProfileDir:  strings.TrimSpace(env("SAP_NOTES_PROFILE_DIR")),
		DownloadDir: strings.TrimSpace(env("SAP_NOTES_DOWNLOAD_DIR")),
		Username:    strings.TrimSpace(env("SAP_USERNAME")),
		Password:    envRaw("SAP_PASSWORD"),
	}
	switch strings.ToLower(strings.TrimSpace(env("SAP_NOTES_SHOW_BROWSER"))) {
	case "1", "true", "yes", "x":
		cfg.Visible = true
	}
	if cfg.ProfileDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			base = os.TempDir()
		}
		cfg.ProfileDir = filepath.Join(base, "sap-notes", "browser-profile")
	}
	if cfg.DownloadDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		cfg.DownloadDir = filepath.Join(home, "Downloads", "SAP Notes")
	}
	return cfg
}

// env — значение переменной; незаполненная подстановка расширения ("${user_config.x}") считается пустой.
func env(name string) string { return strings.TrimSpace(envRaw(name)) }

func envRaw(name string) string {
	v := os.Getenv(name)
	if strings.HasPrefix(strings.TrimSpace(v), "${") {
		return ""
	}
	return v
}

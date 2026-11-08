package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const toolName = "sap_notes"

const descBrowser = `SAP Notes and KBAs from the SAP support portal (me.sap.com) through the user's signed-in session in the built-in browser of Claude Desktop. Read-only. Use it for SAP errors and dumps after checking the live system, and to see what a note changes in code.

THE SERVER DOES NOT CALL SAP. Every action (except status) returns {status: "run_in_browser", steps, javascript}. Follow the steps: use (or open) a built-in browser tab on https://me.sap.com, then run the javascript UNCHANGED with the browser's JavaScript tool on that tab. It returns the JSON described below. If it returns status "login_required", ask the user to sign in on that tab — never type credentials yourself. If it returns "timeout", reload the tab once and rerun; do not loop. Keep the pace human: one script at a time, no bulk runs.`

const descChrome = `SAP Notes and KBAs from the SAP support portal (me.sap.com). The server drives its own Chrome/Edge in the background, signed in with the user's SAP account from its configuration, and returns the results directly. Read-only. Use it for SAP errors and dumps after checking the live system, and to see what a note changes in code. If a call returns status "login_window", a browser window with the SAP sign-in is open on the user's computer (two-factor authentication or no password configured): ask the user to sign in there, then call again. Never ask for the password in chat. Keep the pace human: one call at a time, no bulk runs.`

const descActions = `QUERIES IN ENGLISH: the exact message class and number (F5A 194), the dump name (ITAB_DUPLICATE_KEY), a program, class or method name.

Actions:
- search: notes and KBAs by keywords. Params: query (required), software_component + release (e.g. S4CORE + 106, from table CVERS of the customer system) to keep only notes valid for that release, support_package (the installed package, e.g. "02" or "SAPK-10602INS4CORE") to drop notes already contained in it, app_component (prefix such as FI-GL, filtered on the returned hits), limit (1-50, default 10). Returns number, title, component, category, priority, released, url.
- read: one note: header, full text as markdown, validity, support packages, prerequisites, references, side effects, summary of correction instructions. Params: number (required), software_component + release (optional: keep only the prerequisites for that release), part (for long notes).
- corrections: the correction instructions of a note. Params: number (required), software_component and release to select the instruction for the customer release; without release only the list of instructions is returned; instruction_key (from that list) instead of number/release to fetch one instruction; object (part of an object name, e.g. CREATE_CHANGE_REQUEST) to return only the code blocks of matching objects — large notes have dozens of objects, and every part refetches; part. For the selected instruction: valid-to support package, affected objects (TADIR), prerequisite notes, and the code changes in SNOTE form (context lines, START/END OF DELETION/INSERTION). The portal wraps code lines longer than 132 characters, so a line may continue on the next one.
- download: save the SNOTE file (.SAR, for SNOTE → Upload SAP Note) and/or a PDF of the note. Params: number (required), kind (sar, pdf or both; default both). DOWNLOAD_MODE_TEXT
- status: server version, mode and the portal services used.

A release filter only says a note formally applies. Before advising, check the system: do the objects exist (TADIR), are the note and its prerequisites implemented (CWBNTCUST), is the inserted code already there.`

func toolDescription(chrome bool) string {
	if chrome {
		return descChrome + "\n\n" + strings.Replace(descActions, "DOWNLOAD_MODE_TEXT", "Files go straight into the configured download folder; the result lists their paths.", 1)
	}
	return descBrowser + "\n\n" + strings.Replace(descActions, "DOWNLOAD_MODE_TEXT", "In this mode the script returns the links; ask the user before opening them in the built-in browser — the .SAR link downloads a file (the browser asks where to save it), the print page opens the print dialog (Save as PDF).", 1)
}

type toolArgs struct {
	Action            string `json:"action"`
	Query             string `json:"query"`
	SoftwareComponent string `json:"software_component"`
	Release           string `json:"release"`
	SupportPackage    string `json:"support_package"`
	AppComponent      string `json:"app_component"`
	Limit             int    `json:"limit"`
	Number            string `json:"number"`
	InstructionKey    string `json:"instruction_key"`
	Object            string `json:"object"`
	Kind              string `json:"kind"`
	Part              int    `json:"part"`
}

type toolset struct {
	chrome *chromeSession // nil — режим browser: скрипты для встроенного браузера
}

func newToolset() *toolset { return &toolset{} }

func (ts *toolset) list() []map[string]any {
	return []map[string]any{{
		"name":        toolName,
		"title":       "SAP Notes",
		"description": toolDescription(ts.chrome != nil),
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":             map[string]any{"type": "string", "enum": []string{"search", "read", "corrections", "download", "status"}},
				"query":              map[string]any{"type": "string", "description": "search: keywords in English."},
				"software_component": map[string]any{"type": "string", "description": "search / read / corrections: software component, e.g. S4CORE, SAP_BASIS, SAP_APPL."},
				"release":            map[string]any{"type": "string", "description": "search / read / corrections: release of that component, e.g. 106, 756."},
				"support_package":    map[string]any{"type": "string", "description": "search: installed support package, two digits (02) or full name (SAPK-10602INS4CORE); notes contained in it are dropped."},
				"app_component":      map[string]any{"type": "string", "description": "search: application component prefix, e.g. FI-GL."},
				"limit":              map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "search: how many results (default 10)."},
				"number":             map[string]any{"type": "string", "description": "read / corrections / download: note number, e.g. 3771041."},
				"kind":               map[string]any{"type": "string", "enum": []string{"sar", "pdf", "both"}, "description": "download: what to save (default both)."},
				"instruction_key":    map[string]any{"type": "string", "description": "corrections: key of one instruction from a previous corrections list."},
				"object":             map[string]any{"type": "string", "description": "corrections: part of an object name (method, include, class) to return only its code blocks."},
				"part":               map[string]any{"type": "integer", "minimum": 1, "description": "read / corrections: part of a long text."},
			},
			"required":             []string{"action"},
			"additionalProperties": false,
		},
		"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": true},
	}}
}

var (
	reSoftComp  = regexp.MustCompile(`^[A-Z][A-Z0-9_/-]{1,29}$`)
	reRelease   = regexp.MustCompile(`^[0-9A-Z]{1,10}$`)
	reSPShort   = regexp.MustCompile(`^[0-9]{1,4}$`)
	reSPFull    = regexp.MustCompile(`^SAPK[A-Z0-9-]{3,30}$`)
	reAppComp   = regexp.MustCompile(`^[A-Z][A-Z0-9-]{0,29}$`)
	reNumber    = regexp.MustCompile(`^[0-9]{1,10}$`)
	reInstrKey  = regexp.MustCompile(`^[0-9]{1,10}/[0-9]{1,10}/[0-9]{1,10}/[0-9]{1,4}/[A-Z][A-Z0-9_/-]{1,29}/[0-9A-Z]{1,10}/[0-9A-Z]{1,10}$`)
	reCtrlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	reObject    = regexp.MustCompile(`^[A-Z0-9_/=~ -]{2,80}$`)
)

// spName — имя пакета поддержки по короткому номеру: SAPK-<релиз><номер>IN<компонент без _>.
// Так названы пакеты S4CORE и большинства компонентов последних лет; если у
// компонента иначе — передать полное имя.
func spName(sp, comp, rel string) (string, string) {
	sp = strings.ToUpper(strings.TrimSpace(sp))
	switch {
	case sp == "":
		return "", ""
	case reSPFull.MatchString(sp):
		return sp, ""
	case reSPShort.MatchString(sp):
		if comp == "" || rel == "" {
			return "", "support_package as a number needs software_component and release; or pass the full package name (SAPK-…)."
		}
		n := strings.TrimLeft(sp, "0")
		if len(n) < 2 {
			n = strings.Repeat("0", 2-len(n)) + n
		}
		return "SAPK-" + rel + n + "IN" + strings.ReplaceAll(comp, "_", ""), ""
	default:
		return "", "support_package: two digits (02) or the full package name (SAPK-10602INS4CORE)."
	}
}

func normNumber(s string) (string, string) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://me.sap.com/notes/"), "SAP Note ")
	s = strings.TrimSpace(strings.SplitN(s, "/", 2)[0])
	if !reNumber.MatchString(s) {
		return "", "number: the note number, digits only (e.g. 3771041)."
	}
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "", "number: the note number, digits only (e.g. 3771041)."
	}
	return s, ""
}

func (ts *toolset) call(name string, raw json.RawMessage) (string, bool) {
	if name != toolName {
		return "Unknown tool: " + name + ". This server has one tool, " + toolName + ".", true
	}
	var a toolArgs
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			return "Invalid arguments: " + err.Error(), true
		}
	}
	if a.Part < 1 {
		a.Part = 1
	}
	comp := strings.ToUpper(strings.TrimSpace(a.SoftwareComponent))
	rel := strings.ToUpper(strings.TrimSpace(a.Release))
	if comp != "" && !reSoftComp.MatchString(comp) {
		return "software_component: a component name such as S4CORE, SAP_BASIS, SAP_APPL.", true
	}
	if rel != "" && !reRelease.MatchString(rel) {
		return "release: the component release as in CVERS, such as 106 or 756.", true
	}

	var script string
	switch strings.TrimSpace(a.Action) {
	case "search":
		q := strings.TrimSpace(reCtrlChars.ReplaceAllString(a.Query, " "))
		if q == "" {
			return "Pass query — keywords in English (message class and number, dump name, program or method).", true
		}
		if len([]rune(q)) > 200 {
			return "query: at most 200 characters; use the distinctive words only.", true
		}
		if a.Limit == 0 {
			a.Limit = 10
		}
		if a.Limit < 1 || a.Limit > 50 {
			return "limit: 1 to 50.", true
		}
		sp, msg := spName(a.SupportPackage, comp, rel)
		if msg != "" {
			return msg, true
		}
		app := strings.ToUpper(strings.TrimSpace(a.AppComponent))
		if app != "" && !reAppComp.MatchString(app) {
			return "app_component: an application component prefix such as FI-GL.", true
		}
		script = searchJS(searchParams{Query: q, SoftwareComponent: comp, Release: rel, SupportPackage: sp, AppComponent: app, Limit: a.Limit})
	case "read":
		n, msg := normNumber(a.Number)
		if msg != "" {
			return msg, true
		}
		script = readJS(n, a.Part, comp, rel)
	case "corrections":
		key := strings.TrimSpace(a.InstructionKey)
		var n string
		if key != "" {
			if !reInstrKey.MatchString(key) {
				return "instruction_key: pass the key exactly as a previous corrections list returned it.", true
			}
		}
		if a.Number != "" || key == "" {
			var msg string
			if n, msg = normNumber(a.Number); msg != "" {
				return msg, true
			}
		}
		obj := strings.ToUpper(strings.TrimSpace(a.Object))
		if obj != "" && !reObject.MatchString(obj) {
			return "object: part of an object name such as CREATE_CHANGE_REQUEST or CL_FINS_ACDOC_DOCUMENT_CHANGE.", true
		}
		script = correctionsJS(corrParams{Number: n, SoftwareComponent: comp, Release: rel, InstructionKey: key, Object: obj, Part: a.Part})
	case "download":
		n, msg := normNumber(a.Number)
		if msg != "" {
			return msg, true
		}
		kind := strings.ToLower(strings.TrimSpace(a.Kind))
		if kind == "" {
			kind = "both"
		}
		if kind != "sar" && kind != "pdf" && kind != "both" {
			return "kind: sar, pdf or both.", true
		}
		if ts.chrome != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			res, err := ts.chrome.download(ctx, n, kind)
			if err != nil {
				return chromeError(err)
			}
			out, _ := encode(res)
			return out, res.Status != "ok"
		}
		script = linksJS(n)
	case "status":
		mode := "browser: returns scripts for a me.sap.com tab of the built-in browser; makes no network calls itself"
		if ts.chrome != nil {
			mode = "chrome: drives its own browser in the background; downloads go to " + ts.chrome.cfg.DownloadDir
		}
		return encode(map[string]any{
			"server":  serverName,
			"version": serverVersion,
			"mode":    mode,
			"portal_services": map[string]string{
				"search":      portalOrigin + notesSearchSv + "SAPNotes",
				"read":        portalOrigin + noteDetailSv + "<number>",
				"corrections": portalOrigin + corrinsSv + "CorrInsSet",
			},
			"note": "These portal services are not publicly documented and may change.",
		})
	case "":
		return "Pass action: search, read, corrections, download or status.", true
	default:
		return fmt.Sprintf("Unknown action %q. Use search, read, corrections, download or status.", a.Action), true
	}
	if ts.chrome != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		raw, err := ts.chrome.run(ctx, script)
		if err != nil {
			return chromeError(err)
		}
		var v any
		_ = json.Unmarshal(raw, &v)
		out, _ := encode(v)
		st, _ := v.(map[string]any)
		return out, st == nil || st["status"] != "ok"
	}
	return encode(map[string]any{
		"status": "run_in_browser",
		"steps": []string{
			"1. Use a built-in browser tab on " + portalOrigin + " (any page of SAP for Me). If there is none, open " + portalOrigin + "/notes and wait until it shows SAP for Me.",
			"2. If the tab shows the SAP sign-in page, ask the user to sign in there. Never type credentials yourself.",
			"3. Run the javascript below UNCHANGED with the browser's JavaScript tool on that tab. It returns JSON with a status field.",
			"4. status login_required -> step 2, then run again. status timeout -> reload the tab once and run again; do not loop.",
		},
		"javascript": script,
	})
}

func chromeError(err error) (string, bool) {
	var lp loginPending
	if errors.As(err, &lp) {
		out, _ := encode(map[string]any{"status": "login_window", "message": lp.msg})
		return out, true
	}
	return "Browser error: " + err.Error(), true
}

func encode(v any) (string, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(v); err != nil {
		return "Failed to encode the result: " + err.Error(), true
	}
	return strings.TrimSpace(buf.String()), false
}

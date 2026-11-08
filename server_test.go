package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- протокол

func rpc(t *testing.T, lines ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, newToolset())
	var res []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not JSON: %s", sc.Text())
		}
		res = append(res, m)
	}
	return res
}

func TestProtocol(t *testing.T) {
	res := rpc(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"nope"}`,
	)
	if len(res) != 3 {
		t.Fatalf("expected 3 answers (notification gets none), got %d", len(res))
	}
	init := res[0]["result"].(map[string]any)
	if !strings.Contains(init["instructions"].(string), "built-in browser") {
		t.Errorf("browser mode instructions must mention the built-in browser")
	}
	if strings.Contains(serverInstructions(true), "never contacts SAP") {
		t.Errorf("chrome mode instructions must not say the server never contacts SAP")
	}
	if init["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocol version not echoed: %v", init["protocolVersion"])
	}
	tools := res[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != toolName {
		t.Errorf("expected exactly one tool %s", toolName)
	}
	if res[2]["error"] == nil {
		t.Errorf("unknown method must be an error")
	}
}

func call(t *testing.T, args string) (string, bool) {
	t.Helper()
	return newToolset().call(toolName, json.RawMessage(args))
}

func TestValidation(t *testing.T) {
	bad := []string{
		`{"action":"search"}`,
		`{"action":"search","query":"x","limit":99}`,
		`{"action":"search","query":"x","support_package":"02"}`,
		`{"action":"search","query":"x","software_component":"s4core'; drop"}`,
		`{"action":"search","query":"x","app_component":"FI GL"}`,
		`{"action":"read"}`,
		`{"action":"read","number":"12a"}`,
		`{"action":"corrections","instruction_key":"1/2"}`,
		`{"action":"nope"}`,
		`{"action":"read","number":"1","extra":1}`,
		`{"action":"download","number":"3771041","kind":"zip"}`,
		`{"action":"download"}`,
	}
	for _, a := range bad {
		if _, isErr := call(t, a); !isErr {
			t.Errorf("expected an error for %s", a)
		}
	}
	good := []string{
		`{"action":"search","query":"F5A 194"}`,
		`{"action":"search","query":"x","software_component":"s4core","release":"106","support_package":"2"}`,
		`{"action":"read","number":"https://me.sap.com/notes/3771041"}`,
		`{"action":"corrections","number":"0003771041","software_component":"S4CORE","release":"106"}`,
		`{"action":"corrections","instruction_key":"0002009391/19773/0020751258/0001/S4CORE/106/106"}`,
		`{"action":"status"}`,
		`{"action":"download","number":"3771041"}`,
		`{"action":"download","number":"3771041","kind":"sar"}`,
	}
	for _, a := range good {
		if text, isErr := call(t, a); isErr {
			t.Errorf("unexpected error for %s: %s", a, text)
		}
	}
}

func TestSupportPackageName(t *testing.T) {
	cases := map[[3]string]string{
		{"02", "S4CORE", "106"}:                 "SAPK-10602INS4CORE",
		{"2", "SAP_BASIS", "756"}:               "SAPK-75602INSAPBASIS",
		{"sapk-10602ins4core", "", ""}:          "SAPK-10602INS4CORE",
		{"SAPKB75602", "", ""}:                  "SAPKB75602",
		{"14", "S4CORE", "104"}:                 "SAPK-10414INS4CORE",
		{"", "S4CORE", "106"}:                   "",
		{"SAPK-10602INS4CORE", "S4CORE", "106"}: "SAPK-10602INS4CORE",
	}
	for in, want := range cases {
		got, msg := spName(in[0], in[1], in[2])
		if got != want || msg != "" {
			t.Errorf("spName%v = %q, %q; want %q", in, got, msg, want)
		}
	}
}

func TestSearchFilterQuoting(t *testing.T) {
	f := searchFilter(searchParams{Query: "it's", SoftwareComponent: "S4CORE", Release: "106", SupportPackage: "SAPK-10602INS4CORE"})
	want := "FilterKeywordsFuzzy eq 'it''s' and FilterFuzzyThreshold eq '0.9' and FilterSoftwareComponent eq 'S4CORE' and FilterSoftwareComponentVersion eq '106' and FilterSupportPackage eq 'SAPK-10602INS4CORE'"
	if f != want {
		t.Errorf("filter:\n got %s\nwant %s", f, want)
	}
	if strings.Contains(searchJS(searchParams{Query: "x", Limit: 5}), "$format") {
		t.Errorf("$format breaks the OData answer and must not be used")
	}
}

// ---------------------------------------------------------------- скрипты в node

// Ответы портала в той форме, в какой они пришли вживую 06.11.2026 (укорочены).
var fixtures = map[string]any{
	"/backend/raw/sapnotes/Detail": map[string]any{
		"Request": map[string]any{"Number": "3771041"},
		"Response": map[string]any{"Error": map[string]any{}, "SAPNote": map[string]any{
			"Title": map[string]any{"value": "3771041 - ITAB_DUPLICATE_KEY during document change"},
			"Header": map[string]any{
				"Number": map[string]any{"value": "3771041"}, "Type": map[string]any{"value": "SAP Note"},
				"Version": map[string]any{"value": 2}, "ReleasedOn": map[string]any{"value": "21.07.2026"},
				"SAPComponentKey": map[string]any{"value": "FI-GL-GL"}, "Category": map[string]any{"value": "Program error"},
				"Priority": map[string]any{"value": "Correction with medium priority"}, "Status": map[string]any{"value": "Released to Customer"},
			},
			"LongText":               map[string]any{"value": "<h3 data-toc-skip class=\"section\" id=\"Symptom\">Symptom</h3>\r\n<p>You face&#160;a duplicate key error <b>ITAB_DUPLICATE_KEY</b>.</p>\r\n<h3>Solution</h3>\r\n<p>See <a href=\"/notes/3589893\">3589893</a>.</p><ul><li>one</li><li>two</li></ul>"},
			"CorrectionsInfo":        map[string]any{"Corrections": map[string]any{"value": 6}, "ManualActivities": map[string]any{"value": 0}, "Prerequisites": map[string]any{"value": 3}},
			"CorrectionInstructions": map[string]any{"Items": []any{map[string]any{"NumberOfCorrin": 6, "SoftwareComponent": "S4CORE", "URL": "/corrins/0003771041/19773"}}},
			"Validity":               map[string]any{"Items": []any{map[string]any{"SoftwareComponent": "S4CORE", "From": "104", "To": "104"}, map[string]any{"SoftwareComponent": "S4CORE", "From": "106", "To": "106"}}},
			"SupportPackage":         map[string]any{"Items": []any{map[string]any{"SoftwareComponentVersion": "S4CORE 106", "SupportPackage": "SAPK-10611INS4CORE"}}},
			"Preconditions": map[string]any{"Items": []any{
				map[string]any{"Number": "3589893 ", "Title": "Document change", "Component": "FI-GL-GL", "SoftwareComponent": "S4CORE", "ValidFrom": "104", "ValidTo": "104"},
				map[string]any{"Number": "3589893 ", "Title": "Document change", "Component": "FI-GL-GL", "SoftwareComponent": "S4CORE", "ValidFrom": "106", "ValidTo": "106"},
				map[string]any{"Number": "3248268 ", "Title": "Custom field values", "Component": "AC-COB", "SoftwareComponent": "S4CORE", "ValidFrom": "106", "ValidTo": "107"},
				map[string]any{"Number": "2687879 ", "Title": "FB02 BAdI", "Component": "FI-GL-GL-A", "SoftwareComponent": "S4CORE", "ValidFrom": "102", "ValidTo": "102"},
			}},
			"References":    map[string]any{"RefTo": map[string]any{"Items": []any{}}, "RefBy": map[string]any{"Items": []any{}}},
			"ManualActions": map[string]any{"value": ""},
		}},
	},
	"SAPNotes": map[string]any{"d": map[string]any{"__count": "37", "results": []any{
		map[string]any{"Number": "2509112", "Title": "FIDCCH: Error message F5A 194", "Component": "FI-GL-GL-M", "ComponentName": "Ext. Interfaces", "Category": "Program error", "Priority": "Correction with medium priority", "ReleasedOn": "/Date(1500987221000)/", "Version": 1},
		map[string]any{"Number": "2551875", "Title": "Error F5A 194 for creation of invoice list in SD", "Component": "FI-AR-AR-J", "Category": "Program error", "ReleasedOn": "/Date(1519025960000)/", "Version": 3},
	}}},
	"CorrInsSet?": map[string]any{"d": map[string]any{"__count": "2", "results": []any{
		map[string]any{"Aleid": "0002009389", "PakId": "19773", "Insta": "0020751258", "Vernr": "0001", "Name": "S4CORE", "VerFrom": "104", "VerTo": "104"},
		map[string]any{"Aleid": "0002009391", "PakId": "19773", "Insta": "0020751258", "Vernr": "0001", "Name": "S4CORE", "VerFrom": "106", "VerTo": "106"},
	}}},
	"/Code":           map[string]any{"d": map[string]any{"results": []any{map[string]any{"Snipet": "<P>*$*$------$*$*<BR>*$ Correction Inst.&nbsp; 0002009391<BR>*&amp;--------------------------------------------------------------------*<BR>*&amp; Object&nbsp;&nbsp;METH CL_FINS_ACDOC_DOCUMENT_CHANGE<BR>*&amp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;IF_X~CREATE_CHANGE_REQUEST<BR>*&amp;--------------------------------------------------------------------*<BR>...<BR><BR>*&gt;&gt;&gt;&gt; START OF DELETION &lt;&lt;&lt;&lt;&lt;<BR>&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;requested = value #( ).<BR>*&gt;&gt;&gt;&gt; END OF DELETION &lt;&lt;&lt;&lt;&lt;&lt;&lt;<BR>*&amp;--------------------------------------------------------------------*<BR>*&amp; Object&nbsp;&nbsp;REPS LFACGF08<BR>*&amp;--------------------------------------------------------------------*<BR>*&gt;&gt;&gt;&gt; START OF INSERTION &lt;&lt;&lt;&lt;<BR>form x.<BR>*&gt;&gt;&gt;&gt; END OF INSERTION &lt;&lt;&lt;&lt;&lt;&lt;<BR/></P>"}}}},
	"/TADIR":          map[string]any{"d": map[string]any{"results": []any{map[string]any{"ProgId": "LIMU", "ObjType": "METH", "ObjName": "CL_FINS_ACDOC_DOCUMENT_CHANGE IF_X~M", "TProgId": "R3TR", "TObjType": "CLAS", "TObjName": "CL_FINS_ACDOC_DOCUMENT_CHANGE"}}}},
	"/Prerequisite":   map[string]any{"d": map[string]any{"results": []any{map[string]any{"SapNotesNumber": "3589893", "Title": "Document change", "AleidInNote": "0001796405"}}}},
	"/ValidityDetail": map[string]any{"d": map[string]any{"results": []any{map[string]any{"AleTxtEnd": "SAPK-10610INS4CORE"}}}},
}

// runScript выполняет скрипт в node с подменённым fetch; mode: ok | login | html | slow.
func runScript(t *testing.T, script, mode string) (map[string]any, []string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	fx, _ := json.Marshal(fixtures)
	harness := `
globalThis.location = {origin: 'https://me.sap.com'};
const FX = ` + string(fx) + `, MODE = ` + jsLit(mode) + `, SEEN = [];
globalThis.fetch = async (url, opt) => {
  SEEN.push(url);
  if (MODE === 'slow') { return await new Promise((res, rej) => opt.signal.addEventListener('abort', () => { const e = new Error('t'); e.name = 'TimeoutError'; rej(e); })); }
  if (MODE === 'login') return {url: 'https://accounts.sap.com/saml2/idp/sso?x=1', status: 200, ok: true, text: async () => '<html>sign in</html>'};
  if (MODE === 'html') return {url: 'https://me.sap.com' + url, status: 200, ok: true, text: async () => '<!DOCTYPE html><html>SAP for Me</html>'};
  const u = decodeURIComponent(url);
  const key = ['/Code', '/TADIR', '/Prerequisite', '/ValidityDetail', 'CorrInsSet?', 'SAPNotes', '/backend/raw/sapnotes/Detail'].find(k => u.includes(k));
  if (!key) return {url: 'https://me.sap.com' + url, status: 404, ok: false, text: async () => 'not found'};
  return {url: 'https://me.sap.com' + url, status: 200, ok: true, text: async () => JSON.stringify(FX[key])};
};
const KEEP = setInterval(() => {}, 100); // таймер AbortSignal.timeout не держит node
(async () => {
  let res;
  try { res = ` + script + `; } catch (e) { res = {thrown: String(e.message)}; }
  console.log(JSON.stringify({res, seen: SEEN}));
  clearInterval(KEEP);
})();`
	dir := t.TempDir()
	p := filepath.Join(dir, "h.mjs")
	if err := os.WriteFile(p, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("node", p).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	var r struct {
		Res  map[string]any `json:"res"`
		Seen []string       `json:"seen"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("bad node output: %s", out)
	}
	return r.Res, r.Seen
}

func TestSearchScript(t *testing.T) {
	res, seen := runScript(t, searchJS(searchParams{Query: "F5A 194", SoftwareComponent: "S4CORE", Release: "106", SupportPackage: "SAPK-10602INS4CORE", Limit: 10}), "ok")
	if res["status"] != "ok" || res["returned"].(float64) != 2 || res["found_total"].(float64) != 37 {
		t.Fatalf("unexpected: %v", res)
	}
	first := res["results"].([]any)[0].(map[string]any)
	if first["number"] != "2509112" || first["released"] != "2017-07-25" || first["url"] != "https://me.sap.com/notes/2509112" {
		t.Errorf("row: %v", first)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "FilterSupportPackage%20eq%20'SAPK-10602INS4CORE'") {
		t.Errorf("request: %v", seen)
	}
	res, _ = runScript(t, searchJS(searchParams{Query: "F5A 194", AppComponent: "FI-AR", Limit: 10}), "ok")
	if res["returned"].(float64) != 1 {
		t.Errorf("app_component prefix not applied: %v", res)
	}
}

func TestReadScript(t *testing.T) {
	res, _ := runScript(t, readJS("3771041", 1, "", ""), "ok")
	if res["status"] != "ok" || res["number"] != "3771041" || res["component"] != "FI-GL-GL" {
		t.Fatalf("unexpected: %v", res)
	}
	text := res["text"].(string)
	for _, want := range []string{"### Symptom", "You face a duplicate key error ITAB_DUPLICATE_KEY.", "[3589893](https://me.sap.com/notes/3589893)", "- one", "- two"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if res["hint"] == nil {
		t.Errorf("a note with instructions must point to action=corrections")
	}
	if pre := res["prerequisites"].([]any); len(pre) != 3 || len(pre[0].(map[string]any)["releases"].([]any)) != 2 {
		t.Errorf("prerequisites must be merged by number: %v", pre)
	}
	res, _ = runScript(t, readJS("3771041", 1, "S4CORE", "106"), "ok")
	if pre := res["prerequisites"].([]any); len(pre) != 2 {
		t.Errorf("only the prerequisites covering S4CORE 106 are expected: %v", pre)
	}
}

func TestCorrectionsScript(t *testing.T) {
	res, seen := runScript(t, correctionsJS(corrParams{Number: "3771041", SoftwareComponent: "S4CORE", Release: "106", Part: 1}), "ok")
	if res["status"] != "ok" {
		t.Fatalf("unexpected: %v", res)
	}
	ins := res["correction_instructions"].([]any)
	if len(ins) != 1 || ins[0].(map[string]any)["instruction"] != "2009391" {
		t.Fatalf("expected the 106 instruction only: %v", ins)
	}
	code := res["code"].(string)
	for _, want := range []string{"*>>>> START OF DELETION <<<<<", "\n      requested = value #( ).", "*& Object  METH CL_FINS_ACDOC_DOCUMENT_CHANGE", "form x."} {
		if !strings.Contains(code, want) {
			t.Errorf("code lacks %q:\n%s", want, code)
		}
	}
	var corrinsList string
	for _, s := range seen {
		if strings.Contains(s, "CorrInsSet?") {
			corrinsList = s
		}
	}
	if !strings.Contains(corrinsList, "SapNotesNumber%20eq%20'0003771041'") {
		t.Errorf("list request: %v", seen)
	}
	// только один объект: шапка и его блок, без LFACGF08
	res, _ = runScript(t, correctionsJS(corrParams{Number: "3771041", SoftwareComponent: "S4CORE", Release: "106", Object: "CREATE_CHANGE_REQUEST", Part: 1}), "ok")
	code = res["code"].(string)
	if !strings.Contains(code, "requested = value") || strings.Contains(code, "LFACGF08") || !strings.Contains(code, "Correction Inst.") {
		t.Errorf("object filter:\n%s", code)
	}
	blocks := res["code_blocks"].([]any)
	if len(blocks) != 2 || blocks[0].(map[string]any)["object"] != "METH CL_FINS_ACDOC_DOCUMENT_CHANGE IF_X~CREATE_CHANGE_REQUEST" {
		t.Errorf("code_blocks: %v", blocks)
	}
	// без релиза — только список
	res, _ = runScript(t, correctionsJS(corrParams{Number: "3771041", Part: 1}), "ok")
	if res["code"] != nil || len(res["instructions"].([]any)) != 2 {
		t.Errorf("without release only the list is expected: %v", res)
	}
	// релиз, которого нет
	res, _ = runScript(t, correctionsJS(corrParams{Number: "3771041", SoftwareComponent: "S4CORE", Release: "108", Part: 1}), "ok")
	if res["note"] == nil || res["code"] != nil {
		t.Errorf("release 108 is not covered: %v", res)
	}
	// по ключу — без чтения ноты и списка
	res, seen = runScript(t, correctionsJS(corrParams{InstructionKey: "0002009391/19773/0020751258/0001/S4CORE/106/106", Part: 1}), "ok")
	if res["code"] == nil || len(seen) != 4 {
		t.Errorf("by key: 4 requests expected, got %d: %v", len(seen), res)
	}
}

func TestLoginAndTimeout(t *testing.T) {
	for _, mode := range []string{"login", "html"} {
		res, _ := runScript(t, readJS("1", 1, "", ""), mode)
		if res["status"] != "login_required" {
			t.Errorf("%s: expected login_required, got %v", mode, res)
		}
	}
	if testing.Short() {
		return
	}
	src := strings.Replace(searchJS(searchParams{Query: "x", Limit: 1}), jsLit(scriptBudget), "4000", 1)
	res, _ := runScript(t, src, "slow")
	if res["status"] != "timeout" {
		t.Errorf("expected timeout, got %v", res)
	}
}

func TestWrongOrigin(t *testing.T) {
	src := strings.Replace(readJS("1", 1, "", ""), "'https://me.sap.com'", "'https://example.com'", 1)
	res, _ := runScript(t, src, "ok")
	if !strings.Contains(res["thrown"].(string), "me.sap.com tab") {
		t.Errorf("expected a refusal outside me.sap.com: %v", res)
	}
}

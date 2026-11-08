package main

import (
	"encoding/json"
	"strings"
)

// Скрипты для вкладки me.sap.com.
//
// Портал отдаёт данные только настоящему браузеру с сессией пользователя:
// программа получает заглушку уже на втором запросе. Поэтому сервер не делает
// запросов вовсе, а собирает скрипт: адреса, фильтры OData и разбор ответа.
// Claude выполняет его инструментом JavaScript встроенного браузера и получает
// компактный JSON.
//
// Ограничения среды, под которые написано:
//   - инструмент JavaScript ждёт около 45 секунд, а сервисы портала бывают
//     медленными (поиск — 15–20 с) и от лишних фильтров зависают. Поэтому
//     запросы идут по одному, с общим сроком; не уложились — частичный ответ
//     и подсказка, как добрать;
//   - `$format=json` ломает ответ OData (пустой список) — JSON просим заголовком;
//   - протухшая сессия видна как переход на accounts.sap.com или HTML вместо JSON.

const (
	portalOrigin  = "https://me.sap.com"
	notesSearchSv = "/backend/raw/core/W7LegacyProxyVerticle/odata/sfm/snogwsmynotes/"
	corrinsSv     = "/backend/raw/core/W7LegacyProxyVerticle/odata/svt/snogwscorrins/"
	noteDetailSv  = "/backend/raw/sapnotes/Detail?q="
	maxTextChars  = 12000
	scriptBudget  = 40000 // мс на весь скрипт: инструмент JavaScript ждёт ~45 с
)

func jsLit(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSpace(b.String())
}

// jsCommon — запрос с общим сроком, распознавание входа, HTML в текст.
const jsCommon = `
const ORIGIN = 'https://me.sap.com';
const T0 = Date.now(), BUDGET = ` + "SCRIPT_BUDGET" + `;
class Stop extends Error { constructor(kind, msg) { super(msg); this.kind = kind; } }
const left = () => BUDGET - (Date.now() - T0);
const getJSON = async (path) => {
  const ms = left();
  if (ms < 3000) throw new Stop('timeout', 'The time budget of this script ran out.');
  let r;
  try {
    r = await fetch(path, {credentials: 'include', headers: {Accept: 'application/json'}, signal: AbortSignal.timeout(ms)});
  } catch (e) {
    if (e && (e.name === 'TimeoutError' || e.name === 'AbortError')) throw new Stop('timeout', 'The SAP portal did not answer in time.');
    throw new Stop('error', 'Request failed: ' + (e && e.message || e));
  }
  if (r.url && !r.url.startsWith(ORIGIN)) throw new Stop('login_required', 'The portal redirected to ' + r.url.split('?')[0] + ' — the session has expired.');
  if (r.status === 401 || r.status === 403) throw new Stop('login_required', 'HTTP ' + r.status + ' from the portal — the session has expired or is not allowed.');
  const text = await r.text();
  if (!r.ok) throw new Stop('error', 'HTTP ' + r.status + ' from ' + path.split('?')[0] + ': ' + text.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim().slice(0, 200));
  try { return JSON.parse(text); }
  catch (e) { throw new Stop(/<html/i.test(text) ? 'login_required' : 'error', 'The portal answered with something that is not JSON (' + text.replace(/\s+/g, ' ').slice(0, 80) + '…) — usually a sign-in page.'); }
};
const odataList = (j) => ((j && j.d && (j.d.results || (Array.isArray(j.d) ? j.d : null))) || []);
const odataCount = (j) => (j && j.d && j.d.__count != null ? Number(j.d.__count) : null);
const oq = (s) => "'" + String(s).replace(/'/g, "''") + "'";
const ENT = {amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: ' '};
const decode = (s) => String(s).replace(/&(#x[0-9a-f]+|#\d+|[a-z]+);/gi, (m, e) => {
  if (e[0] === '#') { const n = e[1] === 'x' || e[1] === 'X' ? parseInt(e.slice(2), 16) : parseInt(e.slice(1), 10); return isNaN(n) ? m : String.fromCodePoint(n === 160 ? 32 : n); }
  const v = ENT[e.toLowerCase()]; return v !== undefined ? v : m;
});
// Текст ноты: заголовки, абзацы, списки, ссылки, таблицы, код.
const toText = (html) => {
  let s = String(html || '').replace(/\r?\n/g, ' ');
  s = s.replace(/<(script|style)\b[^>]*>[\s\S]*?<\/\1\s*>/gi, ' ');
  s = s.replace(/<pre\b[^>]*>([\s\S]*?)<\/pre\s*>/gi, (m, c) => '\n` + "```" + `\n' + c.replace(/<br\s*\/?>/gi, '\n').replace(/<[^>]+>/g, '') + '\n` + "```" + `\n');
  s = s.replace(/<h([1-6])\b[^>]*>([\s\S]*?)<\/h\1\s*>/gi, (m, l, t) => '\n\n### ' + t.replace(/<[^>]+>/g, '').trim() + '\n\n');
  s = s.replace(/<a\b[^>]*href\s*=\s*"([^"]+)"[^>]*>([\s\S]*?)<\/a\s*>/gi, (m, h, t) => {
    t = t.replace(/<[^>]+>/g, '').trim();
    if (h.startsWith('/')) h = ORIGIN + h;
    return t && t !== h ? '[' + t + '](' + h + ')' : h;
  });
  s = s.replace(/<\/t[dh]\s*>/gi, ' | ').replace(/<tr\b[^>]*>/gi, '\n| ');
  s = s.replace(/<br\s*\/?>/gi, '\n').replace(/<li\b[^>]*>/gi, '\n- ');
  s = s.replace(/<\/(p|div|li|ul|ol|table|blockquote)\s*>/gi, '\n').replace(/<(p|div|ul|ol|table|blockquote)\b[^>]*>/gi, '\n');
  s = decode(s.replace(/<[^>]+>/g, ''));
  return s.split('\n').map(l => l.replace(/[ \t\xa0]+/g, ' ').trim()).join('\n').replace(/\n{3,}/g, '\n\n').trim();
};
// Код инструкции: строки по <BR>, отступы из &nbsp; сохраняются как есть.
const codeText = (html) => {
  let s = String(html || '').replace(/\r?\n/g, '');
  s = s.replace(/<br\s*\/?>/gi, '\n').replace(/<\/p\s*>/gi, '\n');
  s = decode(s.replace(/<[^>]+>/g, ''));
  return s.split('\n').map(l => l.replace(/\s+$/, '')).join('\n').replace(/\n{3,}/g, '\n\n').trim();
};
const sapDate = (v) => { const m = /\d+/.exec(String(v || '')); return m ? new Date(Number(m[0])).toISOString().slice(0, 10) : String(v || ''); };
const pad10 = (n) => String(n).replace(/\D/g, '').padStart(10, '0');
const unpad = (n) => String(n || '').trim().replace(/^0+/, '');
const noteURL = (n) => ORIGIN + '/notes/' + unpad(n);
const slice = (text, part, max) => {
  const chars = Array.from(text);
  const total = Math.max(1, Math.ceil(chars.length / max));
  const p = Math.min(Math.max(1, part), total);
  return {part: p, total_parts: total, text: chars.slice((p - 1) * max, p * max).join('')};
};
const fail = (e) => {
  if (e instanceof Stop) {
    if (e.kind === 'login_required') return {status: 'login_required', message: e.message,
      next: 'Show the me.sap.com tab to the user and ask them to sign in there (never type credentials yourself). When the portal shows SAP for Me, run the same script again.'};
    if (e.kind === 'timeout') return {status: 'timeout', message: e.message,
      next: 'The portal is slow or this tab is stale. Reload the tab (navigate it to https://me.sap.com/notes), wait until it shows SAP for Me, and run the same script once more. Do not loop.'};
    return {status: 'error', message: e.message};
  }
  return {status: 'error', message: String(e && e.message || e)};
};
if (typeof location === 'undefined' || location.origin !== ORIGIN) {
  throw new Error('Run this script on a https://me.sap.com tab of the built-in browser, not on ' + (typeof location === 'undefined' ? 'this page' : location.origin) + '.');
}
`

func common() string {
	return strings.Replace(jsCommon, "SCRIPT_BUDGET", jsLit(scriptBudget), 1)
}

func wrap(body string) string {
	return "await (async () => {\n" + common() + "try {\n" + body + "\n} catch (e) { return fail(e); }\n})()"
}

// ---------------------------------------------------------------- поиск

type searchParams struct {
	Query             string
	SoftwareComponent string
	Release           string
	SupportPackage    string // полное имя пакета, «выше чем»
	AppComponent      string // префикс компонента приложения, фильтр в скрипте
	Limit             int
}

func searchFilter(p searchParams) string {
	f := []string{
		"FilterKeywordsFuzzy eq " + odataQuote(p.Query),
		"FilterFuzzyThreshold eq '0.9'",
	}
	if p.SoftwareComponent != "" {
		f = append(f, "FilterSoftwareComponent eq "+odataQuote(p.SoftwareComponent))
	}
	if p.Release != "" {
		f = append(f, "FilterSoftwareComponentVersion eq "+odataQuote(p.Release))
	}
	if p.SupportPackage != "" {
		f = append(f, "FilterSupportPackage eq "+odataQuote(p.SupportPackage))
	}
	return strings.Join(f, " and ")
}

func odataQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func searchJS(p searchParams) string {
	top := p.Limit
	if p.AppComponent != "" {
		top = 100 // компонент приложения отсеивается в скрипте: берём с запасом
	}
	path := notesSearchSv + "SAPNotes?$skip=0&$top=" + itoa(top) + "&$inlinecount=allpages&$filter="
	return wrap(`
const PATH = ` + jsLit(path) + `, FILTER = ` + jsLit(searchFilter(p)) + `, LIMIT = ` + jsLit(p.Limit) + `, APPCOMP = ` + jsLit(p.AppComponent) + `;
const APPLIED = ` + jsLit(appliedFilters(p)) + `;
const j = await getJSON(PATH + encodeURIComponent(FILTER));
let rows = odataList(j);
const total = odataCount(j);
if (APPCOMP) rows = rows.filter(n => String(n.Component || '').toUpperCase().startsWith(APPCOMP));
const results = rows.slice(0, LIMIT).map(n => ({
  number: unpad(n.Number), title: String(n.Title || '').trim(), component: n.Component || '', component_name: n.ComponentName || '',
  category: n.Category || '', priority: n.Priority || '', released: sapDate(n.ReleasedOn), version: n.Version, url: noteURL(n.Number)}));
const out = {status: 'ok', query: ` + jsLit(p.Query) + `, filters: APPLIED, found_total: total, returned: results.length, results,
  hint: 'Read a note with action=read; its correction instructions for the customer release with action=corrections.'};
if (APPCOMP) out.note = 'The application component prefix was applied to the first ' + rows.length + ' hits only; found_total counts all of them.';
if (!results.length) out.note = (out.note ? out.note + ' ' : '') + 'Nothing found. Use the exact message class and number, the dump name or the program/method name; drop the version filters to see whether the note exists at all.';
if (total != null && total > results.length && !APPCOMP) out.more = 'More hits exist: narrow the query or add software_component/release/support_package.';
return out;`)
}

func appliedFilters(p searchParams) map[string]string {
	m := map[string]string{}
	if p.SoftwareComponent != "" {
		m["software_component"] = p.SoftwareComponent
	}
	if p.Release != "" {
		m["release"] = p.Release
	}
	if p.SupportPackage != "" {
		m["support_package_above"] = p.SupportPackage
	}
	if p.AppComponent != "" {
		m["app_component_prefix"] = p.AppComponent
	}
	return m
}

// ---------------------------------------------------------------- чтение

func readJS(number string, part int, comp, rel string) string {
	return wrap(`
const NUM = ` + jsLit(number) + `, PART = ` + jsLit(part) + `, MAX = ` + jsLit(maxTextChars) + `, SC = ` + jsLit(comp) + `, REL = ` + jsLit(rel) + `;
const j = await getJSON(` + jsLit(noteDetailSv) + ` + encodeURIComponent(NUM));
const resp = (j && j.Response) || {};
const err = resp.Error || {};
if (err && Object.keys(err).length) return {status: 'error', message: 'The portal reported an error for note ' + NUM + ': ' + JSON.stringify(err).slice(0, 300)};
const n = resp.SAPNote;
if (!n) return {status: 'error', message: 'No note ' + NUM + ' in the answer. It may not exist or not be released to customers.'};
const h = n.Header || {};
const v = (x) => (x && x.value != null ? x.value : '');
const items = (x) => (x && Array.isArray(x.Items) ? x.Items : []);
const text = toText(v(n.LongText));
const pg = slice(text, PART, MAX);
const out = {
  status: 'ok', number: unpad(v(h.Number) || NUM), title: String(v(n.Title)).trim(), type: v(h.Type), version: v(h.Version),
  released: v(h.ReleasedOn), component: v(h.SAPComponentKey), component_text: v(h.SAPComponentKeyText),
  category: v(h.Category), priority: v(h.Priority), release_status: v(h.Status), language: v(h.Language),
  url: noteURL(v(h.Number) || NUM), part: pg.part, total_parts: pg.total_parts, text: pg.text};
if (pg.part === 1) {
  const ci = n.CorrectionsInfo || {};
  out.corrections = {instructions: v(ci.Corrections), manual_activities: v(ci.ManualActivities), prerequisites: v(ci.Prerequisites),
    by_software_component: items(n.CorrectionInstructions).map(c => ({software_component: c.SoftwareComponent, count: c.NumberOfCorrin}))};
  out.validity = items(n.Validity).map(x => x.SoftwareComponent + ' ' + x.From + (x.To && x.To !== x.From ? '-' + x.To : ''));
  out.support_packages = items(n.SupportPackage).map(x => x.SoftwareComponentVersion + ': ' + x.SupportPackage);
  // Предпосылки приходят строкой на каждый релиз: фильтр по релизу клиента и склейка по номеру.
  const covers = (x) => (!SC || x.SoftwareComponent === SC) && (!REL || (String(x.ValidFrom).length === REL.length && String(x.ValidFrom) <= REL && REL <= String(x.ValidTo)));
  const pre = new Map();
  for (const x of items(n.Preconditions).filter(covers)) {
    const k = unpad(x.Number);
    if (!pre.has(k)) pre.set(k, {number: k, title: x.Title, component: x.Component, releases: []});
    pre.get(k).releases.push(x.SoftwareComponent + ' ' + x.ValidFrom + (x.ValidTo !== x.ValidFrom ? '-' + x.ValidTo : ''));
  }
  out.prerequisites = [...pre.values()].slice(0, 40);
  if (pre.size > 40) out.prerequisites_more = pre.size - 40;
  if (SC || REL) out.prerequisites_for = (SC + ' ' + REL).trim();
  const refs = (x) => items(x).slice(0, 30).map(r => ({number: unpad(r.RefNumber), title: r.RefTitle, component: r.RefComponent}));
  const R = n.References || {}, S = n.SideEffects || {};
  out.refers_to = refs(R.RefTo); out.referenced_by = refs(R.RefBy);
  out.side_effects_caused = refs(S.SideEffectsSolving); out.side_effects_solved = refs(S.SideEffectsCausing);
  const manual = toText(v(n.ManualActions));
  if (manual) out.manual_activities_text = manual;
  out.attachments = items(n.Attachments).map(a => ({file: a.FileName, url: a.URL}));
  const act = n.Actions || {};
  out.downloads = {snote_file: (act.Download && act.Download.url) || undefined, print_page: (act.Print && act.Print.url) || undefined,
    how: 'action=download with this number fetches the SNOTE file (.SAR) and/or a PDF.'};
  for (const k of ['prerequisites', 'refers_to', 'referenced_by', 'side_effects_caused', 'side_effects_solved', 'attachments', 'support_packages', 'validity'])
    if (Array.isArray(out[k]) && !out[k].length) delete out[k];
}
if (pg.part < pg.total_parts) out.more = 'Part ' + pg.part + ' of ' + pg.total_parts + '; next: action=read with the same number and part=' + (pg.part + 1) + '.';
if (out.corrections && Number(out.corrections.instructions) > 0) out.hint = 'Correction instructions exist: action=corrections with this number, software_component and release shows the code for that release.';
return out;`)
}

// ---------------------------------------------------------------- инструкции исправления

type corrParams struct {
	Number            string
	SoftwareComponent string
	Release           string
	InstructionKey    string // «Aleid/PakId/Insta/Vernr/Name/VerFrom/VerTo» из списка
	Object            string // часть имени объекта: только его блоки кода
	Part              int
}

func correctionsJS(p corrParams) string {
	return wrap(`
const NUM = ` + jsLit(p.Number) + `, SC = ` + jsLit(p.SoftwareComponent) + `, REL = ` + jsLit(p.Release) + `, KEY = ` + jsLit(p.InstructionKey) + `;
const PART = ` + jsLit(p.Part) + `, MAX = ` + jsLit(maxTextChars) + `, SV = ` + jsLit(corrinsSv) + `, OBJ = ` + jsLit(p.Object) + `;
const keyOf = (c) => [c.Aleid, c.PakId, c.Insta, c.Vernr, c.Name, c.VerFrom, c.VerTo].join('/');
const entity = (k) => { const [Aleid, PakId, Insta, Vernr, Name, VerFrom, VerTo] = k.split('/');
  return 'CorrInsSet(Aleid=' + oq(Aleid) + ',PakId=' + oq(PakId) + ',Insta=' + oq(Insta) + ',Vernr=' + oq(Vernr) + ',Name=' + oq(Name) + ',VerFrom=' + oq(VerFrom) + ',VerTo=' + oq(VerTo) + ')'; };
const inRange = (c) => (!SC || c.Name === SC) && (!REL || (String(c.VerFrom) <= REL && REL <= String(c.VerTo) && String(c.VerFrom).length === REL.length));
let list = [], keys = [];
if (KEY) {
  keys = [KEY];
} else {
  // По одному номеру ноты: PakId для списка не нужен (проверено вживую).
  const j = await getJSON(SV + 'CorrInsSet?$skip=0&$top=200&$inlinecount=allpages&$filter=' + encodeURIComponent('SapNotesNumber eq ' + oq(pad10(NUM))));
  list = odataList(j);
  if (!list.length) return {status: 'ok', number: unpad(NUM), url: noteURL(NUM), instructions: [],
    note: 'This note has no correction instructions (a KBA, a note with manual activities only, or a note delivered by support packages only). See action=read.'};
  keys = list.filter(inRange).map(keyOf);
}
const out = {status: 'ok', number: unpad(NUM), url: noteURL(NUM)};
if (!KEY) {
  out.instructions = list.map(c => ({key: keyOf(c), software_component: c.Name, from: c.VerFrom, to: c.VerTo, matches: inRange(c)}));
  if (!REL) {
    out.hint = 'Pass release (and software_component) to get the code of the instruction for that release, or instruction_key from this list.';
    return out;
  }
  if (!keys.length) {
    out.note = 'No instruction of this note covers ' + (SC || 'any component') + ' ' + REL + '. The note does not apply to that release (or is already part of it).';
    return out;
  }
}
const details = [];
for (const k of keys) {
  if (left() < 6000) { out.partial = 'Ran out of time before instruction ' + k + '; call action=corrections with instruction_key=' + k + '.'; break; }
  const e = entity(k);
  const [code, tadir, pre, val] = await Promise.all(['/Code', '/TADIR?$skip=0&$top=200', '/Prerequisite?$skip=0&$top=200', '/ValidityDetail?$skip=0&$top=50'].map(s => getJSON(SV + e + s)));
  const codeRows = odataList(code).length ? odataList(code) : (code && code.d ? [code.d] : []);
  const [Aleid, , , , Name, VerFrom, VerTo] = k.split('/');
  details.push({
    key: k, instruction: unpad(Aleid), software_component: Name, from: VerFrom, to: VerTo,
    valid_to_support_package: odataList(val).map(x => x.AleTxtEnd).filter(Boolean),
    objects: odataList(tadir).map(t => ({object: t.ProgId + ' ' + t.ObjType + ' ' + t.ObjName, main_object: t.TProgId + ' ' + t.TObjType + ' ' + t.TObjName})),
    prerequisites: odataList(pre).map(x => ({note: unpad(x.SapNotesNumber), title: x.Title, instruction: unpad(x.AleidInNote)})),
    code: codeRows.map(c => codeText(c.Snipet)).join('\n\n')});
}
// Код режется на блоки по объектам: «*&---…*» + «*& Object …». Первый блок — шапка инструкции.
const blocksOf = (code) => {
  const parts = code.split(/\n(?=\*&-{10,}\*\n\*& Object)/);
  return parts.map((t, i) => {
    const m = /\*& Object\s+(.+)((?:\n\*&\s{10,}.+)*)/.exec(t);
    const name = m ? (m[1] + m[2].replace(/\n\*&\s+/g, ' ')).replace(/\s+/g, ' ').replace(/ ~/g, '~').trim() : (i === 0 ? 'header' : 'block ' + i);
    return {name, text: t};
  });
};
let all = '';
const index = [];
for (const d of details) {
  let blocks = blocksOf(d.code);
  index.push(...blocks.filter(b => b.name !== 'header').map(b => ({instruction: d.instruction, object: b.name, chars: b.text.length})));
  if (OBJ) blocks = blocks.filter(b => b.name === 'header' || b.name.toUpperCase().includes(OBJ));
  all += (all ? '\n\n' : '') + '==== instruction ' + d.instruction + ' (' + d.software_component + ' ' + d.from + (d.to !== d.from ? '-' + d.to : '') + ')\n' + blocks.map(b => b.text).join('\n');
}
const pg = slice(all, PART, MAX);
out.correction_instructions = details.map(d => { const x = Object.assign({}, d); delete x.code; return x; });
if (pg.total_parts > 1 || OBJ) out.code_blocks = index;
if (OBJ && !index.some(b => b.object.toUpperCase().includes(OBJ))) out.note = 'No code block matches object ' + OBJ + '; see code_blocks for the names.';
out.code = pg.text; out.part = pg.part; out.total_parts = pg.total_parts;
if (pg.part < pg.total_parts) out.more = 'Part ' + pg.part + ' of ' + pg.total_parts + '. Each part refetches the instruction (10-25 s): prefer object=<part of a name from code_blocks> to get only that object; otherwise the same call with part=' + (pg.part + 1) + '.';
out.check = 'Before advising: does each object exist in the system (TADIR), are the note and its prerequisite notes implemented (CWBNTCUST), and is the inserted code already in the object? A release filter only says the note formally applies.';
return out;`)
}

func itoa(n int) string { return strings.TrimSpace(jsLit(n)) }

// linksJS — ссылки на файл SNOTE и страницу печати; токен страницы печати живёт в сессии.
func linksJS(number string) string {
	return wrap(`
const NUM = ` + jsLit(number) + `;
const j = await getJSON(` + jsLit(noteDetailSv) + ` + encodeURIComponent(NUM));
const n = j && j.Response && j.Response.SAPNote;
if (!n) return {status: 'error', message: 'No note ' + NUM + ' found.'};
const a = n.Actions || {}, h = n.Header || {};
const v = (x) => (x && x.value != null ? x.value : '');
return {status: 'ok', number: unpad(NUM), title: String(v(n.Title)).trim(), version: v(h.Version),
  sar_url: (a.Download && a.Download.url) || '', pdf_print_url: (a.Print && a.Print.url) || ''};`)
}

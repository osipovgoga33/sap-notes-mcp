# sap-notes-mcp (Go)

An MCP server for Claude that searches and reads **SAP Notes and KBAs** on the
SAP support portal (me.sap.com) — including the correction instructions and
their code for your release, the SNOTE file (.SAR) and a PDF.

A single self-contained Go binary — no Python, no Node, no Playwright. Two modes:

| Mode | How it reaches SAP | Sign-in | Files |
|---|---|---|---|
| `browser` (default) | scripts that Claude runs in the built-in browser of Claude Desktop | yours, in that browser; no stored password | the browser asks where to save; PDF via the print dialog |
| `chrome` | its own Chrome or Edge in the background, separate profile | by the server with the user and password from the configuration; with two-factor authentication a window opens for you | `.SAR` and a real PDF straight into a folder |

[Русская версия](README.ru.md)

## How it works

The SAP portal serves its data to a real browser only: a program gets a dummy
page after one or two requests. So in browser mode the server never contacts SAP: every action
returns a short script; Claude runs it with the JavaScript tool of the built-in
browser on a me.sap.com tab, where you are signed in, and gets compact JSON
back.

- You sign in on me.sap.com in the built-in browser once. Two-factor
  authentication works as usual — you complete it yourself.
- The session lives in the browser profile: you can close the browser; Claude
  opens a me.sap.com tab when it needs one.
- When the session expires, the script returns `login_required` and Claude asks
  you to sign in again. Claude never types credentials.
- Requests go one at a time, at a human pace. Do not use it for bulk exports.

### chrome mode

The server starts Chrome or Edge (found automatically, or `SAP_NOTES_BROWSER`)
with its own profile folder and drives it over the DevTools protocol on
127.0.0.1. The same scripts run there, and the results come back directly.

- Sign-in: the server fills the SAP ID form (two-step or one-step) with
  `SAP_USERNAME` / `SAP_PASSWORD` and ticks "keep me signed in". The password
  is sent **at most once per call**: if the form comes back, the call stops
  with "rejected" — SAP locks an account after several wrong passwords.
- Two-factor authentication or no password configured: the browser restarts
  as a visible window on the sign-in page, the tool answers `login_window`,
  you sign in, and the next call continues in the background.
- `download` saves the `.SAR` (as SAP names it, e.g. `0003771041_00.SAR`) and
  prints the note's print page to `SAP Note <number> v<version>.pdf`.
- The password never appears in the log; the start line says only whether it
  is set.

## The tool

One tool, `sap_notes`, with an `action` parameter.

| action | What it does | Parameters |
|---|---|---|
| `search` | Notes and KBAs by keywords | `query`, `software_component` + `release` (keep notes valid for that release), `support_package` (drop notes already in the installed package; `02` or `SAPK-10602INS4CORE`), `app_component` (prefix such as `FI-GL`), `limit` 1–50 |
| `read` | One note: header, text as markdown, validity, support packages, prerequisites, references, side effects, summary of instructions | `number`, optional `software_component` + `release` to keep only the prerequisites for that release, `part` |
| `corrections` | The correction instruction for a release: valid-to support package, objects (TADIR), prerequisite notes, code changes in SNOTE form | `number` + `software_component` + `release`, or `instruction_key`; `object` to return only the code of matching objects; `part` |
| `download` | The SNOTE file (`.SAR`, for SNOTE → Upload SAP Note) and/or a PDF | `number`, `kind` = `sar` / `pdf` / `both` |
| `status` | Version, mode and the portal services used | — |

Typical flow for an error in a system: read `CVERS` of the system (component,
release, support package) → `search` with the exact message or dump name →
`read` → `corrections` for your release → check in the system whether the
objects exist, whether the note and its prerequisites are implemented
(`CWBNTCUST`) and whether the code is already there. A release filter only
says that a note formally applies.

## Install (Claude Desktop)

browser mode requires the built-in browser of Claude Desktop; chrome mode
requires Chrome or Edge.

**Extension:** Settings → Extensions → Advanced settings → Install Extension →
pick `sap-notes.mcpb`.

**Binary:** put `sap-notes.exe` somewhere permanent and add it to
`claude_desktop_config.json` (Settings → Developer → Edit Config):

```json
"sap-notes": {
  "command": "C:/Tools/MCP/sap-notes.exe"
}
```

chrome mode — add `env`:

```json
"sap-notes": {
  "command": "C:/Tools/MCP/sap-notes.exe",
  "env": {
    "SAP_NOTES_MODE": "chrome",
    "SAP_USERNAME": "your.name@example.com",
    "SAP_PASSWORD": "..."
  }
}
```

| Variable | Default |
|---|---|
| `SAP_NOTES_MODE` | `browser` |
| `SAP_USERNAME`, `SAP_PASSWORD` | — (no password: sign in by hand in a window) |
| `SAP_NOTES_DOWNLOAD_DIR` | `Downloads/SAP Notes` in your home folder |
| `SAP_NOTES_PROFILE_DIR` | `sap-notes/browser-profile` in the user config folder |
| `SAP_NOTES_BROWSER` | Chrome, then Edge, found automatically |
| `SAP_NOTES_SHOW_BROWSER` | empty; `1` keeps the window visible |

Quit Claude Desktop from the tray icon and start it again. Check: ask Claude to
call `sap_notes` with `action=status`; the first line of
`%APPDATA%\Claude\logs\mcp-server-sap-notes.log` shows the version and mode.

## Build

Go 1.22+. No external modules.

```powershell
go test ./...
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w" -o dist\sap-notes.exe .
```

All platforms and the `.mcpb`: `bash build.sh`. The tests run the browser
scripts in Node against recorded portal answers; without Node those tests are
skipped. `SAP_NOTES_E2E=1` adds an end-to-end test of chrome mode: a real
Chrome against a fake portal on local HTTPS (sign-in in two steps, a wrong
password, `.SAR` download, PDF).

## Notes

- The portal services are not publicly documented and may change.
- Search is slow on the SAP side (10–20 s); a script has about 40 s.
- The portal wraps code lines longer than 132 characters.
- Use it within the terms of your SAP support contract.

## License

[MIT](LICENSE). SAP and SAP product names are trademarks of SAP SE; this project
is not affiliated with SAP. All content reached through the server belongs to
SAP SE; the server stores and redistributes nothing.

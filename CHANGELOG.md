# Changelog

## [0.1.2] — 2026-11-08

- The server instructions sent at start now match the mode: in chrome mode
  they no longer tell Claude to run scripts in the built-in browser.
- First public release.

## [0.1.1] — 2026-11-08

- chrome mode: "signed in" is now checked by a request to the portal API.
  me.sap.com loads first and only then sends the browser to the sign-in by its
  own script, so reaching the host was not enough and the first script ran in
  the middle of the redirect ("Run this script on a https://me.sap.com tab").

## [0.1.0] — 2026-11-07

First version.

- One tool `sap_notes` with actions `search`, `read`, `corrections`, `download`, `status`.
- The SAP portal gives a program a dummy page after one or two requests; a
  real browser gets the data. In browser mode every action returns a script
  for a me.sap.com tab of the built-in browser of Claude Desktop, run in the
  user's own session.
- `search`: keywords plus software component, release and installed support
  package, so only notes still relevant for the system come back.
- `read`: header, text as markdown, validity, support packages, prerequisites
  (optionally only for one release, merged by note), references, side effects.
- `corrections`: the instruction for one release — valid-to support package,
  objects, prerequisite notes and the code changes; `object` returns only the
  code of matching objects.
- Two modes. `browser`: the server makes no network calls (see above).
  `chrome`: the server drives its own Chrome/Edge in the background over the
  DevTools protocol, signs in to SAP ID with the user and password from the
  configuration (the password is sent at most once per call), and opens a
  window for the user on two-factor authentication.
- `download`: the SNOTE file (`.SAR`) and a PDF of the note; in chrome mode
  straight into a folder, in browser mode as links.
- Expired session and timeouts are reported as `login_required` / `timeout`
  with what to do next.

package main

import (
	"bufio"
	"encoding/json"
	"io"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

var supportedProtocols = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const instrFlow = `Typical flow for an error in a customer system: read the system's software component versions from table CVERS (component, release, support package) -> action=search with the error text and those versions -> action=read the promising notes -> action=corrections for the user's release -> check in the system whether the objects exist (TADIR), whether the note and its prerequisites are implemented (CWBNTCUST) and whether the code is already there. action=download saves the SNOTE file (.SAR) and a PDF.

QUERIES IN ENGLISH: note texts are English; use the exact message class and number, the short dump name, the program or method name.`

func serverInstructions(chrome bool) string {
	if chrome {
		return "SAP Notes and KBAs from the SAP support portal (me.sap.com). One tool, sap_notes (parameter action). The server drives its own browser in the background, signed in with the user's SAP account, and returns results directly. If a call returns status login_window, ask the user to sign in in the window that opened, then call again; never ask for the password in chat.\n\n" + instrFlow
	}
	return "SAP Notes and KBAs from the SAP support portal (me.sap.com), read through the user's own signed-in session in the built-in browser of Claude Desktop. One tool, sap_notes (parameter action).\n\nThis server never contacts SAP itself: every action returns a short JavaScript that you run with the browser's JavaScript tool on a me.sap.com tab. SAP serves these services only to a real browser; a program gets a dummy page after one or two requests.\n\n" + instrFlow
}

func serve(in io.Reader, out io.Writer, ts *toolset) {
	reader := bufio.NewReaderSize(in, 1<<20)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for {
		line, err := reader.ReadBytes('\n')
		if len(trimSpaceBytes(line)) > 0 {
			if resp := handle(line, ts); resp != nil {
				_ = enc.Encode(resp)
			}
		}
		if err != nil {
			return
		}
	}
}

func trimSpaceBytes(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\r' || b[j-1] == '\n') {
		j--
	}
	return b[i:j]
}

func handle(line []byte, ts *toolset) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: -32700, Message: "parse error"}}
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil // уведомление: ответа нет
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := supportedProtocols[0]
		for _, v := range supportedProtocols {
			if v == p.ProtocolVersion {
				version = v
				break
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
			"instructions":    serverInstructions(ts.chrome != nil),
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": ts.list()}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			return resp
		}
		text, isErr := ts.call(p.Name, p.Arguments)
		resp.Result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": isErr,
		}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

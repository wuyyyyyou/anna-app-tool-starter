package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// The protocol manifest travels inside the executable; no working-directory
// assumptions or external Go installation are needed by the installed binary.
//
//go:embed manifest.json
var manifestJSON []byte

type parameter struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	ItemsType string `json:"items_type"`
	Default   any    `json:"default"`
}

type toolManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Tools   []struct {
		Name       string      `json:"name"`
		Parameters []parameter `json:"parameters"`
	} `json:"tools"`
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber() // Preserve integers and request IDs without float rounding.
	return decoder.Decode(target)
}

func readManifest() (toolManifest, error) {
	var manifest toolManifest
	err := decodeJSON(manifestJSON, &manifest)
	return manifest, err
}

func validType(value any, kind string) bool {
	switch kind {
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := number.Int64()
		return err == nil
	case "number":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := number.Float64()
		return err == nil
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	}
	return false
}

func getTypes(arguments map[string]any, parameters []parameter) (map[string]any, *rpcError) {
	data := make(map[string]any, len(parameters))
	for _, p := range parameters {
		value, provided := arguments[p.Name]
		if !provided {
			value = p.Default
		}
		if !validType(value, p.Type) {
			return nil, &rpcError{-32602, p.Name + " must be " + p.Type}
		}
		if p.Type == "array" {
			for _, item := range value.([]any) {
				if !validType(item, p.ItemsType) {
					return nil, &rpcError{-32602, p.Name + " items must be " + p.ItemsType}
				}
			}
		}
		data[p.Name] = value
	}
	for name := range arguments {
		if _, known := data[name]; !known {
			return nil, &rpcError{-32602, "Unknown argument: " + name}
		}
	}
	return data, nil
}

func handle(line []byte, manifest toolManifest) *response {
	resp := &response{JSONRPC: "2.0", ID: json.RawMessage("null")}
	if !json.Valid(line) {
		resp.Error = &rpcError{-32700, "Parse error"}
		return resp
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		resp.Error = &rpcError{-32600, "Invalid JSON-RPC request"}
		return resp
	}
	if len(req.ID) > 0 {
		var id any
		_ = decodeJSON(req.ID, &id)
		switch id.(type) {
		case nil, string, json.Number:
			resp.ID = req.ID
		default:
			resp.Error = &rpcError{-32600, "Request id must be a string, number or null"}
			return resp
		}
	}

	params := map[string]any{}
	if len(req.Params) > 0 {
		if err := decodeJSON(req.Params, &params); err != nil || params == nil {
			resp.Error = &rpcError{-32602, "params must be an object"}
		}
	}
	if resp.Error == nil {
		switch req.Method {
		case "initialize":
			version, exists := params["protocolVersion"]
			if !exists {
				version = "2.0"
			}
			if version != "2.0" && version != "1.1" {
				resp.Error = &rpcError{-32602, "Unsupported protocolVersion"}
				break
			}
			resp.Result = map[string]any{
				"protocolVersion": version,
				"server_info":     map[string]string{"name": manifest.Name, "version": manifest.Version},
				"capabilities":    map[string]any{},
			}
		case "describe":
			resp.Result = json.RawMessage(manifestJSON) // Bare manifest, not an InvokeResult.
		case "health":
			resp.Result = map[string]any{"status": "ready", "version": manifest.Version}
		case "invoke":
			tool, ok := params["tool"].(string)
			if !ok || tool == "" {
				resp.Error = &rpcError{-32602, "tool must be a non-empty string"}
				break
			}
			if tool != "get_types" {
				resp.Error = &rpcError{-32601, "Unknown tool: " + tool}
				break
			}
			arguments := map[string]any{}
			if supplied, exists := params["arguments"]; exists {
				arguments, ok = supplied.(map[string]any)
				if !ok {
					resp.Error = &rpcError{-32602, "arguments must be an object"}
					break
				}
			}
			data, err := getTypes(arguments, manifest.Tools[0].Parameters)
			if err != nil {
				resp.Error = err
				break
			}
			resp.Result = map[string]any{"success": true, "data": data, "tool": tool}
		default:
			resp.Error = &rpcError{-32601, "Method not found: " + req.Method}
		}
	}
	// JSON-RPC notifications have no id and must not produce a response.
	if len(req.ID) == 0 {
		return nil
	}
	return resp
}

func serve(input io.Reader, output io.Writer, manifest toolManifest) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if resp := handle(line, manifest); resp != nil {
			// Write directly to stdout: one UTF-8 JSON frame + LF, no buffered logs.
			if err := encoder.Encode(resp); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func main() {
	manifest, err := readManifest()
	if err == nil {
		err = serve(os.Stdin, os.Stdout, manifest)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

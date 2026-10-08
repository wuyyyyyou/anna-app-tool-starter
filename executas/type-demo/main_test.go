package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func testManifest(t *testing.T) toolManifest {
	t.Helper()
	manifest, err := readManifest()
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func invokeForTest(t *testing.T, arguments string) *response {
	t.Helper()
	line := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"invoke","params":{"tool":"get_types","arguments":%s}}`, arguments)
	return handle([]byte(line), testManifest(t))
}

func TestDefaultTypesAndExplicitEmptyValues(t *testing.T) {
	for _, args := range []string{`{}`, `{"string_value":"你好","integer_value":0,"number_value":0.5,"boolean_value":false,"array_value":[],"object_value":{}}`} {
		resp := invokeForTest(t, args)
		if resp.Error != nil {
			t.Fatalf("%s: %v", args, resp.Error)
		}
		result := resp.Result.(map[string]any)
		if result["success"] != true {
			t.Fatal("missing success: true")
		}
		data := result["data"].(map[string]any)
		if len(data) != 6 {
			t.Fatalf("expected six fields, got %v", data)
		}
		if args != `{}` && (data["boolean_value"] != false || data["integer_value"] != json.Number("0") || len(data["array_value"].([]any)) != 0 || len(data["object_value"].(map[string]any)) != 0) {
			t.Fatalf("explicit empty values were replaced with defaults: %v", data)
		}
	}
}

func TestRejectInvalidArguments(t *testing.T) {
	for _, args := range []string{
		`[]`, `null`, `{"unknown":1}`, `{"string_value":1}`, `{"integer_value":"42"}`,
		`{"integer_value":1.5}`, `{"number_value":"3.14"}`, `{"boolean_value":1}`,
		`{"array_value":"[]"}`, `{"array_value":["Anna",42]}`, `{"object_value":[]}`,
		`{"string_value":null}`, `{"number_value":1e400}`,
	} {
		t.Run(args, func(t *testing.T) {
			resp := invokeForTest(t, args)
			if resp.Error == nil || resp.Error.Code != -32602 {
				t.Fatalf("expected -32602, got %#v", resp)
			}
		})
	}
}

func TestFramingErrorsAndRecovery(t *testing.T) {
	input := strings.Join([]string{
		``,
		`not-json`,
		`{"jsonrpc":"1.0","id":2,"method":"describe"}`,
		`{"jsonrpc":"2.0","id":3,"method":"missing"}`,
		`{"jsonrpc":"2.0","id":4,"method":"invoke","params":{"tool":"missing"}}`,
		`{"jsonrpc":"2.0","method":"health"}`, // Notification: no response.
		`{"jsonrpc":"2.0","id":"after-errors","method":"describe"}`,
		`{"jsonrpc":"2.0","id":9007199254740993,"method":"health"}`,
		`{"jsonrpc":"2.0","id":7,"method":"invoke","params":{"tool":"get_types","arguments":{}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := serve(strings.NewReader(input), &output, testManifest(t)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 7 {
		t.Fatalf("expected seven protocol frames, got %d: %s", len(lines), output.String())
	}
	codes := []int{-32700, -32600, -32601, -32601}
	for index, line := range lines {
		var reply response
		if err := json.Unmarshal([]byte(line), &reply); err != nil {
			t.Fatalf("invalid stdout frame: %v", err)
		}
		if reply.JSONRPC != "2.0" {
			t.Fatal("missing JSON-RPC version")
		}
		if index < len(codes) && (reply.Error == nil || reply.Error.Code != codes[index]) {
			t.Fatalf("wrong error response: %#v", reply)
		}
	}
	if !strings.Contains(lines[4], `"id":"after-errors"`) || !strings.Contains(lines[5], `"id":9007199254740993`) {
		t.Fatal("request IDs were not preserved")
	}
	var described struct{ Result json.RawMessage }
	_ = json.Unmarshal([]byte(lines[4]), &described)
	var protocol map[string]any
	if err := json.Unmarshal(described.Result, &protocol); err != nil {
		t.Fatal(err)
	}
	if protocol["tools"] == nil || protocol["version"] == nil || protocol["manifest"] != nil || protocol["success"] != nil {
		t.Fatalf("describe must return the manifest itself: %v", protocol)
	}
}

func TestInitializeAndInvalidRequestIDs(t *testing.T) {
	manifest := testManifest(t)
	resp := handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2.0"}}`), manifest)
	if resp.Error != nil || resp.Result.(map[string]any)["protocolVersion"] != "2.0" {
		t.Fatalf("bad handshake: %#v", resp)
	}
	for _, line := range []string{`[]`, `null`, `{"jsonrpc":"2.0","id":true,"method":"describe"}`} {
		resp := handle([]byte(line), manifest)
		if resp.Error == nil || resp.Error.Code != -32600 {
			t.Fatalf("expected invalid request, got %#v", resp)
		}
	}
}

// Run from the repository root: go run scripts/build-binary.go [--all] [--check]
package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"
)

type artifact struct {
	Path       string `json:"path"`
	Entrypoint string `json:"entrypoint"`
	Format     string `json:"format"`
}

type archiveEntry struct {
	Name string
	Mode os.FileMode
	Data []byte
}

var platforms = map[string][2]string{
	"darwin-arm64":   {"darwin", "arm64"},
	"darwin-x86_64":  {"darwin", "amd64"},
	"linux-x86_64":   {"linux", "amd64"},
	"linux-aarch64":  {"linux", "arm64"},
	"windows-x86_64": {"windows", "amd64"},
}

func hostPlatform() string {
	for name, target := range platforms {
		if target[0] == runtime.GOOS && target[1] == runtime.GOARCH {
			return name
		}
	}
	return runtime.GOOS + "-" + runtime.GOARCH
}

func writeArchive(path, format string, entries []archiveEntry) (err error) {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	if format == "zip" {
		writer := zip.NewWriter(file)
		for _, entry := range entries {
			header := &zip.FileHeader{Name: entry.Name, Method: zip.Deflate}
			header.SetMode(entry.Mode)
			header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
			output, createErr := writer.CreateHeader(header)
			if createErr != nil {
				_ = writer.Close()
				return createErr
			}
			if _, writeErr := output.Write(entry.Data); writeErr != nil {
				_ = writer.Close()
				return writeErr
			}
		}
		return writer.Close()
	}
	if format != "tar.gz" {
		return fmt.Errorf("unsupported archive format %q", format)
	}
	compressed := gzip.NewWriter(file)
	writer := tar.NewWriter(compressed)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.Name, Mode: int64(entry.Mode.Perm()), Size: int64(len(entry.Data)), ModTime: time.Unix(0, 0)}
		if entry.Mode.IsDir() {
			header.Typeflag = tar.TypeDir
		} else {
			header.Typeflag = tar.TypeReg
		}
		if err := writer.WriteHeader(header); err != nil {
			_ = writer.Close()
			_ = compressed.Close()
			return err
		}
		if _, err := writer.Write(entry.Data); err != nil {
			_ = writer.Close()
			_ = compressed.Close()
			return err
		}
	}
	if err := writer.Close(); err != nil {
		_ = compressed.Close()
		return err
	}
	return compressed.Close()
}

func readArchive(path, format string) (map[string][]byte, error) {
	entries := map[string][]byte{}
	if format == "zip" {
		reader, err := zip.OpenReader(path)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		for _, file := range reader.File {
			if file.FileInfo().IsDir() {
				continue
			}
			input, err := file.Open()
			if err != nil {
				return nil, err
			}
			data, readErr := io.ReadAll(input)
			_ = input.Close()
			if readErr != nil {
				return nil, readErr
			}
			entries[file.Name] = data
		}
		return entries, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		entries[header.Name] = data
	}
	return entries, nil
}

func sameJSON(a, b []byte) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}

// Keep stdin open between calls: this catches plugins that exit after describe.
func smokeBinary(binary string, manifestJSON []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var logs bytes.Buffer
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = input.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	encoder := json.NewEncoder(input)
	reader := bufio.NewReader(output)
	call := func(id any, method string, params any) (json.RawMessage, *struct {
		Code int `json:"code"`
	}, error) {
		frame := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
		if err := encoder.Encode(frame); err != nil {
			return nil, nil, err
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w (%s)", method, err, logs.String())
		}
		var reply struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      any             `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &reply); err != nil {
			return nil, nil, fmt.Errorf("stdout is not a JSON-RPC frame: %w", err)
		}
		if reply.JSONRPC != "2.0" || !reflect.DeepEqual(reply.ID, id) {
			return nil, nil, fmt.Errorf("response protocol or id mismatch")
		}
		return reply.Result, reply.Error, nil
	}

	if _, rpcErr, err := call(float64(1), "initialize", map[string]any{"protocolVersion": "2.0"}); err != nil {
		return err
	} else if rpcErr != nil {
		return fmt.Errorf("initialize failed: %d", rpcErr.Code)
	}
	result, rpcErr, err := call("describe-check", "describe", map[string]any{})
	if err != nil {
		return err
	}
	if rpcErr != nil || !sameJSON(result, manifestJSON) {
		return fmt.Errorf("binary describe differs from the source protocol manifest")
	}
	var manifest struct {
		Tools []struct {
			Name       string
			Parameters []struct {
				Name    string
				Default any
			}
		}
	}
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return err
	}
	defaults := map[string]any{}
	for _, p := range manifest.Tools[0].Parameters {
		defaults[p.Name] = p.Default
	}
	invoke := func(id float64, args map[string]any, want map[string]any) error {
		result, rpcErr, err := call(id, "invoke", map[string]any{"tool": manifest.Tools[0].Name, "arguments": args})
		if err != nil {
			return err
		}
		if rpcErr != nil {
			return fmt.Errorf("invoke failed: %d", rpcErr.Code)
		}
		var reply struct {
			Success bool
			Data    map[string]any
			Tool    string
		}
		if err := json.Unmarshal(result, &reply); err != nil {
			return err
		}
		if !reply.Success || reply.Tool != manifest.Tools[0].Name || !reflect.DeepEqual(reply.Data, want) {
			return fmt.Errorf("invoke payload or success wrapper mismatch")
		}
		return nil
	}
	if err := invoke(3, map[string]any{}, defaults); err != nil {
		return err
	}
	_, rpcErr, err = call(float64(4), "invoke", map[string]any{"tool": manifest.Tools[0].Name, "arguments": map[string]any{"integer_value": "42"}})
	if err != nil {
		return err
	}
	if rpcErr == nil || rpcErr.Code != -32602 {
		return fmt.Errorf("invalid argument did not return -32602")
	}
	custom := map[string]any{"string_value": "你好，Anna", "integer_value": float64(0), "number_value": 0.5, "boolean_value": false, "array_value": []any{}, "object_value": map[string]any{}}
	if err := invoke(5, custom, custom); err != nil {
		return err
	}
	if err := invoke(6, map[string]any{}, defaults); err != nil {
		return err
	}
	_, rpcErr, err = call(float64(7), "unknown_method", map[string]any{})
	if err != nil {
		return err
	}
	if rpcErr == nil || rpcErr.Code != -32601 {
		return fmt.Errorf("unknown method did not return -32601")
	}
	result, rpcErr, err = call(float64(8), "health", map[string]any{})
	if err != nil {
		return err
	}
	var health struct{ Status string }
	if rpcErr != nil || json.Unmarshal(result, &health) != nil || health.Status != "ready" {
		return fmt.Errorf("health failed after repeated calls")
	}
	if err := input.Close(); err != nil {
		return err
	}
	tail, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(tail)) != 0 {
		return fmt.Errorf("unexpected stdout after final response")
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("binary did not exit cleanly on EOF: %w", err)
	}
	return nil
}

func build(toolDir, platform, version, name string, spec artifact, protocolJSON []byte, check bool) error {
	target, supported := platforms[platform]
	if !supported {
		return fmt.Errorf("unsupported platform %q", platform)
	}
	expectedEntry := "bin/" + name
	if target[0] == "windows" {
		expectedEntry += ".exe"
	}
	if spec.Entrypoint != expectedEntry {
		return fmt.Errorf("%s entrypoint must be %s", platform, expectedEntry)
	}
	expectedFormat := "tar.gz"
	if target[0] == "windows" {
		expectedFormat = "zip"
	}
	if spec.Format != expectedFormat {
		return fmt.Errorf("%s format must be %s", platform, expectedFormat)
	}
	archivePath := filepath.Join(toolDir, filepath.FromSlash(strings.ReplaceAll(spec.Path, "{version}", version)))
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "anna-type-demo-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	binaryPath := filepath.Join(temp, filepath.Base(expectedEntry))
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", binaryPath, ".")
	cmd.Dir = toolDir
	cmd.Env = append(os.Environ(), "GOOS="+target[0], "GOARCH="+target[1], "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	binaryJSON, err := json.MarshalIndent(map[string]any{
		"name": name, "version": version,
		"runtime": map[string]any{"binary": map[string]any{
			"entrypoint":  spec.Entrypoint,
			"permissions": map[string]string{spec.Entrypoint: "0o755"},
		}},
	}, "", "  ")
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return err
	}
	entries := []archiveEntry{{"manifest.json", 0o644, append(binaryJSON, '\n')}, {"bin/", os.ModeDir | 0o755, nil}, {spec.Entrypoint, 0o755, binary}}
	if err := writeArchive(archivePath, spec.Format, entries); err != nil {
		return err
	}
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	checksum := fmt.Sprintf("%x  %s\n", sum, filepath.Base(archivePath))
	if err := os.WriteFile(archivePath+".sha256", []byte(checksum), 0o644); err != nil {
		return err
	}
	fmt.Printf("Built %s\n", archivePath)
	if check {
		unpacked, err := readArchive(archivePath, spec.Format)
		if err != nil {
			return err
		}
		if len(unpacked) != 2 || !sameJSON(unpacked["manifest.json"], binaryJSON) || !bytes.Equal(unpacked[spec.Entrypoint], binary) {
			return fmt.Errorf("%s archive layout or contents mismatch", platform)
		}
		if platform == hostPlatform() {
			// Execute the bytes read back from the final archive, not the build file.
			if err := os.WriteFile(binaryPath, unpacked[spec.Entrypoint], 0o755); err != nil {
				return err
			}
			if err := smokeBinary(binaryPath, protocolJSON); err != nil {
				return err
			}
			fmt.Println("  Passed: archived binary initialize / describe / invoke / errors / persistent process / EOF")
		} else {
			fmt.Printf("  Passed: %s archive structure (execution requires the target OS/architecture)\n", platform)
		}
	}
	return nil
}

func run() error {
	all := flag.Bool("all", false, "cross-compile all declared platforms locally")
	platform := flag.String("platform", "", "Anna platform key (default: current host)")
	check := flag.Bool("check", false, "verify archives and smoke-test the current host binary")
	flag.Parse()
	if *all && *platform != "" {
		return fmt.Errorf("use --all or --platform, not both")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	toolDir := filepath.Join(root, "executas", "type-demo")
	metadataJSON, err := os.ReadFile(filepath.Join(toolDir, "executa.json"))
	if err != nil {
		return fmt.Errorf("run this script from the repository root: %w", err)
	}
	var metadata struct {
		Version      string
		Distribution struct {
			BinaryArtifacts map[string]artifact `json:"binary_artifacts"`
		}
	}
	if err := json.Unmarshal(metadataJSON, &metadata); err != nil {
		return err
	}
	protocolJSON, err := os.ReadFile(filepath.Join(toolDir, "manifest.json"))
	if err != nil {
		return err
	}
	var protocol struct {
		Name    string
		Version string
	}
	if err := json.Unmarshal(protocolJSON, &protocol); err != nil {
		return err
	}
	if protocol.Version != metadata.Version {
		return fmt.Errorf("executa.json and protocol manifest versions differ; update both before building")
	}
	selected := []string{}
	if *all {
		for name := range metadata.Distribution.BinaryArtifacts {
			selected = append(selected, name)
		}
		sort.Strings(selected)
	} else {
		if *platform == "" {
			*platform = hostPlatform()
		}
		selected = append(selected, *platform)
	}
	for _, platform := range selected {
		spec, exists := metadata.Distribution.BinaryArtifacts[platform]
		if !exists {
			return fmt.Errorf("no binary_artifacts declaration for %s", platform)
		}
		if err := build(toolDir, platform, metadata.Version, protocol.Name, spec, protocolJSON, *check); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

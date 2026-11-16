package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// embedEngine is a long-running embedding sidecar (llama-server) spoken to
// over HTTP, so the harness itself stays pure Go on every platform.
type embedEngine struct {
	name        string
	bin         string
	args        []string
	queryPrefix string
	docPrefix   string
	port        int
	cmd         *exec.Cmd
	logFile     *os.File
	client      *http.Client
	dims        int

	phase      atomic.Int32 // 0 = indexing, 1 = querying
	exited     atomic.Bool
	indexPeak  atomic.Int64 // KB
	queryPeak  atomic.Int64
	stopSample chan struct{}
}

// parseSpec splits "name=bin args..." with quote awareness. The tokens
// query-prefix=... and doc-prefix=... are harness instructions (models like
// nomic and bge want task prefixes) and never reach the server.
func parseSpec(spec string) (*embedEngine, error) {
	tokens, err := splitSpec(spec)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("empty engine spec")
	}
	e := &embedEngine{client: &http.Client{Timeout: 5 * time.Minute}}
	first := tokens[0]
	if name, bin, found := strings.Cut(first, "="); found {
		e.name, e.bin = name, bin
		tokens = tokens[1:]
	} else {
		e.bin = first
		e.name = strings.TrimSuffix(filepath.Base(first), filepath.Ext(first))
		tokens = tokens[1:]
	}
	if e.bin == "" && len(tokens) > 0 {
		e.bin = tokens[0]
		tokens = tokens[1:]
	}
	if e.bin == "" {
		return nil, fmt.Errorf("engine %q: binary path is missing", e.name)
	}
	for _, tok := range tokens {
		if key, value, found := strings.Cut(tok, "="); found && key == "query-prefix" {
			e.queryPrefix = value
			continue
		}
		if key, value, found := strings.Cut(tok, "="); found && key == "doc-prefix" {
			e.docPrefix = value
			continue
		}
		e.args = append(e.args, tok)
	}
	return e, nil
}

func splitSpec(spec string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	quoted := false
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for _, r := range spec {
		switch {
		case r == '"':
			quoted = !quoted
		case (r == ' ' || r == '\t') && !quoted:
			flush()
		default:
			current.WriteRune(r)
		}
	}
	if quoted {
		return nil, fmt.Errorf("unbalanced quote in engine spec")
	}
	flush()
	return tokens, nil
}

// start launches the sidecar on a free port and waits until it reports
// healthy. The server log is kept so a failed load can be diagnosed.
func (e *embedEngine) start(logDir string) error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	e.port = l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	args := make([]string, len(e.args))
	for i, arg := range e.args {
		args[i] = strings.ReplaceAll(arg, "{port}", strconv.Itoa(e.port))
	}
	e.cmd = exec.Command(e.bin, args...)
	// No Dir override: Windows resolves the sidecar's DLLs from its own
	// directory, and keeping the harness cwd lets -m use relative paths.

	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	e.logFile, err = os.Create(filepath.Join(logDir, e.name+".log"))
	if err != nil {
		return err
	}
	e.cmd.Stdout = e.logFile
	e.cmd.Stderr = e.logFile
	if err := e.cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", e.name, err)
	}
	go func() {
		_ = e.cmd.Wait()
		e.exited.Store(true)
	}()

	e.stopSample = make(chan struct{})
	go e.sampleMemory()

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := e.client.Get(e.baseURL() + "/health")
		if err == nil {
			status := resp.StatusCode
			_ = resp.Body.Close()
			if status == http.StatusOK {
				return nil
			}
		}
		if e.exited.Load() {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("%s never became healthy; log tail:\n%s", e.name, e.logTail())
}

func (e *embedEngine) baseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", e.port)
}

func (e *embedEngine) logTail() string {
	if e.logFile == nil {
		return "(no log)"
	}
	raw, err := os.ReadFile(e.logFile.Name())
	if err != nil {
		return "(log unreadable)"
	}
	if len(raw) > 2000 {
		raw = raw[len(raw)-2000:]
	}
	return string(raw)
}

// embed sends one batch and returns L2-normalised vectors in input order.
func (e *embedEngine) embed(texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{
		"input":           texts,
		"encoding_format": "float",
	})
	if err != nil {
		return nil, err
	}
	resp, err := e.client.Post(e.baseURL()+"/v1/embeddings", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s embeddings: %w", e.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tail := make([]byte, 512)
		n, _ := resp.Body.Read(tail)
		return nil, fmt.Errorf("%s embeddings: HTTP %d: %s", e.name, resp.StatusCode, tail[:n])
	}
	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("%s embeddings decode: %w", e.name, err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("%s embeddings: got %d vectors for %d inputs", e.name, len(parsed.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(texts) {
			return nil, fmt.Errorf("%s embeddings: index %d out of range", e.name, item.Index)
		}
		out[item.Index] = normalize(item.Embedding)
	}
	for _, vec := range out {
		if vec == nil {
			return nil, fmt.Errorf("%s embeddings: missing vector in response", e.name)
		}
	}
	if e.dims == 0 && len(out) > 0 {
		e.dims = len(out[0])
	}
	return out, nil
}

func normalize(vec []float32) []float32 {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return vec
	}
	inv := float32(1.0 / math.Sqrt(sum))
	for i := range vec {
		vec[i] *= inv
	}
	return vec
}

func (e *embedEngine) close() {
	if e.stopSample != nil {
		close(e.stopSample)
	}
	if e.cmd != nil && e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
		_ = e.cmd.Wait()
	}
	if e.logFile != nil {
		_ = e.logFile.Close()
	}
}

// sampleMemory tracks the sidecar's resident set per phase, because the PRD
// baseline cares about indexing memory and steady-state memory separately.
func (e *embedEngine) sampleMemory() {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopSample:
			return
		case <-ticker.C:
			kb, err := processRSSKB(e.cmd.Process.Pid)
			if err != nil || kb == 0 {
				continue
			}
			if e.phase.Load() == 0 {
				for {
					old := e.indexPeak.Load()
					if kb <= old || e.indexPeak.CompareAndSwap(old, kb) {
						break
					}
				}
			} else {
				for {
					old := e.queryPeak.Load()
					if kb <= old || e.queryPeak.CompareAndSwap(old, kb) {
						break
					}
				}
			}
		}
	}
}

func processRSSKB(pid int) (int64, error) {
	switch runtime.GOOS {
	case "windows":
		out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
		if err != nil {
			return 0, err
		}
		record, err := csv.NewReader(bytes.NewReader(out)).Read()
		if err != nil || len(record) < 5 {
			return 0, fmt.Errorf("tasklist: no row for pid %d", pid)
		}
		field := strings.TrimSuffix(strings.TrimSpace(record[len(record)-1]), " K")
		field = strings.ReplaceAll(field, ",", "")
		return strconv.ParseInt(field, 10, 64)
	case "linux":
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					return strconv.ParseInt(fields[1], 10, 64)
				}
			}
		}
		return 0, fmt.Errorf("VmRSS not found for pid %d", pid)
	case "darwin":
		out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return 0, err
		}
		return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	default:
		return 0, fmt.Errorf("memory sampling unsupported on %s", runtime.GOOS)
	}
}

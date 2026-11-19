package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

// Server is one llama-server sidecar serving embeddings over loopback HTTP.
// The app stays pure Go (CGO disabled); the runtime is a prebuilt binary the
// installer placed, started on demand and stopped with the app.
type Server struct {
	cmd    *exec.Cmd
	port   int
	client *http.Client
	probe  *http.Client
	log    *os.File
	exited atomic.Bool
	done   chan struct{}
}

// serverFlags are the benchmark-measured settings for the limited-spec
// baseline. Four threads matches the baseline's four logical cores; a bigger
// machine still answers inside the latency budget with them.
func serverArgs(modelPath string, port int) []string {
	return []string{
		"--embeddings",
		"--port", strconv.Itoa(port),
		"-m", modelPath,
		"-c", "8192",
		"-b", "8192",
		"-ub", "2048",
		"--parallel", "4",
		"-t", "4",
	}
}

// StartServer launches the sidecar and waits until /health reports the model
// loaded. The log file keeps the runtime's own output for diagnosing a failed
// start; a startup error carries its tail so the operator sees the reason.
func StartServer(ctx context.Context, binPath, modelPath, logPath string) (*Server, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(binPath, serverArgs(modelPath, port)...)
	cmd.Stdout = log
	cmd.Stderr = log
	hideConsole(cmd)
	if err := cmd.Start(); err != nil {
		log.Close()
		return nil, fmt.Errorf("start embedding runtime: %w", err)
	}

	s := &Server{
		cmd:    cmd,
		port:   port,
		client: &http.Client{Timeout: 5 * time.Minute},
		probe:  &http.Client{Timeout: 2 * time.Second},
		log:    log,
		done:   make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		s.exited.Store(true)
		close(s.done)
	}()

	if err := s.waitHealthy(ctx); err != nil {
		s.Stop()
		return nil, fmt.Errorf("embedding runtime did not become healthy: %w (log: %s)", err, logTail(logPath))
	}
	return s, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

func (s *Server) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if s.exited.Load() {
			return errors.New("the runtime process exited during startup")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url("/health"), nil)
		if err != nil {
			return err
		}
		resp, err := s.probe.Do(req)
		if err == nil {
			status := resp.StatusCode
			resp.Body.Close()
			if status == http.StatusOK {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the model to load")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (s *Server) url(path string) string {
	return "http://127.0.0.1:" + strconv.Itoa(s.port) + path
}

// Exited reports whether the process has died; the service restarts it on the
// next use rather than holding a dead handle.
func (s *Server) Exited() bool { return s.exited.Load() }

type embeddingsRequest struct {
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns one L2-normalized vector per input text, in input order.
// Normalizing here means retrieval is a plain dot product.
func (s *Server) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embeddingsRequest{Input: texts, EncodingFormat: "float"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url("/v1/embeddings"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return nil, fmt.Errorf("embed request: status %s: %s", resp.Status, bytes.TrimSpace(msg))
	}

	var parsed embeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("embed response: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embed response: got %d vectors for %d texts", len(parsed.Data), len(texts))
	}
	vectors := make([][]float32, len(texts))
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(texts) {
			return nil, fmt.Errorf("embed response: index %d out of range", item.Index)
		}
		vectors[item.Index] = normalize(item.Embedding)
	}
	for i, v := range vectors {
		if v == nil {
			return nil, fmt.Errorf("embed response: missing vector %d", i)
		}
	}
	return vectors, nil
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := float32(math.Sqrt(sum))
	if norm == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

// Stop kills the sidecar and releases its log. Safe to call more than once.
func (s *Server) Stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		<-s.done
	}
	if s.log != nil {
		s.log.Close()
		s.log = nil
	}
}

func logTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "unavailable"
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "unavailable"
	}
	const max = 2000
	offset := int64(0)
	if info.Size() > max {
		offset = info.Size() - max
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return "unavailable"
	}
	return string(bytes.TrimSpace(buf))
}

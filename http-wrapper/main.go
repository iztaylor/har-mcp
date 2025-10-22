package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// MCPRequest represents an MCP JSON-RPC request
type MCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// MCPResponse represents an MCP JSON-RPC response
type MCPResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// MCPSession manages a single MCP server process
type MCPSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	mu     sync.Mutex
	reader *bufio.Reader
}

// NewMCPSession creates a new MCP server session
func NewMCPSession(ctx context.Context) (*MCPSession, error) {
	cmd := exec.CommandContext(ctx, "har-mcp")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start MCP server: %w", err)
	}

	session := &MCPSession{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		reader: bufio.NewReader(stdout),
	}

	// Log stderr in background
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("[MCP stderr] %s", scanner.Text())
		}
	}()

	return session, nil
}

// SendRequest sends a request to the MCP server and waits for response
func (s *MCPSession) SendRequest(req MCPRequest) (*MCPResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Marshal request
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Send request
	reqBytes = append(reqBytes, '\n')
	if _, err := s.stdin.Write(reqBytes); err != nil {
		return nil, fmt.Errorf("failed to write request: %w", err)
	}

	// Read response
	respBytes, err := s.reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse response
	var resp MCPResponse
	if err := json.Unmarshal(bytes.TrimSpace(respBytes), &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &resp, nil
}

// Close closes the MCP session
func (s *MCPSession) Close() error {
	s.stdin.Close()
	s.stdout.Close()
	s.stderr.Close()
	return s.cmd.Wait()
}

// HTTPWrapper wraps the MCP server with HTTP endpoints
type HTTPWrapper struct {
	sessions sync.Map // map[string]*MCPSession
}

// NewHTTPWrapper creates a new HTTP wrapper
func NewHTTPWrapper() *HTTPWrapper {
	return &HTTPWrapper{}
}

// getOrCreateSession gets or creates an MCP session for a client
func (w *HTTPWrapper) getOrCreateSession(sessionID string) (*MCPSession, error) {
	if val, ok := w.sessions.Load(sessionID); ok {
		return val.(*MCPSession), nil
	}

	ctx := context.Background()
	session, err := NewMCPSession(ctx)
	if err != nil {
		return nil, err
	}

	w.sessions.Store(sessionID, session)

	// Auto-cleanup after 30 minutes of inactivity
	go func() {
		time.Sleep(30 * time.Minute)
		w.sessions.Delete(sessionID)
		session.Close()
	}()

	return session, nil
}

// handleMCP handles MCP JSON-RPC requests over HTTP
func (w *HTTPWrapper) handleMCP(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get or create session
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = "default"
	}

	session, err := w.getOrCreateSession(sessionID)
	if err != nil {
		log.Printf("Failed to create session: %v", err)
		http.Error(rw, fmt.Sprintf("Failed to create MCP session: %v", err), http.StatusInternalServerError)
		return
	}

	// Parse request
	var req MCPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Forward to MCP server
	resp, err := session.SendRequest(req)
	if err != nil {
		log.Printf("MCP request failed: %v", err)
		http.Error(rw, fmt.Sprintf("MCP request failed: %v", err), http.StatusInternalServerError)
		return
	}

	// Send response
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(resp)
}

// handleHealth handles health check requests
func (w *HTTPWrapper) handleHealth(rw http.ResponseWriter, r *http.Request) {
	rw.WriteHeader(http.StatusOK)
	rw.Write([]byte("OK"))
}

func main() {
	wrapper := NewHTTPWrapper()

	http.HandleFunc("/mcp", wrapper.handleMCP)
	http.HandleFunc("/health", wrapper.handleHealth)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addr := fmt.Sprintf(":%s", port)
	log.Printf("Starting HTTP wrapper on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

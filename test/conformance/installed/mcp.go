package installed

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"time"
)

// mcpClient speaks MCP (JSON-RPC 2.0, one message per line) to `kenaz mcp`
// over stdio. It is written by hand rather than with the SDK the server
// uses, so that it checks the wire and not the library.
type mcpClient struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan []byte
	id    int
}

type rpcResponse struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *sandbox) startMCP(ctx context.Context) (*mcpClient, error) {
	if err := s.fileStoreRecorded(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, s.exe, "mcp")
	cmd.Dir, cmd.Env = s.work, s.env
	cmd.WaitDelay = 5 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &mcpClient{cmd: cmd, in: in, lines: make(chan []byte, 16)}
	go func() {
		defer close(c.lines)
		r := bufio.NewReaderSize(out, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				c.lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
	return c, nil
}

func (c *mcpClient) close() {
	_ = c.in.Close()
	done := make(chan struct{})
	go func() { _ = c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
	}
}

func (c *mcpClient) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.in.Write(append(b, '\n'))
	return err
}

// request sends method and waits for its response, skipping notifications
// and server requests.
func (c *mcpClient) request(method string, params any) (json.RawMessage, error) {
	c.id++
	id := c.id
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := c.send(msg); err != nil {
		return nil, err
	}
	deadline := time.After(30 * time.Second)
	for {
		select {
		case line, ok := <-c.lines:
			if !ok {
				return nil, errors.New("the MCP server closed its output")
			}
			var r rpcResponse
			if json.Unmarshal(line, &r) != nil || r.ID == nil || *r.ID != id {
				continue
			}
			if r.Error != nil {
				return nil, fmt.Errorf("JSON-RPC error %d", r.Error.Code)
			}
			return r.Result, nil
		case <-deadline:
			return nil, fmt.Errorf("no response to %s within 30 s", method)
		}
	}
}

// initialize performs the MCP handshake as clientName and returns the
// server's name.
func (c *mcpClient) initialize(clientName string) (string, error) {
	raw, err := c.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": "0.0.0-conformance"},
	})
	if err != nil {
		return "", err
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	if res.ProtocolVersion == "" {
		return "", errors.New("initialize result has no protocolVersion")
	}
	if _, ok := res.Capabilities["tools"]; !ok {
		return "", errors.New("the server does not declare the tools capability")
	}
	return res.ServerInfo.Name, c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
}

// tool is one tools/list entry.
type tool struct {
	Name        string          `json:"name"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func (c *mcpClient) tools() (map[string]tool, error) {
	out := map[string]tool{}
	cursor := ""
	for page := 0; page < 20; page++ {
		var params map[string]any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		raw, err := c.request("tools/list", params)
		if err != nil {
			return nil, err
		}
		var res struct {
			Tools      []tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, err
		}
		for _, t := range res.Tools {
			out[t.Name] = t
		}
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	return out, nil
}

// checkSchema reports why a tool's input schema does not parse as a JSON
// Schema object description ("" when it does).
func checkSchema(raw json.RawMessage) string {
	var s struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "inputSchema is not a JSON object"
	}
	if s.Type != "object" {
		return "inputSchema type is not object"
	}
	for name, p := range s.Properties {
		var prop map[string]json.RawMessage
		if json.Unmarshal(p, &prop) != nil {
			return "property " + name + " is not a schema object"
		}
		if _, ok := prop["type"]; !ok {
			if _, any := prop["anyOf"]; !any {
				return "property " + name + " declares no type"
			}
		}
	}
	for _, r := range s.Required {
		if _, ok := s.Properties[r]; !ok {
			return "required property " + r + " is not declared"
		}
	}
	return ""
}

// callResult is a tools/call outcome reduced to what a check needs. Text is
// used for matching only and is never written to evidence.
type callResult struct {
	IsError    bool
	Structured map[string]any
	Text       string
}

// errorCode is structuredContent.error.code, or "".
func (r callResult) errorCode() string {
	e, _ := r.Structured["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func (c *mcpClient) call(name string, args map[string]any) (callResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := c.request("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return callResult{}, fmt.Errorf("%s: %w", name, err)
	}
	var res struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent map[string]any `json:"structuredContent"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return callResult{}, fmt.Errorf("%s: result does not parse", name)
	}
	out := callResult{IsError: res.IsError, Structured: res.StructuredContent}
	for _, c := range res.Content {
		if c.Type == "text" {
			out.Text += c.Text
		}
	}
	if out.Structured == nil {
		// Older shape: the JSON is in the text block.
		_ = json.Unmarshal([]byte(out.Text), &out.Structured)
	}
	return out, nil
}

func sortedNames(m map[string]tool) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type agent func(context.Context, agentInput) (string, error)

type agentInput struct {
	DocumentURI  string
	Instructions string
	Text         string
	OnTokenUsage func(tokenUsage)
}

type tokenUsage struct {
	InputTokens       int64 `json:"inputTokens"`
	CachedInputTokens int64 `json:"cachedInputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	TotalTokens       int64 `json:"totalTokens"`
}

type appMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// One App Server process serves all files. Threads are retained by URI for the
// lifetime of this connection, including when a document is closed and reopened.
type codexClient struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	encoder     *json.Encoder
	decoder     *json.Decoder
	nextID      int
	initialized bool
	threads     map[string]string
}

func (c *codexClient) close() {
	if c.cmd != nil {
		c.stdin.Close()
		c.cmd.Process.Kill()
		c.cmd.Wait()
	}
	*c = codexClient{}
}

func (c *codexClient) run(ctx context.Context, input agentInput) (text string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.cmd == nil {
		cmd := exec.Command("codex", "app-server", "--listen", "stdio://")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return "", err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			stdin.Close()
			return "", err
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			stdin.Close()
			stdout.Close()
			return "", fmt.Errorf("start Codex App Server: %w", err)
		}
		c.cmd, c.stdin = cmd, stdin
		c.encoder, c.decoder = json.NewEncoder(stdin), json.NewDecoder(stdout)
	}
	// Cancellation must also unblock a pending read. Reset all thread IDs if the
	// connection is lost; ephemeral threads belong to this App Server process.
	cmd := c.cmd
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { cmd.Process.Kill(); close(stopped) })
	defer func() {
		if !stop() {
			<-stopped
		}
		if ctx.Err() != nil {
			text, err = "", fmt.Errorf("agent: %w", ctx.Err())
		}
		if err != nil {
			c.close()
		}
	}()
	return c.exchange(input)
}

func (c *codexClient) exchange(prompt agentInput) (string, error) {
	var queued []appMessage
	read := func() (appMessage, error) {
		var msg appMessage
		if err := c.decoder.Decode(&msg); err != nil {
			return msg, fmt.Errorf("read Codex App Server: %w", err)
		}
		if msg.Method != "" && len(msg.ID) != 0 {
			return msg, fmt.Errorf("unsupported Codex request: %s", msg.Method)
		}
		return msg, nil
	}
	request := func(method string, params any, result any) error {
		c.nextID++
		if err := c.encoder.Encode(map[string]any{"id": c.nextID, "method": method, "params": params}); err != nil {
			return err
		}
		for {
			msg, err := read()
			if err != nil {
				return err
			}
			if string(msg.ID) != fmt.Sprint(c.nextID) {
				if msg.Method != "" {
					queued = append(queued, msg)
				}
				continue
			}
			if msg.Error != nil {
				return fmt.Errorf("%s: %s", method, msg.Error.Message)
			}
			return json.Unmarshal(msg.Result, result)
		}
	}
	if !c.initialized {
		var ignored any
		if err := request("initialize", map[string]any{"clientInfo": map[string]string{"name": "honnyaku", "version": "0.1.0"}}, &ignored); err != nil {
			return "", err
		}
		if err := c.encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
			return "", err
		}
		c.initialized = true
		c.threads = make(map[string]string)
	}
	threadID := c.threads[prompt.DocumentURI]
	if threadID == "" {
		var started struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := request("thread/start", map[string]any{
			"approvalPolicy": "never", "sandbox": "read-only", "ephemeral": true,
			"baseInstructions": prompt.Instructions,
		}, &started); err != nil {
			return "", err
		}
		if started.Thread.ID == "" {
			return "", fmt.Errorf("Codex returned an empty thread ID")
		}
		threadID = started.Thread.ID
		c.threads[prompt.DocumentURI] = threadID
	}
	var turn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := request("turn/start", map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": prompt.Text}}}, &turn); err != nil {
		return "", err
	}
	if turn.Turn.ID == "" {
		return "", fmt.Errorf("Codex returned an empty turn ID")
	}
	var text string
	for {
		var msg appMessage
		var err error
		if len(queued) > 0 {
			msg, queued = queued[0], queued[1:]
		} else {
			msg, err = read()
		}
		if err != nil {
			return "", err
		}
		var event struct {
			TokenUsage *struct {
				Total tokenUsage `json:"total"`
			} `json:"tokenUsage"`
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			Item     struct {
				Type  string `json:"type"`
				Text  string `json:"text"`
				Phase string `json:"phase"`
			} `json:"item"`
			Turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(msg.Params, &event); err != nil {
			return "", fmt.Errorf("invalid Codex notification: %w", err)
		}
		if event.ThreadID != threadID {
			continue
		}
		if msg.Method == "thread/tokenUsage/updated" && event.TurnID == turn.Turn.ID && event.TokenUsage != nil && prompt.OnTokenUsage != nil {
			prompt.OnTokenUsage(event.TokenUsage.Total)
		}
		if msg.Method == "item/completed" && event.TurnID == turn.Turn.ID && event.Item.Type == "agentMessage" && event.Item.Phase != "commentary" {
			text = event.Item.Text
		}
		if msg.Method == "turn/completed" && event.Turn.ID == turn.Turn.ID {
			if event.Turn.Status != "completed" {
				if event.Turn.Error != nil {
					return "", fmt.Errorf("agent failed: %s", event.Turn.Error.Message)
				}
				return "", fmt.Errorf("agent ended with status %q", event.Turn.Status)
			}
			if strings.TrimSpace(text) == "" {
				return "", fmt.Errorf("Codex returned no agent response")
			}
			return text, nil
		}
	}
}

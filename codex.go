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
	Instructions string
	Text         string
}

type appMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func runAgent(ctx context.Context, input agentInput) (string, error) {
	return runAgentProcess(ctx, exec.CommandContext(ctx, "codex", "app-server", "--listen", "stdio://"), input)
}

// Each agent run owns its process and thread. Closing or timing out a request
// cannot leave a conversation running for the next request.
func runAgentProcess(ctx context.Context, cmd *exec.Cmd, input agentInput) (text string, err error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return "", fmt.Errorf("start Codex App Server: %w", err)
	}
	defer func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
		if ctx.Err() != nil {
			text, err = "", fmt.Errorf("agent: %w", ctx.Err())
		}
	}()
	return exchangeAgent(stdin, stdout, input)
}

func exchangeAgent(input io.Writer, output io.Reader, prompt agentInput) (string, error) {
	encoder, decoder := json.NewEncoder(input), json.NewDecoder(output)
	var nextID int
	var queued []appMessage
	read := func() (appMessage, error) {
		var msg appMessage
		if err := decoder.Decode(&msg); err != nil {
			return msg, fmt.Errorf("read Codex App Server: %w", err)
		}
		if msg.Method != "" && len(msg.ID) != 0 {
			return msg, fmt.Errorf("unsupported Codex request: %s", msg.Method)
		}
		return msg, nil
	}
	request := func(method string, params any, result any) error {
		nextID++
		if err := encoder.Encode(map[string]any{"id": nextID, "method": method, "params": params}); err != nil {
			return err
		}
		for {
			msg, err := read()
			if err != nil {
				return err
			}
			if string(msg.ID) != fmt.Sprint(nextID) {
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
	var ignored any
	if err := request("initialize", map[string]any{"clientInfo": map[string]string{"name": "honnyaku", "version": "0.1.0"}}, &ignored); err != nil {
		return "", err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return "", err
	}
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
	var turn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := request("turn/start", map[string]any{"threadId": started.Thread.ID, "input": []any{map[string]any{"type": "text", "text": prompt.Text}}}, &turn); err != nil {
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
		if event.ThreadID != started.Thread.ID {
			continue
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

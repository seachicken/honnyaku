package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

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

type initializeResult struct {
	Capabilities serverCapabilities `json:"capabilities"`
	ServerInfo   serverInfo         `json:"serverInfo"`
}

type serverCapabilities struct {
	TextDocumentSync textDocumentSyncOptions `json:"textDocumentSync"`
}

type textDocumentSyncOptions struct {
	OpenClose bool        `json:"openClose"`
	Change    int         `json:"change"`
	Save      saveOptions `json:"save"`
}

type saveOptions struct {
	IncludeText bool `json:"includeText"`
}

type initializationParams struct {
	InitializationOptions struct {
		TargetLanguage string `json:"targetLanguage"`
	} `json:"initializationOptions"`
}

type serverInfo struct {
	Name string `json:"name"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func serve(input io.Reader, output io.Writer) (int, error) {
	return serveWithAgent(input, output, runAgent)
}

func serveWithAgent(input io.Reader, output io.Writer, run agent) (int, error) {
	reader := bufio.NewReader(input)
	initialized, shutdown := false, false
	docs := make(map[string]*document)
	targetLanguage := "English"
	for {
		body, err := readMessage(reader)
		if errors.Is(err, io.EOF) {
			return 0, nil
		}
		if err != nil {
			return 1, err
		}
		var req request
		var result any
		var failure *rpcError
		if err := json.Unmarshal(body, &req); err != nil {
			req.ID = json.RawMessage("null")
			failure = &rpcError{-32700, "Parse error"}
		} else if req.JSONRPC != "2.0" || req.Method == "" {
			req.ID = json.RawMessage("null")
			failure = &rpcError{-32600, "Invalid request"}
		} else if req.Method == "exit" {
			if shutdown {
				return 0, nil
			}
			return 1, nil
		} else if len(req.ID) == 0 {
			if initialized && !shutdown {
				if err := handleDocumentNotification(req, docs, targetLanguage, output, run); err != nil {
					return 1, err
				}
			}
			// Notifications never receive a response; save results use a separate notification.
			continue
		} else if shutdown {
			failure = &rpcError{-32600, "Server has shut down"}
		} else if !initialized && req.Method != "initialize" {
			failure = &rpcError{-32002, "Server not initialized"}
		} else {
			switch req.Method {
			case "initialize":
				if initialized {
					failure = &rpcError{-32600, "Server already initialized"}
					break
				}
				var params initializationParams
				if len(req.Params) > 0 {
					if err := json.Unmarshal(req.Params, &params); err != nil {
						failure = &rpcError{-32602, "Invalid initialization parameters"}
						break
					}
				}
				if strings.TrimSpace(params.InitializationOptions.TargetLanguage) != "" {
					targetLanguage = params.InitializationOptions.TargetLanguage
				}
				initialized = true
				result = &initializeResult{
					ServerInfo:   serverInfo{Name: "Honnyaku"},
					Capabilities: serverCapabilities{TextDocumentSync: textDocumentSyncOptions{OpenClose: true, Change: 1, Save: saveOptions{IncludeText: true}}},
				}
			case "shutdown":
				shutdown = true
			default:
				failure = &rpcError{-32601, "Method not found"}
			}
		}
		reply := response{JSONRPC: "2.0", ID: req.ID, Error: failure}
		if failure == nil {
			// A nil result encodes shutdown's required JSON null.
			reply.Result, err = json.Marshal(result)
			if err != nil {
				return 1, err
			}
		}
		body, err = json.Marshal(reply)
		if err != nil {
			return 1, err
		}
		if _, err := fmt.Fprintf(output, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
			return 1, err
		}
	}
}

func readMessage(reader *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && (length >= 0 || len(line) > 0) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if line == "\r\n" {
			break
		}
		name, value, ok := strings.Cut(strings.TrimSuffix(line, "\r\n"), ":")
		if !ok {
			return nil, fmt.Errorf("invalid LSP header")
		}
		if strings.EqualFold(name, "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil || length < 0 || length > 16<<20 {
				return nil, fmt.Errorf("invalid Content-Length (maximum 16 MiB)")
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	body := make([]byte, length)
	_, err := io.ReadFull(reader, body)
	return body, err
}

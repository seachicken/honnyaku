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
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type initializeResult struct {
	Capabilities struct{}   `json:"capabilities"`
	ServerInfo   serverInfo `json:"serverInfo"`
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
	reader := bufio.NewReader(input)
	initialized, shutdown := false, false
	for {
		body, err := readMessage(reader)
		if errors.Is(err, io.EOF) {
			return 0, nil
		}
		if err != nil {
			return 1, err
		}
		var req request
		var result *initializeResult
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
			// Notifications, including initialized, never receive a response.
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
				initialized = true
				result = &initializeResult{
					ServerInfo: serverInfo{Name: "Honnyaku"},
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

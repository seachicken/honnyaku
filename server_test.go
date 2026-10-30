package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func Test_Lifecycle(t *testing.T) {
	var input, output bytes.Buffer
	messages := []string{
		`{"jsonrpc":"2.0","id":"1","method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"shutdown"}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	}
	for _, message := range messages {
		fmt.Fprintf(&input, "Content-Length: %d\r\n\r\n%s", len(message), message)
	}
	code, err := serve(&input, &output)
	if err != nil || code != 0 {
		t.Fatalf("serve = %d, %v", code, err)
	}
	reader := bufio.NewReader(&output)
	for i, want := range []string{
		`{"jsonrpc":"2.0","id":"1","result":{"capabilities":{},"serverInfo":{"name":"Honnyaku"}}}`,
		`{"jsonrpc":"2.0","id":2,"result":null}`,
	} {
		body, err := readMessage(reader)
		if err != nil {
			t.Fatal(err)
		}
		var gotValue, wantValue any
		if err := json.Unmarshal(body, &gotValue); err != nil {
			t.Fatal(err)
		}
		json.Unmarshal([]byte(want), &wantValue)
		gotJSON, _ := json.Marshal(gotValue)
		wantJSON, _ := json.Marshal(wantValue)
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Fatalf("response %d: got %s, want %s", i, gotJSON, wantJSON)
		}
	}
	if reader.Buffered() != 0 || output.Len() != 0 {
		t.Fatal("unexpected response to notification")
	}
}

func Test_ExitWithoutShutdown(t *testing.T) {
	message := `{"jsonrpc":"2.0","method":"exit"}`
	input := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(message), message)
	code, err := serve(bytes.NewBufferString(input), &bytes.Buffer{})
	if code != 1 || err != nil {
		t.Fatalf("serve = %d, %v; want 1, nil", code, err)
	}
}

func Test_Framing(t *testing.T) {
	for _, input := range []string{
		"\r\n", "Content-Length: -1\r\n\r\n", "Content-Length: 4\r\n\r\n{}",
		"Content-Length: 999999999\r\n\r\n",
	} {
		if _, err := readMessage(bufio.NewReader(bytes.NewBufferString(input))); err == nil {
			t.Errorf("expected error for %q", input)
		}
	}
}

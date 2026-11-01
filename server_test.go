package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
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
		`{"jsonrpc":"2.0","id":"1","result":{"capabilities":{"textDocumentSync":{"openClose":true,"change":1,"save":{"includeText":true}}},"serverInfo":{"name":"Honnyaku"}}}`,
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

func Test_SaveTranslationLSP(t *testing.T) {
	var input, output bytes.Buffer
	for _, message := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"initializationOptions":{"targetLanguage":"French"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///a.md","languageId":"markdown","version":1,"text":"unchanged\n\nold"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///a.md","version":2},"contentChanges":[{"text":"unchanged\n\n日本語😀\n\n追加"}]}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"}}}`,
	} {
		fmt.Fprintf(&input, "Content-Length: %d\r\n\r\n%s", len(message), message)
	}
	calls := 0
	code, err := serveWithAgent(&input, &output, func(_ context.Context, input agentInput) (string, error) {
		calls++
		if input.DocumentURI != "file:///a.md" {
			t.Fatalf("unexpected document URI: %q", input.DocumentURI)
		}
		var params translationParams
		if err := json.Unmarshal([]byte(input.Text), &params); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(params, translationParams{Texts: []string{"日本語😀", "追加"}, TargetLanguage: "French"}) {
			t.Fatalf("unexpected translation input: %+v", params)
		}
		return `["translated", "added"]`, nil
	})
	if code != 0 || err != nil || calls != 1 {
		t.Fatalf("code=%d err=%v calls=%d", code, err, calls)
	}
	reader := bufio.NewReader(&output)
	if _, err := readMessage(reader); err != nil {
		t.Fatal(err)
	} // initialize
	body, err := readMessage(reader)
	if err != nil {
		t.Fatal(err)
	}
	var got notification[translationUpdate]
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := notification[translationUpdate]{
		JSONRPC: "2.0",
		Method:  "honnyaku/translations",
		Params: translationUpdate{
			URI: "file:///a.md", Version: 2, TargetLanguage: "French",
			Translations: []unitTranslation{
				{Range: textRange{position{2, 0}, position{2, 5}}, SourceText: "日本語😀", Text: "translated"},
				{Range: textRange{position{4, 0}, position{4, 2}}, SourceText: "追加", Text: "added"},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := readMessage(reader); err != io.EOF {
		t.Fatalf("unexpected extra message: %v", err)
	}
}

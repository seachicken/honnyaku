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

func TestChangedParagraphs(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		want                []string
	}{
		{"unchanged", "first\n\nsecond", "first\n\nsecond", nil},
		{"edit", "first\n\nsecond", "first edited\n\nsecond", []string{"first edited"}},
		{"separate edits", "a\n\nb\n\nc", "A\n\nb\n\nC", []string{"A", "C"}},
		{"insert", "a\n\nb", "new\n\na\n\nb", []string{"new"}},
		{"delete", "a\n\nb", "b", nil},
		{"duplicate", "a", "a\n\na", []string{"a"}},
		{"merge", "a\n\nb", "a\nb", []string{"a\nb"}},
		{"split", "a\nb", "a\n\nb", []string{"a", "b"}},
		{"line endings", "a\r\n\r\nb", "a\n\nb", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, p := range changedParagraphs(tc.before, tc.after) {
				got = append(got, p.Text)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	p := paragraphs("\r\n日本語\r\n😀!\r\n\r\nnext")[0]
	if p.Range != (textRange{position{1, 0}, position{2, 3}}) {
		t.Fatalf("UTF-16 range: %+v", p.Range)
	}
}

func TestSaveTranslationLSP(t *testing.T) {
	var input, output bytes.Buffer
	messages := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"initializationOptions":{"targetLanguage":"French"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///a.md","languageId":"markdown","version":1,"text":"unchanged\n\nold"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///a.md","version":2},"contentChanges":[{"text":"unchanged\n\n日本語😀"}]}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"},"text":"unchanged"}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didClose","params":{"textDocument":{"uri":"file:///a.md"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"},"text":"closed"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"shutdown"}`,
		`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"},"text":"after shutdown"}}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	}
	for _, m := range messages {
		fmt.Fprintf(&input, "Content-Length: %d\r\n\r\n%s", len(m), m)
	}
	calls := 0
	code, err := serveWithAgent(&input, &output, func(_ context.Context, input agentInput) (string, error) {
		var p translationParams
		if err := json.Unmarshal([]byte(input.Text), &p); err != nil {
			t.Fatal(err)
		}
		calls++
		if p.Text != "日本語😀" || p.TargetLanguage != "French" {
			t.Errorf("unexpected translation: %+v", p)
		}
		return "translated", nil
	})
	if code != 0 || err != nil || calls != 1 {
		t.Fatalf("code=%d err=%v calls=%d", code, err, calls)
	}
	reader := bufio.NewReader(&output)
	readMessage(reader) // initialize
	body, err := readMessage(reader)
	if err != nil {
		t.Fatal(err)
	}
	var update notification[translationUpdate]
	if err = json.Unmarshal(body, &update); err != nil {
		t.Fatal(err)
	}
	want := paragraphTranslation{Range: textRange{position{2, 0}, position{2, 5}}, SourceText: "日本語😀", Text: "translated"}
	if update.Method != "honnyaku/translations" || update.Params.URI != "file:///a.md" || update.Params.Version != 2 || update.Params.TargetLanguage != "French" || !reflect.DeepEqual(update.Params.Translations, []paragraphTranslation{want}) {
		t.Fatalf("%s", body)
	}
	body, err = readMessage(reader)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(body, &update)
	if len(update.Params.Translations) != 0 {
		t.Fatalf("deletion must not translate: %s", body)
	}
	readMessage(reader) // shutdown
	if _, err = readMessage(reader); err != io.EOF {
		t.Fatalf("unexpected extra message: %v", err)
	}
}

func TestSaveRetryAndDocumentIsolation(t *testing.T) {
	docs := map[string]*document{}
	var output bytes.Buffer
	fail := true
	calls := 0
	run := func(_ context.Context, input agentInput) (string, error) {
		var p translationParams
		if err := json.Unmarshal([]byte(input.Text), &p); err != nil {
			t.Fatal(err)
		}
		calls++
		if fail {
			return "", fmt.Errorf("unavailable")
		}
		return "translated", nil
	}
	send := func(method, params string) {
		t.Helper()
		err := handleDocumentNotification(request{Method: method, Params: json.RawMessage(params)}, docs, "English", &output, run)
		if err != nil {
			t.Fatal(err)
		}
	}
	send("textDocument/didOpen", `{"textDocument":{"uri":"a","languageId":"markdown","version":1,"text":"original"}}`)
	send("textDocument/didOpen", `{"textDocument":{"uri":"b","languageId":"markdown","version":1,"text":"other"}}`)
	send("textDocument/didOpen", `{"textDocument":{"uri":"code","languageId":"go","text":"code"}}`)
	send("textDocument/didSave", `{"textDocument":{"uri":"code"},"text":"edited"}`)
	send("textDocument/didChange", `{"textDocument":{"uri":"a","version":2},"contentChanges":[{"text":"changed"}]}`)
	send("textDocument/didChange", `{"textDocument":{"uri":"a","version":1},"contentChanges":[{"text":"stale"}]}`)
	send("textDocument/didSave", `{"textDocument":{"uri":"a"}}`)
	if docs["a"].baseline != "original" || docs["b"].text != "other" || calls != 1 {
		t.Fatalf("docs=%+v calls=%d", docs, calls)
	}
	body, err := readMessage(bufio.NewReader(&output))
	if err != nil {
		t.Fatal(err)
	}
	var update notification[translationUpdate]
	json.Unmarshal(body, &update)
	if update.Params.Translations[0].Error != "unavailable" {
		t.Fatalf("%s", body)
	}
	fail = false
	send("textDocument/didSave", `{"textDocument":{"uri":"a"}}`)
	send("textDocument/didSave", `{"textDocument":{"uri":"a"}}`)
	if calls != 2 || docs["a"].baseline != "changed" {
		t.Fatalf("calls=%d baseline=%s", calls, docs["a"].baseline)
	}
}

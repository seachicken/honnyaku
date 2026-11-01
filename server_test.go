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
	code, err := serveWithAgent(&input, &output, func(context.Context, agentInput) (string, error) { t.Fatal("unexpected inference"); return "", nil })
	if err != nil || code != 0 {
		t.Fatalf("serve = %d, %v", code, err)
	}
	reader := bufio.NewReader(&output)
	for i, want := range []string{
		`{"jsonrpc":"2.0","id":"1","result":{"capabilities":{"experimental":{"honnyakuModelSelection":true,"honnyakuModelCatalog":true},"textDocumentSync":{"openClose":true,"change":1,"save":{"includeText":true}}},"serverInfo":{"name":"Honnyaku"}}}`,
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
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"initializationOptions":{"targetLanguage":"French","model":"  test-model  "}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///a.md","languageId":"markdown","version":1,"text":"unchanged\n\nold"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///a.md","version":2},"contentChanges":[{"text":"unchanged\n\n日本語😀\n\n追加"}]}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"}}}`,
	} {
		fmt.Fprintf(&input, "Content-Length: %d\r\n\r\n%s", len(message), message)
	}
	calls := 0
	code, err := serveWithAgent(&input, &output, func(_ context.Context, input agentInput) (string, error) {
		calls++
		effort := "medium"
		input.OnReasoningEffort(&effort)
		if input.Model != "test-model" {
			t.Fatalf("unexpected model: %q", input.Model)
		}
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
		return `{"translations":["translated", "added"]}`, nil
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
	var effortNotification notification[reasoningEffortUpdate]
	if err := json.Unmarshal(body, &effortNotification); err != nil {
		t.Fatal(err)
	}
	if effortNotification.Method != "honnyaku/reasoningEffort" || effortNotification.Params.URI != "file:///a.md" || effortNotification.Params.ReasoningEffort == nil || *effortNotification.Params.ReasoningEffort != "medium" {
		t.Fatalf("unexpected effort notification: %s", body)
	}
	body, err = readMessage(reader)
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

func Test_ModelCatalogSelectionAndRecovery(t *testing.T) {
	low := []reasoningEffortOption{{ReasoningEffort: "low"}}
	for _, tt := range []struct {
		name     string
		selected string
		models   []codexModel
		failures int
		want     string
	}{
		{"default", "", []codexModel{{Model: "fallback", SupportedReasoningEfforts: low}, {Model: "default", IsDefault: true, SupportedReasoningEfforts: low}}, 0, "default"},
		{"eligible fallback", "", []codexModel{{Model: "unsupported", IsDefault: true}, {Model: "fallback", SupportedReasoningEfforts: low}}, 0, "fallback"},
		{"explicit recovery", "chosen", []codexModel{{Model: "chosen", SupportedReasoningEfforts: low}}, 2, "chosen"},
		{"omitted recovery", "", []codexModel{{Model: "default", IsDefault: true, SupportedReasoningEfforts: low}}, 2, "default"},
		{"invalid explicit", "missing", []codexModel{{Model: "default", IsDefault: true, SupportedReasoningEfforts: low}}, 0, ""},
		{"no eligible model", "", []codexModel{{Model: "unsupported", IsDefault: true}}, 0, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var input, output bytes.Buffer
			messages := []string{
				fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"initializationOptions":{"model":%q}}}`, tt.selected),
				`{"jsonrpc":"2.0","method":"initialized"}`,
				`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///a.md","languageId":"markdown","version":1,"text":"old"}}}`,
				`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///a.md","version":2},"contentChanges":[{"text":"new"}]}}`,
			}
			// A failed save must preserve the baseline so the next save can recover.
			for i := 0; i < 3; i++ {
				messages = append(messages, `{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"}}}`)
			}
			for _, message := range messages {
				fmt.Fprintf(&input, "Content-Length: %d\r\n\r\n%s", len(message), message)
			}
			catalogCalls, runs := 0, 0
			code, err := serveWithAgent(&input, &output, func(_ context.Context, input agentInput) (string, error) {
				runs++
				if input.Model != tt.want || tt.want == "" {
					t.Fatalf("unexpected selected model %q", input.Model)
				}
				return `{"translations":["translated"]}`, nil
			}, func(context.Context) ([]codexModel, error) {
				catalogCalls++
				if catalogCalls <= tt.failures {
					return nil, fmt.Errorf("temporary catalog failure")
				}
				return tt.models, nil
			})
			wantRuns := 1
			if tt.want == "" {
				wantRuns = 0
			}
			if code != 0 || err != nil || runs != wantRuns || catalogCalls != tt.failures+1 {
				t.Fatalf("code=%d err=%v runs=%d catalogCalls=%d", code, err, runs, catalogCalls)
			}
		})
	}
}

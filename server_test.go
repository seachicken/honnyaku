package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
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
		if !reflect.DeepEqual(params, translationParams{Texts: []string{"unchanged", "日本語😀", "追加"}, TargetLanguage: "French"}) {
			t.Fatalf("unexpected translation input: %+v", params)
		}
		return `{"translations":["same", "translated", "added"]}`, nil
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
	var pending notification[translationUpdate]
	if err := json.Unmarshal(body, &pending); err != nil || !pending.Params.Pending || len(pending.Params.Translations) != 3 {
		t.Fatalf("missing progress: %s", body)
	}
	body, err = readMessage(reader)
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
				{Range: textRange{position{0, 0}, position{0, 9}}, SourceText: "unchanged", Text: "same"},
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
			// A failed save must not cache a failure; the next save must retry.
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

func Test_AppendLineSendsOnlyAddedLineInSameConversation(t *testing.T) {
	var input, output bytes.Buffer
	for _, message := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///a.md","languageId":"markdown","version":1,"text":"元の行"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///a.md","version":2},"contentChanges":[{"text":"翻訳する行"}]}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///a.md","version":3},"contentChanges":[{"text":"翻訳する行\n追加の行"}]}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///a.md","version":4},"contentChanges":[{"text":"翻訳する行\n\n追加の行\n"}]}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"}}}`,
	} {
		fmt.Fprintf(&input, "Content-Length: %d\r\n\r\n%s", len(message), message)
	}
	var calls []translationParams
	code, err := serveWithAgent(&input, &output, func(_ context.Context, request agentInput) (string, error) {
		var params translationParams
		if err := json.Unmarshal([]byte(request.Text), &params); err != nil {
			t.Fatal(err)
		}
		if request.DocumentURI != "file:///a.md" {
			t.Fatalf("lost file conversation: %q", request.DocumentURI)
		}
		calls = append(calls, params)
		return `{"translations":["translated"]}`, nil
	})
	if err != nil || code != 0 {
		t.Fatalf("serve = %d, %v", code, err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	if !reflect.DeepEqual(calls[0].Texts, []string{"翻訳する行"}) || !reflect.DeepEqual(calls[1].Texts, []string{"追加の行"}) {
		t.Fatalf("unchanged lines were retranslated: %+v", calls)
	}
	encoded, err := json.Marshal(calls[1])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields["texts"] == nil || fields["targetLanguage"] == nil {
		t.Fatalf("unexpected prompt fields: %s", encoded)
	}
}

func runCacheSession(t *testing.T, cache *translationCache, run agent, messages ...string) []translationUpdate {
	t.Helper()
	var input, output bytes.Buffer
	for _, message := range messages {
		fmt.Fprintf(&input, "Content-Length: %d\r\n\r\n%s", len(message), message)
	}
	code, err := serveWithCache(&input, &output, run, cache)
	if err != nil || code != 0 {
		t.Fatalf("serve = %d, %v", code, err)
	}
	var updates []translationUpdate
	reader := bufio.NewReader(&output)
	for {
		body, err := readMessage(reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var result notification[translationUpdate]
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		if result.Method == "honnyaku/translations" {
			updates = append(updates, result.Params)
		}
	}
	return updates
}

const initializeEnglish = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
const openUnchanged = `{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///a.md","languageId":"markdown","version":1,"text":"already\nuntouched"}}}`
const saveUnchanged = `{"jsonrpc":"2.0","method":"textDocument/didSave","params":{"textDocument":{"uri":"file:///a.md"}}}`

func Test_StandardSavesTranslateMissingAndReuseAllExistingLines(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, request agentInput) (string, error) {
		if request.DocumentURI != "file:///a.md" {
			t.Fatal("file conversation lost")
		}
		var params translationParams
		if err := json.Unmarshal([]byte(request.Text), &params); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, params.Texts)
		var translations []string
		for _, text := range params.Texts {
			translations = append(translations, strings.ToUpper(text))
		}
		data, _ := json.Marshal(map[string]any{"translations": translations})
		return string(data), nil
	}
	updates := runCacheSession(t, &translationCache{}, run, initializeEnglish, openUnchanged,
		saveUnchanged, saveUnchanged,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///a.md","version":2},"contentChanges":[{"text":"already\nuntouched\nadded"}]}}`,
		saveUnchanged)
	if !reflect.DeepEqual(calls, [][]string{{"already", "untouched"}, {"added"}}) {
		t.Fatalf("unexpected inference: %q", calls)
	}
	if !updates[0].Pending || len(updates[0].Translations) != 2 {
		t.Fatal("missing initial progress")
	}
	if !updates[3].Pending || len(updates[3].Translations) != 1 || updates[3].Translations[0].SourceText != "added" {
		t.Fatal("cached lines included in progress")
	}
	last := updates[len(updates)-1]
	if last.Pending || len(last.Translations) != 3 {
		t.Fatalf("missing cached results: %+v", last)
	}
}

func Test_ClientRestoresResultsIntoNewServerWithoutInferenceOnOpen(t *testing.T) {
	calls := 0
	saved := runCacheSession(t, &translationCache{}, func(context.Context, agentInput) (string, error) {
		calls++
		return `{"translations":["already", ""]}`, nil
	}, initializeEnglish, openUnchanged, saveUnchanged)
	if calls != 1 {
		t.Fatalf("runs = %d", calls)
	}
	initialParams, err := json.Marshal(map[string]any{"initializationOptions": map[string]any{"initialTranslations": saved}})
	if err != nil {
		t.Fatal(err)
	}
	initialized, err := json.Marshal(request{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "initialize", Params: initialParams})
	if err != nil {
		t.Fatal(err)
	}
	updates := runCacheSession(t, &translationCache{}, func(context.Context, agentInput) (string, error) {
		t.Fatal("client-supplied cached results must not invoke inference")
		return "", nil
	}, string(initialized), openUnchanged, saveUnchanged)
	if len(updates) != 2 || !updates[0].Restored || len(updates[0].Translations) != 2 {
		t.Fatalf("restore = %+v", updates)
	}
	if updates[0].Translations[1].Text != "" {
		t.Fatal("empty translation lost")
	}
	// With no client-supplied state, another process has no saved results to read.
	calls = 0
	runCacheSession(t, &translationCache{}, func(context.Context, agentInput) (string, error) {
		calls++
		return `{"translations":["already", ""]}`, nil
	}, initializeEnglish, openUnchanged, saveUnchanged)
	if calls != 1 {
		t.Fatal("server cache unexpectedly shared across process lifetimes")
	}
}

func Test_OnlySuccessfulTranslationsAreCachedAndFailuresRetryOnSave(t *testing.T) {
	calls := 0
	updates := runCacheSession(t, &translationCache{}, func(context.Context, agentInput) (string, error) {
		calls++
		if calls == 1 {
			return "", fmt.Errorf("temporary failure")
		}
		return `{"translations":["one", "two"]}`, nil
	}, initializeEnglish, openUnchanged, saveUnchanged, saveUnchanged, saveUnchanged)
	if calls != 2 {
		t.Fatalf("runs = %d", calls)
	}
	if updates[1].Translations[0].Error == "" || updates[len(updates)-1].Translations[0].Error != "" {
		t.Fatal("failure retry failed")
	}
}

func Test_LegacyInitializationImportsTranslationsWithoutOverwritingNewerCache(t *testing.T) {
	cache := &translationCache{}
	cache.remember("file:///a.md", "English", "already", "newer")
	updates := runCacheSession(t, cache, func(context.Context, agentInput) (string, error) {
		t.Fatal("migrated results must not invoke the agent")
		return "", nil
	}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"initializationOptions":{"initialTranslations":[{"uri":"file:/a.md","targetLanguage":"English","translations":[{"sourceText":"already","text":"old"},{"sourceText":"untouched","text":"migrated"}]}]}}}`,
		openUnchanged, saveUnchanged)
	if len(updates) != 2 || updates[0].Translations[0].Text != "newer" || updates[0].Translations[1].Text != "migrated" {
		t.Fatalf("migration = %+v", updates)
	}

}

func Test_CacheIsSeparatedByFileAndTargetLanguage(t *testing.T) {
	cache := &translationCache{}
	cache.remember("file:///a.md", "English", "source", "translation")
	if _, missing := cache.selectUnits("file:/a.md", "English", "source"); len(missing) != 0 {
		t.Fatal("equivalent URI not reused")
	}
	for _, tc := range []struct{ uri, language string }{{"file:///b.md", "English"}, {"file:///a.md", "French"}} {
		if _, missing := cache.selectUnits(tc.uri, tc.language, "source"); len(missing) != 1 {
			t.Fatal("cache leaked between file or language")
		}
	}
}

func Test_LatestLegacyResultWinsAndFailedOrPendingResultsDoNotBecomeCached(t *testing.T) {
	cache := &translationCache{}
	updates := runCacheSession(t, cache, func(context.Context, agentInput) (string, error) {
		t.Fatal("initialization and open must not run inference")
		return "", nil
	}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"initializationOptions":{"initialTranslations":[{"uri":"file:///a.md","targetLanguage":"English","translations":[{"sourceText":"already","text":"old"}]},{"uri":"file:///a.md","targetLanguage":"English","translations":[{"sourceText":"already","text":"latest"},{"sourceText":"untouched","error":"failed"}]},{"uri":"file:///a.md","targetLanguage":"English","pending":true,"translations":[{"sourceText":"untouched","text":""}]}]}}}`,
		openUnchanged)
	if len(updates) != 1 || len(updates[0].Translations) != 1 || updates[0].Translations[0].Text != "latest" {
		t.Fatalf("migration = %+v", updates)
	}
	_, missing := cache.selectUnits("file:///a.md", "English", "already\nuntouched")
	if len(missing) != 1 || missing[0].Text != "untouched" {
		t.Fatalf("failed or pending result cached: %+v", missing)
	}
}

func Test_LegacyParagraphCacheKeepsCoverageWithoutRetranslatingIndividualLines(t *testing.T) {
	cache := &translationCache{}
	cache.importTranslation("file:///a.md", "English", "first\nsecond", "paragraph")
	cached, missing := cache.selectUnits("file:///a.md", "English", "first\nsecond\nthird")
	if len(cached) != 1 || cached[0].Text != "paragraph" || len(missing) != 1 || missing[0].Text != "third" {
		t.Fatalf("cached=%+v missing=%+v", cached, missing)
	}
}

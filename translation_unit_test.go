package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func Test_MissingTranslationUnits(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		want                []string
	}{
		{"unchanged", "first\n\nsecond", "first\n\nsecond", nil},
		{"edit", "first\n\nsecond", "first edited\n\nsecond", []string{"first edited"}},
		{"separate edits", "a\n\nb\n\nc", "A\n\nb\n\nC", []string{"A", "C"}},
		{"insert", "a\n\nb", "new\n\na\n\nb", []string{"new"}},
		{"delete", "a\n\nb", "b", nil},
		{"duplicate", "a", "a\n\na", nil},
		{"merge", "a\n\nb", "a\nb", nil},
		{"split", "a\nb", "a\n\nb", nil},
		{"append line", "既訳", "既訳\n新規行", []string{"新規行"}},
		{"insert line", "既訳一\n既訳二", "既訳一\n追加\n既訳二", []string{"追加"}},
		{"edit line", "既訳一\n既訳二", "既訳一\n変更", []string{"変更"}},
		{"blank line", "既訳", "既訳\n", nil},
		{"line endings", "a\r\n\r\nb", "a\n\nb", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := &translationCache{}
			for _, unit := range translationUnits(tc.before) {
				cache.remember("file:///a.md", "English", unit.Text, "cached")
			}
			_, missing := cache.selectUnits("file:///a.md", "English", tc.after)
			var got []string
			for _, p := range missing {
				got = append(got, p.Text)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	p := translationUnits("\r\n日本語\r\n😀!\r\n\r\nnext")[0]
	if p.Range != (textRange{position{1, 0}, position{1, 3}}) {
		t.Fatalf("UTF-16 range: %+v", p.Range)
	}
}

func Test_RangedTranslationUnits(t *testing.T) {
	source := "😀 code // hello\n/* first\nsecond */ code"
	ranges := []textRange{{position{0, 8}, position{0, 16}}, {position{1, 0}, position{2, 9}}}
	if err := validateTranslationRanges(source, ranges); err != nil {
		t.Fatal(err)
	}
	units := rangedTranslationUnits(source, ranges)
	if len(units) != 3 || units[0].Text != "// hello" || units[0].Range.Start.Character != 8 || units[2].Text != "second */" {
		t.Fatalf("units = %+v", units)
	}
	if len(rangedTranslationUnits(source, []textRange{})) != 0 {
		t.Fatal("empty must select nothing")
	}
	for _, r := range []textRange{{position{0, 1}, position{0, 2}}, {position{0, 0}, position{5, 0}}, {position{1, 2}, position{0, 2}}} {
		if validateTranslationRanges(source, []textRange{r}) == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
}

func Test_RangedTranslationUnitsCRLF(t *testing.T) {
	source := "a\r\n😀b\r\nc"
	ranges := []textRange{{position{0, 0}, position{2, 1}}}
	if err := validateTranslationRanges(source, ranges); err != nil {
		t.Fatal(err)
	}
	want := []translationUnit{
		{Range: textRange{position{0, 0}, position{0, 1}}, Text: "a"},
		{Range: textRange{position{1, 0}, position{1, 3}}, Text: "😀b"},
		{Range: textRange{position{2, 0}, position{2, 1}}, Text: "c"},
	}
	got := rangedTranslationUnits(source, ranges)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("units = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(got, translationUnits(source)) {
		t.Fatal("ranged units differ from whole-document units")
	}
	for _, p := range []position{{0, 2}, {1, 4}, {1, 1}} {
		if validateTranslationRanges(source, []textRange{{p, p}}) == nil {
			t.Fatalf("accepted invalid position %+v", p)
		}
	}
}

func Test_RangeProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, selection string
		want            []string
		invalid         bool
	}{
		{"omitted", "", []string{"code // hello"}, false},
		{"empty", `,"ranges":[]`, nil, false},
		{"comment", `,"ranges":[{"start":{"line":0,"character":5},"end":{"line":0,"character":13}}]`, []string{"// hello"}, false},
		{"invalid", `,"ranges":[{"start":{"line":0,"character":5},"end":{"line":0,"character":99}}]`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var input, output bytes.Buffer
			messages := []string{
				`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
				`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///a.go","languageId":"go","version":1,"text":"code // hello"}}}`,
				`{"jsonrpc":"2.0","id":2,"method":"honnyaku/translateVisible","params":{"textDocument":{"uri":"file:///a.go","version":1,"text":"code // hello"},"startLine":0,"endLine":1` + tc.selection + `}}`,
			}
			for _, m := range messages {
				fmt.Fprintf(&input, "Content-Length: %d\r\n\r\n%s", len(m), m)
			}
			var got []string
			cache := &translationCache{}
			if tc.name == "comment" || tc.name == "empty" {
				cache.remember("file:///a.go", "English", "code // hello", "cached whole line")
			}
			code, err := serveWithCache(&input, &output, func(_ context.Context, in agentInput) (string, error) {
				var params translationParams
				if err := json.Unmarshal([]byte(in.Text), &params); err != nil {
					t.Fatal(err)
				}
				for _, line := range params.Lines {
					got = append(got, line.Text)
				}
				data, err := json.Marshal(map[string]any{"lines": params.Lines})
				if err != nil {
					t.Fatal(err)
				}
				return string(data), nil
			}, cache)
			if code != 0 || err != nil {
				t.Fatalf("serve = %d, %v", code, err)
			}
			if (tc.name == "comment" || tc.name == "empty") && strings.Contains(output.String(), "cached whole line") {
				t.Fatal("cached text outside requested ranges leaked")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("translated %v, want %v", got, tc.want)
			}
			if strings.Contains(output.String(), `"code":-32602`) != tc.invalid {
				t.Fatalf("responses: %s", output.String())
			}
		})
	}
}

func Test_NumberedTranslationsMapByPosition(t *testing.T) {
	params := translationParams{TargetLanguage: "ja", Lines: []translationLine{
		{Line: 2, Text: "The CLI snapshots Git configuration once during branch discovery instead of"},
		{Line: 3, Text: "starting git config for each remote and branch setting."},
		{Line: 8, Text: "## Results (2026-11-13)"},
		{Line: 8, Character: 30, Text: "comment"},
	}}
	got, err := translate(context.Background(), params, nil, func(_ context.Context, in agentInput) (string, error) {
		var received translationParams
		if err := json.Unmarshal([]byte(in.Text), &received); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(received, params) {
			t.Fatalf("input = %+v", received)
		}
		if in.OnTextDelta != nil {
			t.Fatal("unvalidated streaming enabled")
		}
		return `{"lines":[{"line":8,"character":30,"text":"コメント"},{"line":8,"character":0,"text":"## 結果（2026-11-13）"},{"line":3,"character":0,"text":"設定ごとの起動を省きます。"},{"line":2,"character":0,"text":"CLI は設定を一度取得し、"}]}`, nil
	})
	want := []string{"CLI は設定を一度取得し、", "設定ごとの起動を省きます。", "## 結果（2026-11-13）", "コメント"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, %v", got, err)
	}
}

func Test_InvalidNumberedTranslationsRetryThenFail(t *testing.T) {
	for _, output := range []string{
		`{"lines":[]}`,
		`{"lines":[{"line":2,"character":0,"text":"a"},{"line":2,"character":0,"text":"b"}]}`,
		`{"lines":[{"line":2,"character":0,"text":"a"},{"line":9,"character":0,"text":"b"}]}`,
		`{"lines":[{"line":2,"character":0,"text":"a\nb"},{"line":3,"character":0,"text":"c"}]}`,
		`{"lines":[{"line":2,"character":0,"text":"a\rb"},{"line":3,"character":0,"text":"c"}]}`,
		`{"lines":[{"line":2,"character":0,"text":" "},{"line":3,"character":0,"text":"b"}]}`,
		`{"lines":[{"line":2,"text":"a"},{"line":3,"character":0,"text":"b"}]}`,
		`{"lines":[{"line":2,"character":0,"text":"a"},{"line":3,"character":0,"text":"b"}]} {}`,
	} {
		t.Run(output, func(t *testing.T) {
			calls := 0
			got, err := translate(context.Background(), translationParams{Lines: []translationLine{{Line: 2, Text: "first"}, {Line: 3, Text: "second"}}}, nil, func(_ context.Context, in agentInput) (string, error) {
				calls++
				if calls == 2 && !strings.Contains(in.Instructions, "previous response was invalid") {
					t.Fatal("retry lacks error")
				}
				return output, nil
			})
			if err == nil || got != nil || calls != 2 {
				t.Fatalf("got %q, %v; calls=%d", got, err, calls)
			}
		})
	}
}

func Test_AgentErrorsDoNotRetry(t *testing.T) {
	calls := 0
	_, err := translate(context.Background(), translationParams{}, nil, func(context.Context, agentInput) (string, error) { calls++; return "", fmt.Errorf("agent failure") })
	if err == nil || calls != 1 {
		t.Fatal("agent errors must not retry")
	}
}

func translationReply(t *testing.T, input agentInput, texts []string) string {
	t.Helper()
	var params translationParams
	if err := json.Unmarshal([]byte(input.Text), &params); err != nil {
		t.Fatal(err)
	}
	lines := params.Lines
	if len(texts) != len(lines) {
		t.Fatalf("reply has %d texts for %d input lines", len(texts), len(lines))
	}
	for i := range lines {
		lines[i].Text = texts[i]
	}
	data, err := json.Marshal(map[string]any{"lines": lines})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func Test_NumberedLinesStreamBeforeAgentCompletion(t *testing.T) {
	params := translationParams{Lines: []translationLine{{Line: 2, Text: "first"}, {Line: 8, Character: 3, Text: "second"}}}
	want := []translationLine{{Line: 8, Character: 3, Text: "引用\"と\\日本語😀"}, {Line: 2, Text: "最初"}}
	output, err := json.Marshal(map[string]any{"lines": want})
	if err != nil {
		t.Fatal(err)
	}
	var progress [][]translationLine
	got, err := translate(context.Background(), params, func(lines []translationLine) {
		progress = append(progress, append([]translationLine(nil), lines...))
	}, func(_ context.Context, in agentInput) (string, error) {
		// Byte-sized chunks split UTF-8 characters, escape sequences, and JSON objects.
		for _, b := range output {
			in.OnTextDelta(string([]byte{b}))
		}
		if len(progress) != 2 || !reflect.DeepEqual(progress[0], want[:1]) || !reflect.DeepEqual(progress[1], want) {
			t.Fatalf("progress before agent completion = %+v", progress)
		}
		return string(output), nil
	})
	if err != nil || !reflect.DeepEqual(got, []string{want[1].Text, want[0].Text}) {
		t.Fatalf("got %q, %v", got, err)
	}
}

func Test_InvalidStreamedLinesAreNotPublished(t *testing.T) {
	for _, invalid := range []string{
		`{"line":99,"character":0,"text":"unknown"}`,
		`{"line":2,"character":0,"text":"duplicate"}`,
		`{"line":3,"character":0,"text":"two\nlines"}`,
		`{"line":3,"text":"missing column"}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			var progress []translationLine
			_, err := translate(context.Background(), translationParams{Lines: []translationLine{{Line: 2, Text: "first"}, {Line: 3, Text: "second"}}}, func(lines []translationLine) { progress = append([]translationLine(nil), lines...) }, func(_ context.Context, in agentInput) (string, error) {
				in.OnTextDelta(`{"lines":[{"line":2,"character":0,"text":"valid"},` + invalid + `,{"line":3,"character":0,"text":"later"}]}`)
				if !reflect.DeepEqual(progress, []translationLine{{Line: 2, Text: "valid"}}) {
					t.Fatalf("invalid result published: %+v", progress)
				}
				return "", context.DeadlineExceeded
			})
			if err == nil {
				t.Fatal("expected failure")
			}
		})
	}
}

func Test_RetryClearsPreviousStreamedLines(t *testing.T) {
	params := translationParams{Lines: []translationLine{{Line: 2, Text: "first"}, {Line: 3, Text: "second"}}}
	var progress []translationLine
	resets, calls := 0, 0
	got, err := translate(context.Background(), params, func(lines []translationLine) {
		if len(lines) == 0 {
			resets++
		}
		progress = append([]translationLine(nil), lines...)
	}, func(_ context.Context, in agentInput) (string, error) {
		calls++
		if calls == 1 {
			partial := `{"lines":[{"line":2,"character":0,"text":"old"}]}`
			in.OnTextDelta(partial)
			return partial, nil
		}
		if resets != 1 || len(progress) != 0 {
			t.Fatalf("retry retained stale progress: %+v", progress)
		}
		result := translationReply(t, in, []string{"new first", "new second"})
		in.OnTextDelta(result)
		return result, nil
	})
	if err != nil || calls != 2 || !reflect.DeepEqual(got, []string{"new first", "new second"}) {
		t.Fatalf("got %q, %v (%d calls)", got, err, calls)
	}
}

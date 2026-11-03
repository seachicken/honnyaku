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

func TestTranslatePreservesLicenseLineCount(t *testing.T) {
	// A wrapped license paragraph must remain one output per supplied source line.
	texts := []string{"Permission is hereby granted, free of charge, to any person obtaining a copy", "of this software and associated documentation files (the Software), to deal"}
	want := []string{"許可を取得する者に対し、無償で許諾します。", "本ソフトウェアおよび関連文書について、"}
	got, err := translate(context.Background(), translationParams{Texts: texts, TargetLanguage: "Japanese"}, func(_ context.Context, input agentInput) (string, error) {
		schema := input.OutputSchema["properties"].(map[string]any)["translations"].(map[string]any)
		if schema["minItems"] != len(texts) || schema["maxItems"] != len(texts) {
			t.Fatalf("output length is not constrained: %+v", schema)
		}
		if !strings.Contains(input.Instructions, "Never merge") {
			t.Fatal("missing fragment instruction")
		}
		output, _ := json.Marshal(map[string]any{"translations": want})
		return string(output), nil
	})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("translate = %v, %v", got, err)
	}
}

func TestTranslateRejectsMismatchedCount(t *testing.T) {
	_, err := translate(context.Background(), translationParams{Texts: []string{"first", "second"}}, func(context.Context, agentInput) (string, error) {
		return `{"translations":["merged"]}`, nil
	})
	if err == nil || !strings.Contains(err.Error(), "expected 2 translations, got 1") {
		t.Fatalf("error = %v", err)
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
				got = append(got, params.Texts...)
				data, _ := json.Marshal(map[string]any{"translations": params.Texts})
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

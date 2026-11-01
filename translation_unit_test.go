package main

import (
	"reflect"
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

package main

import (
	"reflect"
	"testing"
)

func Test_ChangedTranslationUnits(t *testing.T) {
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
			for _, p := range changedTranslationUnits(tc.before, tc.after) {
				got = append(got, p.Text)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	p := translationUnits("\r\n日本語\r\n😀!\r\n\r\nnext")[0]
	if p.Range != (textRange{position{1, 0}, position{2, 3}}) {
		t.Fatalf("UTF-16 range: %+v", p.Range)
	}
}

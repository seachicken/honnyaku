package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf16"
)

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type textRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}
type paragraph struct {
	Range textRange `json:"range"`
	Text  string    `json:"text"`
}
type paragraphTranslation struct {
	Range      textRange `json:"range"`
	SourceText string    `json:"sourceText"`
	Text       string    `json:"text,omitempty"`
	Error      string    `json:"error,omitempty"`
}
type translationUpdate struct {
	URI            string                 `json:"uri"`
	Version        int                    `json:"version"`
	TargetLanguage string                 `json:"targetLanguage"`
	Translations   []paragraphTranslation `json:"translations"`
}
type notification[P any] struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  P      `json:"params"`
}
type document struct {
	text     string
	baseline string
	version  int
}
type documentParams struct {
	TextDocument struct {
		URI        string `json:"uri"`
		LanguageID string `json:"languageId"`
		Version    int    `json:"version"`
		Text       string `json:"text"`
	} `json:"textDocument"`
	ContentChanges []struct {
		Range *textRange `json:"range"`
		Text  string     `json:"text"`
	} `json:"contentChanges"`
	Text *string `json:"text"`
}

func writeMessage(output io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

// Paragraphs are blank-line-delimited blocks; this deliberately does not parse
// Markdown syntax. LSP positions count UTF-16 code units, not UTF-8 bytes.
func paragraphs(text string) []paragraph {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var result []paragraph
	for i := 0; i < len(lines); {
		if strings.TrimSpace(lines[i]) == "" {
			i++
			continue
		}
		start := i
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
			i++
		}
		result = append(result, paragraph{
			Range: textRange{position{start, 0}, position{i - 1, len(utf16.Encode([]rune(lines[i-1])))}},
			Text:  strings.Join(lines[start:i], "\n"),
		})
	}
	return result
}

// Matching occurrences avoids retranslating unchanged blocks when lines move.
// An added duplicate still consumes a new occurrence and gets translated.
func changedParagraphs(before, after string) []paragraph {
	counts := make(map[string]int)
	for _, p := range paragraphs(before) {
		counts[p.Text]++
	}
	var changed []paragraph
	for _, p := range paragraphs(after) {
		if counts[p.Text] > 0 {
			counts[p.Text]--
			continue
		}
		changed = append(changed, p)
	}
	return changed
}

func handleDocumentNotification(req request, docs map[string]*document, target string, output io.Writer, run agent) error {
	switch req.Method {
	case "textDocument/didOpen", "textDocument/didChange", "textDocument/didSave", "textDocument/didClose":
	default:
		return nil
	}
	var params documentParams
	if err := json.Unmarshal(req.Params, &params); err != nil || params.TextDocument.URI == "" {
		return nil
	}
	uri := params.TextDocument.URI
	doc := docs[uri]
	switch req.Method {
	case "textDocument/didOpen":
		if params.TextDocument.LanguageID == "markdown" {
			docs[uri] = &document{text: params.TextDocument.Text, baseline: params.TextDocument.Text, version: params.TextDocument.Version}
		}
	case "textDocument/didClose":
		delete(docs, uri)
	case "textDocument/didChange":
		if doc == nil || params.TextDocument.Version <= doc.version {
			return nil
		}
		// Full synchronization was negotiated. Ignore unsupported incremental updates.
		for _, change := range params.ContentChanges {
			if change.Range != nil {
				return nil
			}
		}
		for _, change := range params.ContentChanges {
			doc.text = change.Text
		}
		doc.version = params.TextDocument.Version
	case "textDocument/didSave":
		if doc == nil {
			return nil
		}
		if params.Text != nil {
			doc.text = *params.Text
		}
		if doc.text == doc.baseline {
			return nil
		}
		update := translationUpdate{URI: uri, Version: doc.version, TargetLanguage: target, Translations: []paragraphTranslation{}}
		failed := false
		for _, p := range changedParagraphs(doc.baseline, doc.text) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			text, err := translate(ctx, translationParams{Text: p.Text, TargetLanguage: target}, run)
			cancel()
			item := paragraphTranslation{Range: p.Range, SourceText: p.Text, Text: text}
			if err != nil {
				item.Text = ""
				item.Error = err.Error()
				failed = true
			}
			update.Translations = append(update.Translations, item)
		}
		if !failed {
			doc.baseline = doc.text
		}
		return writeMessage(output, notification[translationUpdate]{"2.0", "honnyaku/translations", update})
	}
	return nil
}

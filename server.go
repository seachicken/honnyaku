package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

type document struct {
	text    string
	version int
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type initializeResult struct {
	Capabilities serverCapabilities `json:"capabilities"`
	ServerInfo   serverInfo         `json:"serverInfo"`
}

type serverCapabilities struct {
	TextDocumentSync textDocumentSyncOptions `json:"textDocumentSync"`
	Experimental     map[string]bool         `json:"experimental,omitempty"`
}

type textDocumentSyncOptions struct {
	OpenClose bool        `json:"openClose"`
	Change    int         `json:"change"`
	Save      saveOptions `json:"save"`
}

type saveOptions struct {
	IncludeText bool `json:"includeText"`
}

type initializationParams struct {
	InitializationOptions struct {
		TargetLanguage      string              `json:"targetLanguage"`
		Model               string              `json:"model"`
		InitialTranslations []translationUpdate `json:"initialTranslations"`
	} `json:"initializationOptions"`
}

type serverInfo struct {
	Name string `json:"name"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func serve(input io.Reader, output io.Writer) (int, error) {
	client := &codexClient{}
	defer client.close()
	return serveWithAgent(input, output, client.run, client.listModels)
}

func serveWithAgent(input io.Reader, output io.Writer, run agent, catalog ...func(context.Context) ([]codexModel, error)) (int, error) {
	return serveWithCache(input, output, run, &translationCache{}, catalog...)
}

func serveWithCache(input io.Reader, output io.Writer, run agent, cache *translationCache, catalog ...func(context.Context) ([]codexModel, error)) (int, error) {
	reader := bufio.NewReader(input)
	initialized, shutdown := false, false
	docs := make(map[string]*document)
	targetLanguage := "English"
	model := ""
	var knownModels []codexModel
	catalogLoaded := false
	refreshCatalog := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		models, err := catalog[0](ctx)
		update := modelListUpdate{Models: []codexModel{}}
		if err != nil {
			update.Error = err.Error()
		} else {
			catalogLoaded = true
			knownModels = models
			update.Models = models
			if model == "" {
				for _, available := range models {
					if available.Model != "" && !available.Hidden && available.supportsLow() {
						if model == "" || available.IsDefault {
							model = available.Model
						}
						if available.IsDefault {
							break
						}
					}
				}
			}
		}
		return writeMessage(output, notification[modelListUpdate]{"2.0", "honnyaku/models", update})
	}

	translateSaved := func(uri string, doc *document) error {
		cached, units := cache.selectUnits(uri, targetLanguage, doc.text)

		if len(catalog) > 0 && !catalogLoaded {
			if err := refreshCatalog(); err != nil {
				return err
			}
		}
		update := translationUpdate{URI: uri, Version: doc.version, TargetLanguage: targetLanguage, Translations: cached}
		var usageErr error
		withUsage := func(ctx context.Context, input agentInput) (string, error) {
			if len(catalog) > 0 {
				supported := false
				for _, available := range knownModels {
					if available.Model == model && available.supportsLow() {
						supported = true
						break
					}
				}
				if !supported {
					return "", fmt.Errorf("select an available model supporting low reasoning before translating")
				}
			}
			input.DocumentURI = uri
			input.Model = model
			input.OnReasoningEffort = func(effort *string) {
				if usageErr == nil {
					usageErr = writeMessage(output, notification[reasoningEffortUpdate]{"2.0", "honnyaku/reasoningEffort", reasoningEffortUpdate{URI: uri, ReasoningEffort: effort}})
				}
			}
			input.OnTokenUsage = func(usage tokenUsage) {
				if usageErr == nil {
					usageErr = writeMessage(output, notification[tokenUsage]{"2.0", "honnyaku/tokenUsage", usage})
				}
			}
			return run(ctx, input)
		}

		if len(units) > 0 {
			pending := translationUpdate{URI: uri, Version: doc.version, TargetLanguage: targetLanguage, Pending: true, Translations: []unitTranslation{}}
			for _, unit := range units {
				pending.Translations = append(pending.Translations, unitTranslation{Range: unit.Range, SourceText: unit.Text})
			}
			if err := writeMessage(output, notification[translationUpdate]{"2.0", "honnyaku/translations", pending}); err != nil {
				return err
			}
			texts := make([]string, len(units))
			for i, unit := range units {
				texts[i] = unit.Text
			}
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			translated, err := translate(ctx, translationParams{Texts: texts, TargetLanguage: targetLanguage}, withUsage)
			cancel()
			if usageErr != nil {
				return usageErr
			}
			for i, unit := range units {
				item := unitTranslation{Range: unit.Range, SourceText: unit.Text}
				if err != nil {
					item.Error = err.Error()
				} else {
					item.Text = translated[i]
					cache.remember(uri, targetLanguage, item.SourceText, item.Text)
				}
				update.Translations = append(update.Translations, item)
			}
		}

		sort.SliceStable(update.Translations, func(i, j int) bool {
			return update.Translations[i].Range.Start.Line < update.Translations[j].Range.Start.Line
		})
		return writeMessage(output, notification[translationUpdate]{"2.0", "honnyaku/translations", update})
	}
messages:
	for {
		body, err := readMessage(reader)
		if errors.Is(err, io.EOF) {
			return 0, nil
		}
		if err != nil {
			return 1, err
		}
		var req request
		var result any
		var failure *rpcError
		if err := json.Unmarshal(body, &req); err != nil {
			req.ID = json.RawMessage("null")
			failure = &rpcError{-32700, "Parse error"}
		} else if req.JSONRPC != "2.0" || req.Method == "" {
			req.ID = json.RawMessage("null")
			failure = &rpcError{-32600, "Invalid request"}
		} else if req.Method == "exit" {
			if shutdown {
				return 0, nil
			}
			return 1, nil
		} else if len(req.ID) == 0 {
			if !initialized || shutdown {
				continue
			}
			if req.Method == "initialized" && len(catalog) > 0 && !catalogLoaded {
				if err := refreshCatalog(); err != nil {
					return 1, err
				}
				continue messages
			}

			switch req.Method {
			case "textDocument/didOpen", "textDocument/didChange", "textDocument/didSave", "textDocument/didClose":
			default:
				continue messages
			}
			var params documentParams
			if err := json.Unmarshal(req.Params, &params); err != nil || params.TextDocument.URI == "" {
				continue messages
			}
			uri := params.TextDocument.URI
			doc := docs[uri]
			switch req.Method {
			case "textDocument/didOpen":
				if params.TextDocument.LanguageID == "markdown" {
					docs[uri] = &document{text: params.TextDocument.Text, version: params.TextDocument.Version}
					cached, _ := cache.selectUnits(uri, targetLanguage, params.TextDocument.Text)
					if len(cached) > 0 {
						update := translationUpdate{URI: uri, Version: params.TextDocument.Version, TargetLanguage: targetLanguage, Translations: cached, Restored: true}
						if err := writeMessage(output, notification[translationUpdate]{"2.0", "honnyaku/translations", update}); err != nil {
							return 1, err
						}
					}
				}
			case "textDocument/didClose":
				delete(docs, uri)
			case "textDocument/didChange":
				if doc == nil || params.TextDocument.Version <= doc.version {
					continue messages
				}
				// Full synchronization was negotiated. Ignore unsupported incremental updates.
				for _, change := range params.ContentChanges {
					if change.Range != nil {
						continue messages
					}
				}
				for _, change := range params.ContentChanges {
					doc.text = change.Text
				}
				doc.version = params.TextDocument.Version
			case "textDocument/didSave":
				if doc == nil {
					continue messages
				}
				if params.Text != nil {
					doc.text = *params.Text
				}
				if err := translateSaved(uri, doc); err != nil {
					return 1, err
				}
			}
			// Notifications never receive a response; save results use a separate notification.
			continue
		} else if shutdown {
			failure = &rpcError{-32600, "Server has shut down"}
		} else if !initialized && req.Method != "initialize" {
			failure = &rpcError{-32002, "Server not initialized"}
		} else {
			switch req.Method {
			case "initialize":
				if initialized {
					failure = &rpcError{-32600, "Server already initialized"}
					break
				}
				var params initializationParams
				if len(req.Params) > 0 {
					if err := json.Unmarshal(req.Params, &params); err != nil {
						failure = &rpcError{-32602, "Invalid initialization parameters"}
						break
					}
				}
				if strings.TrimSpace(params.InitializationOptions.TargetLanguage) != "" {
					targetLanguage = params.InitializationOptions.TargetLanguage
				}
				model = strings.TrimSpace(params.InitializationOptions.Model)
				for i := len(params.InitializationOptions.InitialTranslations) - 1; i >= 0; i-- {
					update := params.InitializationOptions.InitialTranslations[i]
					if update.Pending {
						continue
					}
					for j := len(update.Translations) - 1; j >= 0; j-- {
						item := update.Translations[j]
						if item.Error == "" {
							cache.importTranslation(update.URI, update.TargetLanguage, item.SourceText, item.Text)
						}
					}
				}
				initialized = true
				result = &initializeResult{
					ServerInfo:   serverInfo{Name: "Honnyaku"},
					Capabilities: serverCapabilities{Experimental: map[string]bool{"honnyakuModelSelection": true, "honnyakuModelCatalog": true}, TextDocumentSync: textDocumentSyncOptions{OpenClose: true, Change: 1, Save: saveOptions{IncludeText: true}}},
				}
			case "shutdown":
				shutdown = true
			default:
				failure = &rpcError{-32601, "Method not found"}
			}
		}
		reply := response{JSONRPC: "2.0", ID: req.ID, Error: failure}
		if failure == nil {
			// A nil result encodes shutdown's required JSON null.
			reply.Result, err = json.Marshal(result)
			if err != nil {
				return 1, err
			}
		}
		if err := writeMessage(output, reply); err != nil {
			return 1, err
		}
	}
}

func readMessage(reader *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && (length >= 0 || len(line) > 0) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if line == "\r\n" {
			break
		}
		name, value, ok := strings.Cut(strings.TrimSuffix(line, "\r\n"), ":")
		if !ok {
			return nil, fmt.Errorf("invalid LSP header")
		}
		if strings.EqualFold(name, "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil || length < 0 || length > 16<<20 {
				return nil, fmt.Errorf("invalid Content-Length (maximum 16 MiB)")
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	body := make([]byte, length)
	_, err := io.ReadFull(reader, body)
	return body, err
}

type unitTranslation struct {
	Range      textRange `json:"range"`
	SourceText string    `json:"sourceText"`
	Text       string    `json:"text"`
	Error      string    `json:"error,omitempty"`
}
type translationUpdate struct {
	URI            string            `json:"uri"`
	Version        int               `json:"version"`
	TargetLanguage string            `json:"targetLanguage"`
	Translations   []unitTranslation `json:"translations"`
	Restored       bool              `json:"restored,omitempty"`
	Pending        bool              `json:"pending,omitempty"`
}
type notification[P any] struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  P      `json:"params"`
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

// A nil effort means Codex did not report a resolved value.
type reasoningEffortUpdate struct {
	URI             string  `json:"uri"`
	ReasoningEffort *string `json:"reasoningEffort"`
}

type modelListUpdate struct {
	Models []codexModel `json:"models"`
	Error  string       `json:"error,omitempty"`
}

// The client owns persistence and seeds this per-process memory cache on initialization.
type translationCacheKey struct{ uri, language string }
type translationCache struct {
	files map[translationCacheKey]map[string]string
}

func cacheKey(uri, language string) translationCacheKey {
	if parsed, err := url.Parse(uri); err == nil {
		uri = strings.ToLower(parsed.Scheme) + "://" + parsed.Host + path.Clean(parsed.Path)
	}
	return translationCacheKey{uri: uri, language: language}
}

func (c *translationCache) entries(uri, language string) map[string]string {
	key := cacheKey(uri, language)
	if c.files == nil {
		c.files = make(map[translationCacheKey]map[string]string)
	}
	if c.files[key] == nil {
		c.files[key] = make(map[string]string)
	}
	return c.files[key]
}

func (c *translationCache) remember(uri, language, source, translated string) {
	c.entries(uri, language)[source] = translated
}

func (c *translationCache) importTranslation(uri, language, source, translated string) {
	if uri == "" || language == "" || strings.TrimSpace(source) == "" {
		return
	}
	if _, exists := c.entries(uri, language)[source]; !exists {
		c.remember(uri, language, source, translated)
	}
	originalLines, translatedLines := strings.Split(source, "\n"), strings.Split(translated, "\n")
	if len(originalLines) > 1 && len(originalLines) == len(translatedLines) {
		for i, line := range originalLines {
			c.importTranslation(uri, language, line, translatedLines[i])
		}
	}
}

func (c *translationCache) selectUnits(uri, language, source string) ([]unitTranslation, []translationUnit) {
	entries := c.entries(uri, language)
	all := translationUnits(source)
	cached := []unitTranslation{}
	covered := make(map[int]bool)
	// Preserve previously stored paragraph results during migration, even when they aren't line-aligned.
	var paragraphs []string
	for text := range entries {
		if strings.Contains(text, "\n") {
			paragraphs = append(paragraphs, text)
		}
	}
	sort.Slice(paragraphs, func(i, j int) bool {
		if len(paragraphs[i]) == len(paragraphs[j]) {
			return paragraphs[i] < paragraphs[j]
		}
		return len(paragraphs[i]) > len(paragraphs[j])
	})
	lines := strings.Split(source, "\n")
	for _, text := range paragraphs {
		original := strings.Split(text, "\n")
		for start := 0; start+len(original) <= len(lines); start++ {
			end := start + len(original) - 1
			if strings.Join(lines[start:end+1], "\n") != text {
				continue
			}
			overlaps := false
			for line := start; line <= end; line++ {
				if covered[line] {
					overlaps = true
				}
			}
			if overlaps {
				continue
			}
			last := translationUnits(original[len(original)-1])
			endCharacter := 0
			if len(last) > 0 {
				endCharacter = last[0].Range.End.Character
			}
			cached = append(cached, unitTranslation{Range: textRange{position{start, 0}, position{end, endCharacter}}, SourceText: text, Text: entries[text]})
			for line := start; line <= end; line++ {
				covered[line] = true
			}
		}
	}
	var missing []translationUnit
	for _, unit := range all {
		if covered[unit.Range.Start.Line] {
			continue
		}
		if text, ok := entries[unit.Text]; ok {
			cached = append(cached, unitTranslation{Range: unit.Range, SourceText: unit.Text, Text: text})
		} else {
			missing = append(missing, unit)
		}
	}
	return cached, missing
}

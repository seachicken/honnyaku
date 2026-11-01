package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func Test_FileThreads(t *testing.T) {
	// First save of a.md, another save of a.md, then the first save of b.md.
	responses := []string{
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-a"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
		`{"method":"item/completed","params":{"threadId":"thread-a","turnId":"turn-1","item":{"type":"agentMessage","text":"first"}}}`,
		`{"method":"turn/completed","params":{"threadId":"thread-a","turn":{"id":"turn-1","status":"completed"}}}`,
		`{"id":4,"result":{"turn":{"id":"turn-2"}}}`,
		`{"method":"item/completed","params":{"threadId":"thread-a","turnId":"turn-2","item":{"type":"agentMessage","text":"second"}}}`,
		`{"method":"turn/completed","params":{"threadId":"thread-a","turn":{"id":"turn-2","status":"completed"}}}`,
		`{"id":5,"result":{"thread":{"id":"thread-b"}}}`,
		`{"id":6,"result":{"turn":{"id":"turn-3"}}}`,
		`{"method":"item/completed","params":{"threadId":"thread-b","turnId":"turn-3","item":{"type":"agentMessage","text":"third"}}}`,
		`{"method":"turn/completed","params":{"threadId":"thread-b","turn":{"id":"turn-3","status":"completed"}}}`,
	}
	var requests bytes.Buffer
	client := codexClient{encoder: json.NewEncoder(&requests), decoder: json.NewDecoder(strings.NewReader(strings.Join(responses, "\n")))}
	for i, uri := range []string{"file:///a.md", "file:///a.md", "file:///b.md"} {
		got, err := client.exchange(agentInput{DocumentURI: uri, Text: fmt.Sprint(i)})
		want := []string{"first", "second", "third"}[i]
		if err != nil || got != want {
			t.Fatalf("save %d: got %q, %v; want %q", i, got, err, want)
		}
	}
	decoder := json.NewDecoder(&requests)
	var methods, turnThreads, texts []string
	for {
		var req struct {
			Method string `json:"method"`
			Params struct {
				ThreadID string `json:"threadId"`
				Input    []struct {
					Text string `json:"text"`
				} `json:"input"`
			} `json:"params"`
		}
		err := decoder.Decode(&req)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		methods = append(methods, req.Method)
		if req.Method == "turn/start" {
			turnThreads = append(turnThreads, req.Params.ThreadID)
			texts = append(texts, req.Params.Input[0].Text)
		}
	}
	if !reflect.DeepEqual(methods, []string{"initialize", "initialized", "thread/start", "turn/start", "turn/start", "thread/start", "turn/start"}) {
		t.Fatalf("methods: %v", methods)
	}
	if !reflect.DeepEqual(turnThreads, []string{"thread-a", "thread-a", "thread-b"}) || !reflect.DeepEqual(texts, []string{"0", "1", "2"}) {
		t.Fatalf("turns: threads=%v texts=%v", turnThreads, texts)
	}
}

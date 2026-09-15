package main

import (
	"testing"

	"github.com/ollama/ollama/api"
)

func TestResolveToolFromResponse_ContentJSON(t *testing.T) {
	m := api.Message{Content: "{\"tool\": \"fs\", \"args\": {\"op\": \"write\", \"path\": \"sample.go\", \"content\": \"x\"}}"}
	name, args, ok := resolveToolFromResponse(m)
	if !ok || name != "fs" {
		t.Fatalf("resolveToolFromResponse failed: %s %v %v", name, args, ok)
	}
}

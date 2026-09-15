package main

import "testing"

func TestMapFSOp(t *testing.T) {
	cases := map[string]string{
		"read":     "read",
		"ReadFile": "read",
		"WRITE":    "write",
		"ls":       "list",
		"tree":     "tree",
		"stat":     "info",
	}
	for in, want := range cases {
		if out := mapFSOp(in); out != want {
			t.Fatalf("mapFSOp(%q) = %q; want %q", in, out, want)
		}
	}
}

func TestNormalizeFSArgs(t *testing.T) {
	in := map[string]string{"OP": "Write", "Path": "\"sample.go\"", "content": "hi"}
	out := normalizeFSArgs(in)
	if out["op"] != "write" {
		t.Fatalf("expected op write, got %q", out["op"])
	}
	if out["path"] != "sample.go" {
		t.Fatalf("expected path sample.go, got %q", out["path"])
	}
}

func TestCleanJSONAndParseToolJSON(t *testing.T) {
	raw := "```json\n{\n  \"tool\": \"fs\",\n  \"args\": {\"op\": \"write\", \"path\": \"sample.go\", \"content\": \"x\"}\n}\n```"
	tool, args, err := parseToolJSON(raw)
	if err != nil {
		t.Fatalf("parseToolJSON error: %v", err)
	}
	if tool != "fs" {
		t.Fatalf("expected tool fs, got %q", tool)
	}
	if args["op"] != "write" || args["path"] != "sample.go" || args["content"] != "x" {
		t.Fatalf("unexpected args: %v", args)
	}
}

func TestForceFSArgsFromContent(t *testing.T) {
	raw := "Here is the response:\n```json\n{\"op\":\"write\",\"path\":\"sample.go\",\"content\":\"package main\\n\\nfunc main(){}\"}\n```"
	args, ok := forceFSArgsFromContent(raw)
	if !ok {
		t.Fatalf("forceFSArgsFromContent failed to parse")
	}
	if args["op"] != "write" || args["path"] != "sample.go" {
		t.Fatalf("unexpected parsed args: %v", args)
	}
}

func TestExtractFSCommandFromTextAndParseKeyValueArgs(t *testing.T) {
	line := "fs write path=sample.go content='package main\nfunc main(){}'"
	args, ok := extractFSCommandFromText(line)
	if !ok {
		t.Fatalf("extractFSCommandFromText failed")
	}
	if args["path"] != "sample.go" || args["content"] == "" {
		t.Fatalf("unexpected args: %v", args)
	}
}

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/396xwz/toolloop/internal/engine"
	"github.com/396xwz/toolloop/internal/tools"
)

func TestPlanNextStepWorkspaceInspection(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile("overview.md", []byte("A fixture project."), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	linkErr := os.Symlink(outside, "outside-link")
	for _, test := range []struct {
		name, op, path string
		allowed        bool
	}{
		{"list working directory", "list", ".", true},
		{"tree working directory", "tree", ".", true},
		{"read discovered file", "read", "overview.md", true},
		{"read multiple files", "read_many", "overview.md, ./overview.md", true},
		{"file metadata", "info", "overview.md", true},
		{"absolute workspace file", "read", filepath.Join(root, "overview.md"), true},
		{"parent traversal", "list", "..", false},
		{"absolute outside", "list", outside, false},
		{"mixed read many", "read_many", "overview.md, ../outside.md", false},
		{"symlink escape", "list", "outside-link", false},
		{"implicit write", "write", "overview.md", false},
		{"implicit edit", "edit", "overview.md", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "symlink escape" && linkErr != nil {
				t.Skipf("symlinks unavailable: %v", linkErr)
			}
			call, err := json.Marshal(map[string]interface{}{
				"tool": "fs", "args": map[string]string{
					"op": test.op, "path": test.path, "content": "changed",
					"old_snippet": "fixture", "new_snippet": "changed",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			step, _ := runPlanNextStep(t, &engine.Task{Description: "Summarize what this project does"}, string(call))
			if got := step.ToolCall != nil; got != test.allowed {
				t.Fatalf("allowed = %v, want %v; plan = %q", got, test.allowed, step.Plan)
			}
		})
	}
}

func TestLlamaNativeProjectInspectionExecutesFilesystem(t *testing.T) {
	t.Chdir(t.TempDir())
	const contents = "This fixture indexes local documents."
	if err := os.WriteFile("overview.md", []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []LlamaMessage `json:"messages"`
			Tools    []struct {
				Function struct {
					Name       string `json:"name"`
					Parameters struct {
						Required   []string                   `json:"required"`
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		requests++
		if requests <= 2 {
			if len(request.Tools) != 1 || request.Tools[0].Function.Name != "fs" ||
				len(request.Tools[0].Function.Parameters.Properties["op"]) == 0 ||
				len(request.Tools[0].Function.Parameters.Properties["path"]) == 0 {
				t.Errorf("missing filesystem schema: %+v", request.Tools)
			}
			op, path := "list", "."
			if requests == 2 {
				last := request.Messages[len(request.Messages)-1]
				if last.Role != "tool" || !strings.Contains(last.Content, "overview.md") {
					t.Errorf("listing not provided to model: %+v", last)
				}
				op, path = "read", "overview.md"
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []interface{}{map[string]interface{}{
					"finish_reason": "tool_calls",
					"message": LlamaMessage{Role: "assistant", ToolCalls: []LlamaToolCall{{
						ID: "native_fs", Type: "function",
						Function: LlamaToolCallFunction{Name: "fs", Arguments: map[string]interface{}{"op": op, "path": path}},
					}}},
				}},
			})
			return
		}
		last := request.Messages[len(request.Messages)-1]
		if !strings.Contains(last.Content, contents) {
			t.Errorf("file content missing from model context: %+v", last)
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"This fixture indexes local documents."}}]}`))
	}))
	defer server.Close()
	reg := tools.NewToolRegistry()
	reg.Register("fs", tools.FileSystemTool{})
	model := &LlamaCppModel{Client: NewLlamaClient(server.URL)}
	task := &engine.Task{Description: "Summarize what this project does", Tools: reg}
	if err := (&engine.Engine{Model: model}).RunTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if len(task.Steps) != 3 || task.Steps[1].Err != nil || !strings.Contains(task.Steps[1].Result, contents) {
		t.Fatalf("filesystem inspection failed: %+v", task.Steps)
	}
	answer, err := model.GenerateFinalAnswer(context.Background(), task)
	if err != nil || answer != contents || requests != 4 {
		t.Fatalf("answer=%q err=%v requests=%d", answer, err, requests)
	}
}

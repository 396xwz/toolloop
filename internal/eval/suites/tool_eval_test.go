package suites

import (
	"encoding/json"
	"testing"
)

func TestFilesystemToolSuiteUsesDeterministicDirectCalls(t *testing.T) {
	suite, err := Lookup(FilesystemToolSuite, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if suite.Name != FilesystemToolSuite || suite.Component != "tool:fs" {
		t.Fatalf("unexpected suite: %+v", suite)
	}
	if len(suite.Cases) != 5 {
		t.Fatalf("got %d cases, want 5", len(suite.Cases))
	}
	operations := map[string]bool{}
	for _, testCase := range suite.Cases {
		var request struct {
			Tool string            `json:"tool"`
			Args map[string]string `json:"args"`
		}
		if err := json.Unmarshal([]byte(testCase.Input.Task), &request); err != nil {
			t.Fatalf("%s does not contain a direct JSON call: %v", testCase.Name, err)
		}
		if request.Tool != "fs" {
			t.Fatalf("%s uses tool %q", testCase.Name, request.Tool)
		}
		operations[request.Args["op"]] = true
	}
	for _, operation := range []string{"read", "write", "edit"} {
		if !operations[operation] {
			t.Fatalf("suite does not cover %q", operation)
		}
	}
}

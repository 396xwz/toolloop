package suites

import (
	"fmt"
	"sort"
	"strings"

	"github.com/396xwz/toolloop/internal/eval"
)

type Options struct {
	Model     ModelSuiteOptions
	Task      string
	TaskJudge *eval.LLMJudgeScorer
}

type suiteFactory func(Options) eval.Suite

var registry = map[string]suiteFactory{
	FilesystemToolSuite: func(Options) eval.Suite {
		return NewFilesystemToolSuite()
	},
	ModelBackendSuite: func(opts Options) eval.Suite {
		return NewModelStabilitySuite(opts.Model)
	},
	TaskSuite: func(opts Options) eval.Suite {
		return NewTaskSuite(opts.Task, opts.TaskJudge)
	},
}

// List returns registered CLI suite names in stable lexical order.
func List() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Lookup constructs a registered suite using caller-provided dependencies.
func Lookup(name string, opts Options) (eval.Suite, error) {
	name = strings.TrimSpace(name)
	factory, ok := registry[name]
	if !ok {
		return eval.Suite{}, fmt.Errorf(
			"unknown evaluation suite %q (available: %s)",
			name,
			strings.Join(List(), ", "),
		)
	}
	return factory(opts), nil
}

package main

import (
	"bytes"
	"testing"
)

func TestConfirmDecision(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty line approves", "\n", true},
		{"non-empty line denies", "no\n", false},
		{"EOF denies", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := confirmDecision(bytes.NewReader([]byte(tt.in)))
			if got != tt.want {
				t.Errorf("confirmDecision(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

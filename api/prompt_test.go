package main

import (
	"testing"
	"testing/fstest"
)

func mustPrompt(t *testing.T, version string) string {
	t.Helper()
	prompt, err := loadPrompt(version)
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

func TestLoadPrompt(t *testing.T) {
	if got := mustPrompt(t, "V1"); got == "" {
		t.Fatal("empty prompt")
	}
	for _, version := range []string{"", "../V1", "v1", "V999", "V1.txt"} {
		if _, err := loadPrompt(version); err == nil {
			t.Errorf("accepted invalid/missing version %q", version)
		}
	}
	if _, err := readPrompt(fstest.MapFS{"prompts/EMPTY.txt": &fstest.MapFile{Data: []byte(" \n\t")}}, "EMPTY"); err == nil {
		t.Fatal("accepted empty prompt")
	}
}

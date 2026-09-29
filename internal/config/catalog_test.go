package config

import (
	"strings"
	"testing"
)

func catalogOption(model string, context int, levels ...string) ModelOption {
	return ModelOption{Model: model, Difficulty: levels, Strengths: "code", ContextTokens: context}
}

func TestCatalogRequiresContextAndEnglishDifficulties(t *testing.T) {
	valid := ModelCategory{Description: "code", Options: []ModelOption{catalogOption("a/flash", 1000, "low", "medium"), catalogOption("a/pro", 1000, "low", "medium", "high")}}
	if err := validateCategory("engineering", valid); err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}
	noContext := ModelCategory{Description: "code", Options: []ModelOption{catalogOption("a/pro", 0, "low", "medium", "high")}}
	if err := validateCategory("engineering", noContext); err == nil || !strings.Contains(err.Error(), "context_tokens") {
		t.Fatalf("missing context_tokens: %v", err)
	}
	spanish := ModelCategory{Description: "code", Options: []ModelOption{catalogOption("a/pro", 1000, "baja", "media", "alta")}}
	if err := validateCategory("engineering", spanish); err == nil {
		t.Fatal("accepted Spanish difficulty values")
	}
}

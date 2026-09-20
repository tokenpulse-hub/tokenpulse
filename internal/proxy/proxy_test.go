package proxy

import (
	"os"
	"testing"
)

func TestExtractUsageFromJSON(t *testing.T) {
	body, _ := os.ReadFile("../../../.temp/mock_response.json")
	u := extractUsageFromJSON(body, "fallback-model")
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.Model != "deepseek-v4-flash" {
		t.Errorf("model = %q, want deepseek-v4-flash", u.Model)
	}
	if u.InputTokens != 100 {
		t.Errorf("input = %d, want 100", u.InputTokens)
	}
	if u.OutputTokens != 20 {
		t.Errorf("output = %d, want 20", u.OutputTokens)
	}
}

func TestExtractUsageFromStream(t *testing.T) {
	body, _ := os.ReadFile("../../../.temp/mock_stream.txt")
	u := extractUsageFromStream(body, "fallback-model")
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.Model != "deepseek-v4-flash" {
		t.Errorf("model = %q, want deepseek-v4-flash", u.Model)
	}
	if u.InputTokens != 50 {
		t.Errorf("input = %d, want 50", u.InputTokens)
	}
	if u.OutputTokens != 10 {
		t.Errorf("output = %d, want 10", u.OutputTokens)
	}
}

func TestExtractUsageEmpty(t *testing.T) {
	if u := extractUsageFromJSON([]byte(`{}`), "m"); u != nil {
		t.Error("expected nil for empty usage")
	}
	if u := extractUsageFromJSON(nil, "m"); u != nil {
		t.Error("expected nil for nil body")
	}
	if u := extractUsageFromStream([]byte("data: [DONE]\n\n"), "m"); u != nil {
		t.Error("expected nil for stream with no usage chunk")
	}
}

package pairing

import (
	"encoding/json"
	"strings"
	"testing"
)

func sample() Payload {
	return New("https://exocomp.example.ts.net", "exo_42_secretbytes", "AB:CD:EF", "operator", "pixel-8")
}

func TestPayloadJSONRoundTrips(t *testing.T) {
	js, err := sample().JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var got Payload
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != sample() {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, sample())
	}
	if got.V != Version {
		t.Fatalf("version = %d, want %d", got.V, Version)
	}
}

func TestFingerprintOmittedWhenEmpty(t *testing.T) {
	js, err := New("https://h", "exo_1_x", "", "readonly", "d").JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if strings.Contains(js, "fingerprint") {
		t.Fatalf("empty fingerprint should be omitted: %s", js)
	}
}

func TestSVGRendersModuleRects(t *testing.T) {
	svg, err := sample().SVG()
	if err != nil {
		t.Fatalf("SVG: %v", err)
	}
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatalf("not a self-contained svg: %.40s…", svg)
	}
	if strings.Count(svg, "<rect") < 10 {
		t.Fatalf("expected module rects, got: %.80s…", svg)
	}
	// currentColor keeps it theme-aware; the token must never leak into the markup.
	if !strings.Contains(svg, "currentColor") {
		t.Fatal("svg module fill is not currentColor")
	}
	if strings.Contains(svg, "exo_42_secretbytes") {
		t.Fatal("token plaintext leaked into svg markup")
	}
}

func TestTerminalNonEmpty(t *testing.T) {
	term, err := sample().Terminal()
	if err != nil {
		t.Fatalf("Terminal: %v", err)
	}
	if strings.TrimSpace(term) == "" {
		t.Fatal("terminal QR is empty")
	}
}

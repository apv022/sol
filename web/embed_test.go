package web

import (
	"strings"
	"testing"
)

func TestTokensBlock(t *testing.T) {
	tok := Tokens()
	for _, want := range []string{"--bg:", "--accent:", "--mono:", `:root[data-theme="light"]`, `data-theme="system"`} {
		if !strings.Contains(tok, want) {
			t.Fatalf("token block lacks %q; did the sol:tokens delimiters in style.css move?", want)
		}
	}
	if strings.Contains(tok, "button") {
		t.Fatal("token block swallowed rules; sol:tokens:end moved")
	}
}

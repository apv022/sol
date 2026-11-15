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

func TestBaseBlock(t *testing.T) {
	base := Base()
	if !strings.HasPrefix(base, Tokens()) || len(base) <= len(Tokens()) {
		t.Fatal("base must extend the token block; did the sol:base delimiters move?")
	}
	for _, want := range []string{"button, .button", ".rows", ".file", ".empty", ".brand"} {
		if !strings.Contains(base, want) {
			t.Fatalf("base block lacks %q", want)
		}
	}
	for _, not := range []string{".nav ", ".side ", "dialog", ".toast"} {
		if strings.Contains(base, not) {
			t.Fatalf("base block swallowed the panel's own layout (%q); sol:base:end moved", not)
		}
	}
}

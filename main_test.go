package main

import (
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedWebAssetsComplete(t *testing.T) {
	sub, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		t.Fatal(err)
	}
	mustExist := []string{
		"index.html",
		"css/app.css",
		"js/main.js",
		"js/api.js",
		"js/dialogs.js",
		"js/panel.js",
		"js/filters.js",
		"js/tables.js",
		"js/util.js",
		"js/video.js",
	}
	for _, p := range mustExist {
		b, err := fs.ReadFile(sub, p)
		if err != nil || len(b) == 0 {
			t.Fatalf("missing or empty embedded web file %s: %v", p, err)
		}
		if p == "index.html" && !strings.Contains(string(b), "阿里云 OSS Go SDK") {
			t.Fatalf("embedded index.html missing Go SDK label")
		}
	}
}

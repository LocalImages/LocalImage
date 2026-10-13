package proxy

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRewriteWS(t *testing.T) {
	body := `[{"webSocketDebuggerUrl":"ws://127.0.0.1:32769/devtools/page/ABC"}]`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Request:    &http.Request{URL: &url.URL{Path: "/json/list"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	if err := rewriteWS("daemon:7777", "32769", "/t/tok/cdp/")(resp); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	want := `ws://daemon:7777/t/tok/cdp/devtools/page/ABC`
	if !strings.Contains(string(got), want) {
		t.Fatalf("rewritten body %q does not contain %q", got, want)
	}
	if resp.ContentLength != int64(len(got)) {
		t.Fatalf("content length %d, body %d", resp.ContentLength, len(got))
	}
}

func TestRewriteWSLeavesOtherPathsAlone(t *testing.T) {
	body := `ws://127.0.0.1:32769/x`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Request:    &http.Request{URL: &url.URL{Path: "/devtools/browser/1"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	if err := rewriteWS("daemon:7777", "32769", "/t/tok/cdp/")(resp); err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(resp.Body); string(got) != body {
		t.Fatalf("body changed: %q", got)
	}
}

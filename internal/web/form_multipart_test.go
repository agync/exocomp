package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNetgroupMultipartRefusalEchoesValues is the regression gate for the
// form-fetch bug: the browser's fetch submit path POSTs a multipart FormData
// (a fetch body cannot carry urlencoded), and ParseForm alone does not read
// multipart — so the server echoed empty fields on a 422 and the dialog lost
// the operator's typed values after the first refusal. writeGuard now parses
// multipart too, so the refusal re-render must carry the submitted values back.
func TestNetgroupMultipartRefusalEchoesValues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "home-cid jjjbh H")
	_ = mw.WriteField("cidrs", "192.168.86.1/24")
	_ = mw.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/netgroups/add", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := op.Do(req)
	if err != nil {
		t.Fatalf("multipart POST: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	// The refusal must keep the operator's typed values (the exact bug), so a
	// corrected re-submit does not start from an empty form.
	if !strings.Contains(body, `value="home-cid jjjbh H"`) {
		t.Errorf("multipart refusal lost the name value:\n%s", body)
	}
	if !strings.Contains(body, `value="192.168.86.1/24"`) {
		t.Errorf("multipart refusal lost the cidr value:\n%s", body)
	}
	// And the human sentence is present (no regex leak). The message is
	// HTML-escaped in the render, so the apostrophe reads as &#39;.
	if !strings.Contains(body, "Names can&#39;t have spaces.") {
		t.Errorf("multipart refusal missing the human message:\n%s", body)
	}
}

package eval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeTarget struct {
	value string
	err   error
}

func (f fakeTarget) EvalCode(context.Context, string) (string, error) {
	return f.value, f.err
}

func TestRegisterLookupUnregister(t *testing.T) {
	target := fakeTarget{value: "ok"}
	Register("t1", target)
	got, ok := Lookup("t1")
	if !ok {
		t.Fatal("target not found")
	}
	if got.(fakeTarget).value != "ok" {
		t.Fatal("wrong target")
	}
	Unregister("t1")
	if _, ok := Lookup("t1"); ok {
		t.Fatal("target still registered")
	}
}

func TestHandleEval(t *testing.T) {
	Register("t2", fakeTarget{value: "42"})
	defer Unregister("t2")

	body := strings.NewReader(`{"target":"t2","code":"1+1"}`)
	req := httptest.NewRequest(http.MethodPost, "/eval", body)
	rec := httptest.NewRecorder()
	if err := handleEval(rec, req); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result != "42" {
		t.Fatalf("result = %q", resp.Result)
	}
}

func TestHandleEvalUnknownTarget(t *testing.T) {
	body := strings.NewReader(`{"target":"missing","code":"1"}`)
	req := httptest.NewRequest(http.MethodPost, "/eval", body)
	rec := httptest.NewRecorder()
	if err := handleEval(rec, req); err == nil {
		t.Fatal("expected error for unknown target")
	}
}

func TestHandleEvalRejectsGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/eval", nil)
	rec := httptest.NewRecorder()
	if err := handleEval(rec, req); err == nil {
		t.Fatal("expected error for GET")
	}
}

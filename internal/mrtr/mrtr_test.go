package mrtr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/mcp"
)

func TestResolveAnswersInSortedKeyOrder(t *testing.T) {
	var order []string
	h := func(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error) {
		order = append(order, method)
		return json.RawMessage(`{"answered":"` + method + `"}`), nil
	}
	reqs := mcp.InputRequests{
		"b": {Method: "second/method"},
		"a": {Method: "first/method"},
		"c": {Method: "third/method"},
	}
	out, err := Resolve(context.Background(), reqs, h)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("responses = %v, want 3 entries", out)
	}
	want := []string{"first/method", "second/method", "third/method"}
	for i, m := range want {
		if order[i] != m {
			t.Fatalf("answer order %v, want %v (sorted by key)", order, want)
		}
	}
	if string(out["a"]) != `{"answered":"first/method"}` {
		t.Fatalf(`out["a"] = %s`, out["a"])
	}
}

func TestResolveRejectsSampling(t *testing.T) {
	h := func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		t.Fatal("handler must not run for sampling")
		return nil, nil
	}
	_, err := Resolve(context.Background(), mcp.InputRequests{
		"llm": {Method: mcp.MethodSamplingCreate},
	}, h)
	if !errors.Is(err, ErrSamplingUnsupported) {
		t.Fatalf("err = %v, want ErrSamplingUnsupported", err)
	}
}

func TestResolveEmptyRequestsFailsClosed(t *testing.T) {
	_, err := Resolve(context.Background(), nil, func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return nil, nil
	})
	if !errors.Is(err, ErrNoInputRequests) {
		t.Fatalf("err = %v, want ErrNoInputRequests", err)
	}
}

func TestResolveFirstFailureAbortsWithNoPartialMap(t *testing.T) {
	boom := errors.New("boom")
	h := func(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "fails/here" {
			return nil, boom
		}
		return json.RawMessage(`{}`), nil
	}
	out, err := Resolve(context.Background(), mcp.InputRequests{
		"a": {Method: "works/fine"},
		"b": {Method: "fails/here"},
		"c": {Method: "never/reached"},
	}, h)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the handler failure", err)
	}
	if out != nil {
		t.Fatalf("partial responses %v returned alongside the error", out)
	}
}

func TestResolveNilHandler(t *testing.T) {
	_, err := Resolve(context.Background(), mcp.InputRequests{"a": {Method: "x"}}, nil)
	if err == nil {
		t.Fatal("want error for nil handler")
	}
}

func TestResolveErrorNamesKeyAndMethod(t *testing.T) {
	h := func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return nil, fmt.Errorf("nope")
	}
	_, err := Resolve(context.Background(), mcp.InputRequests{
		"k9": {Method: mcp.MethodElicitationCreate},
	}, h)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"k9", mcp.MethodElicitationCreate} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

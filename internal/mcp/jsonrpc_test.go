package mcp

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestIDMarshalUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
		wantSet bool
		wantKey string
	}{
		{name: "string", in: `"abc"`, wantSet: true, wantKey: `"abc"`},
		{name: "int", in: `42`, wantSet: true, wantKey: `42`},
		{name: "big int keeps raw text", in: `9007199254740993`, wantSet: true, wantKey: `9007199254740993`},
		{name: "float", in: `1.5`, wantSet: true, wantKey: `1.5`},
		{name: "null is unset", in: `null`, wantSet: false},
		{name: "bool rejected", in: `true`, wantErr: true},
		{name: "object rejected", in: `{"a":1}`, wantErr: true},
		{name: "array rejected", in: `[1]`, wantErr: true},
		{name: "garbage rejected", in: `{`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var id ID
			// Call UnmarshalJSON directly: json.Unmarshal pre-validates
			// syntax and would mask our own error for invalid JSON.
			err := id.UnmarshalJSON([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%q): want error, got nil", tt.in)
				}
				if !errors.Is(err, ErrMalformedFrame) {
					t.Fatalf("Unmarshal(%q): error %v not ErrMalformedFrame", tt.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%q): %v", tt.in, err)
			}
			if id.IsSet() != tt.wantSet {
				t.Fatalf("IsSet = %v, want %v", id.IsSet(), tt.wantSet)
			}
			if tt.wantSet && id.Key() != tt.wantKey {
				t.Fatalf("Key = %q, want %q", id.Key(), tt.wantKey)
			}
			out, err := json.Marshal(id)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			want := tt.in
			if !tt.wantSet {
				want = "null"
			}
			if string(out) != want {
				t.Fatalf("round trip = %s, want %s", out, want)
			}
		})
	}
}

func TestIDStringVsNumberDistinct(t *testing.T) {
	if NewIntID(1).Key() == NewStringID("1").Key() {
		t.Fatal(`numeric id 1 and string id "1" must not collide`)
	}
}

func TestParseMessage(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    any // *Request, *Response, *Notification (type + key fields checked)
		wantErr bool
	}{
		{
			name: "request",
			in:   `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
			want: &Request{},
		},
		{
			name: "request with string id",
			in:   `{"jsonrpc":"2.0","id":"a1","method":"roots/list"}`,
			want: &Request{},
		},
		{
			name: "notification",
			in:   `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`,
			want: &Notification{},
		},
		{
			name: "method with null id is notification",
			in:   `{"jsonrpc":"2.0","id":null,"method":"notifications/initialized"}`,
			want: &Notification{},
		},
		{
			name: "response result",
			in:   `{"jsonrpc":"2.0","id":7,"result":{"tools":[]}}`,
			want: &Response{},
		},
		{
			name: "response error",
			in:   `{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"nope"}}`,
			want: &Response{},
		},
		{
			name: "error response with null id",
			in:   `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse"}}`,
			want: &Response{},
		},
		{name: "invalid json", in: `{"jsonrpc":"2.0",`, wantErr: true},
		{name: "not json at all", in: `garbage`, wantErr: true},
		{name: "missing jsonrpc", in: `{"id":1,"method":"ping"}`, wantErr: true},
		{name: "wrong jsonrpc version", in: `{"jsonrpc":"1.0","id":1,"method":"ping"}`, wantErr: true},
		{name: "bool id", in: `{"jsonrpc":"2.0","id":true,"method":"ping"}`, wantErr: true},
		{name: "neither req nor resp", in: `{"jsonrpc":"2.0","id":3}`, wantErr: true},
		{name: "empty object", in: `{}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMessage([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %T", got)
				}
				if !errors.Is(err, ErrMalformedFrame) {
					t.Fatalf("error %v is not ErrMalformedFrame", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch tt.want.(type) {
			case *Request:
				if _, ok := got.(*Request); !ok {
					t.Fatalf("got %T, want *Request", got)
				}
			case *Response:
				if _, ok := got.(*Response); !ok {
					t.Fatalf("got %T, want *Response", got)
				}
			case *Notification:
				if _, ok := got.(*Notification); !ok {
					t.Fatalf("got %T, want *Notification", got)
				}
			}
		})
	}
}

func TestParseMessageFieldFidelity(t *testing.T) {
	got, err := ParseMessage([]byte(`{"jsonrpc":"2.0","id":"x9","method":"tools/call","params":{"name":"t","arguments":{"a":1}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req, ok := got.(*Request)
	if !ok {
		t.Fatalf("got %T", got)
	}
	if req.Method != "tools/call" {
		t.Fatalf("method %q", req.Method)
	}
	if req.ID.Key() != `"x9"` {
		t.Fatalf("id key %q", req.ID.Key())
	}
	var p CallToolParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "t" || string(p.Arguments) != `{"a":1}` {
		t.Fatalf("params %+v", p)
	}
}

func TestNewResponseNilResult(t *testing.T) {
	b, err := json.Marshal(NewResponse(NewIntID(1), nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"jsonrpc":"2.0","id":1,"result":null}` {
		t.Fatalf("got %s", b)
	}
}

func TestErrorResponseRoundTrip(t *testing.T) {
	resp := NewErrorResponse(NewStringID("e"), &Error{Code: CodeMethodNotFound, Message: "no such method"})
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := got.(*Response)
	if !ok || r.Error == nil {
		t.Fatalf("got %#v", got)
	}
	if r.Error.Code != CodeMethodNotFound {
		t.Fatalf("code %d", r.Error.Code)
	}
	var asErr error = r.Error
	if asErr.Error() == "" {
		t.Fatal("Error() empty")
	}
}

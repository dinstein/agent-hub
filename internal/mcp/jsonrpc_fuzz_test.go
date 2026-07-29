package mcp

import "testing"

// FuzzParseMessage drives the JSON-RPC frame parser with arbitrary bytes.
//
// Every byte ParseMessage sees arrives from a downstream MCP server, so a
// panic here is a gateway crash that a malicious or merely broken server can
// trigger. The parser also decides which of three shapes a frame is
// (request / response / notification) from an overlapping probe struct, which
// is the kind of dispatch that misclassifies before it crashes.
//
// The post-conditions are the ones a caller relies on: a nil error means a
// usable message, and one of the three concrete types — never a nil `any`
// that type-switches into the default branch at the call site.
func FuzzParseMessage(f *testing.F) {
	for _, s := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":null,"result":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"a","error":{"code":-32000,"message":"x"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"both"}}`,
		`{}`, ``, `null`, `[]`, `{"id":{"a":1}}`, `{"jsonrpc":"1.0","id":1}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := ParseMessage(data)
		if err != nil {
			return
		}
		if msg == nil {
			t.Fatal("nil message with nil error: the caller's type switch has no case for this")
		}
		switch msg.(type) {
		case *Request, *Response, *Notification:
		default:
			t.Fatalf("parsed into an unexpected type %T", msg)
		}
	})
}

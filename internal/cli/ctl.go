package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/ctlapi"
)

// This file is the CLI's raw control-plane access for the surfaces the typed
// api client does not cover. It speaks the same
// envelope over the same UDS; wire DTOs come from internal/ctlapi (the CLI
// lives inside the module, unlike the public api package).

// ctlMaxResponse bounds control-plane response bodies (same discipline as
// the api client).
const ctlMaxResponse = 16 << 20

// ctlError is a structured error envelope from the daemon.
type ctlError struct {
	Status  int
	Code    string
	Message string
	Hint    string
}

func (e *ctlError) Error() string { return e.Message }

// ctlClient performs enveloped REST calls against the daemon socket.
type ctlClient struct {
	hc     *http.Client
	socket string
}

// newCtlClient resolves the control socket and builds the client. No I/O
// happens until the first call.
func (a *App) newCtlClient() (*ctlClient, error) {
	socket, err := a.resolver.CtlSocketPath()
	if err != nil {
		return nil, err
	}
	return &ctlClient{
		socket: socket,
		hc: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		}},
		// No client timeout: long polls come with caller-context deadlines
		// where needed.
	}, nil
}

// do performs one enveloped call. path is absolute ("/v1/...").
//
// Failure direction: a transport-level failure means the daemon is offline
// and maps to the frozen exit-4 error; anything that is not a well-formed
// success envelope is an error — a torn body never reads as success.
func (c *ctlClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encoding %s body: %w", path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agenthub"+path, body)
	if err != nil {
		return fmt.Errorf("building %s request: %w", path, err)
	}
	req.Header.Set(api.HeaderAPIVersion, api.APIVersion)
	req.Header.Set(ctlapi.HeaderActor, "cli")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return DaemonDownf("daemon is not reachable at %s", c.socket)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, ctlMaxResponse+1))
	if err != nil {
		return fmt.Errorf("%s: reading response: %w", path, err)
	}
	if len(raw) > ctlMaxResponse {
		return fmt.Errorf("%s: response exceeds %d bytes", path, ctlMaxResponse)
	}
	var env struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: status %d with undecodable body: %w", path, resp.StatusCode, err)
	}
	if env.Error != nil {
		return &ctlError{
			Status:  resp.StatusCode,
			Code:    env.Error.Code,
			Message: env.Error.Message,
			Hint:    env.Error.Hint,
		}
	}
	if !env.OK || resp.StatusCode >= 400 {
		return fmt.Errorf("%s: status %d without error body", path, resp.StatusCode)
	}
	if out != nil {
		if len(env.Data) == 0 {
			return fmt.Errorf("%s: success envelope missing data", path)
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("%s: decoding data: %w", path, err)
		}
	}
	return nil
}

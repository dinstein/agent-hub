package clients_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMergeGolden pins the merged BYTES for one representative of every
// configuration shape. Determinism is the contract here: the same input
// must always produce the same file, and every foreign key — siblings of
// the section at each nesting level, foreign server entries, unknown
// fields inside them — must come out the other side intact.
func TestMergeGolden(t *testing.T) {
	cases := []struct {
		name   string
		client string
		// file is the path relative to the project directory; a leading
		// "~/" makes it relative to the fake home instead.
		file   string
		before string
		golden string
	}{
		{
			name:   "mcpServers-map (claude-code project .mcp.json)",
			client: "claude-code",
			file:   ".mcp.json",
			before: `{
  "$schema": "https://example.com/mcp.schema.json",
  "mcpServers": {
    "other": {
      "command": "npx",
      "args": ["-y", "some-server"],
      "env": {"TOKEN": "keep-me"},
      "vendorExtension": {"nested": [1, 2, 3]}
    }
  },
  "unknownTop": {"keep": ["me", true, null]}
}`,
			golden: `{
  "$schema": "https://example.com/mcp.schema.json",
  "mcpServers": {
    "agenthub": {
      "command": "/opt/agenthub/bin/agenthub",
      "args": [
        "connect",
        "--client",
        "claude-code"
      ]
    },
    "other": {
      "command": "npx",
      "args": [
        "-y",
        "some-server"
      ],
      "env": {
        "TOKEN": "keep-me"
      },
      "vendorExtension": {
        "nested": [
          1,
          2,
          3
        ]
      }
    }
  },
  "unknownTop": {
    "keep": [
      "me",
      true,
      null
    ]
  }
}
`,
		},
		{
			name:   "mcpServers-map created from nothing (cursor user file)",
			client: "cursor",
			file:   "~/.cursor/mcp.json",
			before: "",
			golden: `{
  "mcpServers": {
    "agenthub": {
      "command": "/opt/agenthub/bin/agenthub",
      "args": [
        "connect",
        "--client",
        "cursor"
      ]
    }
  }
}
`,
		},
		{
			name:   "nested two-level section (vscode settings.json mcp.servers)",
			client: "vscode",
			file:   "~/Library/Application Support/Code/User/settings.json",
			before: `{
  "editor.fontSize": 13,
  "mcp": {
    "inputs": [{"id": "tok", "type": "promptString"}],
    "servers": {
      "fetch": {"command": "uvx", "args": ["mcp-server-fetch"]}
    }
  },
  "workbench.colorTheme": "Default Dark+"
}`,
			golden: `{
  "editor.fontSize": 13,
  "mcp": {
    "inputs": [
      {
        "id": "tok",
        "type": "promptString"
      }
    ],
    "servers": {
      "agenthub": {
        "command": "/opt/agenthub/bin/agenthub",
        "args": [
          "connect",
          "--client",
          "vscode"
        ]
      },
      "fetch": {
        "command": "uvx",
        "args": [
          "mcp-server-fetch"
        ]
      }
    }
  },
  "workbench.colorTheme": "Default Dark+"
}
`,
		},
		{
			name:   "nested section created inside an existing document (zed context_servers)",
			client: "zed",
			file:   "~/.config/zed/settings.json",
			before: `{"theme": "One Dark", "vim_mode": true}`,
			golden: `{
  "context_servers": {
    "agenthub": {
      "command": "/opt/agenthub/bin/agenthub",
      "args": [
        "connect",
        "--client",
        "zed"
      ]
    }
  },
  "theme": "One Dark",
  "vim_mode": true
}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, "darwin")
			path := resolveTestPath(e, tc.file)
			if tc.before != "" {
				write(t, path, tc.before)
			}
			res, err := e.format(t, tc.client).Connect(path, entry(tc.client))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			if !res.Changed {
				t.Fatalf("result = %+v, want a write", res)
			}
			if got := read(t, path); got != tc.golden {
				t.Errorf("merged file mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, tc.golden)
			}
			// The previous bytes are recoverable from the central backup.
			if tc.before != "" {
				if res.Backup == "" {
					t.Fatal("existing file overwritten without a backup")
				}
				if got := read(t, res.Backup); got != tc.before {
					t.Errorf("backup mismatch:\n%s", got)
				}
			} else if res.Backup != "" {
				t.Errorf("backup %q written for a file that did not exist", res.Backup)
			}
		})
	}
}

// TestManualSnippetGolden pins the fragments the probe-only and remote
// shapes hand back. These strings are the ONLY remedy for those clients,
// so they are a contract just as much as merged bytes are.
func TestManualSnippetGolden(t *testing.T) {
	e := newEnv(t, "darwin")
	cases := map[string]string{
		"codex": `[mcp_servers.agenthub]
command = "/opt/agenthub/bin/agenthub"
args = ["connect", "--client", "codex"]
`,
		"continue": `mcpServers:
  - name: agenthub
    command: "/opt/agenthub/bin/agenthub"
    args:
      - "connect"
      - "--client"
      - "continue"
`,
		"open-webui": `# Open WebUI has no local MCP configuration file.
# 1. start the shared daemon:   /opt/agenthub/bin/agenthub daemon
# 2. register the daemon's streamable-http MCP endpoint in Open WebUI.
# ` + "`/opt/agenthub/bin/agenthub connect --client open-webui`" + ` is the stdio entry point and is NOT used here.
`,
	}
	for id, want := range cases {
		if got := e.format(t, id).ManualSnippet(entry(id)); got != want {
			t.Errorf("%s snippet:\n--- got ---\n%s\n--- want ---\n%s", id, got, want)
		}
	}

	// JSON shapes render the fragment for their own section path.
	wantVSCode := `{
  "servers": {
    "agenthub": {
      "args": [
        "connect",
        "--client",
        "vscode"
      ],
      "command": "/opt/agenthub/bin/agenthub"
    }
  }
}
`
	if got := e.format(t, "vscode").ManualSnippet(entry("vscode")); got != wantVSCode {
		t.Errorf("vscode snippet:\n--- got ---\n%s\n--- want ---\n%s", got, wantVSCode)
	}
}

// resolveTestPath maps "~/x" onto the fake home and everything else onto
// the fake project directory.
func resolveTestPath(e env, rel string) string {
	if strings.HasPrefix(rel, "~/") {
		return filepath.Join(e.home, filepath.FromSlash(rel[2:]))
	}
	return filepath.Join(e.project, filepath.FromSlash(rel))
}

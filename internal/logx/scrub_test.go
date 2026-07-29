package logx_test

import (
	"bytes"
	"errors"
	"log/slog"
	"testing"

	"github.com/dinstein/agent-hub/internal/logx"
)

// goldenLogger returns a scrub-wrapped JSON logger writing to buf with the
// time attribute dropped, so output is fully deterministic and comparable
// as exact strings.
func goldenLogger(buf *bytes.Buffer) *slog.Logger {
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{} // normalize the unstable timestamp away
			}
			return a
		},
	})
	return slog.New(logx.NewScrubHandler(h))
}

// TestScrubGolden pins the exact JSON output for fixed inputs. These lines
// are contract: a diff here means the scrubbing behavior changed and must
// be reviewed as a security-relevant change.
func TestScrubGolden(t *testing.T) {
	tests := []struct {
		name string
		log  func(l *slog.Logger)
		want string
	}{
		{
			name: "clean record with mandatory fields untouched",
			log: func(l *slog.Logger) {
				l.Info("calling downstream",
					logx.Server("github"), logx.Tool("create_issue"),
					logx.Client("cursor"), logx.Session("cursor:3"), logx.Rev(7))
			},
			want: `{"level":"INFO","msg":"calling downstream","server":"github","tool":"create_issue","client":"cursor","session":"cursor:3","rev":7}`,
		},
		{
			name: "authorization header in attr value",
			log: func(l *slog.Logger) {
				l.Info("header dump", slog.String("h", "Authorization: Bearer abc123.def-456"))
			},
			want: `{"level":"INFO","msg":"header dump","h":"Authorization: [REDACTED]"}`,
		},
		{
			name: "AGENTHUB_SECRET env assignment in message",
			log: func(l *slog.Logger) {
				l.Info("env AGENTHUB_SECRET_GITHUB=ghp_abcdefghij1234567890 loaded")
			},
			want: `{"level":"INFO","msg":"env AGENTHUB_SECRET_GITHUB=[REDACTED] loaded"}`,
		},
		{
			name: "bare bearer token in message",
			log: func(l *slog.Logger) {
				l.Info("retrying with Bearer tok.en_value-1 now")
			},
			want: `{"level":"INFO","msg":"retrying with Bearer [REDACTED] now"}`,
		},
		{
			name: "github token shape in attr value",
			log: func(l *slog.Logger) {
				l.Warn("possible leak", slog.String("v", "saw ghp_ABCDEFGHIJKLMNOPQRST1234 in output"))
			},
			want: `{"level":"WARN","msg":"possible leak","v":"saw [REDACTED] in output"}`,
		},
		{
			name: "openai style key in message",
			log: func(l *slog.Logger) {
				l.Info("found sk-proj-abcd1234efgh5678ijkl in env")
			},
			want: `{"level":"INFO","msg":"found [REDACTED] in env"}`,
		},
		{
			name: "jwt shape in message",
			log: func(l *slog.Logger) {
				l.Info("got eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dGVzdHNpZ25hdHVyZQ back")
			},
			want: `{"level":"INFO","msg":"got [REDACTED] back"}`,
		},
		{
			name: "generic long random value redacted, short value kept",
			log: func(l *slog.Logger) {
				l.Info("oauth callback", slog.String("q", "code=abc&state=abc123def456ghi789jkl012mno345pqr678stu9"))
			},
			want: `{"level":"INFO","msg":"oauth callback","q":"code=abc&state=[REDACTED]"}`,
		},
		{
			name: "filesystem path is not mistaken for a secret",
			log: func(l *slog.Logger) {
				l.Info("writing", slog.String("path", "/home/alice/.local/share/agenthub/logs/server-github.log"))
			},
			want: `{"level":"INFO","msg":"writing","path":"/home/alice/.local/share/agenthub/logs/server-github.log"}`,
		},
		{
			name: "long purely alphabetic identifier is kept",
			log: func(l *slog.Logger) {
				l.Info("id", slog.String("v", "name=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
			},
			want: `{"level":"INFO","msg":"id","v":"name=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		},
		{
			name: "sensitive attr key masks whole value",
			log: func(l *slog.Logger) {
				l.Info("auth done", slog.String("access_token", "short"))
			},
			want: `{"level":"INFO","msg":"auth done","access_token":"[REDACTED]"}`,
		},
		{
			name: "sensitive attr key masks non-string value too",
			log: func(l *slog.Logger) {
				l.Info("cfg", slog.Any("password", 12345))
			},
			want: `{"level":"INFO","msg":"cfg","password":"[REDACTED]"}`,
		},
		{
			name: "sensitive key match is substring based and fail-closed",
			log: func(l *slog.Logger) {
				l.Info("usage", slog.Int("prompt_token_count", 5))
			},
			want: `{"level":"INFO","msg":"usage","prompt_token_count":"[REDACTED]"}`,
		},
		{
			name: "api-key spelling variants are masked",
			log: func(l *slog.Logger) {
				l.Info("cfg", slog.String("Api-Key", "v1"), slog.String("apikey", "v2"))
			},
			want: `{"level":"INFO","msg":"cfg","Api-Key":"[REDACTED]","apikey":"[REDACTED]"}`,
		},
		{
			name: "nested group attrs are walked",
			log: func(l *slog.Logger) {
				l.Info("request", slog.Group("http",
					slog.String("url", "https://api.example.com"),
					slog.String("apiKey", "supersecret")))
			},
			want: `{"level":"INFO","msg":"request","http":{"url":"https://api.example.com","apiKey":"[REDACTED]"}}`,
		},
		{
			name: "WithAttrs bound attrs are scrubbed",
			log: func(l *slog.Logger) {
				l.With(slog.String("secret", "s3cr3t"), logx.Client("cursor")).Info("boot")
			},
			want: `{"level":"INFO","msg":"boot","secret":"[REDACTED]","client":"cursor"}`,
		},
		{
			name: "error attr text is scrubbed",
			log: func(l *slog.Logger) {
				l.Error("call failed", slog.Any("err", errors.New("upstream said: Bearer abc123 rejected")))
			},
			want: `{"level":"ERROR","msg":"call failed","err":"upstream said: Bearer [REDACTED] rejected"}`,
		},
		{
			name: "non-sensitive non-string attrs pass through",
			log: func(l *slog.Logger) {
				l.Info("retry", slog.Int("attempt", 3), slog.Bool("last", false))
			},
			want: `{"level":"INFO","msg":"retry","attempt":3,"last":false}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			tt.log(goldenLogger(&buf))
			got := buf.String()
			want := tt.want + "\n"
			if got != want {
				t.Fatalf("golden mismatch\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func TestScrubString(t *testing.T) {
	tests := []struct{ in, want string }{
		{"nothing to hide", "nothing to hide"},
		{"token=abc", "token=" + logx.Redacted},
		{`password: "hunter2" done`, `password: "` + logx.Redacted + `" done`},
		{"AKIAIOSFODNN7EXAMPLE used", logx.Redacted + " used"},
		{"xoxb-1234567890-abcdef ok", logx.Redacted + " ok"},
		// Value shorter than 32 chars with a non-sensitive key is kept.
		{"trace=abc123", "trace=abc123"},
	}
	for _, tt := range tests {
		if got := logx.ScrubString(tt.in); got != tt.want {
			t.Errorf("ScrubString(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSensitiveKey(t *testing.T) {
	sensitive := []string{"secret", "AGENTHUB_SECRET_FOO", "access-token", "Password", "passwd", "Authorization", "api_key", "apiKey", "aws_access_key_id", "credentials", "bearer"}
	for _, k := range sensitive {
		if !logx.SensitiveKey(k) {
			t.Errorf("SensitiveKey(%q) = false, want true", k)
		}
	}
	clean := []string{"server", "tool", "client", "session", "rev", "path", "url", "attempt"}
	for _, k := range clean {
		if logx.SensitiveKey(k) {
			t.Errorf("SensitiveKey(%q) = true, want false", k)
		}
	}
}

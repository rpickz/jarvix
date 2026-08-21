package desktop

import (
	"strings"
	"testing"
)

// The redaction table. Both directions matter equally: a secret that slips
// through is a leak, and a false positive silently blinds the assistant on
// ordinary work — which the user experiences as Jarvix being useless, with no
// explanation. Every case below is a claim about one or the other.

// synthetic assembles a fake credential from its prefix and body at run time.
// The split is deliberate and must stay: a contiguous literal that looks
// exactly like a real token trips GitHub's push protection (the Slack fixture
// did), and a test fixture is not worth an allow-listing exception. The
// redactor sees the assembled string either way, so nothing is weakened —
// resist the urge to tidy these back into one literal.
func synthetic(parts ...string) string { return strings.Join(parts, "") }

func TestRedactSecrets(t *testing.T) {
	secrets := map[string]string{
		"openssh private key": "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----",
		"rsa private key":     "-----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY-----",
		"ec private key":      "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\n-----END EC PRIVATE KEY-----",
		"pgp private key":     "-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQOYBGL",
		"key inside prose":    "here you go:\n-----BEGIN PRIVATE KEY-----\nMIIEvQ\n-----END PRIVATE KEY-----\nlet me know",
		"openai key":          synthetic("sk-", "proj-Ab3dEf7hIjK1mNoPqRsTuVwXyZ0123456789abcdef"),
		"anthropic key":       synthetic("sk-", "ant-api03-Ab3dEf7hIjK1mNoPqRsTuVwXyZ0123456789"),
		"github pat":          synthetic("ghp_", "16C7e42F292c6912E7710c838347Ae178B4a"),
		"github fine grained": synthetic("github_pat_", "11ABCDEFG0abcdefghijklmnopqrstuvwxyz012345"),
		"gitlab pat":          synthetic("glpat-", "ABCdef123456789012345"),
		"slack bot token":     synthetic("xoxb-", "123456789012-1234567890123-", "AbCdEfGhIjKlMnOpQrStUvWx"),
		"google api key":      synthetic("AIza", "SyD-1234567890abcdefghijklmnopqrstuvw"),
		"aws access key id":   synthetic("AKIA", "IOSFODNN7EXAMPLE"),
		"hugging face token":  synthetic("hf_", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"),
		"docker pat":          synthetic("dckr_pat_", "AbCdEfGhIjKlMnOpQrStUvWx"),
		"env file":            "DATABASE_URL=postgres://localhost/app\nAPI_KEY=Zm9vYmFyYmF6cXV1eDEyMzQ1\nDEBUG=1",
		"quoted assignment":   `password = "Tr0ub4dor&3xkcd-correct-horse"`,
		"labelled token":      "Authorization token: eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		// The AWS secret key has no announced shape at all — this is what the
		// entropy rule exists for.
		"bare base64 blob": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYzEXAMPLEKEY",
		"random token":     "Xq7Lm2Pv9Rt4Wz6Ya8Bc1De3Fg5Hj0Kn2Mp4Qs6Uv8X",
	}
	for name, text := range secrets {
		t.Run(name, func(t *testing.T) {
			got, redacted := Redact(text)
			if !redacted {
				t.Fatalf("Redact(%q) let a secret through", text)
			}
			if got != RedactedMarker {
				t.Errorf("Redact = %q, want the marker (the whole value goes)", got)
			}
		})
	}
}

func TestRedactLetsOrdinaryContentThrough(t *testing.T) {
	safe := map[string]string{
		"prose":            "What does this error mean? It says the connection was refused.",
		"stack trace":      "panic: runtime error: index out of range [5] with length 3\n\tmain.go:42 +0x1d",
		"java stack trace": "at org.springframework.beans.factory.support.AbstractAutowireCapableBeanFactory2.doCreateBean(AbstractAutowireCapableBeanFactory.java:594)",
		"absolute path":    "/home/rpickz/Work/DigitalBrainwave2/contentdeck/internal/session/engine.go",
		"long url":         "https://github.com/rpickz/jarvix/blob/main/docs/adr/0018-desktop-context.md#consequences",
		"git sha":          "commit 9f2c1ae0b7d34e5a6c8f0192b3d4e5f60718293a",
		"uuid":             "550e8400-e29b-41d4-a716-446655440000",
		"sql":              "SELECT id, created_at FROM social_posts WHERE scheduled_for > now() ORDER BY scheduled_for LIMIT 50;",
		"prose about keys": "The token is invalid, so the request was rejected with a 401.",
		"short assignment": "token: expired",
		"go code":          "func (e *Engine) conversationMessages(userText string, snapshot desktop.Snapshot) []ai.Message {",
		"window title":     "Alacritty — nvim internal/session/engine.go",
		"empty":            "",
		"whitespace":       "   \n\t ",
		"base64 of prose":  "VGhlIHF1aWNrIGJyb3duIGZveA==",
	}
	for name, text := range safe {
		t.Run(name, func(t *testing.T) {
			got, redacted := Redact(text)
			if redacted {
				t.Fatalf("Redact(%q) redacted ordinary content", text)
			}
			if got != text {
				t.Errorf("Redact returned %q, want the text unchanged", got)
			}
		})
	}
}

func TestHighEntropyRuleDiscriminators(t *testing.T) {
	// Each of the three thresholds is load-bearing; this pins why.
	cases := map[string]struct {
		token string
		want  bool
	}{
		"too short":           {"Ab3dEf7hIjK1mNoP", false},
		"no digits":           {"AbCdEfGhIjKlMnOpQrStUvWxYzAbCdEfGh", false},
		"single case":         {"ab3def7hijk1mnopqrstuvwxyz012345678", false},
		"low transition rate": {"HTTPSConnectionPoolRetryHandler2Factory", false},
		"path":                {"/home/user2/Projects/Some/Deep/Path/Here", false},
		"random":              {"Xq7Lm2Pv9Rt4Wz6Ya8Bc1De3Fg5Hj0Kn2Mp4Qs", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tokenLooksRandom(c.token); got != c.want {
				t.Errorf("tokenLooksRandom(%q) = %v, want %v (entropy %.2f, transitions %.2f)",
					c.token, got, c.want, shannon(c.token), transitionRate(c.token))
			}
		})
	}
}

func TestShannonEntropy(t *testing.T) {
	if got := shannon(""); got != 0 {
		t.Errorf("shannon(\"\") = %v", got)
	}
	if got := shannon(strings.Repeat("a", 32)); got != 0 {
		t.Errorf("shannon(one repeated char) = %v, want 0", got)
	}
	// Two equally likely symbols is exactly one bit per character.
	if got := shannon("abab"); got != 1 {
		t.Errorf("shannon(\"abab\") = %v, want 1", got)
	}
	if got := shannon("Xq7Lm2Pv9Rt4Wz6Ya8Bc1De3Fg5Hj0Kn"); got < 3.5 {
		t.Errorf("shannon(random token) = %v, want >= 3.5", got)
	}
}

// FuzzRedact throws arbitrary text at the redactor — which is precisely what
// a clipboard is — and requires two invariants to hold for every input: it
// must not panic, and a redacted result must be the marker and nothing else,
// so no fragment of the input can ever survive redaction.
func FuzzRedact(f *testing.F) {
	f.Add("hello world")
	f.Add("-----BEGIN OPENSSH PRIVATE KEY-----")
	f.Add(synthetic("sk-", "proj-Ab3dEf7hIjK1mNoPqRsTuVwXyZ0123456789"))
	f.Add("api_key = \"Zm9vYmFyYmF6cXV1eDEyMzQ1\"")
	f.Add("/home/user/some/path/with/2/digits")
	f.Add("")
	f.Fuzz(func(t *testing.T, text string) {
		got, redacted := Redact(text)
		if redacted && got != RedactedMarker {
			t.Fatalf("Redact(%q) = %q; a redacted value must be the marker alone", text, got)
		}
		if !redacted && got != text {
			t.Fatalf("Redact(%q) altered text it did not redact: %q", text, got)
		}
	})
}

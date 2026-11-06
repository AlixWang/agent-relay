package resources

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseMCPValid(t *testing.T) {
	canon, refs, err := ParseMCP([]byte(`{"transport":"stdio","command":"npx",
		"args":["-y","@modelcontextprotocol/server-github","--token=${secret:GH}"],
		"env":{"Z":"1","GITHUB_TOKEN":"${secret:GH}","OTHER":"pre-${secret:OAI}-post"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(refs, []string{"GH", "OAI"}) {
		t.Fatalf("refs %v", refs)
	}
	// Canonical form is stable regardless of input key order / whitespace.
	canon2, _, _ := ParseMCP([]byte(`{"env":{"OTHER":"pre-${secret:OAI}-post","GITHUB_TOKEN":"${secret:GH}","Z":"1"},"command":"npx","transport":"stdio","args":["-y","@modelcontextprotocol/server-github","--token=${secret:GH}"]}`))
	if string(canon) != string(canon2) {
		t.Fatalf("canonical differs:\n%s\n%s", canon, canon2)
	}
	if _, refs, err := ParseMCP([]byte(`{"transport":"http","url":"https://mcp.example.com/${secret:PATH}/sse","headers":{"Authorization":"Bearer ${secret:TOK}"}}`)); err != nil || len(refs) != 2 {
		t.Fatalf("http: %v %v", refs, err)
	}
}

func TestParseMCPInvalid(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":         `{`,
		"unknown field":    `{"transport":"stdio","command":"x","cwd":"/"}`,
		"bad transport":    `{"transport":"ws","url":"wss://x"}`,
		"stdio no command": `{"transport":"stdio"}`,
		"stdio with url":   `{"transport":"stdio","command":"x","url":"https://x"}`,
		"http no url":      `{"transport":"http"}`,
		"http with cmd":    `{"transport":"http","url":"https://x","command":"y"}`,
		"relative url":     `{"transport":"http","url":"/sse"}`,
		"ftp url":          `{"transport":"http","url":"ftp://x"}`,
		"secret in cmd":    `{"transport":"stdio","command":"${secret:X}"}`,
		"malformed ref":    `{"transport":"stdio","command":"x","env":{"A":"${secret:bad name}"}}`,
		"unclosed ref":     `{"transport":"stdio","command":"x","env":{"A":"${secret:X"}}`,
		"bad env key":      `{"transport":"stdio","command":"x","env":{"A-B":"1"}}`,
		"header newline":   `{"transport":"http","url":"https://x","headers":{"A":"1\r\nB: 2"}}`,
		"bad header key":   `{"transport":"http","url":"https://x","headers":{"A B":"1"}}`,
		"trailing data":    `{"transport":"stdio","command":"x"} {}`,
	} {
		if _, _, err := ParseMCP([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if _, _, err := ParseMCP([]byte(`{"transport":"stdio","command":"` + strings.Repeat("x", maxMCPBytes) + `"}`)); !errors.Is(err, ErrInvalid) {
		t.Error("oversized accepted")
	}
}

func TestResolveMCP(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, _ = svc.PutSecret("GH", "", "ghp_123")
	_, _ = svc.PutSecret("OAI", "", "sk-9")
	if _, err := svc.PutMCP("github", "gh server", []byte(`{"transport":"stdio","command":"npx",
		"args":["--token=${secret:GH}"],"env":{"GITHUB_TOKEN":"${secret:GH}","K":"${secret:OAI}"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveMCP("alice", "github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted: %v", err)
	}
	_ = svc.Grant(KindMCP, "github", "alice")
	_ = svc.Grant(KindSecret, "GH", "alice")
	_, _, err := svc.ResolveMCP("alice", "github")
	var miss *MissingSecretsError
	if !errors.As(err, &miss) || !reflect.DeepEqual(miss.Names, []string{"OAI"}) || !errors.Is(err, ErrMissingSecrets) {
		t.Fatalf("missing: %v", err)
	}
	if m, _ := svc.MissingForGrant("github", "alice"); !reflect.DeepEqual(m, []string{"OAI"}) {
		t.Fatalf("MissingForGrant: %v", m)
	}
	_ = svc.Grant(KindSecret, "OAI", AllPeers)
	if m, _ := svc.MissingForGrant("github", AllPeers); !reflect.DeepEqual(m, []string{"GH"}) {
		t.Fatalf("MissingForGrant(*): %v", m)
	}
	cfg, revealed, err := svc.ResolveMCP("alice", "github")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Args[0] != "--token=ghp_123" || cfg.Env["GITHUB_TOKEN"] != "ghp_123" || cfg.Env["K"] != "sk-9" {
		t.Fatalf("resolved: %+v", cfg)
	}
	if !reflect.DeepEqual(revealed, []string{"GH", "OAI"}) {
		t.Fatalf("revealed: %v", revealed)
	}
	// The stored template and the manifest keep the references unresolved.
	m, _ := svc.Manifest("alice")
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), "ghp_123") || !strings.Contains(string(b), "${secret:GH}") {
		t.Fatalf("manifest leaked or lost refs: %s", b)
	}
	var mcpEntry *ManifestEntry
	for _, e := range m.Resources {
		if e.Kind == KindMCP {
			mcpEntry = e
		}
	}
	if mcpEntry == nil || mcpEntry.FetchURL != "/resources/mcp/github" || !reflect.DeepEqual(mcpEntry.RequiresSecrets, []string{"GH", "OAI"}) {
		t.Fatalf("mcp entry: %+v", mcpEntry)
	}
}

func TestMCPDigestFollowsSecretRotation(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, _ = svc.PutSecret("GH", "", "a")
	_, _ = svc.PutMCP("github", "", []byte(`{"transport":"stdio","command":"x","env":{"T":"${secret:GH}"}}`))
	_ = svc.Grant(KindMCP, "github", "alice")
	_ = svc.Grant(KindSecret, "GH", "alice")
	m1, _ := svc.Manifest("alice")
	// Reformatted but equivalent config: same canonical bytes, same digest.
	_, _ = svc.PutMCP("github", "", []byte(`{ "env": {"T": "${secret:GH}"}, "command": "x", "transport": "stdio" }`))
	m2, _ := svc.Manifest("alice")
	if m1.ManifestRev != m2.ManifestRev {
		t.Fatal("equivalent mcp config moved the digest")
	}
	// Rotating a referenced secret moves the digest (the secret is in the manifest too).
	_, _ = svc.PutSecret("GH", "", "b")
	m3, _ := svc.Manifest("alice")
	if m3.ManifestRev == m2.ManifestRev {
		t.Fatal("rotation of a referenced secret did not move the digest")
	}
}

func TestPromptResource(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.PutPrompt("reviewer", "", "   "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank prompt: %v", err)
	}
	if _, err := svc.PutPrompt("reviewer", "code review tone", "你是代码审查员。"); err != nil {
		t.Fatal(err)
	}
	_ = svc.Grant(KindPrompt, "reviewer", AllPeers)
	m, _ := svc.Manifest("bob")
	if len(m.Resources) != 1 || m.Resources[0].Content != "你是代码审查员。" || m.Resources[0].FetchURL != "" {
		t.Fatalf("prompt entry: %+v", m.Resources[0])
	}
	v, _ := svc.Get(KindPrompt, "reviewer")
	if v.Content != "你是代码审查员。" || v.Size == 0 {
		t.Fatalf("admin view: %+v", v)
	}
}

// The neutral MCP shape is a contract with every assistant's prompt.
func TestMCPSchemaStability(t *testing.T) {
	var got []string
	ty := reflect.TypeOf(MCPConfig{})
	for i := 0; i < ty.NumField(); i++ {
		got = append(got, strings.Split(ty.Field(i).Tag.Get("json"), ",")[0])
	}
	want := []string{"transport", "command", "args", "env", "url", "headers"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MCPConfig fields %v, want %v", got, want)
	}
}

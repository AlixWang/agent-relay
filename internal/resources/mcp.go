package resources

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// MCPConfig is the runtime-neutral MCP server description (DESIGN §4.12).
// Each assistant runtime translates it into its own config format following
// its prompt. Field names are a wire contract: see TestMCPSchemaStability.
type MCPConfig struct {
	Transport string            `json:"transport"` // "stdio" | "http"
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"

	maxMCPBytes    = 16 << 10
	maxMCPArgs     = 64
	maxMCPMapItems = 64
	maxPromptBytes = 64 << 10
)

var (
	secretRefRe = regexp.MustCompile(`\$\{secret:([A-Za-z0-9][A-Za-z0-9_.-]{0,63})\}`)
	envKeyRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	headerKeyRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,128}$`)
)

const secretRefOpen = "${secret:"

// secretRefs returns the secret names referenced by s, or an error when s
// contains a "${secret:" that is not a well-formed reference — a typo must
// fail loudly at save time, not ship a literal "${secret:..." to a server.
func secretRefs(field, s string) ([]string, error) {
	matches := secretRefRe.FindAllStringSubmatch(s, -1)
	if strings.Count(s, secretRefOpen) != len(matches) {
		return nil, fmt.Errorf("%w: %s has a malformed ${secret:NAME} reference", ErrInvalid, field)
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out, nil
}

func noSecretRef(field, s string) error {
	if strings.Contains(s, secretRefOpen) {
		return fmt.Errorf("%w: ${secret:} is not allowed in %s", ErrInvalid, field)
	}
	return nil
}

func checkText(field, s string) error {
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return fmt.Errorf("%w: %s must be UTF-8 text", ErrInvalid, field)
	}
	return nil
}

// ParseMCP strictly decodes and validates a neutral MCP config. It returns
// the canonical JSON encoding (what gets stored and hashed) and the sorted,
// de-duplicated secret names it references.
func ParseMCP(raw []byte) (canonical []byte, refs []string, err error) {
	if len(raw) > maxMCPBytes {
		return nil, nil, fmt.Errorf("%w: mcp config larger than %d bytes", ErrInvalid, maxMCPBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c MCPConfig
	if err := dec.Decode(&c); err != nil {
		return nil, nil, fmt.Errorf("%w: mcp config: %v", ErrInvalid, err)
	}
	if dec.More() {
		return nil, nil, fmt.Errorf("%w: mcp config: trailing data", ErrInvalid)
	}
	set := map[string]bool{}
	add := func(field, s string) error {
		if err := checkText(field, s); err != nil {
			return err
		}
		names, err := secretRefs(field, s)
		for _, n := range names {
			set[n] = true
		}
		return err
	}
	switch c.Transport {
	case TransportStdio:
		if strings.TrimSpace(c.Command) == "" {
			return nil, nil, fmt.Errorf("%w: stdio transport needs command", ErrInvalid)
		}
		if c.URL != "" || len(c.Headers) > 0 {
			return nil, nil, fmt.Errorf("%w: stdio transport takes no url/headers", ErrInvalid)
		}
		if err := checkText("command", c.Command); err != nil {
			return nil, nil, err
		}
		if err := noSecretRef("command", c.Command); err != nil {
			return nil, nil, err
		}
		if len(c.Args) > maxMCPArgs {
			return nil, nil, fmt.Errorf("%w: more than %d args", ErrInvalid, maxMCPArgs)
		}
		for i, a := range c.Args {
			if err := add(fmt.Sprintf("args[%d]", i), a); err != nil {
				return nil, nil, err
			}
		}
		if len(c.Env) > maxMCPMapItems {
			return nil, nil, fmt.Errorf("%w: more than %d env entries", ErrInvalid, maxMCPMapItems)
		}
		for k, v := range c.Env {
			if !envKeyRe.MatchString(k) {
				return nil, nil, fmt.Errorf("%w: env name %q", ErrInvalid, k)
			}
			if err := add("env."+k, v); err != nil {
				return nil, nil, err
			}
		}
	case TransportHTTP:
		if c.Command != "" || len(c.Args) > 0 || len(c.Env) > 0 {
			return nil, nil, fmt.Errorf("%w: http transport takes no command/args/env", ErrInvalid)
		}
		if err := add("url", c.URL); err != nil {
			return nil, nil, err
		}
		// Validate the URL shape with references blanked out.
		u, perr := url.Parse(secretRefRe.ReplaceAllString(c.URL, "x"))
		if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, nil, fmt.Errorf("%w: url must be an absolute http(s) URL", ErrInvalid)
		}
		if len(c.Headers) > maxMCPMapItems {
			return nil, nil, fmt.Errorf("%w: more than %d headers", ErrInvalid, maxMCPMapItems)
		}
		for k, v := range c.Headers {
			if !headerKeyRe.MatchString(k) {
				return nil, nil, fmt.Errorf("%w: header name %q", ErrInvalid, k)
			}
			if strings.ContainsAny(v, "\r\n") {
				return nil, nil, fmt.Errorf("%w: header %s contains a newline", ErrInvalid, k)
			}
			if err := add("headers."+k, v); err != nil {
				return nil, nil, err
			}
		}
	default:
		return nil, nil, fmt.Errorf("%w: transport must be %q or %q", ErrInvalid, TransportStdio, TransportHTTP)
	}
	canonical, err = json.Marshal(c) // map keys sorted → stable hash
	if err != nil {
		return nil, nil, err
	}
	for n := range set {
		refs = append(refs, n)
	}
	sort.Strings(refs)
	return canonical, refs, nil
}

// substitute replaces every ${secret:NAME} in s using vals.
func substitute(s string, vals map[string]string) string {
	return secretRefRe.ReplaceAllStringFunc(s, func(m string) string {
		return vals[secretRefRe.FindStringSubmatch(m)[1]]
	})
}

func resolveMCP(c MCPConfig, vals map[string]string) MCPConfig {
	out := MCPConfig{Transport: c.Transport, Command: c.Command, URL: substitute(c.URL, vals)}
	for _, a := range c.Args {
		out.Args = append(out.Args, substitute(a, vals))
	}
	if c.Env != nil {
		out.Env = map[string]string{}
		for k, v := range c.Env {
			out.Env[k] = substitute(v, vals)
		}
	}
	if c.Headers != nil {
		out.Headers = map[string]string{}
		for k, v := range c.Headers {
			out.Headers[k] = substitute(v, vals)
		}
	}
	return out
}

// CheckPrompt validates a prompt snippet.
func CheckPrompt(text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("%w: prompt text required", ErrInvalid)
	}
	if len(text) > maxPromptBytes {
		return fmt.Errorf("%w: prompt larger than %d bytes", ErrInvalid, maxPromptBytes)
	}
	return checkText("prompt", text)
}

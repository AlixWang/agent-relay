// Package resources is the resource hub (DESIGN §4.12, ADR 0004/0005): the
// operator registers a secret / MCP config / skill / prompt snippet once,
// grants it to peers, and assistants pull it. The relay distributes config
// only — it never proxies MCP calls or runs skills.
package resources

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AlixWang/agent-relay/internal/store"
)

// Resource kinds. The set is closed: the manifest schema and the assistant
// prompt both depend on it.
const (
	KindSecret = "secret"
	KindMCP    = "mcp"
	KindSkill  = "skill"
	KindPrompt = "prompt"
)

// AllPeers is the wildcard grant target.
const AllPeers = "*"

const (
	maxSecretBytes  = 16 << 10
	maxDescRunes    = 500
	actorOperator   = "operator"
	emptyManifestRV = "" // see Manifest.NeedsUpdate
)

var (
	ErrNotFound        = errors.New("resource not found")
	ErrInvalid         = errors.New("invalid resource")
	ErrUnknownPeer     = errors.New("unknown peer")
	ErrUnsupportedKind = errors.New("unsupported resource kind")
	// ErrMissingSecrets: an MCP config references secrets the peer is not
	// granted (or that no longer exist). Wrapped by *MissingSecretsError.
	ErrMissingSecrets = errors.New("mcp config references secrets not granted")
)

// MissingSecretsError lists the ${secret:NAME} references a peer cannot resolve.
type MissingSecretsError struct{ Names []string }

func (e *MissingSecretsError) Error() string {
	return ErrMissingSecrets.Error() + ": " + strings.Join(e.Names, ", ")
}
func (e *MissingSecretsError) Unwrap() error { return ErrMissingSecrets }

// Names appear in URL paths and in ${secret:NAME} references, so keep them to
// a conservative, case-sensitive charset.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ValidName reports whether name is usable as a resource name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

func knownKind(kind string) bool {
	switch kind {
	case KindSecret, KindMCP, KindSkill, KindPrompt:
		return true
	}
	return false
}

// enabledKind gates kinds whose validation/distribution has shipped.
func enabledKind(kind string) bool { return kind != KindSkill && knownKind(kind) }

// Service owns validation, secret encryption and manifest computation.
type Service struct {
	st  store.Store
	key []byte
	now func() time.Time
}

// New loads or creates data_dir/secrets.key.
func New(st store.Store, dataDir string) (*Service, error) {
	key, err := loadOrCreateKey(dataDir)
	if err != nil {
		return nil, err
	}
	return &Service{st: st, key: key, now: time.Now}, nil
}

// View is the admin-safe description of a live resource. It never carries
// secret plaintext or ciphertext.
type View struct {
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	RevisionID  string   `json:"revision_id"`
	Size        int      `json:"size"`
	CreatedBy   string   `json:"created_by"`
	UpdatedAt   int64    `json:"updated_at"`
	Grants      []string `json:"grants"`
	// Content is the editable body for mcp (neutral config object) and
	// prompt (text). Never set for secrets.
	Content any `json:"content,omitempty"`
	// RequiresSecrets lists an MCP config's ${secret:NAME} references.
	RequiresSecrets []string `json:"requires_secrets,omitempty"`
}

// CheckKind reports ErrInvalid for an unknown kind and ErrUnsupportedKind for
// a known kind this build cannot store yet.
func CheckKind(kind string) error {
	if !knownKind(kind) {
		return fmt.Errorf("%w: %q", ErrInvalid, kind)
	}
	if !enabledKind(kind) {
		return fmt.Errorf("%w: %s", ErrUnsupportedKind, kind)
	}
	return nil
}

func cleanDesc(d string) (string, error) {
	d = strings.TrimSpace(d)
	if !utf8.ValidString(d) {
		return "", fmt.Errorf("%w: description must be UTF-8", ErrInvalid)
	}
	if utf8.RuneCountInString(d) > maxDescRunes {
		return "", fmt.Errorf("%w: description longer than %d characters", ErrInvalid, maxDescRunes)
	}
	return d, nil
}

// PutSecret creates or overwrites a secret. An empty value on an existing
// secret keeps the stored value and only updates the description (a new
// revision with identical content, so the manifest digest does not move).
func (s *Service) PutSecret(name, description, value string) (*View, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("%w: name must match %s", ErrInvalid, nameRe)
	}
	desc, err := cleanDesc(description)
	if err != nil {
		return nil, err
	}
	cur, err := s.st.GetResource(KindSecret, name)
	if err != nil {
		return nil, err
	}
	rev := &store.ResourceRevision{
		ID: newRevisionID(), Kind: KindSecret, Name: name, Description: desc,
		CreatedAt: s.now().Unix(), CreatedBy: actorOperator,
	}
	switch {
	case value == "" && cur == nil:
		return nil, fmt.Errorf("%w: value required", ErrInvalid)
	case value == "":
		rev.Content, rev.ContentHash = cur.Content, cur.ContentHash
	default:
		if len(value) > maxSecretBytes {
			return nil, fmt.Errorf("%w: value larger than %d bytes", ErrInvalid, maxSecretBytes)
		}
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("%w: value must be UTF-8 text", ErrInvalid)
		}
		sealed, err := seal(s.key, []byte(value))
		if err != nil {
			return nil, err
		}
		rev.Content, rev.ContentHash = sealed, secretHash(s.key, value)
	}
	if err := s.st.PutResource(rev); err != nil {
		return nil, err
	}
	return s.view(KindSecret, name)
}

func (s *Service) view(kind, name string) (*View, error) {
	r, err := s.st.GetResource(kind, name)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrNotFound
	}
	grants, err := s.st.ListResourceGrants(kind, name)
	if err != nil {
		return nil, err
	}
	return toView(r, grants), nil
}

func toView(r *store.Resource, grants []*store.ResourceGrant) *View {
	v := &View{
		Kind: r.Kind, Name: r.Name, Description: r.Description, RevisionID: r.ID,
		Size: len(r.Content), CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		Grants: []string{},
	}
	if r.Kind == KindSecret {
		v.Size = 0 // ciphertext length leaks the plaintext length; not useful anyway
	}
	for _, g := range grants {
		v.Grants = append(v.Grants, g.PeerID)
	}
	v.Content, v.RequiresSecrets = decodeContent(r.Kind, r.Content)
	return v
}

// decodeContent turns stored mcp/prompt bytes into their wire form.
func decodeContent(kind string, b []byte) (any, []string) {
	switch kind {
	case KindMCP:
		var c MCPConfig
		if json.Unmarshal(b, &c) != nil {
			return nil, nil
		}
		_, refs, err := ParseMCP(b)
		if err != nil {
			refs = nil
		}
		return c, refs
	case KindPrompt:
		return string(b), nil
	}
	return nil, nil
}

// PutMCP creates or overwrites an MCP config (canonicalized before storing).
func (s *Service) PutMCP(name, description string, raw []byte) (*View, error) {
	canonical, _, err := ParseMCP(raw)
	if err != nil {
		return nil, err
	}
	return s.putPlain(KindMCP, name, description, canonical)
}

// PutPrompt creates or overwrites a prompt snippet.
func (s *Service) PutPrompt(name, description, text string) (*View, error) {
	if err := CheckPrompt(text); err != nil {
		return nil, err
	}
	return s.putPlain(KindPrompt, name, description, []byte(text))
}

func (s *Service) putPlain(kind, name, description string, content []byte) (*View, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("%w: name must match %s", ErrInvalid, nameRe)
	}
	desc, err := cleanDesc(description)
	if err != nil {
		return nil, err
	}
	rev := &store.ResourceRevision{
		ID: newRevisionID(), Kind: kind, Name: name, Description: desc,
		Content: content, ContentHash: contentHash(content),
		CreatedAt: s.now().Unix(), CreatedBy: actorOperator,
	}
	if err := s.st.PutResource(rev); err != nil {
		return nil, err
	}
	return s.view(kind, name)
}

// ResolveMCP returns peerID's MCP config with every ${secret:NAME} filled
// in, plus the secret names it revealed (for auditing). All-or-nothing: any
// reference the peer cannot resolve yields *MissingSecretsError.
func (s *Service) ResolveMCP(peerID, name string) (*MCPConfig, []string, error) {
	granted, err := s.st.ListGrantedResources(peerID)
	if err != nil {
		return nil, nil, err
	}
	var cfgRes *store.Resource
	secrets := map[string]*store.Resource{}
	for _, r := range granted {
		switch {
		case r.Kind == KindMCP && r.Name == name:
			cfgRes = r
		case r.Kind == KindSecret:
			secrets[r.Name] = r
		}
	}
	if cfgRes == nil {
		return nil, nil, ErrNotFound
	}
	_, refs, err := ParseMCP(cfgRes.Content)
	if err != nil {
		return nil, nil, fmt.Errorf("resources: stored mcp %s invalid: %w", name, err)
	}
	var missing []string
	vals := map[string]string{}
	for _, ref := range refs {
		r, ok := secrets[ref]
		if !ok {
			missing = append(missing, ref)
			continue
		}
		plain, err := open(s.key, r.Content)
		if err != nil {
			return nil, nil, fmt.Errorf("resources: decrypt %s: %w", ref, err)
		}
		vals[ref] = string(plain)
	}
	if len(missing) > 0 {
		return nil, nil, &MissingSecretsError{Names: missing}
	}
	var c MCPConfig
	if err := json.Unmarshal(cfgRes.Content, &c); err != nil {
		return nil, nil, err
	}
	out := resolveMCP(c, vals)
	return &out, refs, nil
}

// MissingForGrant reports which secrets an MCP config needs that peerID
// (or, for AllPeers, the wildcard) would not get. Console hint only.
func (s *Service) MissingForGrant(name, peerID string) ([]string, error) {
	r, err := s.st.GetResource(KindMCP, name)
	if err != nil || r == nil {
		return nil, err
	}
	_, refs, err := ParseMCP(r.Content)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	if peerID == AllPeers {
		gs, err := s.st.ListResourceGrants(KindSecret, "")
		if err != nil {
			return nil, err
		}
		for _, g := range gs {
			if g.PeerID == AllPeers {
				have[g.Name] = true
			}
		}
	} else {
		rs, err := s.st.ListGrantedResources(peerID)
		if err != nil {
			return nil, err
		}
		for _, x := range rs {
			if x.Kind == KindSecret {
				have[x.Name] = true
			}
		}
	}
	var missing []string
	for _, ref := range refs {
		if !have[ref] {
			missing = append(missing, ref)
		}
	}
	return missing, nil
}

// Get returns one resource's admin view.
func (s *Service) Get(kind, name string) (*View, error) {
	if !knownKind(kind) {
		return nil, ErrNotFound
	}
	return s.view(kind, name)
}

// List returns admin views, optionally filtered by kind.
func (s *Service) List(kind string) ([]*View, error) {
	if kind != "" && !knownKind(kind) {
		return nil, fmt.Errorf("%w: %q", ErrInvalid, kind)
	}
	rs, err := s.st.ListResources(kind)
	if err != nil {
		return nil, err
	}
	grants, err := s.st.ListResourceGrants("", "")
	if err != nil {
		return nil, err
	}
	byRes := map[string][]*store.ResourceGrant{}
	for _, g := range grants {
		k := g.Kind + "\x00" + g.Name
		byRes[k] = append(byRes[k], g)
	}
	out := make([]*View, 0, len(rs))
	for _, r := range rs {
		out = append(out, toView(r, byRes[r.Kind+"\x00"+r.Name]))
	}
	return out, nil
}

// Delete removes a resource and its grants; history is kept.
func (s *Service) Delete(kind, name string) error {
	if !knownKind(kind) {
		return ErrNotFound
	}
	ok, err := s.st.DeleteResource(kind, name)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// Revisions lists a resource's history (no content).
func (s *Service) Revisions(kind, name string) ([]*store.ResourceRevision, error) {
	if !knownKind(kind) {
		return nil, ErrNotFound
	}
	return s.st.ListResourceRevisions(kind, name)
}

// Grant authorizes peerID (or AllPeers) to use a resource. Idempotent.
func (s *Service) Grant(kind, name, peerID string) error {
	if !knownKind(kind) {
		return ErrNotFound
	}
	r, err := s.st.GetResource(kind, name)
	if err != nil {
		return err
	}
	if r == nil {
		return ErrNotFound
	}
	if peerID != AllPeers {
		p, err := s.st.GetPeer(peerID)
		if err != nil {
			return err
		}
		if p == nil {
			return ErrUnknownPeer
		}
	}
	return s.st.GrantResource(kind, name, peerID, s.now().Unix())
}

// Revoke removes one grant. Returns ErrNotFound when it did not exist.
func (s *Service) Revoke(kind, name, peerID string) error {
	if !knownKind(kind) {
		return ErrNotFound
	}
	ok, err := s.st.RevokeResource(kind, name, peerID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// granted returns the live resource when peerID may use it, else nil.
func (s *Service) granted(peerID, kind, name string) (*store.Resource, error) {
	rs, err := s.st.ListGrantedResources(peerID)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.Kind == kind && r.Name == name {
			return r, nil
		}
	}
	return nil, nil
}

// RevealSecret returns the plaintext of a secret granted to peerID.
// Missing and not-granted are indistinguishable (ErrNotFound) by design.
func (s *Service) RevealSecret(peerID, name string) (string, error) {
	r, err := s.granted(peerID, KindSecret, name)
	if err != nil {
		return "", err
	}
	if r == nil {
		return "", ErrNotFound
	}
	plain, err := open(s.key, r.Content)
	if err != nil {
		return "", fmt.Errorf("resources: decrypt %s: %w", name, err)
	}
	return string(plain), nil
}

// ManifestEntry is one resource in a peer's manifest (ADR 0004). Field names
// are a wire contract with the assistant prompt: see TestManifestSchemaStability.
type ManifestEntry struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	RevisionID  string `json:"revision_id"`
	Hash        string `json:"hash"`
	UpdatedAt   int64  `json:"updated_at"`
	FetchURL    string `json:"fetch_url,omitempty"`
	Content     any    `json:"content,omitempty"`
	// RequiresSecrets lists the ${secret:NAME} references of an MCP entry.
	RequiresSecrets []string `json:"requires_secrets,omitempty"`
}

// Manifest is the response body of GET /resources/current.
type Manifest struct {
	ManifestRev string           `json:"manifest_rev"`
	Resources   []*ManifestEntry `json:"resources"`
}

// Manifest computes peerID's manifest and its digest.
func (s *Service) Manifest(peerID string) (*Manifest, error) {
	rs, err := s.st.ListGrantedResources(peerID)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Resources: []*ManifestEntry{}}
	for _, r := range rs {
		if !enabledKind(r.Kind) {
			continue // stored by a newer build; not distributable yet
		}
		e := &ManifestEntry{
			Kind: r.Kind, Name: r.Name, Description: r.Description,
			RevisionID: r.ID, Hash: r.ContentHash, UpdatedAt: r.UpdatedAt,
		}
		switch r.Kind {
		case KindSecret:
			e.FetchURL = "/resources/secrets/" + r.Name
		case KindMCP:
			// Inline template with ${secret:} unresolved; the resolved
			// config is a separate, audited fetch (ADR 0005).
			e.Content, e.RequiresSecrets = decodeContent(r.Kind, r.Content)
			e.FetchURL = "/resources/mcp/" + r.Name
		case KindPrompt:
			e.Content, _ = decodeContent(r.Kind, r.Content)
		}
		m.Resources = append(m.Resources, e)
	}
	m.ManifestRev = digest(m.Resources)
	return m, nil
}

// digest is SHA-256 over sorted "kind:name:hash" lines. Content-addressed, so
// recreating identical content does not nudge anybody. Empty manifest → "".
func digest(es []*ManifestEntry) string {
	if len(es) == 0 {
		return emptyManifestRV
	}
	lines := make([]string, 0, len(es))
	for _, e := range es {
		lines = append(lines, e.Kind+":"+e.Name+":"+e.Hash)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// NeedsUpdate reports whether a peer that last applied digest `applied`
// should be nudged towards `current`. A peer that never had resources and
// still has none is left alone. A peer whose grants all went away is nudged
// (so it deletes local copies) until it reports NoneMarker.
func NeedsUpdate(applied, current string) bool {
	return normalizeReceipt(applied) != current
}

// NoneMarker is what an assistant reports after syncing an empty manifest:
// heartbeat treats an empty string as "not reporting", so the empty digest
// needs a spelling of its own.
const NoneMarker = "none"

func normalizeReceipt(r string) string {
	if r == NoneMarker {
		return emptyManifestRV
	}
	return r
}

func newRevisionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("rr_%d", time.Now().UnixNano())
	}
	return "rr_" + hex.EncodeToString(b[:])
}

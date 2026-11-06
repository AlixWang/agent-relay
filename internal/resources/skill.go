package resources

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

// Skill = a SKILL.md directory packed as tar.gz, 1 MiB unpacked (DESIGN §4.12).
// The relay only stores and serves the archive; it never executes it.
const (
	maxSkillBytes   = 1 << 20 // total unpacked file bytes
	maxSkillFiles   = 256
	maxSkillName    = 200
	frontmatterName = "SKILL.md"
)

// SkillMeta is what the relay parses out of an archive for display.
type SkillMeta struct {
	Name        string
	Description string
	Files       int
	Size        int
}

// ParseSkill validates a tar.gz skill archive and returns its metadata.
//
// Rules: every entry must be a regular file with a clean relative path under
// a single top-level directory (or at the root), no absolute paths, no `..`,
// no links/devices; names are ASCII printable; total unpacked size and file
// count are capped; SKILL.md must exist with YAML frontmatter carrying
// non-empty `name` and `description`.
func ParseSkill(raw []byte) (*SkillMeta, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty archive", ErrInvalid)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: not a gzip archive: %v", ErrInvalid, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	meta := &SkillMeta{}
	var (
		skillMD     []byte
		skillMDPath string
		total       int
		roots       = map[string]bool{}
		seenFile    = map[string]bool{}
	)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: read archive: %v", ErrInvalid, err)
		}
		name := hdr.Name
		if err := checkEntryPath(name); err != nil {
			return nil, err
		}
		clean := path.Clean(name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("%w: %s: only regular files and directories are allowed", ErrInvalid, clean)
		}
		if seenFile[clean] {
			return nil, fmt.Errorf("%w: %s appears twice", ErrInvalid, clean)
		}
		seenFile[clean] = true
		parts := strings.Split(clean, "/")
		if len(parts) > 1 {
			roots[parts[0]] = true
		} else {
			roots[""] = true // a file at the archive root
		}
		meta.Files++
		if meta.Files > maxSkillFiles {
			return nil, fmt.Errorf("%w: more than %d files", ErrInvalid, maxSkillFiles)
		}
		data, err := io.ReadAll(io.LimitReader(tr, int64(maxSkillBytes)+1))
		if err != nil {
			return nil, fmt.Errorf("%w: read %s: %v", ErrInvalid, clean, err)
		}
		if len(data) > maxSkillBytes {
			return nil, fmt.Errorf("%w: %s alone exceeds %d bytes", ErrInvalid, clean, maxSkillBytes)
		}
		total += len(data)
		if total > maxSkillBytes {
			return nil, fmt.Errorf("%w: unpacked size exceeds %d bytes", ErrInvalid, maxSkillBytes)
		}
		if path.Base(clean) == frontmatterName && strings.Count(clean, "/") <= 1 {
			if skillMD != nil {
				return nil, fmt.Errorf("%w: more than one %s", ErrInvalid, frontmatterName)
			}
			skillMD, skillMDPath = data, clean
		}
	}
	meta.Size = total
	if meta.Files == 0 {
		return nil, fmt.Errorf("%w: archive has no files", ErrInvalid)
	}
	if skillMD == nil {
		return nil, fmt.Errorf("%w: %s missing (put it at the archive root or one level down)", ErrInvalid, frontmatterName)
	}
	// Either the skill directory itself (SKILL.md at the root) or exactly
	// one wrapper directory around it (`tar czf x.tgz skill-dir/`); never
	// several unrelated top-level trees.
	if wrapped := strings.Contains(skillMDPath, "/"); wrapped && len(roots) != 1 {
		return nil, fmt.Errorf("%w: archive must hold one skill directory", ErrInvalid)
	}
	meta.Name, meta.Description, err = parseSkillFrontmatter(skillMD)
	if err != nil {
		return nil, err
	}
	return meta, nil
}

func checkEntryPath(name string) error {
	if name == "" || len(name) > maxSkillName {
		return fmt.Errorf("%w: bad entry name %q", ErrInvalid, name)
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "\\") {
		return fmt.Errorf("%w: absolute path %q", ErrInvalid, name)
	}
	if strings.ContainsAny(name, "\\\x00") {
		return fmt.Errorf("%w: bad entry name %q", ErrInvalid, name)
	}
	for _, r := range name {
		if r < 0x20 || r > 0x7e {
			return fmt.Errorf("%w: entry name must be printable ASCII: %q", ErrInvalid, name)
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return fmt.Errorf("%w: path traversal in %q", ErrInvalid, name)
		}
	}
	return nil
}

// parseSkillFrontmatter reads the minimal YAML subset skills use:
//
//	---
//	name: code-review
//	description: Review code changes
//	---
//
// Unknown keys are ignored; continuation lines of a value (indented) are
// appended, so multi-line descriptions survive.
func parseSkillFrontmatter(b []byte) (name, description string, err error) {
	if !utf8.Valid(b) {
		return "", "", fmt.Errorf("%w: %s is not UTF-8", ErrInvalid, frontmatterName)
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.HasPrefix(text, "---") {
		return "", "", fmt.Errorf("%w: %s must start with YAML frontmatter (---)", ErrInvalid, frontmatterName)
	}
	lines := strings.Split(text, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return "", "", fmt.Errorf("%w: %s frontmatter must start on line 1", ErrInvalid, frontmatterName)
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return "", "", fmt.Errorf("%w: %s frontmatter has no closing ---", ErrInvalid, frontmatterName)
	}
	vals := map[string]string{}
	key := ""
	for _, line := range lines[1:end] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if key != "" {
				vals[key] = strings.TrimSpace(vals[key] + " " + trimmed)
			}
			continue
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(k)
		vals[key] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	name, description = vals["name"], vals["description"]
	if name == "" || description == "" {
		return "", "", fmt.Errorf("%w: %s frontmatter needs name and description", ErrInvalid, frontmatterName)
	}
	if !ValidName(name) {
		return "", "", fmt.Errorf("%w: skill name %q must match %s", ErrInvalid, name, nameRe)
	}
	if utf8.RuneCountInString(description) > maxDescRunes {
		return "", "", fmt.Errorf("%w: description longer than %d characters", ErrInvalid, maxDescRunes)
	}
	return name, description, nil
}

// ValidSkill reports whether b is a well-formed skill archive.
func ValidSkill(b []byte) bool { _, err := ParseSkill(b); return err == nil }

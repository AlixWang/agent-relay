package resources

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"
)

type tarEntry struct {
	name string
	body string
	typ  byte
	link string
}

func makeTarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: typ, Linkname: e.link}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if typ == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

const goodSkillMD = "---\nname: code-review\ndescription: Review code changes\n  carefully and kindly\n---\n# Code review\n"

func TestParseSkillValid(t *testing.T) {
	// Wrapper directory, the common `tar czf x.tgz dir/` shape.
	m, err := ParseSkill(makeTarGz(t,
		tarEntry{name: "code-review/", typ: tar.TypeDir},
		tarEntry{name: "code-review/SKILL.md", body: goodSkillMD},
		tarEntry{name: "code-review/scripts/run.sh", body: "echo hi\n"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "code-review" || m.Description != "Review code changes carefully and kindly" || m.Files != 2 {
		t.Fatalf("meta: %+v", m)
	}
	// Files at the archive root also work; CRLF + quoted values.
	if m, err := ParseSkill(makeTarGz(t,
		tarEntry{name: "SKILL.md", body: "---\r\nname: \"x\"\r\ndescription: 'y'\r\n---\r\n"},
		tarEntry{name: "ref.md", body: "z"},
	)); err != nil || m.Name != "x" || m.Description != "y" {
		t.Fatalf("root layout: %+v %v", m, err)
	}
}

func TestParseSkillRejects(t *testing.T) {
	big := strings.Repeat("x", maxSkillBytes)
	cases := map[string][]byte{
		"not gzip":        []byte("PK\x03\x04 zip file"),
		"empty":           nil,
		"no files":        makeTarGz(t, tarEntry{name: "d/", typ: tar.TypeDir}),
		"no SKILL.md":     makeTarGz(t, tarEntry{name: "d/README.md", body: "x"}),
		"no frontmatter":  makeTarGz(t, tarEntry{name: "SKILL.md", body: "# hi\n"}),
		"unclosed fm":     makeTarGz(t, tarEntry{name: "SKILL.md", body: "---\nname: a\ndescription: b\n"}),
		"missing desc":    makeTarGz(t, tarEntry{name: "SKILL.md", body: "---\nname: a\n---\n"}),
		"bad name":        makeTarGz(t, tarEntry{name: "SKILL.md", body: "---\nname: ../evil\ndescription: b\n---\n"}),
		"traversal":       makeTarGz(t, tarEntry{name: "SKILL.md", body: goodSkillMD}, tarEntry{name: "../../etc/cron.d/x", body: "x"}),
		"inner traversal": makeTarGz(t, tarEntry{name: "d/SKILL.md", body: goodSkillMD}, tarEntry{name: "d/a/../../../x", body: "x"}),
		"absolute":        makeTarGz(t, tarEntry{name: "SKILL.md", body: goodSkillMD}, tarEntry{name: "/etc/passwd", body: "x"}),
		"symlink":         makeTarGz(t, tarEntry{name: "SKILL.md", body: goodSkillMD}, tarEntry{name: "l", typ: tar.TypeSymlink, link: "/etc/passwd"}),
		"hardlink":        makeTarGz(t, tarEntry{name: "SKILL.md", body: goodSkillMD}, tarEntry{name: "l", typ: tar.TypeLink, link: "SKILL.md"}),
		"non-ascii name":  makeTarGz(t, tarEntry{name: "SKILL.md", body: goodSkillMD}, tarEntry{name: "说明.md", body: "x"}),
		"backslash":       makeTarGz(t, tarEntry{name: "SKILL.md", body: goodSkillMD}, tarEntry{name: "a\\..\\b", body: "x"}),
		"duplicate":       makeTarGz(t, tarEntry{name: "SKILL.md", body: goodSkillMD}, tarEntry{name: "SKILL.md", body: goodSkillMD}),
		"too big":         makeTarGz(t, tarEntry{name: "SKILL.md", body: goodSkillMD}, tarEntry{name: "blob", body: big}),
		"two trees":       makeTarGz(t, tarEntry{name: "a/SKILL.md", body: goodSkillMD}, tarEntry{name: "b/x", body: "x"}, tarEntry{name: "top.txt", body: "x"}),
	}
	for name, raw := range cases {
		if _, err := ParseSkill(raw); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	// File-count cap.
	es := []tarEntry{{name: "SKILL.md", body: goodSkillMD}}
	for i := 0; i < maxSkillFiles; i++ {
		es = append(es, tarEntry{name: "f" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26)), body: "x"})
	}
	if _, err := ParseSkill(makeTarGz(t, es...)); !errors.Is(err, ErrInvalid) {
		t.Errorf("too many files accepted: %v", err)
	}
}

// A gzip bomb (tiny archive, huge unpacked size) must be cut off by the
// unpacked-size cap, not read to the end.
func TestParseSkillGzipBomb(t *testing.T) {
	raw := makeTarGz(t, tarEntry{name: "SKILL.md", body: goodSkillMD}, tarEntry{name: "zeros", body: strings.Repeat("\x00", 8<<20)})
	if len(raw) > 64<<10 {
		t.Fatalf("bomb fixture unexpectedly large: %d", len(raw))
	}
	if _, err := ParseSkill(raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bomb accepted: %v", err)
	}
}

func TestSkillResourceFlow(t *testing.T) {
	svc, _, _ := newTestService(t)
	raw := makeTarGz(t, tarEntry{name: "code-review/SKILL.md", body: goodSkillMD})
	v, err := svc.PutSkill(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "code-review" || v.Kind != KindSkill || v.Description != "Review code changes carefully and kindly" {
		t.Fatalf("view: %+v", v)
	}
	if v2, _ := svc.PutSkill(raw, "团队审查规范"); v2.Description != "团队审查规范" {
		t.Fatalf("description override: %+v", v2)
	}
	if _, err := svc.SkillArchive("alice", "code-review"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted: %v", err)
	}
	_ = svc.Grant(KindSkill, "code-review", "alice")
	r, err := svc.SkillArchive("alice", "code-review")
	if err != nil || !bytes.Equal(r.Content, raw) {
		t.Fatalf("archive: %v", err)
	}
	m, _ := svc.Manifest("alice")
	e := m.Resources[0]
	if e.Kind != KindSkill || e.FetchURL != "/resources/skills/code-review" || e.Content.(map[string]any)["files"] != 1 {
		t.Fatalf("manifest entry: %+v", e)
	}
	before := m.ManifestRev
	_, _ = svc.PutSkill(makeTarGz(t, tarEntry{name: "code-review/SKILL.md", body: goodSkillMD + "v2\n"}), "")
	m2, _ := svc.Manifest("alice")
	if m2.ManifestRev == before {
		t.Fatal("new archive content did not move the digest")
	}
}

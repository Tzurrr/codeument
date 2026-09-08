package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

const sample = `Rotated the certificate and reloaded nginx.

**Why:** the old one expired.

## Steps

1. Renew

   ` + "```bash" + `
   certbot renew --nginx
   echo "]]> tricky"
   ` + "```" + `

2. Reload the service

## Checks

- [x] nginx -t passes
- [ ] monitoring green

| Host | Status |
|------|--------|
| web-01 | ok |

Line one
line two with a <br> and *emphasis* and a [link](https://example.com).

<!-- codeument-draft:abc -->

---

_Recorded by codeument._
`

func TestConfluenceStorageGolden(t *testing.T) {
	got, err := ConfluenceStorage(sample)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "confluence.golden")
	if *update {
		_ = os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("missing golden (run with -update): %v", err)
	}
	if string(want) != got {
		t.Fatalf("output differs from golden:\n%s", got)
	}
	for _, must := range []string{
		`<ac:structured-macro ac:name="code"`, `<ac:parameter ac:name="language">bash</ac:parameter>`,
		`<![CDATA[certbot renew --nginx`, `]]]]><![CDATA[>`, `<hr />`, `<table>`, `&#9745;`, `&#9744;`,
	} {
		if !strings.Contains(got, must) {
			t.Errorf("missing %q", must)
		}
	}
	for _, mustNot := range []string{"<!--", "<br>", "<input"} {
		if strings.Contains(got, mustNot) {
			t.Errorf("unexpected %q in output", mustNot)
		}
	}
	if !strings.Contains(Footer("doc-1", "web-01"), `ac:name="info"`) {
		t.Fatal("footer must be an info macro")
	}
}

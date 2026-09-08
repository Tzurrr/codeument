// Package render converts canonical Markdown into provider formats. The
// Confluence renderer emits storage-format XHTML with the code, info and
// task-list macros Confluence expects.
package render

import (
	"bytes"
	"fmt"
	"html"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer"
	ghtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
)

// ConfluenceStorage renders Markdown to Confluence storage format.
func ConfluenceStorage(markdown string) (string, error) {
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(
			ghtml.WithXHTML(),
			renderer.WithNodeRenderers(util.Prioritized(&confluenceNodes{}, 100)),
		),
	)
	var buf bytes.Buffer
	if err := md.Convert([]byte(markdown), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// InfoMacro renders a Confluence info panel with the given XHTML body.
func InfoMacro(bodyXHTML string) string {
	return `<ac:structured-macro ac:name="info" ac:schema-version="1"><ac:rich-text-body>` + bodyXHTML + `</ac:rich-text-body></ac:structured-macro>`
}

// CodeMacro renders a code block macro.
func CodeMacro(language, code string) string {
	var b strings.Builder
	b.WriteString(`<ac:structured-macro ac:name="code" ac:schema-version="1">`)
	if language != "" {
		b.WriteString(`<ac:parameter ac:name="language">` + html.EscapeString(mapLanguage(language)) + `</ac:parameter>`)
	}
	b.WriteString(`<ac:plain-text-body><![CDATA[`)
	b.WriteString(strings.ReplaceAll(code, "]]>", "]]]]><![CDATA[>"))
	b.WriteString(`]]></ac:plain-text-body></ac:structured-macro>`)
	return b.String()
}

// mapLanguage maps common fence names to Confluence code macro languages.
func mapLanguage(l string) string {
	switch strings.ToLower(l) {
	case "sh", "shell", "zsh", "console":
		return "bash"
	case "yml":
		return "yaml"
	case "js":
		return "javascript"
	case "ts":
		return "typescript"
	case "golang":
		return "go"
	case "dockerfile":
		return "text"
	}
	return strings.ToLower(l)
}

type confluenceNodes struct{}

func (r *confluenceNodes) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, r.renderFenced)
	reg.Register(ast.KindCodeBlock, r.renderCode)
	reg.Register(ast.KindHTMLBlock, r.renderNothing)
	reg.Register(ast.KindRawHTML, r.renderNothing)
	reg.Register(extast.KindTaskCheckBox, r.renderCheckbox)
}

func (r *confluenceNodes) renderFenced(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.FencedCodeBlock)
	lang := ""
	if l := n.Language(source); l != nil {
		lang = string(l)
	}
	_, _ = w.WriteString(CodeMacro(lang, codeText(n, source)))
	return ast.WalkSkipChildren, nil
}

func (r *confluenceNodes) renderCode(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	_, _ = w.WriteString(CodeMacro("", codeText(node, source)))
	return ast.WalkSkipChildren, nil
}

func (r *confluenceNodes) renderNothing(_ util.BufWriter, _ []byte, _ ast.Node, _ bool) (ast.WalkStatus, error) {
	return ast.WalkSkipChildren, nil
}

func (r *confluenceNodes) renderCheckbox(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*extast.TaskCheckBox)
	if n.IsChecked {
		_, _ = w.WriteString("&#9745; ")
	} else {
		_, _ = w.WriteString("&#9744; ")
	}
	return ast.WalkContinue, nil
}

func codeText(n ast.Node, source []byte) string {
	var b strings.Builder
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(source))
	}
	return strings.TrimRight(b.String(), "\n")
}

// Footer renders the visible codeument marker at the bottom of a page.
func Footer(docID, hostname string) string {
	body := fmt.Sprintf("<p>Maintained by codeument. Document id <code>%s</code>", html.EscapeString(docID))
	if hostname != "" {
		body += " · source host <code>" + html.EscapeString(hostname) + "</code>"
	}
	body += ". Edits made here are kept when codeument appends to this page.</p>"
	return InfoMacro(body)
}

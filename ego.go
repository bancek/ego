package ego

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"sort"
	"strings"
	"unicode"
)

// Template represents an entire Ego template.
// A template consists of zero or more blocks.
// Blocks can be either a TextBlock, a PrintBlock, a RawPrintBlock, or a CodeBlock.
type Template struct {
	Path   string
	Blocks []Block
}

// WriteTo writes the template to a writer.
func (t *Template) WriteTo(w io.Writer) (n int64, err error) {
	var buf bytes.Buffer

	// Write "generated" header comment.
	buf.WriteString("// Generated by ego.\n")
	buf.WriteString("// DO NOT EDIT\n\n")

	// Write blocks.
	writeBlocksTo(&buf, t.Blocks)

	// Parse buffer as a Go file.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", buf.Bytes(), parser.ParseComments)
	if err != nil {
		n, _ = buf.WriteTo(w)
		return n, err
	}

	// Inject required packages.
	injectImports(f)

	// Attempt to gofmt.
	var result bytes.Buffer
	if err := format.Node(&result, fset, f); err != nil {
		n, _ = buf.WriteTo(w)
		return n, err
	}

	// Write to output writer.
	return result.WriteTo(w)
}

func writeBlocksTo(buf *bytes.Buffer, blks []Block) {
	for _, blk := range blks {
		// Write line comment.
		if pos := Position(blk); pos.Path != "" && pos.LineNo > 0 {
			fmt.Fprintf(buf, "//line %s:%d\n", pos.Path, pos.LineNo)
		}

		// Write block.
		switch blk := blk.(type) {
		case *TextBlock:
			fmt.Fprintf(buf, `_, _ = io.WriteString(w, %q)`+"\n", blk.Content)

		case *CodeBlock:
			fmt.Fprintln(buf, blk.Content)

		case *PrintBlock:
			fmt.Fprintf(buf, `_, _ = io.WriteString(w, html.EscapeString(fmt.Sprint(%s)))`+"\n", blk.Content)

		case *RawPrintBlock:
			fmt.Fprintf(buf, `_, _ = fmt.Fprint(w, %s)`+"\n", blk.Content)

		case *ComponentStartBlock:
			if blk.Package != "" {
				fmt.Fprintf(buf, "{\nvar EGO %s.%s\n", blk.Package, blk.Name)
			} else {
				fmt.Fprintf(buf, "{\nvar EGO %s\n", blk.Name)
			}

			for _, field := range blk.Fields {
				fmt.Fprintf(buf, "EGO.%s = %s\n", field.Name, field.Value)
			}

			if len(blk.Attrs) > 0 {
				fmt.Fprintf(buf, "EGO.Attrs = map[string]string{\n")
				for _, attr := range blk.Attrs {
					fmt.Fprintf(buf, "	%q: fmt.Sprint(%s),\n", attr.Name, attr.Value)
				}
				fmt.Fprintf(buf, "}\n")
			}

			for _, attrBlock := range blk.AttrBlocks {
				fmt.Fprintf(buf, "EGO.%s = func() {\n", attrBlock.Name)
				writeBlocksTo(buf, attrBlock.Yield)
				fmt.Fprint(buf, "}\n")
			}

			if len(blk.Yield) > 0 {
				buf.WriteString("EGO.Yield = func() {\n")
				writeBlocksTo(buf, blk.Yield)
				buf.WriteString("}\n")
			}

			fmt.Fprint(buf, "EGO.Render(ctx, w) }\n")
		}
	}
}

// Normalize joins together adjacent text blocks.
func normalizeBlocks(a []Block) []Block {
	a = joinAdjacentTextBlocks(a)
	a = trimLeftRight(a)
	a = trimTrailingEmptyTextBlocks(a)
	return a
}

func joinAdjacentTextBlocks(a []Block) []Block {
	var other []Block
	for _, blk := range a {
		curr, isTextBlock := blk.(*TextBlock)

		// Always append the first block.
		if len(other) == 0 {
			other = append(other, blk)
			continue
		}

		// Simply append if this block or prev block are not text blocks.
		prev, isPrevTextBlock := other[len(other)-1].(*TextBlock)
		if !isTextBlock || !isPrevTextBlock {
			other = append(other, blk)
			continue
		}

		// Append this text block's content to the previous block.
		prev.Content += curr.Content
	}

	return other
}

func trimLeftRight(a []Block) []Block {
	for i, blk := range a {
		trimLeft, trimRight := blk.trim()
		if trimLeft && i > 1 {
			if textBlock, ok := a[i-1].(*TextBlock); ok {
				textBlock.Content = strings.TrimRightFunc(textBlock.Content, unicode.IsSpace)
			}
		}
		if trimRight && i+1 < len(a) {
			if textBlock, ok := a[i+1].(*TextBlock); ok {
				textBlock.Content = strings.TrimLeftFunc(textBlock.Content, unicode.IsSpace)
			}
		}
	}
	return a
}

func trimTrailingEmptyTextBlocks(a []Block) []Block {
	for len(a) > 0 {
		blk, ok := a[len(a)-1].(*TextBlock)
		if !ok || strings.TrimSpace(blk.Content) != "" {
			break
		}
		a[len(a)-1] = nil
		a = a[:len(a)-1]
	}
	return a
}

func injectImports(f *ast.File) {
	names := []string{`"fmt"`, `"html"`, `"io"`, `"context"`}

	// Strip packages from existing imports.
	for i := 0; i < len(f.Decls); i++ {
		decl, ok := f.Decls[i].(*ast.GenDecl)
		if !ok || decl.Tok != token.IMPORT {
			continue
		}

		// Remove listed imports.
		removeImportSpecs(decl, names)

		// Remove declaration if it has no imports.
		if len(decl.Specs) == 0 {
			copy(f.Decls[i:], f.Decls[i+1:])
			f.Decls[len(f.Decls)-1] = nil
			f.Decls = f.Decls[:len(f.Decls)-1]
			i--
		}
	}

	// Generate new import.
	for i := len(names) - 1; i >= 0; i-- {
		f.Decls = append([]ast.Decl{&ast.GenDecl{
			Tok: token.IMPORT,
			Specs: []ast.Spec{
				&ast.ImportSpec{Path: &ast.BasicLit{Kind: token.STRING, Value: names[i]}},
			},
		}}, f.Decls...)
	}

	// Add unnamed vars at the end of the file to ensure imports are used.
	f.Decls = append(f.Decls, &ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{
			&ast.ValueSpec{Names: []*ast.Ident{{Name: "_"}}, Type: &ast.Ident{Name: "fmt.Stringer"}},
		},
	})
	f.Decls = append(f.Decls, &ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{
			&ast.ValueSpec{Names: []*ast.Ident{{Name: "_"}}, Type: &ast.Ident{Name: "io.Reader"}},
		},
	})
	f.Decls = append(f.Decls, &ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{
			&ast.ValueSpec{Names: []*ast.Ident{{Name: "_"}}, Type: &ast.Ident{Name: "context.Context"}},
		},
	})
	f.Decls = append(f.Decls, &ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{
			&ast.ValueSpec{Names: []*ast.Ident{{Name: "_"}}, Values: []ast.Expr{&ast.Ident{Name: "html.EscapeString"}}},
		},
	})
}

func removeImportSpecs(decl *ast.GenDecl, names []string) {
	for i := 0; i < len(decl.Specs); i++ {
		spec, ok := decl.Specs[i].(*ast.ImportSpec)
		if !ok || !stringSliceContains(names, spec.Path.Value) {
			continue
		}

		// Delete spec.
		copy(decl.Specs[i:], decl.Specs[i+1:])
		decl.Specs[len(decl.Specs)-1] = nil
		decl.Specs = decl.Specs[:len(decl.Specs)-1]

		i--
	}
}

// Block represents an element of the template.
type Block interface {
	block()
	trim() (bool, bool)
}

func (*TextBlock) block()           {}
func (*CodeBlock) block()           {}
func (*PrintBlock) block()          {}
func (*RawPrintBlock) block()       {}
func (*ComponentStartBlock) block() {}
func (*ComponentEndBlock) block()   {}
func (*AttrStartBlock) block()      {}
func (*AttrEndBlock) block()        {}

func (*TextBlock) trim() (bool, bool)           { return false, false }
func (b *CodeBlock) trim() (bool, bool)         { return b.TrimLeft, b.TrimRight }
func (b *PrintBlock) trim() (bool, bool)        { return b.TrimLeft, b.TrimRight }
func (b *RawPrintBlock) trim() (bool, bool)     { return b.TrimLeft, b.TrimRight }
func (*ComponentStartBlock) trim() (bool, bool) { return false, false }
func (*ComponentEndBlock) trim() (bool, bool)   { return false, false }
func (*AttrStartBlock) trim() (bool, bool)      { return false, false }
func (*AttrEndBlock) trim() (bool, bool)        { return false, false }

// TextBlock represents a UTF-8 encoded block of text that is written to the writer as-is.
type TextBlock struct {
	Pos     Pos
	Content string
}

// CodeBlock represents a Go code block that is printed as-is to the template.
type CodeBlock struct {
	Pos       Pos
	Content   string
	TrimLeft  bool
	TrimRight bool
}

// PrintBlock represents a block that will HTML escape the contents before outputting
type PrintBlock struct {
	Pos       Pos
	Content   string
	TrimLeft  bool
	TrimRight bool
}

// RawPrintBlock represents a block of the template that is printed out to the writer.
type RawPrintBlock struct {
	Pos       Pos
	Content   string
	TrimLeft  bool
	TrimRight bool
}

// ComponentStartBlock represents the opening block of an ego component.
type ComponentStartBlock struct {
	Pos        Pos
	Package    string
	Name       string
	Closed     bool
	Fields     []*Field
	Attrs      []*Attr
	AttrBlocks []*AttrStartBlock
	Yield      []Block
}

// Namespace returns the block package, if defined. Otherwise returns "ego".
func (blk *ComponentStartBlock) Namespace() string {
	if blk.Package == "" {
		return "ego"
	}
	return blk.Package
}

// ComponentEndBlock represents the closing block of an ego component.
type ComponentEndBlock struct {
	Pos     Pos
	Package string
	Name    string
}

// Namespace returns the block package, if defined. Otherwise returns "ego".
func (blk *ComponentEndBlock) Namespace() string {
	if blk.Package == "" {
		return "ego"
	}
	return blk.Package
}

// AttrStartBlock represents the opening block of an ego component attribute.
type AttrStartBlock struct {
	Pos     Pos
	Package string
	Name    string
	Yield   []Block
}

// Namespace returns the block package, if defined. Otherwise returns "ego".
func (blk *AttrStartBlock) Namespace() string {
	if blk.Package == "" {
		return "ego"
	}
	return blk.Package
}

// AttrEndBlock represents the closing block of an ego component attribute.
type AttrEndBlock struct {
	Pos     Pos
	Package string
	Name    string
}

// Namespace returns the block package, if defined. Otherwise returns "ego".
func (blk *AttrEndBlock) Namespace() string {
	if blk.Package == "" {
		return "ego"
	}
	return blk.Package
}

func shortComponentBlockString(blk Block) string {
	switch blk := blk.(type) {
	case *ComponentStartBlock:
		return fmt.Sprintf("<%s:%s>", blk.Namespace(), blk.Name)
	case *ComponentEndBlock:
		return fmt.Sprintf("</%s:%s>", blk.Namespace(), blk.Name)
	case *AttrStartBlock:
		return fmt.Sprintf("<%s::%s>", blk.Namespace(), blk.Name)
	case *AttrEndBlock:
		return fmt.Sprintf("</%s::%s>", blk.Namespace(), blk.Name)
	default:
		return "<UNKNOWN>"
	}
}

// Field represents a key/value pair on a component.
type Field struct {
	Name    string
	NamePos Pos

	Value    string
	ValuePos Pos
}

// Attr represents a key/value passthrough pair on a component.
type Attr struct {
	Name    string
	NamePos Pos

	Value    string
	ValuePos Pos
}

// Position returns the position of the block.
func Position(blk Block) Pos {
	switch blk := blk.(type) {
	case *TextBlock:
		return blk.Pos
	case *CodeBlock:
		return blk.Pos
	case *PrintBlock:
		return blk.Pos
	case *RawPrintBlock:
		return blk.Pos
	case *ComponentStartBlock:
		return blk.Pos
	case *ComponentEndBlock:
		return blk.Pos
	case *AttrStartBlock:
		return blk.Pos
	case *AttrEndBlock:
		return blk.Pos
	default:
		panic("unreachable")
	}
}

// Pos represents a position in a given file.
type Pos struct {
	Path   string
	LineNo int
}

func stringSliceContains(a []string, v string) bool {
	for i := range a {
		if a[i] == v {
			return true
		}
	}
	return false
}

// AttrNames returns a sorted list of names for an attribute set.
func AttrNames(attrs map[string]interface{}) []string {
	a := make([]string, 0, len(attrs))
	for k := range attrs {
		a = append(a, k)
	}
	sort.Strings(a)
	return a
}

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const prompt = "go> "

type session struct {
	dir        string
	imports    []userImport
	decls      []string
	statements []statement
}

type userImport struct {
	text   string
	name   string
	always bool
}

type statement struct {
	text     string
	declared []string
}

func main() {
	dir, err := os.MkdirTemp("", "ohmygosh-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	s := &session{dir: dir}
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Println("ohmygosh interactive Go prompt. Use :quit or Ctrl-D to exit.")
	for {
		fmt.Print(prompt)
		if !scanner.Scan() {
			fmt.Println()
			break
		}

		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "":
			continue
		case ":quit", ":exit":
			return
		case ":help":
			fmt.Println("Enter one Go statement per line. Top-level declarations are remembered; statements run as if inside main.")
			continue
		}

		if err := s.eval(line); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (s *session) eval(line string) error {
	if declared, ok := parseStatement(line); ok {
		return s.runStatement(statement{text: line, declared: declared})
	}

	if imp, ok := parseImport(line); ok {
		s.imports = append(s.imports, imp)
		return nil
	}

	if parseDeclaration(line) {
		s.decls = append(s.decls, line)
		return nil
	}

	return errors.New("syntax error: input is not a valid Go statement or top-level declaration")
}

func (s *session) runStatement(stmt statement) error {
	source := s.source(stmt)
	mainFile := filepath.Join(s.dir, "main.go")
	if err := os.WriteFile(mainFile, []byte(source), 0o600); err != nil {
		return err
	}

	cmd := exec.Command("go", "run", mainFile)
	cmd.Dir = s.dir
	cmd.Stdin = os.Stdin

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if stdout.Len() > 0 {
		fmt.Print(stdout.String())
	}
	if stderr.Len() > 0 {
		fmt.Fprint(os.Stderr, stderr.String())
	}
	if err != nil {
		return fmt.Errorf("statement did not run: %w", err)
	}

	s.statements = append(s.statements, stmt)
	return nil
}

func (s *session) source(current statement) string {
	var b strings.Builder
	b.WriteString("package main\n\n")
	body := s.bodyText(current.text)
	for _, imp := range s.imports {
		if imp.always || strings.Contains(body, imp.name+".") {
			b.WriteString(imp.text)
			b.WriteString("\n")
		}
	}
	if len(s.imports) > 0 {
		b.WriteByte('\n')
	}
	for _, decl := range s.decls {
		b.WriteString(decl)
		b.WriteString("\n\n")
	}

	if len(s.statements) > 0 {
		b.WriteString("import __gosh_io \"io\"\n")
		b.WriteString("import __gosh_os \"os\"\n\n")
	}

	b.WriteString("func main() {\n")
	if len(s.statements) > 0 {
		writeSilencedHistory(&b, s.statements)
	}
	writeStatement(&b, current)
	b.WriteByte('\n')
	b.WriteString("}\n")
	return b.String()
}

func (s *session) bodyText(current string) string {
	var b strings.Builder
	for _, stmt := range s.statements {
		b.WriteString(stmt.text)
		b.WriteByte('\n')
	}
	b.WriteString(current)
	return b.String()
}

func writeSilencedHistory(b *strings.Builder, statements []statement) {
	b.WriteString("__gosh_oldStdout := __gosh_os.Stdout\n")
	b.WriteString("__gosh_oldStderr := __gosh_os.Stderr\n")
	b.WriteString("__gosh_rOut, __gosh_wOut, _ := __gosh_os.Pipe()\n")
	b.WriteString("__gosh_rErr, __gosh_wErr, _ := __gosh_os.Pipe()\n")
	b.WriteString("__gosh_os.Stdout = __gosh_wOut\n")
	b.WriteString("__gosh_os.Stderr = __gosh_wErr\n")

	for _, stmt := range statements {
		writeStatement(b, stmt)
	}

	b.WriteString("__gosh_wOut.Close()\n")
	b.WriteString("__gosh_wErr.Close()\n")
	b.WriteString("__gosh_io.Copy(__gosh_io.Discard, __gosh_rOut)\n")
	b.WriteString("__gosh_io.Copy(__gosh_io.Discard, __gosh_rErr)\n")
	b.WriteString("__gosh_os.Stdout = __gosh_oldStdout\n")
	b.WriteString("__gosh_os.Stderr = __gosh_oldStderr\n")
}

func writeStatement(b *strings.Builder, stmt statement) {
	b.WriteString(stmt.text)
	b.WriteByte('\n')
	for _, name := range stmt.declared {
		b.WriteString("_ = ")
		b.WriteString(name)
		b.WriteByte('\n')
	}
}

func parseStatement(line string) ([]string, bool) {
	src := "package main\nfunc main() {\n" + line + "\n}\n"
	file, err := parser.ParseFile(token.NewFileSet(), "input.go", src, 0)
	if err != nil {
		return nil, false
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		return promptDeclaredNames(fn.Body.List), true
	}
	return nil, true
}

func parseImport(line string) (userImport, bool) {
	src := "package main\n" + line + "\n"
	file, err := parser.ParseFile(token.NewFileSet(), "input.go", src, parser.ImportsOnly)
	if err != nil || len(file.Decls) != 1 || len(file.Imports) != 1 {
		return userImport{}, false
	}
	decl, ok := file.Decls[0].(*ast.GenDecl)
	if !ok || decl.Tok != token.IMPORT {
		return userImport{}, false
	}

	spec := file.Imports[0]
	name, always := importName(spec)
	return userImport{text: line, name: name, always: always}, true
}

func parseDeclaration(line string) bool {
	src := "package main\n" + line + "\n"
	file, err := parser.ParseFile(token.NewFileSet(), "input.go", src, 0)
	return err == nil && len(file.Decls) > 0
}

func promptDeclaredNames(stmts []ast.Stmt) []string {
	if len(stmts) != 1 {
		return nil
	}

	switch stmt := stmts[0].(type) {
	case *ast.AssignStmt:
		if stmt.Tok != token.DEFINE {
			return nil
		}
		var names []string
		for _, expr := range stmt.Lhs {
			if ident, ok := expr.(*ast.Ident); ok && ident.Name != "_" {
				names = append(names, ident.Name)
			}
		}
		return names
	case *ast.DeclStmt:
		decl, ok := stmt.Decl.(*ast.GenDecl)
		if !ok || decl.Tok != token.VAR {
			return nil
		}
		var names []string
		for _, spec := range decl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, ident := range valueSpec.Names {
				if ident.Name != "_" {
					names = append(names, ident.Name)
				}
			}
		}
		return names
	default:
		return nil
	}
}

func importName(spec *ast.ImportSpec) (string, bool) {
	if spec.Name != nil {
		if spec.Name.Name == "_" || spec.Name.Name == "." {
			return spec.Name.Name, true
		}
		return spec.Name.Name, false
	}

	path := strings.Trim(spec.Path.Value, `"`)
	base := path[strings.LastIndex(path, "/")+1:]
	base = strings.ReplaceAll(base, "-", "_")
	return base, false
}

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

const sessionModule = `module ohmygosh-session

go 1.26.1
`

type session struct {
	dir        string
	imports    []userImport
	decls      []string
	statements []statement
}

type userImport struct {
	text   string
	names  []string
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

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(sessionModule), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	s := &session{dir: dir}
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Println("ohmygosh interactive Go prompt. Use /exit or Ctrl-D to exit.")
	for {
		fmt.Print(prompt)
		if !scanner.Scan() {
			fmt.Println()
			break
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		cmd, ok := parseCommand(line)
		if ok {
			if runCommand(cmd) {
				return
			}
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

func parseCommand(line string) (string, bool) {
	if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "/*") {
		return "", false
	}

	if len(line) < 2 {
		return "", false
	}

	if line[0] == '/' {
		return strings.TrimSpace(line[1:]), true
	}
	return "", false
}

func runCommand(cmd string) bool {
	switch cmd {
	case "exit", "quit", "q":
		return true
	case "help", "?":
		fmt.Println("Enter one Go statement per line. Top-level declarations are remembered; statements run as if inside main.")
		fmt.Println("Program commands: /exit, /quit, /help.")
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
	}
	return false
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

	cmd := exec.Command("go", "run", "-mod=mod", ".")
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
		if imp.always || importUsed(body, imp.names) {
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
	names, always := importNames(spec)
	return userImport{text: line, names: names, always: always}, true
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

func importUsed(body string, names []string) bool {
	for _, name := range names {
		if strings.Contains(body, name+".") {
			return true
		}
	}
	return false
}

func importNames(spec *ast.ImportSpec) ([]string, bool) {
	if spec.Name != nil {
		if spec.Name.Name == "_" || spec.Name.Name == "." {
			return []string{spec.Name.Name}, true
		}
		return []string{spec.Name.Name}, false
	}

	path := strings.Trim(spec.Path.Value, `"`)
	parts := strings.Split(path, "/")
	base := parts[len(parts)-1]

	var names []string
	addName := func(name string) {
		name = strings.ReplaceAll(name, "-", "_")
		if name == "" {
			return
		}
		for _, existing := range names {
			if existing == name {
				return
			}
		}
		names = append(names, name)
	}

	addName(base)
	if versioned, ok := trimVersionSuffix(base); ok {
		addName(versioned)
	}
	if isVersionComponent(base) && len(parts) > 1 {
		previous := parts[len(parts)-2]
		addName(previous)
		if hyphen := strings.LastIndex(previous, "-"); hyphen >= 0 && hyphen < len(previous)-1 {
			addName(previous[hyphen+1:])
		}
	}
	if hyphen := strings.LastIndex(base, "-"); hyphen >= 0 && hyphen < len(base)-1 {
		addName(base[hyphen+1:])
	}

	return names, false
}

func trimVersionSuffix(name string) (string, bool) {
	dot := strings.LastIndex(name, ".v")
	if dot < 0 || dot+2 >= len(name) {
		return "", false
	}
	for _, r := range name[dot+2:] {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return name[:dot], true
}

func isVersionComponent(name string) bool {
	if len(name) < 2 || name[0] != 'v' {
		return false
	}
	for _, r := range name[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

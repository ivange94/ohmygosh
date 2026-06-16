package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const prompt = "go> "

var version = "dev"

const sessionModule = `module ohmygosh-session

go 1.26.1
`

const (
	colorReset  = "\x1b[0m"
	colorRed    = "\x1b[31m"
	colorGreen  = "\x1b[32m"
	colorYellow = "\x1b[33m"
	colorCyan   = "\x1b[36m"
)

type session struct {
	dir        string
	imports    []userImport
	decls      []string
	names      []string
	statements []statement
}

type userImport struct {
	text   string
	path   string
	names  []string
	always bool
}

type statement struct {
	text     string
	declared []string
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "gosh",
		Short: "Interactive Go prompt",
		Long:  "ohmygosh is an interactive Go prompt for trying small snippets without setting up a scratch file.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runREPL()
		},
	}

	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the gosh version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "gosh %s\n", version)
		},
	})

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runREPL() error {
	dir, err := os.MkdirTemp("", "ohmygosh-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(sessionModule), 0o600); err != nil {
		return err
	}

	s := &session{dir: dir}
	reader := newLineReader(os.Stdin, os.Stdout, s)

	printIntro()
	for {
		line, ok, err := reader.ReadLine(prompt)
		if err != nil {
			return err
		}
		if !ok {
			break
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		cmd, ok := parseCommand(line)
		if ok {
			if runCommand(cmd) {
				return nil
			}
			continue
		}

		if err := s.eval(line); err != nil {
			printError(err)
		}
	}
	return nil
}

type lineReader interface {
	ReadLine(prompt string) (string, bool, error)
}

type scannerLineReader struct {
	scanner *bufio.Scanner
	out     io.Writer
}

func newLineReader(in *os.File, out *os.File, s *session) lineReader {
	if isTerminal(in) && isTerminal(out) {
		return &terminalLineReader{in: in, out: out, session: s}
	}
	return &scannerLineReader{scanner: bufio.NewScanner(in), out: out}
}

func (r *scannerLineReader) ReadLine(prompt string) (string, bool, error) {
	fmt.Fprint(r.out, style(prompt, colorCyan, os.Stdout))
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return "", false, err
		}
		fmt.Fprintln(r.out)
		return "", false, nil
	}
	return r.scanner.Text(), true, nil
}

type terminalLineReader struct {
	in      *os.File
	out     *os.File
	session *session
	history []string
}

func (r *terminalLineReader) ReadLine(prompt string) (string, bool, error) {
	fd := int(r.in.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return "", false, err
	}
	defer term.Restore(fd, oldState)

	var line []rune
	cursor := 0
	historyIndex := len(r.history)
	draft := ""

	render := func() {
		fmt.Fprintf(r.out, "\r\x1b[2K%s%s", style(prompt, colorCyan, os.Stdout), string(line))
		if back := len(line) - cursor; back > 0 {
			fmt.Fprintf(r.out, "\x1b[%dD", back)
		}
	}
	render()

	var buf [1]byte
	for {
		n, err := r.in.Read(buf[:])
		if err != nil {
			return "", false, err
		}
		if n == 0 {
			continue
		}

		switch b := buf[0]; b {
		case '\r', '\n':
			text := string(line)
			fmt.Fprint(r.out, "\r\n")
			if strings.TrimSpace(text) != "" {
				r.history = append(r.history, text)
			}
			return text, true, nil
		case 3: // Ctrl-C clears the current input.
			fmt.Fprint(r.out, "^C\r\n")
			return "", true, nil
		case 4: // Ctrl-D exits on an empty line.
			if len(line) == 0 {
				fmt.Fprint(r.out, "\r\n")
				return "", false, nil
			}
		case 9: // Tab
			line, cursor = r.complete(line, cursor)
			render()
		case 27:
			line, cursor, historyIndex, draft = r.handleEscape(line, cursor, historyIndex, draft)
			render()
		case 127, 8:
			if cursor > 0 {
				line = append(line[:cursor-1], line[cursor:]...)
				cursor--
				render()
			}
		default:
			if b < 32 {
				continue
			}
			rr, err := r.readRune(b)
			if err != nil {
				return "", false, err
			}
			line = append(line[:cursor], append([]rune{rr}, line[cursor:]...)...)
			cursor++
			historyIndex = len(r.history)
			render()
		}
	}
}

func (r *terminalLineReader) readRune(first byte) (rune, error) {
	if first < utf8.RuneSelf {
		return rune(first), nil
	}

	var buf [utf8.UTFMax]byte
	buf[0] = first
	size := 1
	for size < utf8.UTFMax && !utf8.FullRune(buf[:size]) {
		if _, err := r.in.Read(buf[size : size+1]); err != nil {
			return utf8.RuneError, err
		}
		size++
	}
	rr, _ := utf8.DecodeRune(buf[:size])
	return rr, nil
}

func (r *terminalLineReader) handleEscape(line []rune, cursor int, historyIndex int, draft string) ([]rune, int, int, string) {
	var seq [2]byte
	n, err := r.in.Read(seq[:])
	if err != nil || n < 2 || seq[0] != '[' {
		return line, cursor, historyIndex, draft
	}

	switch seq[1] {
	case 'A': // Up
		if len(r.history) == 0 || historyIndex == 0 {
			return line, cursor, historyIndex, draft
		}
		if historyIndex == len(r.history) {
			draft = string(line)
		}
		historyIndex--
		line = []rune(r.history[historyIndex])
		return line, len(line), historyIndex, draft
	case 'B': // Down
		if historyIndex >= len(r.history) {
			return line, cursor, historyIndex, draft
		}
		historyIndex++
		if historyIndex == len(r.history) {
			line = []rune(draft)
		} else {
			line = []rune(r.history[historyIndex])
		}
		return line, len(line), historyIndex, draft
	case 'C': // Right
		if cursor < len(line) {
			cursor++
		}
	case 'D': // Left
		if cursor > 0 {
			cursor--
		}
	case '1', '3', '4', '7', '8':
		var discard [1]byte
		_, _ = r.in.Read(discard[:])
	}
	return line, cursor, historyIndex, draft
}

func (r *terminalLineReader) complete(line []rune, cursor int) ([]rune, int) {
	start := completionStart(line, cursor)
	prefix := string(line[start:cursor])
	if prefix == "" {
		return line, cursor
	}

	var match string
	var ok bool
	if qualifier, qualifierOK := completionQualifier(line, start); qualifierOK {
		match, ok = completion(prefix, r.session.packageCompletions(qualifier))
	} else {
		match, ok = completion(prefix, r.session.completions())
	}
	if !ok || match == prefix {
		return line, cursor
	}

	replacement := []rune(match)
	line = append(line[:start], append(replacement, line[cursor:]...)...)
	return line, start + len(replacement)
}

func completionStart(line []rune, cursor int) int {
	start := cursor
	for start > 0 {
		r := line[start-1]
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '/') {
			break
		}
		start--
	}
	return start
}

func completionQualifier(line []rune, start int) (string, bool) {
	if start < 2 || line[start-1] != '.' {
		return "", false
	}
	end := start - 1
	begin := end
	for begin > 0 {
		r := line[begin-1]
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			break
		}
		begin--
	}
	if begin == end {
		return "", false
	}
	return string(line[begin:end]), true
}

func completion(prefix string, options []string) (string, bool) {
	var matches []string
	for _, option := range options {
		if strings.HasPrefix(option, prefix) {
			matches = append(matches, option)
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return longestCommonPrefix(matches), true
}

func longestCommonPrefix(values []string) string {
	prefix := values[0]
	for _, value := range values[1:] {
		for !strings.HasPrefix(value, prefix) {
			if prefix == "" {
				return ""
			}
			prefix = prefix[:len(prefix)-1]
		}
	}
	return prefix
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
		printHelp()
	default:
		printError(fmt.Errorf("unknown command: %s", cmd))
	}
	return false
}

func printIntro() {
	fmt.Printf("%s interactive Go prompt. Use %s or Ctrl-D to exit.\n",
		style("ohmygosh", colorGreen, os.Stdout),
		style("/exit", colorYellow, os.Stdout),
	)
}

func printPrompt() {
	fmt.Print(style(prompt, colorCyan, os.Stdout))
}

func printHelp() {
	fmt.Println("Enter one Go statement per line. Top-level var, const, and type declarations are remembered; statements run as if inside main.")
	fmt.Println("Use Up/Down for history and Tab for autocomplete.")
	fmt.Printf("Program commands: %s, %s, %s.\n",
		style("/exit", colorYellow, os.Stdout),
		style("/quit", colorYellow, os.Stdout),
		style("/help", colorYellow, os.Stdout),
	)
}

func printError(err error) {
	fmt.Fprintln(os.Stderr, style(err.Error(), colorRed, os.Stderr))
}

func printCommandStdout(text string) {
	fmt.Print(style(text, colorGreen, os.Stdout))
}

func printCommandStderr(text string) {
	fmt.Fprint(os.Stderr, style(text, colorRed, os.Stderr))
}

func style(text string, color string, file *os.File) string {
	if !shouldColor(file) {
		return text
	}
	return color + text + colorReset
}

func shouldColor(file *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(file)
}

func isTerminal(file *os.File) bool {
	return term.IsTerminal(int(file.Fd()))
}

func (s *session) eval(line string) error {
	if declared, ok := parseStatement(line); ok {
		return s.runStatement(statement{text: line, declared: declared})
	}

	if imp, ok := parseImport(line); ok {
		s.imports = append(s.imports, imp)
		s.addNames(imp.names...)
		return nil
	}

	if parseFunctionDeclaration(line) {
		return errors.New("function declarations are not supported yet")
	}

	if parseDeclaration(line) {
		s.decls = append(s.decls, line)
		s.addNames(declaredNames(line)...)
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
		printCommandStdout(stdout.String())
	}
	if stderr.Len() > 0 {
		printCommandStderr(stderr.String())
	}
	if err != nil {
		return fmt.Errorf("statement did not run: %w", err)
	}

	s.statements = append(s.statements, stmt)
	s.addNames(stmt.declared...)
	return nil
}

func (s *session) addNames(names ...string) {
	for _, name := range names {
		if name == "" || name == "_" || name == "." {
			continue
		}
		exists := false
		for _, existing := range s.names {
			if existing == name {
				exists = true
				break
			}
		}
		if !exists {
			s.names = append(s.names, name)
		}
	}
}

func (s *session) completions() []string {
	options := []string{
		"/exit", "/quit", "/q", "/help",
		"break", "case", "chan", "const", "continue", "default", "defer",
		"else", "fallthrough", "for", "go", "goto", "if", "import",
		"interface", "map", "package", "range", "return", "select",
		"struct", "switch", "type", "var",
		"append", "bool", "byte", "cap", "close", "complex", "complex64",
		"complex128", "copy", "delete", "error", "false", "float32",
		"float64", "imag", "int", "int8", "int16", "int32", "int64",
		"iota", "len", "make", "new", "nil", "panic", "print", "println",
		"real", "recover", "rune", "string", "true", "uint", "uint8",
		"uint16", "uint32", "uint64", "uintptr",
	}
	options = append(options, s.names...)
	return uniqueStrings(options)
}

func (s *session) packageCompletions(qualifier string) []string {
	path := ""
	for _, imp := range s.imports {
		for _, name := range imp.names {
			if name == qualifier {
				path = imp.path
				break
			}
		}
		if path != "" {
			break
		}
	}
	if path == "" {
		return nil
	}
	return docSymbols(s.dir, path)
}

func docSymbols(dir string, path string) []string {
	cmd := exec.Command("go", "doc", "-short", path)
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil
	}

	var symbols []string
	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "func", "type", "var", "const":
			name := symbolName(fields[1])
			if ast.IsExported(name) {
				symbols = append(symbols, name)
			}
		}
	}
	return uniqueStrings(symbols)
}

func symbolName(text string) string {
	for i, r := range text {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			return text[:i]
		}
	}
	return text
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	var unique []string
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	return unique
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
	if len(s.statements) > 0 {
		b.WriteString("import __gosh_io \"io\"\n")
		b.WriteString("import __gosh_os \"os\"\n")
	}
	if len(s.imports) > 0 || len(s.statements) > 0 {
		b.WriteByte('\n')
	}
	for _, decl := range s.decls {
		b.WriteString(decl)
		b.WriteString("\n\n")
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
	return userImport{text: line, path: importPath(spec), names: names, always: always}, true
}

func parseDeclaration(line string) bool {
	src := "package main\n" + line + "\n"
	file, err := parser.ParseFile(token.NewFileSet(), "input.go", src, 0)
	if err != nil || len(file.Decls) == 0 {
		return false
	}
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok {
			return false
		}
		switch genDecl.Tok {
		case token.CONST, token.TYPE, token.VAR:
		default:
			return false
		}
	}
	return true
}

func declaredNames(line string) []string {
	src := "package main\n" + line + "\n"
	file, err := parser.ParseFile(token.NewFileSet(), "input.go", src, 0)
	if err != nil {
		return nil
	}

	var names []string
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range genDecl.Specs {
			switch spec := spec.(type) {
			case *ast.ValueSpec:
				for _, name := range spec.Names {
					names = append(names, name.Name)
				}
			case *ast.TypeSpec:
				names = append(names, spec.Name.Name)
			}
		}
	}
	return names
}

func parseFunctionDeclaration(line string) bool {
	src := "package main\n" + line + "\n"
	file, err := parser.ParseFile(token.NewFileSet(), "input.go", src, 0)
	if err != nil {
		return false
	}
	for _, decl := range file.Decls {
		if _, ok := decl.(*ast.FuncDecl); ok {
			return true
		}
	}
	return false
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

func importPath(spec *ast.ImportSpec) string {
	return strings.Trim(spec.Path.Value, `"`)
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

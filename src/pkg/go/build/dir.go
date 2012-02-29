// Copyright 2011 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package build

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// A Context specifies the supporting context for a build.
type Context struct {
	GOARCH      string   // target architecture
	GOOS        string   // target operating system
	CgoEnabled  bool     // whether cgo can be used
	BuildTags   []string // additional tags to recognize in +build lines
	UseAllFiles bool     // use files regardless of +build lines, file names

	// By default, ScanDir uses the operating system's
	// file system calls to read directories and files.
	// Callers can override those calls to provide other
	// ways to read data by setting ReadDir and ReadFile.
	// ScanDir does not make any assumptions about the
	// format of the strings dir and file: they can be
	// slash-separated, backslash-separated, even URLs.

	// ReadDir returns a slice of os.FileInfo, sorted by Name,
	// describing the content of the named directory.
	// The dir argument is the argument to ScanDir.
	// If ReadDir is nil, ScanDir uses io.ReadDir.
	ReadDir func(dir string) (fi []os.FileInfo, err error)

	// ReadFile returns the content of the file named file
	// in the directory named dir.  The dir argument is the
	// argument to ScanDir, and the file argument is the
	// Name field from an os.FileInfo returned by ReadDir.
	// The returned path is the full name of the file, to be
	// used in error messages.
	//
	// If ReadFile is nil, ScanDir uses filepath.Join(dir, file)
	// as the path and ioutil.ReadFile to read the data.
	ReadFile func(dir, file string) (path string, content []byte, err error)
}

func (ctxt *Context) readDir(dir string) ([]os.FileInfo, error) {
	if f := ctxt.ReadDir; f != nil {
		return f(dir)
	}
	return ioutil.ReadDir(dir)
}

func (ctxt *Context) readFile(dir, file string) (string, []byte, error) {
	if f := ctxt.ReadFile; f != nil {
		return f(dir, file)
	}
	p := filepath.Join(dir, file)
	content, err := ioutil.ReadFile(p)
	return p, content, err
}

// The DefaultContext is the default Context for builds.
// It uses the GOARCH and GOOS environment variables
// if set, or else the compiled code's GOARCH and GOOS.
var DefaultContext Context = defaultContext()

var cgoEnabled = map[string]bool{
	"darwin/386":    true,
	"darwin/amd64":  true,
	"linux/386":     true,
	"linux/amd64":   true,
	"freebsd/386":   true,
	"freebsd/amd64": true,
	"windows/386":   true,
	"windows/amd64": true,
}

func defaultContext() Context {
	var c Context

	c.GOARCH = envOr("GOARCH", runtime.GOARCH)
	c.GOOS = envOr("GOOS", runtime.GOOS)

	s := os.Getenv("CGO_ENABLED")
	switch s {
	case "1":
		c.CgoEnabled = true
	case "0":
		c.CgoEnabled = false
	default:
		c.CgoEnabled = cgoEnabled[c.GOOS+"/"+c.GOARCH]
	}

	return c
}

func envOr(name, def string) string {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	return s
}

type DirInfo struct {
	Package        string                      // Name of package in dir
	PackageComment *ast.CommentGroup           // Package comments from GoFiles
	ImportPath     string                      // Import path of package in dir
	Imports        []string                    // All packages imported by GoFiles
	ImportPos      map[string][]token.Position // Source code location of imports

	// Source files
	GoFiles  []string // .go files in dir (excluding CgoFiles, TestGoFiles, XTestGoFiles)
	HFiles   []string // .h files in dir
	CFiles   []string // .c files in dir
	SFiles   []string // .s (and, when using cgo, .S files in dir)
	CgoFiles []string // .go files that import "C"

	// Cgo directives
	CgoPkgConfig []string // Cgo pkg-config directives
	CgoCFLAGS    []string // Cgo CFLAGS directives
	CgoLDFLAGS   []string // Cgo LDFLAGS directives

	// Test information
	TestGoFiles   []string // _test.go files in package
	XTestGoFiles  []string // _test.go files outside package
	TestImports   []string // All packages imported by (X)TestGoFiles
	TestImportPos map[string][]token.Position
}

func (d *DirInfo) IsCommand() bool {
	// TODO(rsc): This is at least a little bogus.
	return d.Package == "main"
}

// ScanDir calls DefaultContext.ScanDir.
func ScanDir(dir string) (info *DirInfo, err error) {
	return DefaultContext.ScanDir(dir)
}

// TODO(rsc): Move this comment to a more appropriate place.

// ScanDir returns a structure with details about the Go package
// found in the given directory.
//
// Most .go, .c, .h, and .s files in the directory are considered part
// of the package.  The exceptions are:
//
//	- .go files in package main (unless no other package is found)
//	- .go files in package documentation
//	- files starting with _ or .
//	- files with build constraints not satisfied by the context
//
// Build Constraints
//
// A build constraint is a line comment beginning with the directive +build
// that lists the conditions under which a file should be included in the package.
// Constraints may appear in any kind of source file (not just Go), but
// they must be appear near the top of the file, preceded
// only by blank lines and other line comments.
//
// A build constraint is evaluated as the OR of space-separated options;
// each option evaluates as the AND of its comma-separated terms;
// and each term is an alphanumeric word or, preceded by !, its negation.
// That is, the build constraint:
//
//	// +build linux,386 darwin,!cgo
//
// corresponds to the boolean formula:
//
//	(linux AND 386) OR (darwin AND (NOT cgo))
//
// During a particular build, the following words are satisfied:
//
//	- the target operating system, as spelled by runtime.GOOS
//	- the target architecture, as spelled by runtime.GOARCH
//	- "cgo", if ctxt.CgoEnabled is true
//	- any additional words listed in ctxt.BuildTags
//
// If a file's name, after stripping the extension and a possible _test suffix,
// matches *_GOOS, *_GOARCH, or *_GOOS_GOARCH for any known operating
// system and architecture values, then the file is considered to have an implicit
// build constraint requiring those terms.
//
// Examples
//
// To keep a file from being considered for the build:
//
//	// +build ignore
//
// (any other unsatisfied word will work as well, but ``ignore'' is conventional.)
//
// To build a file only when using cgo, and only on Linux and OS X:
//
//	// +build linux,cgo darwin,cgo
// 
// Such a file is usually paired with another file implementing the
// default functionality for other systems, which in this case would
// carry the constraint:
//
//	// +build !linux !darwin !cgo
//
// Naming a file dns_windows.go will cause it to be included only when
// building the package for Windows; similarly, math_386.s will be included
// only when building the package for 32-bit x86.
//
func (ctxt *Context) ScanDir(dir string) (info *DirInfo, err error) {
	dirs, err := ctxt.readDir(dir)
	if err != nil {
		return nil, err
	}

	var Sfiles []string // files with ".S" (capital S)
	var di DirInfo
	var firstFile string
	imported := make(map[string][]token.Position)
	testImported := make(map[string][]token.Position)
	fset := token.NewFileSet()
	for _, d := range dirs {
		if d.IsDir() {
			continue
		}
		name := d.Name()
		if strings.HasPrefix(name, "_") ||
			strings.HasPrefix(name, ".") {
			continue
		}
		if !ctxt.UseAllFiles && !ctxt.goodOSArchFile(name) {
			continue
		}

		ext := path.Ext(name)
		switch ext {
		case ".go", ".c", ".s", ".h", ".S":
			// tentatively okay
		default:
			// skip
			continue
		}

		filename, data, err := ctxt.readFile(dir, name)
		if err != nil {
			return nil, err
		}

		// Look for +build comments to accept or reject the file.
		if !ctxt.UseAllFiles && !ctxt.shouldBuild(data) {
			continue
		}

		// Going to save the file.  For non-Go files, can stop here.
		switch ext {
		case ".c":
			di.CFiles = append(di.CFiles, name)
			continue
		case ".h":
			di.HFiles = append(di.HFiles, name)
			continue
		case ".s":
			di.SFiles = append(di.SFiles, name)
			continue
		case ".S":
			Sfiles = append(Sfiles, name)
			continue
		}

		pf, err := parser.ParseFile(fset, filename, data, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			return nil, err
		}

		pkg := string(pf.Name.Name)
		if pkg == "documentation" {
			continue
		}

		isTest := strings.HasSuffix(name, "_test.go")
		if isTest && strings.HasSuffix(pkg, "_test") {
			pkg = pkg[:len(pkg)-len("_test")]
		}

		if di.Package == "" {
			di.Package = pkg
			firstFile = name
		} else if pkg != di.Package {
			return nil, fmt.Errorf("%s: found packages %s (%s) and %s (%s)", dir, di.Package, firstFile, pkg, name)
		}
		if pf.Doc != nil {
			if di.PackageComment != nil {
				di.PackageComment.List = append(di.PackageComment.List, pf.Doc.List...)
			} else {
				di.PackageComment = pf.Doc
			}
		}

		// Record imports and information about cgo.
		isCgo := false
		for _, decl := range pf.Decls {
			d, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, dspec := range d.Specs {
				spec, ok := dspec.(*ast.ImportSpec)
				if !ok {
					continue
				}
				quoted := string(spec.Path.Value)
				path, err := strconv.Unquote(quoted)
				if err != nil {
					log.Panicf("%s: parser returned invalid quoted string: <%s>", filename, quoted)
				}
				if isTest {
					testImported[path] = append(testImported[path], fset.Position(spec.Pos()))
				} else {
					imported[path] = append(imported[path], fset.Position(spec.Pos()))
				}
				if path == "C" {
					if isTest {
						return nil, fmt.Errorf("%s: use of cgo in test not supported", filename)
					}
					cg := spec.Doc
					if cg == nil && len(d.Specs) == 1 {
						cg = d.Doc
					}
					if cg != nil {
						if err := ctxt.saveCgo(filename, &di, cg); err != nil {
							return nil, err
						}
					}
					isCgo = true
				}
			}
		}
		if isCgo {
			if ctxt.CgoEnabled {
				di.CgoFiles = append(di.CgoFiles, name)
			}
		} else if isTest {
			if pkg == string(pf.Name.Name) {
				di.TestGoFiles = append(di.TestGoFiles, name)
			} else {
				di.XTestGoFiles = append(di.XTestGoFiles, name)
			}
		} else {
			di.GoFiles = append(di.GoFiles, name)
		}
	}
	if di.Package == "" {
		return nil, fmt.Errorf("%s: no Go source files", dir)
	}
	di.Imports = make([]string, len(imported))
	di.ImportPos = imported
	i := 0
	for p := range imported {
		di.Imports[i] = p
		i++
	}
	di.TestImports = make([]string, len(testImported))
	di.TestImportPos = testImported
	i = 0
	for p := range testImported {
		di.TestImports[i] = p
		i++
	}

	// add the .S files only if we are using cgo
	// (which means gcc will compile them).
	// The standard assemblers expect .s files.
	if len(di.CgoFiles) > 0 {
		di.SFiles = append(di.SFiles, Sfiles...)
		sort.Strings(di.SFiles)
	}

	// File name lists are sorted because ReadDir sorts.
	sort.Strings(di.Imports)
	sort.Strings(di.TestImports)
	return &di, nil
}

var slashslash = []byte("//")

// shouldBuild reports whether it is okay to use this file,
// The rule is that in the file's leading run of // comments
// and blank lines, which must be followed by a blank line
// (to avoid including a Go package clause doc comment),
// lines beginning with '// +build' are taken as build directives.
//
// The file is accepted only if each such line lists something
// matching the file.  For example:
//
//	// +build windows linux
//
// marks the file as applicable only on Windows and Linux.
//
func (ctxt *Context) shouldBuild(content []byte) bool {
	// Pass 1. Identify leading run of // comments and blank lines,
	// which must be followed by a blank line.
	end := 0
	p := content
	for len(p) > 0 {
		line := p
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, p = line[:i], p[i+1:]
		} else {
			p = p[len(p):]
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 { // Blank line
			end = cap(content) - cap(line) // &line[0] - &content[0]
			continue
		}
		if !bytes.HasPrefix(line, slashslash) { // Not comment line
			break
		}
	}
	content = content[:end]

	// Pass 2.  Process each line in the run.
	p = content
	for len(p) > 0 {
		line := p
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, p = line[:i], p[i+1:]
		} else {
			p = p[len(p):]
		}
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, slashslash) {
			line = bytes.TrimSpace(line[len(slashslash):])
			if len(line) > 0 && line[0] == '+' {
				// Looks like a comment +line.
				f := strings.Fields(string(line))
				if f[0] == "+build" {
					ok := false
					for _, tok := range f[1:] {
						if ctxt.match(tok) {
							ok = true
							break
						}
					}
					if !ok {
						return false // this one doesn't match
					}
				}
			}
		}
	}
	return true // everything matches
}

// saveCgo saves the information from the #cgo lines in the import "C" comment.
// These lines set CFLAGS and LDFLAGS and pkg-config directives that affect
// the way cgo's C code is built.
//
// TODO(rsc): This duplicates code in cgo.
// Once the dust settles, remove this code from cgo.
func (ctxt *Context) saveCgo(filename string, di *DirInfo, cg *ast.CommentGroup) error {
	text := cg.Text()
	for _, line := range strings.Split(text, "\n") {
		orig := line

		// Line is
		//	#cgo [GOOS/GOARCH...] LDFLAGS: stuff
		//
		line = strings.TrimSpace(line)
		if len(line) < 5 || line[:4] != "#cgo" || (line[4] != ' ' && line[4] != '\t') {
			continue
		}

		// Split at colon.
		line = strings.TrimSpace(line[4:])
		i := strings.Index(line, ":")
		if i < 0 {
			return fmt.Errorf("%s: invalid #cgo line: %s", filename, orig)
		}
		line, argstr := line[:i], line[i+1:]

		// Parse GOOS/GOARCH stuff.
		f := strings.Fields(line)
		if len(f) < 1 {
			return fmt.Errorf("%s: invalid #cgo line: %s", filename, orig)
		}

		cond, verb := f[:len(f)-1], f[len(f)-1]
		if len(cond) > 0 {
			ok := false
			for _, c := range cond {
				if ctxt.match(c) {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}

		args, err := splitQuoted(argstr)
		if err != nil {
			return fmt.Errorf("%s: invalid #cgo line: %s", filename, orig)
		}
		for _, arg := range args {
			if !safeName(arg) {
				return fmt.Errorf("%s: malformed #cgo argument: %s", filename, arg)
			}
		}

		switch verb {
		case "CFLAGS":
			di.CgoCFLAGS = append(di.CgoCFLAGS, args...)
		case "LDFLAGS":
			di.CgoLDFLAGS = append(di.CgoLDFLAGS, args...)
		case "pkg-config":
			di.CgoPkgConfig = append(di.CgoPkgConfig, args...)
		default:
			return fmt.Errorf("%s: invalid #cgo verb: %s", filename, orig)
		}
	}
	return nil
}

var safeBytes = []byte("+-.,/0123456789=ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz:")

func safeName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x80 && bytes.IndexByte(safeBytes, c) < 0 {
			return false
		}
	}
	return true
}

// splitQuoted splits the string s around each instance of one or more consecutive
// white space characters while taking into account quotes and escaping, and
// returns an array of substrings of s or an empty list if s contains only white space.
// Single quotes and double quotes are recognized to prevent splitting within the
// quoted region, and are removed from the resulting substrings. If a quote in s
// isn't closed err will be set and r will have the unclosed argument as the
// last element.  The backslash is used for escaping.
//
// For example, the following string:
//
//     a b:"c d" 'e''f'  "g\""
//
// Would be parsed as:
//
//     []string{"a", "b:c d", "ef", `g"`}
//
func splitQuoted(s string) (r []string, err error) {
	var args []string
	arg := make([]rune, len(s))
	escaped := false
	quoted := false
	quote := '\x00'
	i := 0
	for _, rune := range s {
		switch {
		case escaped:
			escaped = false
		case rune == '\\':
			escaped = true
			continue
		case quote != '\x00':
			if rune == quote {
				quote = '\x00'
				continue
			}
		case rune == '"' || rune == '\'':
			quoted = true
			quote = rune
			continue
		case unicode.IsSpace(rune):
			if quoted || i > 0 {
				quoted = false
				args = append(args, string(arg[:i]))
				i = 0
			}
			continue
		}
		arg[i] = rune
		i++
	}
	if quoted || i > 0 {
		args = append(args, string(arg[:i]))
	}
	if quote != 0 {
		err = errors.New("unclosed quote")
	} else if escaped {
		err = errors.New("unfinished escaping")
	}
	return args, err
}

// match returns true if the name is one of:
//
//	$GOOS
//	$GOARCH
//	cgo (if cgo is enabled)
//	!cgo (if cgo is disabled)
//	tag (if tag is listed in ctxt.BuildTags)
//	!tag (if tag is not listed in ctxt.BuildTags)
//	a slash-separated list of any of these
//
func (ctxt *Context) match(name string) bool {
	if name == "" {
		return false
	}
	if i := strings.Index(name, ","); i >= 0 {
		// comma-separated list
		return ctxt.match(name[:i]) && ctxt.match(name[i+1:])
	}
	if strings.HasPrefix(name, "!!") { // bad syntax, reject always
		return false
	}
	if strings.HasPrefix(name, "!") { // negation
		return !ctxt.match(name[1:])
	}

	// Tags must be letters, digits, underscores.
	// Unlike in Go identifiers, all digits is fine (e.g., "386").
	for _, c := range name {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_' {
			return false
		}
	}

	// special tags
	if ctxt.CgoEnabled && name == "cgo" {
		return true
	}
	if name == ctxt.GOOS || name == ctxt.GOARCH {
		return true
	}

	// other tags
	for _, tag := range ctxt.BuildTags {
		if tag == name {
			return true
		}
	}

	return false
}

// goodOSArchFile returns false if the name contains a $GOOS or $GOARCH
// suffix which does not match the current system.
// The recognized name formats are:
//
//     name_$(GOOS).*
//     name_$(GOARCH).*
//     name_$(GOOS)_$(GOARCH).*
//     name_$(GOOS)_test.*
//     name_$(GOARCH)_test.*
//     name_$(GOOS)_$(GOARCH)_test.*
//
func (ctxt *Context) goodOSArchFile(name string) bool {
	if dot := strings.Index(name, "."); dot != -1 {
		name = name[:dot]
	}
	l := strings.Split(name, "_")
	if n := len(l); n > 0 && l[n-1] == "test" {
		l = l[:n-1]
	}
	n := len(l)
	if n >= 2 && knownOS[l[n-2]] && knownArch[l[n-1]] {
		return l[n-2] == ctxt.GOOS && l[n-1] == ctxt.GOARCH
	}
	if n >= 1 && knownOS[l[n-1]] {
		return l[n-1] == ctxt.GOOS
	}
	if n >= 1 && knownArch[l[n-1]] {
		return l[n-1] == ctxt.GOARCH
	}
	return true
}

var knownOS = make(map[string]bool)
var knownArch = make(map[string]bool)

func init() {
	for _, v := range strings.Fields(goosList) {
		knownOS[v] = true
	}
	for _, v := range strings.Fields(goarchList) {
		knownArch[v] = true
	}
}

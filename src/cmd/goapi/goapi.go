// Copyright 2011 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Goapi computes the exported API of a set of Go packages.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/doc"
	"go/parser"
	"go/printer"
	"go/token"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Flags
var (
	checkFile = flag.String("c", "", "optional filename to check API against")
	verbose   = flag.Bool("v", false, "Verbose debugging")
)

func main() {
	flag.Parse()

	var pkgs []string
	if flag.NArg() > 0 {
		pkgs = flag.Args()
	} else {
		stds, err := exec.Command("go", "list", "std").Output()
		if err != nil {
			log.Fatal(err)
		}
		pkgs = strings.Fields(string(stds))
	}

	w := NewWalker()
	tree, _, err := build.FindTree("os") // some known package
	if err != nil {
		log.Fatalf("failed to find tree: %v", err)
	}

	for _, pkg := range pkgs {
		if strings.HasPrefix(pkg, "cmd/") ||
			strings.HasPrefix(pkg, "exp/") ||
			strings.HasPrefix(pkg, "old/") {
			continue
		}
		if !tree.HasSrc(pkg) {
			log.Fatalf("no source in tree for package %q", pkg)
		}
		pkgSrcDir := filepath.Join(tree.SrcDir(), filepath.FromSlash(pkg))
		w.WalkPackage(pkg, pkgSrcDir)
	}

	bw := bufio.NewWriter(os.Stdout)
	defer bw.Flush()

	if *checkFile != "" {
		bs, err := ioutil.ReadFile(*checkFile)
		if err != nil {
			log.Fatalf("Error reading file %s: %v", *checkFile, err)
		}
		v1 := strings.Split(string(bs), "\n")
		sort.Strings(v1)
		v2 := w.Features()
		take := func(sl *[]string) string {
			s := (*sl)[0]
			*sl = (*sl)[1:]
			return s
		}
		for len(v1) > 0 || len(v2) > 0 {
			switch {
			case len(v2) == 0 || v1[0] < v2[0]:
				fmt.Fprintf(bw, "-%s\n", take(&v1))
			case len(v1) == 0 || v1[0] > v2[0]:
				fmt.Fprintf(bw, "+%s\n", take(&v2))
			default:
				take(&v1)
				take(&v2)
			}
		}
	} else {
		for _, f := range w.Features() {
			fmt.Fprintf(bw, "%s\n", f)
		}
	}
}

type Walker struct {
	fset           *token.FileSet
	scope          []string
	features       map[string]bool // set
	lastConstType  string
	curPackageName string
	curPackage     *ast.Package
	prevConstType  map[string]string // identifer -> "ideal-int"
}

func NewWalker() *Walker {
	return &Walker{
		fset:     token.NewFileSet(),
		features: make(map[string]bool),
	}
}

// hardCodedConstantType is a hack until the type checker is sufficient for our needs.
// Rather than litter the code with unnecessary type annotations, we'll hard-code
// the cases we can't handle yet.
func (w *Walker) hardCodedConstantType(name string) (typ string, ok bool) {
	switch w.scope[0] {
	case "pkg compress/gzip", "pkg compress/zlib":
		switch name {
		case "NoCompression", "BestSpeed", "BestCompression", "DefaultCompression":
			return "ideal-int", true
		}
	case "pkg os":
		switch name {
		case "WNOHANG", "WSTOPPED", "WUNTRACED":
			return "ideal-int", true
		}
	case "pkg path/filepath":
		switch name {
		case "Separator", "ListSeparator":
			return "char", true
		}
	case "pkg unicode/utf8":
		switch name {
		case "RuneError":
			return "char", true
		}
	case "pkg text/scanner":
		// TODO: currently this tool only resolves const types
		// that reference other constant types if they appear
		// in the right order.  the scanner package has
		// ScanIdents and such coming before the Ident/Int/etc
		// tokens, hence this hack.
		if strings.HasPrefix(name, "Scan") || name == "SkipComments" {
			return "ideal-int", true
		}
	}
	return "", false
}

func (w *Walker) Features() (fs []string) {
	for f := range w.features {
		fs = append(fs, f)
	}
	sort.Strings(fs)
	return
}

func (w *Walker) WalkPackage(name, dir string) {
	log.Printf("package %s", name)
	pop := w.pushScope("pkg " + name)
	defer pop()

	info, err := build.ScanDir(dir)
	if err != nil {
		log.Fatalf("pkg %q, dir %q: ScanDir: %v", name, dir, err)
	}

	apkg := &ast.Package{
		Files: make(map[string]*ast.File),
	}

	files := append(append([]string{}, info.GoFiles...), info.CgoFiles...)
	for _, file := range files {
		f, err := parser.ParseFile(w.fset, filepath.Join(dir, file), nil, 0)
		if err != nil {
			log.Fatalf("error parsing package %s, file %s: %v", name, file, err)
		}
		apkg.Files[file] = f
	}

	w.curPackageName = name
	w.curPackage = apkg
	w.prevConstType = map[string]string{}
	for name, afile := range apkg.Files {
		w.walkFile(filepath.Join(dir, name), afile)
	}

	// Now that we're done walking types, vars and consts
	// in the *ast.Package, use go/doc to do the rest
	// (functions and methods). This is done here because
	// go/doc is destructive.  We can't use the
	// *ast.Package after this.
	dpkg := doc.New(apkg, name, 0)

	for _, t := range dpkg.Types {
		// Move funcs up to the top-level, not hiding in the Types.
		dpkg.Funcs = append(dpkg.Funcs, t.Funcs...)

		for _, m := range t.Methods {
			w.walkFuncDecl(m.Decl)
		}
	}

	for _, f := range dpkg.Funcs {
		w.walkFuncDecl(f.Decl)
	}
}

// pushScope enters a new scope (walking a package, type, node, etc)
// and returns a function that will leave the scope (with sanity checking
// for mismatched pushes & pops)
func (w *Walker) pushScope(name string) (popFunc func()) {
	w.scope = append(w.scope, name)
	return func() {
		if len(w.scope) == 0 {
			log.Fatalf("attempt to leave scope %q with empty scope list", name)
		}
		if w.scope[len(w.scope)-1] != name {
			log.Fatalf("attempt to leave scope %q, but scope is currently %#v", name, w.scope)
		}
		w.scope = w.scope[:len(w.scope)-1]
	}
}

func (w *Walker) walkFile(name string, file *ast.File) {
	// Not entering a scope here; file boundaries aren't interesting.

	for _, di := range file.Decls {
		switch d := di.(type) {
		case *ast.GenDecl:
			switch d.Tok {
			case token.IMPORT:
				continue
			case token.CONST:
				for _, sp := range d.Specs {
					w.walkConst(sp.(*ast.ValueSpec))
				}
			case token.TYPE:
				for _, sp := range d.Specs {
					w.walkTypeSpec(sp.(*ast.TypeSpec))
				}
			case token.VAR:
				for _, sp := range d.Specs {
					w.walkVar(sp.(*ast.ValueSpec))
				}
			default:
				log.Fatalf("unknown token type %d in GenDecl", d.Tok)
			}
		case *ast.FuncDecl:
			// Ignore. Handled in subsequent pass, by go/doc.
		default:
			log.Printf("unhandled %T, %#v\n", di, di)
			printer.Fprint(os.Stderr, w.fset, di)
			os.Stderr.Write([]byte("\n"))
		}
	}
}

var constType = map[token.Token]string{
	token.INT:    "ideal-int",
	token.FLOAT:  "ideal-float",
	token.STRING: "ideal-string",
	token.CHAR:   "ideal-char",
	token.IMAG:   "ideal-imag",
}

var varType = map[token.Token]string{
	token.INT:    "int",
	token.FLOAT:  "float64",
	token.STRING: "string",
	token.CHAR:   "rune",
	token.IMAG:   "complex128",
}

var errTODO = errors.New("TODO")

func (w *Walker) constValueType(vi interface{}) (string, error) {
	switch v := vi.(type) {
	case *ast.BasicLit:
		litType, ok := constType[v.Kind]
		if !ok {
			return "", fmt.Errorf("unknown basic literal kind %#v", v)
		}
		return litType, nil
	case *ast.UnaryExpr:
		return w.constValueType(v.X)
	case *ast.SelectorExpr:
		// e.g. compress/gzip's BestSpeed == flate.BestSpeed
		return "", errTODO
	case *ast.Ident:
		if v.Name == "iota" {
			return "ideal-int", nil // hack.
		}
		if v.Name == "false" || v.Name == "true" {
			return "ideal-bool", nil
		}
		if v.Name == "intSize" && w.curPackageName == "strconv" {
			// Hack.
			return "ideal-int", nil
		}
		if t, ok := w.prevConstType[v.Name]; ok {
			return t, nil
		}
		return "", fmt.Errorf("can't resolve existing constant %q", v.Name)
	case *ast.BinaryExpr:
		left, err := w.constValueType(v.X)
		if err != nil {
			return "", err
		}
		right, err := w.constValueType(v.Y)
		if err != nil {
			return "", err
		}
		if left != right {
			if left == "ideal-int" && right == "ideal-float" {
				return "ideal-float", nil // math.Log2E
			}
			if left == "ideal-char" && right == "ideal-int" {
				return "ideal-int", nil // math/big.MaxBase
			}
			if left == "ideal-int" && right == "ideal-char" {
				return "ideal-int", nil // text/scanner.GoWhitespace
			}
			if left == "ideal-int" && right == "Duration" {
				// Hack, for package time.
				return "Duration", nil
			}
			return "", fmt.Errorf("in BinaryExpr, unhandled type mismatch; left=%q, right=%q", left, right)
		}
		return left, nil
	case *ast.CallExpr:
		// Not a call, but a type conversion.
		return w.nodeString(v.Fun), nil
	case *ast.ParenExpr:
		return w.constValueType(v.X)
	}
	return "", fmt.Errorf("unknown const value type %T", vi)
}

func (w *Walker) varValueType(vi interface{}) (string, error) {
	valStr := w.nodeString(vi)
	if strings.HasPrefix(valStr, "errors.New(") {
		return "error", nil
	}

	switch v := vi.(type) {
	case *ast.BasicLit:
		litType, ok := varType[v.Kind]
		if !ok {
			return "", fmt.Errorf("unknown basic literal kind %#v", v)
		}
		return litType, nil
	case *ast.CompositeLit:
		return w.nodeString(v.Type), nil
	case *ast.FuncLit:
		return w.nodeString(w.namelessType(v.Type)), nil
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			typ, err := w.varValueType(v.X)
			return "*" + typ, err
		}
		return "", fmt.Errorf("unknown unary expr: %#v", v)
	case *ast.SelectorExpr:
		return "", errTODO
	case *ast.Ident:
		node, _, ok := w.resolveName(v.Name)
		if !ok {
			return "", fmt.Errorf("unresolved identifier: %q", v.Name)
		}
		return w.varValueType(node)
	case *ast.BinaryExpr:
		left, err := w.varValueType(v.X)
		if err != nil {
			return "", err
		}
		right, err := w.varValueType(v.Y)
		if err != nil {
			return "", err
		}
		if left != right {
			return "", fmt.Errorf("in BinaryExpr, unhandled type mismatch; left=%q, right=%q", left, right)
		}
		return left, nil
	case *ast.ParenExpr:
		return w.varValueType(v.X)
	case *ast.CallExpr:
		funStr := w.nodeString(v.Fun)
		node, _, ok := w.resolveName(funStr)
		if !ok {
			return "", fmt.Errorf("unresolved named %q", funStr)
		}
		if funcd, ok := node.(*ast.FuncDecl); ok {
			// Assume at the top level that all functions have exactly 1 result
			return w.nodeString(w.namelessType(funcd.Type.Results.List[0].Type)), nil
		}
		// maybe a function call; maybe a conversion.  Need to lookup type.
		return "", fmt.Errorf("resolved name %q to a %T: %#v", funStr, node, node)
	default:
		return "", fmt.Errorf("unknown const value type %T", vi)
	}
	panic("unreachable")
}

// resolveName finds a top-level node named name and returns the node
// v and its type t, if known.
func (w *Walker) resolveName(name string) (v interface{}, t interface{}, ok bool) {
	for _, file := range w.curPackage.Files {
		for _, di := range file.Decls {
			switch d := di.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == name {
					return d, d.Type, true
				}
			case *ast.GenDecl:
				switch d.Tok {
				case token.TYPE:
					for _, sp := range d.Specs {
						ts := sp.(*ast.TypeSpec)
						if ts.Name.Name == name {
							return ts, ts.Type, true
						}
					}
				case token.VAR:
					for _, sp := range d.Specs {
						vs := sp.(*ast.ValueSpec)
						for i, vname := range vs.Names {
							if vname.Name == name {
								if len(vs.Values) > i {
									return vs.Values[i], vs.Type, true
								}
								return nil, vs.Type, true
							}
						}
					}
				}
			}
		}
	}
	return nil, nil, false
}

func (w *Walker) walkConst(vs *ast.ValueSpec) {
	for _, ident := range vs.Names {
		if !ast.IsExported(ident.Name) {
			continue
		}
		litType := ""
		if vs.Type != nil {
			litType = w.nodeString(vs.Type)
		} else {
			litType = w.lastConstType
			if vs.Values != nil {
				if len(vs.Values) != 1 {
					log.Fatalf("const %q, values: %#v", ident.Name, vs.Values)
				}
				var err error
				litType, err = w.constValueType(vs.Values[0])
				if err != nil {
					if t, ok := w.hardCodedConstantType(ident.Name); ok {
						litType = t
						err = nil
					} else {
						log.Fatalf("unknown kind in const %q (%T): %v", ident.Name, vs.Values[0], err)
					}
				}
			}
		}
		if litType == "" {
			log.Fatalf("unknown kind in const %q", ident.Name)
		}
		w.lastConstType = litType

		w.emitFeature(fmt.Sprintf("const %s %s", ident, litType))
		w.prevConstType[ident.Name] = litType
	}
}

func (w *Walker) walkVar(vs *ast.ValueSpec) {
	for i, ident := range vs.Names {
		if !ast.IsExported(ident.Name) {
			continue
		}

		typ := ""
		if vs.Type != nil {
			typ = w.nodeString(vs.Type)
		} else {
			if len(vs.Values) == 0 {
				log.Fatalf("no values for var %q", ident.Name)
			}
			if len(vs.Values) > 1 {
				log.Fatalf("more than 1 values in ValueSpec not handled, var %q", ident.Name)
			}
			var err error
			typ, err = w.varValueType(vs.Values[i])
			if err != nil {
				log.Fatalf("unknown type of variable %q, type %T, error = %v\ncode: %s",
					ident.Name, vs.Values[i], err, w.nodeString(vs.Values[i]))
			}
		}
		w.emitFeature(fmt.Sprintf("var %s %s", ident, typ))
	}
}

func (w *Walker) nodeString(node interface{}) string {
	if node == nil {
		return ""
	}
	var b bytes.Buffer
	printer.Fprint(&b, w.fset, node)
	return b.String()
}

func (w *Walker) nodeDebug(node interface{}) string {
	if node == nil {
		return ""
	}
	var b bytes.Buffer
	ast.Fprint(&b, w.fset, node, nil)
	return b.String()
}

func (w *Walker) walkTypeSpec(ts *ast.TypeSpec) {
	name := ts.Name.Name
	if !ast.IsExported(name) {
		return
	}

	switch t := ts.Type.(type) {
	case *ast.StructType:
		w.walkStructType(name, t)
	case *ast.InterfaceType:
		w.walkInterfaceType(name, t)
	default:
		w.emitFeature(fmt.Sprintf("type %s %s", name, w.nodeString(ts.Type)))
		//log.Fatalf("unknown typespec %T", ts.Type)
	}
}

func (w *Walker) walkStructType(name string, t *ast.StructType) {
	typeStruct := fmt.Sprintf("type %s struct", name)
	w.emitFeature(typeStruct)
	pop := w.pushScope(typeStruct)
	defer pop()
	for _, f := range t.Fields.List {
		typ := f.Type
		for _, name := range f.Names {
			if ast.IsExported(name.Name) {
				w.emitFeature(fmt.Sprintf("%s %s", name, w.nodeString(w.namelessType(typ))))
			}
		}
		if f.Names == nil {
			switch v := typ.(type) {
			case *ast.Ident:
				if ast.IsExported(v.Name) {
					w.emitFeature(fmt.Sprintf("embedded %s", v.Name))
				}
			case *ast.StarExpr:
				switch vv := v.X.(type) {
				case *ast.Ident:
					if ast.IsExported(vv.Name) {
						w.emitFeature(fmt.Sprintf("embedded *%s", vv.Name))
					}
				case *ast.SelectorExpr:
					w.emitFeature(fmt.Sprintf("embedded %s", w.nodeString(typ)))
				default:
					log.Fatal("unable to handle embedded starexpr before %T", typ)
				}
			case *ast.SelectorExpr:
				w.emitFeature(fmt.Sprintf("embedded %s", w.nodeString(typ)))
			default:
				log.Fatalf("unable to handle embedded %T", typ)
			}
		}
	}
}

func (w *Walker) walkInterfaceType(name string, t *ast.InterfaceType) {
	methods := []string{}

	pop := w.pushScope("type " + name + " interface")
	for _, f := range t.Methods.List {
		typ := f.Type
		for _, name := range f.Names {
			if ast.IsExported(name.Name) {
				ft := typ.(*ast.FuncType)
				w.emitFeature(fmt.Sprintf("%s%s", name, w.funcSigString(ft)))
				methods = append(methods, name.Name)
			}
		}
	}
	pop()

	sort.Strings(methods)
	if len(methods) == 0 {
		w.emitFeature(fmt.Sprintf("type %s interface {}", name))
	} else {
		w.emitFeature(fmt.Sprintf("type %s interface { %s }", name, strings.Join(methods, ", ")))
	}
}

func (w *Walker) walkFuncDecl(f *ast.FuncDecl) {
	if !ast.IsExported(f.Name.Name) {
		return
	}
	if f.Recv != nil {
		// Method.
		recvType := w.nodeString(f.Recv.List[0].Type)
		keep := ast.IsExported(recvType) ||
			(strings.HasPrefix(recvType, "*") &&
				ast.IsExported(recvType[1:]))
		if !keep {
			return
		}
		w.emitFeature(fmt.Sprintf("method (%s) %s%s", recvType, f.Name.Name, w.funcSigString(f.Type)))
		return
	}
	// Else, a function
	w.emitFeature(fmt.Sprintf("func %s%s", f.Name.Name, w.funcSigString(f.Type)))
}

func (w *Walker) funcSigString(ft *ast.FuncType) string {
	var b bytes.Buffer
	b.WriteByte('(')
	if ft.Params != nil {
		for i, f := range ft.Params.List {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(w.nodeString(w.namelessType(f.Type)))
		}
	}
	b.WriteByte(')')
	if ft.Results != nil {
		if nr := len(ft.Results.List); nr > 0 {
			b.WriteByte(' ')
			if nr > 1 {
				b.WriteByte('(')
			}
			for i, f := range ft.Results.List {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(w.nodeString(w.namelessType(f.Type)))
			}
			if nr > 1 {
				b.WriteByte(')')
			}
		}
	}
	return b.String()
}

// namelessType returns a type node that lacks any variable names.
func (w *Walker) namelessType(t interface{}) interface{} {
	ft, ok := t.(*ast.FuncType)
	if !ok {
		return t
	}
	return &ast.FuncType{
		Params:  w.namelessFieldList(ft.Params),
		Results: w.namelessFieldList(ft.Results),
	}
}

// namelessFieldList returns a deep clone of fl, with the cloned fields
// lacking names.
func (w *Walker) namelessFieldList(fl *ast.FieldList) *ast.FieldList {
	fl2 := &ast.FieldList{}
	if fl != nil {
		for _, f := range fl.List {
			fl2.List = append(fl2.List, w.namelessField(f))
		}
	}
	return fl2
}

// namelessField clones f, but not preserving the names of fields.
// (comments and tags are also ignored)
func (w *Walker) namelessField(f *ast.Field) *ast.Field {
	return &ast.Field{
		Type: f.Type,
	}
}

func (w *Walker) emitFeature(feature string) {
	f := strings.Join(w.scope, ", ") + ", " + feature
	if _, dup := w.features[f]; dup {
		panic("duplicate feature inserted: " + f)
	}

	if strings.Contains(f, "\n") {
		// TODO: for now, just skip over the
		// runtime.MemStatsType.BySize type, which this tool
		// doesn't properly handle. It's pretty low-level,
		// though, so not super important to protect against.
		if strings.HasPrefix(f, "pkg runtime") && strings.Contains(f, "BySize [61]struct") {
			return
		}
		panic("feature contains newlines: " + f)
	}
	w.features[f] = true
	if *verbose {
		log.Printf("feature: %s", f)
	}
}

func strListContains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

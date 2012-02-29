// Copyright 2011 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"container/heap"
	"errors"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

var cmdBuild = &Command{
	UsageLine: "build [-a] [-n] [-o output] [-p n] [-v] [-x] [-work] [importpath... | gofiles...]",
	Short:     "compile packages and dependencies",
	Long: `
Build compiles the packages named by the import paths,
along with their dependencies, but it does not install the results.

If the arguments are a list of .go files, build treats them as a list
of source files specifying a single package.

When the command line specifies a single main package,
build writes the resulting executable to output (default a.out).
Otherwise build compiles the packages but discards the results,
serving only as a check that the packages can be built.

The -a flag forces rebuilding of packages that are already up-to-date.
The -n flag prints the commands but does not run them.
The -v flag prints the names of packages as they are compiled.
The -x flag prints the commands.

The -o flag specifies the output file name.
It is an error to use -o when the command line specifies multiple packages.

The -p flag specifies the number of builds that can be run in parallel.
The default is the number of CPUs available.

The -work flag causes build to print the name of the temporary work
directory and not delete it when exiting.

For more about import paths, see 'go help importpath'.

See also: go install, go get, go clean.
	`,
}

func init() {
	// break init cycle
	cmdBuild.Run = runBuild
	cmdInstall.Run = runInstall

	addBuildFlags(cmdBuild)
	addBuildFlags(cmdInstall)
}

// Flags set by multiple commands.
var buildA bool               // -a flag
var buildN bool               // -n flag
var buildP = runtime.NumCPU() // -p flag
var buildV bool               // -v flag
var buildX bool               // -x flag
var buildO = cmdBuild.Flag.String("o", "", "output file")
var buildWork bool // -work flag

var buildContext = build.DefaultContext

// addBuildFlags adds the flags common to the build and install commands.
func addBuildFlags(cmd *Command) {
	cmd.Flag.BoolVar(&buildA, "a", false, "")
	cmd.Flag.BoolVar(&buildN, "n", false, "")
	cmd.Flag.IntVar(&buildP, "p", buildP, "")
	cmd.Flag.BoolVar(&buildV, "v", false, "")
	cmd.Flag.BoolVar(&buildX, "x", false, "")
	cmd.Flag.BoolVar(&buildWork, "work", false, "")

	// TODO(rsc): This -t flag is used by buildscript.sh but
	// not documented.  Should be documented but the
	// usage lines are getting too long.  Probably need to say
	// that these flags are applicable to every command and
	// document them in one help message instead of on every
	// command's help message.
	cmd.Flag.Var((*stringsFlag)(&buildContext.BuildTags), "t", "")
}

type stringsFlag []string

func (v *stringsFlag) Set(s string) error {
	*v = append(*v, s)
	return nil
}

func (v *stringsFlag) String() string {
	return "<stringsFlag>"
}

func runBuild(cmd *Command, args []string) {
	var b builder
	b.init()

	var pkgs []*Package
	if len(args) > 0 && strings.HasSuffix(args[0], ".go") {
		pkg := goFilesPackage(args, "")
		pkgs = append(pkgs, pkg)
	} else {
		pkgs = packagesForBuild(args)
	}

	if len(pkgs) == 1 && pkgs[0].Name == "main" && *buildO == "" {
		_, *buildO = path.Split(pkgs[0].ImportPath)
		if b.goos == "windows" {
			*buildO += ".exe"
		}
	}

	if *buildO != "" {
		if len(pkgs) > 1 {
			fatalf("go build: cannot use -o with multiple packages")
		}
		p := pkgs[0]
		p.target = "" // must build - not up to date
		a := b.action(modeInstall, modeBuild, p)
		a.target = *buildO
		b.do(a)
		return
	}

	a := &action{}
	for _, p := range packages(args) {
		a.deps = append(a.deps, b.action(modeBuild, modeBuild, p))
	}
	b.do(a)
}

var cmdInstall = &Command{
	UsageLine: "install [-a] [-n] [-p n] [-v] [-x] [-work] [importpath...]",
	Short:     "compile and install packages and dependencies",
	Long: `
Install compiles and installs the packages named by the import paths,
along with their dependencies.

The -a flag forces reinstallation of packages that are already up-to-date.
The -n flag prints the commands but does not run them.
The -v flag prints the names of packages as they are compiled.
The -x flag prints the commands.

The -p flag specifies the number of builds that can be run in parallel.
The default is the number of CPUs available.

The -work flag causes build to print the name of the temporary work
directory and not delete it when exiting.

For more about import paths, see 'go help importpath'.

See also: go build, go get, go clean.
	`,
}

func runInstall(cmd *Command, args []string) {
	pkgs := packagesForBuild(args)

	var b builder
	b.init()
	a := &action{}
	for _, p := range pkgs {
		a.deps = append(a.deps, b.action(modeInstall, modeInstall, p))
	}
	b.do(a)
}

// A builder holds global state about a build.
// It does not hold per-package state, because eventually we will
// build packages in parallel, and the builder will be shared.
type builder struct {
	work        string               // the temporary work directory (ends in filepath.Separator)
	arch        string               // e.g., "6"
	goarch      string               // the $GOARCH
	goos        string               // the $GOOS
	exe         string               // the executable suffix - "" or ".exe"
	gcflags     []string             // additional flags for Go compiler
	actionCache map[cacheKey]*action // a cache of already-constructed actions
	mkdirCache  map[string]bool      // a cache of created directories
	print       func(args ...interface{}) (int, error)

	output    sync.Mutex
	scriptDir string // current directory in printed script

	exec      sync.Mutex
	readySema chan bool
	ready     actionQueue
}

// An action represents a single action in the action graph.
type action struct {
	p          *Package      // the package this action works on
	deps       []*action     // actions that must happen before this one
	triggers   []*action     // inverse of deps
	cgo        *action       // action for cgo binary if needed
	args       []string      // additional args for runProgram
	testOutput *bytes.Buffer // test output buffer

	f          func(*builder, *action) error // the action itself (nil = no-op)
	ignoreFail bool                          // whether to run f even if dependencies fail

	// Generated files, directories.
	link   bool   // target is executable, not just package
	pkgdir string // the -I or -L argument to use when importing this package
	objdir string // directory for intermediate objects
	objpkg string // the intermediate package .a file created during the action
	target string // goal of the action: the created package or executable

	// Execution state.
	pending  int  // number of deps yet to complete
	priority int  // relative execution priority
	failed   bool // whether the action failed
}

// cacheKey is the key for the action cache.
type cacheKey struct {
	mode buildMode
	p    *Package
}

// buildMode specifies the build mode:
// are we just building things or also installing the results?
type buildMode int

const (
	modeBuild buildMode = iota
	modeInstall
)

var (
	gobin  = build.Path[0].BinDir()
	goroot = build.Path[0].Path
)

func (b *builder) init() {
	var err error
	b.print = fmt.Print
	b.actionCache = make(map[cacheKey]*action)
	b.mkdirCache = make(map[string]bool)
	b.goarch = buildContext.GOARCH
	b.goos = buildContext.GOOS
	if b.goos == "windows" {
		b.exe = ".exe"
	}
	b.gcflags = strings.Fields(os.Getenv("GCFLAGS"))

	b.arch, err = build.ArchChar(b.goarch)
	if err != nil {
		fatalf("%s", err)
	}

	if buildN {
		b.work = "$WORK"
	} else {
		b.work, err = ioutil.TempDir("", "go-build")
		if err != nil {
			fatalf("%s", err)
		}
		if buildX || buildWork {
			fmt.Printf("WORK=%s\n", b.work)
		}
		if !buildWork {
			atexit(func() { os.RemoveAll(b.work) })
		}
	}
}

// goFilesPackage creates a package for building a collection of Go files
// (typically named on the command line).  If target is given, the package
// target is target.  Otherwise, the target is named p.a for
// package p or named after the first Go file for package main.
func goFilesPackage(gofiles []string, target string) *Package {
	// TODO: Remove this restriction.
	for _, f := range gofiles {
		if !strings.HasSuffix(f, ".go") || strings.Contains(f, "/") || strings.Contains(f, string(filepath.Separator)) {
			fatalf("named files must be in current directory and .go files")
		}
	}

	// Synthesize fake "directory" that only shows those two files,
	// to make it look like this is a standard package or
	// command directory.
	var dir []os.FileInfo
	for _, file := range gofiles {
		fi, err := os.Stat(file)
		if err != nil {
			fatalf("%s", err)
		}
		if fi.IsDir() {
			fatalf("%s is a directory, should be a Go file", file)
		}
		dir = append(dir, fi)
	}
	ctxt := buildContext
	ctxt.ReadDir = func(string) ([]os.FileInfo, error) { return dir, nil }
	pwd, _ := os.Getwd()
	var stk importStack
	pkg := scanPackage(&ctxt, &build.Tree{Path: "."}, "<command line>", "<command line>", pwd+"/.", &stk, true)
	if pkg.Error != nil {
		fatalf("%s", pkg.Error)
	}
	printed := map[error]bool{}
	for _, err := range pkg.DepsErrors {
		// Since these are errors in dependencies,
		// the same error might show up multiple times,
		// once in each package that depends on it.
		// Only print each once.
		if !printed[err] {
			printed[err] = true
			errorf("%s", err)
		}
	}
	if target != "" {
		pkg.target = target
	} else if pkg.Name == "main" {
		pkg.target = gofiles[0][:len(gofiles[0])-len(".go")]
	} else {
		pkg.target = pkg.Name + ".a"
	}
	pkg.ImportPath = "_/" + pkg.target
	exitIfErrors()
	return pkg
}

// action returns the action for applying the given operation (mode) to the package.
// depMode is the action to use when building dependencies.
func (b *builder) action(mode buildMode, depMode buildMode, p *Package) *action {
	key := cacheKey{mode, p}
	a := b.actionCache[key]
	if a != nil {
		return a
	}

	a = &action{p: p, pkgdir: p.t.PkgDir()}
	if p.pkgdir != "" { // overrides p.t
		a.pkgdir = p.pkgdir
	}

	b.actionCache[key] = a

	for _, p1 := range p.imports {
		a.deps = append(a.deps, b.action(depMode, depMode, p1))
	}

	// If we are not doing a cross-build, then record the binary we'll
	// generate for cgo as a dependency of the build of any package
	// using cgo, to make sure we do not overwrite the binary while
	// a package is using it.  If this is a cross-build, then the cgo we
	// are writing is not the cgo we need to use.
	if b.goos == runtime.GOOS && b.goarch == runtime.GOARCH {
		if len(p.CgoFiles) > 0 || p.Standard && p.ImportPath == "runtime/cgo" {
			var stk importStack
			p1 := loadPackage("cmd/cgo", &stk)
			if p1.Error != nil {
				fatalf("load cmd/cgo: %v", p1.Error)
			}
			a.cgo = b.action(depMode, depMode, p1)
			a.deps = append(a.deps, a.cgo)
		}
	}

	if p.Standard {
		switch p.ImportPath {
		case "builtin", "unsafe":
			// Fake packages - nothing to build.
			return a
		}
		// gccgo standard library is "fake" too.
		if _, ok := buildToolchain.(gccgoToolchain); ok {
			// the target name is needed for cgo.
			a.target = p.target
			return a
		}
	}

	if !p.Stale && !buildA && p.target != "" {
		// p.Stale==false implies that p.target is up-to-date.
		// Record target name for use by actions depending on this one.
		a.target = p.target
		return a
	}

	a.objdir = filepath.Join(b.work, filepath.FromSlash(a.p.ImportPath+"/_obj")) + string(filepath.Separator)
	a.objpkg = buildToolchain.pkgpath(b.work, a.p)
	a.link = p.Name == "main"

	switch mode {
	case modeInstall:
		a.f = (*builder).install
		a.deps = []*action{b.action(modeBuild, depMode, p)}
		a.target = a.p.target
	case modeBuild:
		a.f = (*builder).build
		a.target = a.objpkg
		if a.link {
			// An executable file.
			// (This is the name of a temporary file.)
			a.target = a.objdir + "a.out" + b.exe
		}
	}

	return a
}

// actionList returns the list of actions in the dag rooted at root
// as visited in a depth-first post-order traversal.
func actionList(root *action) []*action {
	seen := map[*action]bool{}
	all := []*action{}
	var walk func(*action)
	walk = func(a *action) {
		if seen[a] {
			return
		}
		seen[a] = true
		for _, a1 := range a.deps {
			walk(a1)
		}
		all = append(all, a)
	}
	walk(root)
	return all
}

// do runs the action graph rooted at root.
func (b *builder) do(root *action) {
	// Build list of all actions, assigning depth-first post-order priority.
	// The original implementation here was a true queue
	// (using a channel) but it had the effect of getting
	// distracted by low-level leaf actions to the detriment
	// of completing higher-level actions.  The order of
	// work does not matter much to overall execution time,
	// but when running "go test std" it is nice to see each test
	// results as soon as possible.  The priorities assigned
	// ensure that, all else being equal, the execution prefers
	// to do what it would have done first in a simple depth-first
	// dependency order traversal.
	all := actionList(root)
	for i, a := range all {
		a.priority = i
	}

	b.readySema = make(chan bool, len(all))
	done := make(chan bool)

	// Initialize per-action execution state.
	for _, a := range all {
		for _, a1 := range a.deps {
			a1.triggers = append(a1.triggers, a)
		}
		a.pending = len(a.deps)
		if a.pending == 0 {
			b.ready.push(a)
			b.readySema <- true
		}
	}

	// Handle runs a single action and takes care of triggering
	// any actions that are runnable as a result.
	handle := func(a *action) {
		var err error
		if a.f != nil && (!a.failed || a.ignoreFail) {
			err = a.f(b, a)
		}

		// The actions run in parallel but all the updates to the
		// shared work state are serialized through b.exec.
		b.exec.Lock()
		defer b.exec.Unlock()

		if err != nil {
			if err == errPrintedOutput {
				setExitStatus(2)
			} else {
				errorf("%s", err)
			}
			a.failed = true
		}

		for _, a0 := range a.triggers {
			if a.failed {
				a0.failed = true
			}
			if a0.pending--; a0.pending == 0 {
				b.ready.push(a0)
				b.readySema <- true
			}
		}

		if a == root {
			close(b.readySema)
			done <- true
		}
	}

	// Kick off goroutines according to parallelism.
	// If we are using the -n flag (just printing commands)
	// drop the parallelism to 1, both to make the output
	// deterministic and because there is no real work anyway.
	par := buildP
	if buildN {
		par = 1
	}
	for i := 0; i < par; i++ {
		go func() {
			for _ = range b.readySema {
				// Receiving a value from b.sema entitles
				// us to take from the ready queue.
				b.exec.Lock()
				a := b.ready.pop()
				b.exec.Unlock()
				handle(a)
			}
		}()
	}

	<-done
}

// build is the action for building a single package or command.
func (b *builder) build(a *action) error {
	if buildN {
		// In -n mode, print a banner between packages.
		// The banner is five lines so that when changes to
		// different sections of the bootstrap script have to
		// be merged, the banners give patch something
		// to use to find its context.
		fmt.Printf("\n#\n# %s\n#\n\n", a.p.ImportPath)
	}

	if buildV {
		fmt.Fprintf(os.Stderr, "%s\n", a.p.ImportPath)
	}

	// Make build directory.
	obj := a.objdir
	if err := b.mkdir(obj); err != nil {
		return err
	}

	var gofiles, cfiles, sfiles, objects, cgoObjects []string
	gofiles = append(gofiles, a.p.GoFiles...)
	cfiles = append(cfiles, a.p.CFiles...)
	sfiles = append(sfiles, a.p.SFiles...)

	// Run cgo.
	if len(a.p.CgoFiles) > 0 {
		// In a package using cgo, cgo compiles the C and assembly files with gcc.  
		// There is one exception: runtime/cgo's job is to bridge the
		// cgo and non-cgo worlds, so it necessarily has files in both.
		// In that case gcc only gets the gcc_* files.
		var gccfiles []string
		if a.p.Standard && a.p.ImportPath == "runtime/cgo" {
			filter := func(files, nongcc, gcc []string) ([]string, []string) {
				for _, f := range files {
					if strings.HasPrefix(f, "gcc_") {
						gcc = append(gcc, f)
					} else {
						nongcc = append(nongcc, f)
					}
				}
				return nongcc, gcc
			}
			cfiles, gccfiles = filter(cfiles, cfiles[:0], gccfiles)
			sfiles, gccfiles = filter(sfiles, sfiles[:0], gccfiles)
		} else {
			gccfiles = append(cfiles, sfiles...)
			cfiles = nil
			sfiles = nil
		}

		cgoExe := tool("cgo")
		if a.cgo != nil {
			cgoExe = a.cgo.target
		}
		outGo, outObj, err := b.cgo(a.p, cgoExe, obj, gccfiles)
		if err != nil {
			return err
		}
		cgoObjects = append(cgoObjects, outObj...)
		gofiles = append(gofiles, outGo...)
	}

	// Prepare Go import path list.
	inc := b.includeArgs("-I", a.deps)

	// Compile Go.
	if len(gofiles) > 0 {
		if out, err := buildToolchain.gc(b, a.p, obj, inc, gofiles); err != nil {
			return err
		} else {
			objects = append(objects, out)
		}
	}

	// Copy .h files named for goos or goarch or goos_goarch
	// to names using GOOS and GOARCH.
	// For example, defs_linux_amd64.h becomes defs_GOOS_GOARCH.h.
	_goos_goarch := "_" + b.goos + "_" + b.goarch + ".h"
	_goos := "_" + b.goos + ".h"
	_goarch := "_" + b.goarch + ".h"
	for _, file := range a.p.HFiles {
		switch {
		case strings.HasSuffix(file, _goos_goarch):
			targ := file[:len(file)-len(_goos_goarch)] + "_GOOS_GOARCH.h"
			if err := b.copyFile(obj+targ, filepath.Join(a.p.Dir, file), 0666); err != nil {
				return err
			}
		case strings.HasSuffix(file, _goarch):
			targ := file[:len(file)-len(_goarch)] + "_GOARCH.h"
			if err := b.copyFile(obj+targ, filepath.Join(a.p.Dir, file), 0666); err != nil {
				return err
			}
		case strings.HasSuffix(file, _goos):
			targ := file[:len(file)-len(_goos)] + "_GOOS.h"
			if err := b.copyFile(obj+targ, filepath.Join(a.p.Dir, file), 0666); err != nil {
				return err
			}
		}
	}

	for _, file := range cfiles {
		out := file[:len(file)-len(".c")] + "." + b.arch
		if err := buildToolchain.cc(b, a.p, obj, obj+out, file); err != nil {
			return err
		}
		objects = append(objects, out)
	}

	// Assemble .s files.
	for _, file := range sfiles {
		out := file[:len(file)-len(".s")] + "." + b.arch
		if err := buildToolchain.asm(b, a.p, obj, obj+out, file); err != nil {
			return err
		}
		objects = append(objects, out)
	}

	// NOTE(rsc): On Windows, it is critically important that the
	// gcc-compiled objects (cgoObjects) be listed after the ordinary
	// objects in the archive.  I do not know why this is.
	// http://golang.org/issue/2601
	objects = append(objects, cgoObjects...)

	// Pack into archive in obj directory
	if err := buildToolchain.pack(b, a.p, obj, a.objpkg, objects); err != nil {
		return err
	}

	// Link if needed.
	if a.link {
		// The compiler only cares about direct imports, but the
		// linker needs the whole dependency tree.
		all := actionList(a)
		all = all[:len(all)-1] // drop a
		if err := buildToolchain.ld(b, a.p, a.target, all, a.objpkg, objects); err != nil {
			return err
		}
	}

	return nil
}

// install is the action for installing a single package or executable.
func (b *builder) install(a *action) error {
	a1 := a.deps[0]
	perm := os.FileMode(0666)
	if a1.link {
		perm = 0777
	}

	// make target directory
	dir, _ := filepath.Split(a.target)
	if dir != "" {
		if err := b.mkdir(dir); err != nil {
			return err
		}
	}

	// remove object dir to keep the amount of
	// garbage down in a large build.  On an operating system
	// with aggressive buffering, cleaning incrementally like
	// this keeps the intermediate objects from hitting the disk.
	if !buildWork {
		defer os.RemoveAll(a1.objdir)
		defer os.Remove(a1.target)
	}

	return b.copyFile(a.target, a1.target, perm)
}

// includeArgs returns the -I or -L directory list for access
// to the results of the list of actions.
func (b *builder) includeArgs(flag string, all []*action) []string {
	inc := []string{}
	incMap := map[string]bool{
		b.work:                 true, // handled later
		build.Path[0].PkgDir(): true, // goroot
		"":                     true, // ignore empty strings
	}

	// Look in the temporary space for results of test-specific actions.
	// This is the $WORK/my/package/_test directory for the
	// package being built, so there are few of these.
	for _, a1 := range all {
		if dir := a1.pkgdir; dir != a1.p.t.PkgDir() && !incMap[dir] {
			incMap[dir] = true
			inc = append(inc, flag, dir)
		}
	}

	// Also look in $WORK for any non-test packages that have
	// been built but not installed.
	inc = append(inc, flag, b.work)

	// Finally, look in the installed package directories for each action.
	for _, a1 := range all {
		if dir := a1.pkgdir; dir == a1.p.t.PkgDir() && !incMap[dir] {
			if _, ok := buildToolchain.(gccgoToolchain); ok {
				dir = filepath.Join(filepath.Dir(dir), "gccgo", filepath.Base(dir))
			}
			incMap[dir] = true
			inc = append(inc, flag, dir)
		}
	}

	return inc
}

// copyFile is like 'cp src dst'.
func (b *builder) copyFile(dst, src string, perm os.FileMode) error {
	if buildN || buildX {
		b.showcmd("", "cp %s %s", src, dst)
		if buildN {
			return nil
		}
	}

	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()

	// Be careful about removing/overwriting dst.
	// Do not remove/overwrite if dst exists and is a directory
	// or a non-object file.
	if fi, err := os.Stat(dst); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("build output %q already exists and is a directory", dst)
		}
		if !isObject(dst) {
			return fmt.Errorf("build output %q already exists and is not an object file", dst)
		}
	}

	// On Windows, remove lingering ~ file from last attempt.
	if toolIsWindows {
		if _, err := os.Stat(dst + "~"); err == nil {
			os.Remove(dst + "~")
		}
	}

	os.Remove(dst)
	df, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil && toolIsWindows {
		// Windows does not allow deletion of a binary file
		// while it is executing.  Try to move it out of the way.
		// If the remove fails, which is likely, we'll try again the
		// next time we do an install of this binary.
		if err := os.Rename(dst, dst+"~"); err == nil {
			os.Remove(dst + "~")
		}
		df, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	}
	if err != nil {
		return err
	}

	_, err = io.Copy(df, sf)
	df.Close()
	if err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

var objectMagic = [][]byte{
	{'!', '<', 'a', 'r', 'c', 'h', '>', '\n'},        // Package archive
	{'\x7F', 'E', 'L', 'F'},                          // ELF
	{0xFE, 0xED, 0xFA, 0xCE},                         // Mach-O big-endian 32-bit
	{0xFE, 0xED, 0xFA, 0xCF},                         // Mach-O big-endian 64-bit
	{0xCE, 0xFA, 0xED, 0xFE},                         // Mach-O little-endian 32-bit
	{0xCF, 0xFA, 0xED, 0xFE},                         // Mach-O little-endian 64-bit
	{0x4d, 0x5a, 0x90, 0x00, 0x03, 0x00, 0x04, 0x00}, // PE (Windows) as generated by 6l/8l
}

func isObject(s string) bool {
	f, err := os.Open(s)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 64)
	io.ReadFull(f, buf)
	for _, magic := range objectMagic {
		if bytes.HasPrefix(buf, magic) {
			return true
		}
	}
	return false
}

// fmtcmd formats a command in the manner of fmt.Sprintf but also:
//
//	If dir is non-empty and the script is not in dir right now,
//	fmtcmd inserts "cd dir\n" before the command.
//
//	fmtcmd replaces the value of b.work with $WORK.
//	fmtcmd replaces the value of goroot with $GOROOT.
//	fmtcmd replaces the value of b.gobin with $GOBIN.
//
//	fmtcmd replaces the name of the current directory with dot (.)
//	but only when it is at the beginning of a space-separated token.
//
func (b *builder) fmtcmd(dir string, format string, args ...interface{}) string {
	cmd := fmt.Sprintf(format, args...)
	if dir != "" {
		cmd = strings.Replace(" "+cmd, " "+dir, " .", -1)[1:]
		if b.scriptDir != dir {
			b.scriptDir = dir
			cmd = "cd " + dir + "\n" + cmd
		}
	}
	if b.work != "" {
		cmd = strings.Replace(cmd, b.work, "$WORK", -1)
	}
	cmd = strings.Replace(cmd, gobin, "$GOBIN", -1)
	cmd = strings.Replace(cmd, goroot, "$GOROOT", -1)
	return cmd
}

// showcmd prints the given command to standard output
// for the implementation of -n or -x.
func (b *builder) showcmd(dir string, format string, args ...interface{}) {
	b.output.Lock()
	defer b.output.Unlock()
	b.print(b.fmtcmd(dir, format, args...) + "\n")
}

// showOutput prints "# desc" followed by the given output.
// The output is expected to contain references to 'dir', usually
// the source directory for the package that has failed to build.
// showOutput rewrites mentions of dir with a relative path to dir
// when the relative path is shorter.  This is usually more pleasant.
// For example, if fmt doesn't compile and we are in src/pkg/html,
// the output is
//
//	$ go build
//	# fmt
//	../fmt/print.go:1090: undefined: asdf
//	$
//
// instead of
//
//	$ go build
//	# fmt
//	/usr/gopher/go/src/pkg/fmt/print.go:1090: undefined: asdf
//	$
//
// showOutput also replaces references to the work directory with $WORK.
//
func (b *builder) showOutput(dir, desc, out string) {
	prefix := "# " + desc
	suffix := "\n" + out
	pwd, _ := os.Getwd()
	if reldir, err := filepath.Rel(pwd, dir); err == nil && len(reldir) < len(dir) {
		suffix = strings.Replace(suffix, " "+dir, " "+reldir, -1)
		suffix = strings.Replace(suffix, "\n"+dir, "\n"+reldir, -1)
	}
	suffix = strings.Replace(suffix, " "+b.work, " $WORK", -1)

	b.output.Lock()
	defer b.output.Unlock()
	b.print(prefix, suffix)
}

// relPaths returns a copy of paths with absolute paths
// made relative to the current directory if they would be shorter.
func relPaths(paths []string) []string {
	var out []string
	pwd, _ := os.Getwd()
	for _, p := range paths {
		rel, err := filepath.Rel(pwd, p)
		if err == nil && len(rel) < len(p) {
			p = rel
		}
		out = append(out, p)
	}
	return out
}

// errPrintedOutput is a special error indicating that a command failed
// but that it generated output as well, and that output has already
// been printed, so there's no point showing 'exit status 1' or whatever
// the wait status was.  The main executor, builder.do, knows not to
// print this error.
var errPrintedOutput = errors.New("already printed output - no need to show error")

// run runs the command given by cmdline in the directory dir.
// If the commnd fails, run prints information about the failure
// and returns a non-nil error.
func (b *builder) run(dir string, desc string, cmdargs ...interface{}) error {
	out, err := b.runOut(dir, desc, cmdargs...)
	if len(out) > 0 {
		if out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		if desc == "" {
			desc = b.fmtcmd(dir, "%s", strings.Join(stringList(cmdargs...), " "))
		}
		b.showOutput(dir, desc, string(out))
		if err != nil {
			err = errPrintedOutput
		}
	}
	return err
}

// runOut runs the command given by cmdline in the directory dir.
// It returns the command output and any errors that occurred.
func (b *builder) runOut(dir string, desc string, cmdargs ...interface{}) ([]byte, error) {
	cmdline := stringList(cmdargs...)
	if buildN || buildX {
		b.showcmd(dir, "%s", strings.Join(cmdline, " "))
		if buildN {
			return nil, nil
		}
	}

	var buf bytes.Buffer
	cmd := exec.Command(cmdline[0], cmdline[1:]...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.Dir = dir
	// TODO: cmd.Env
	err := cmd.Run()
	return buf.Bytes(), err
}

// mkdir makes the named directory.
func (b *builder) mkdir(dir string) error {
	b.exec.Lock()
	defer b.exec.Unlock()
	// We can be a little aggressive about being
	// sure directories exist.  Skip repeated calls.
	if b.mkdirCache[dir] {
		return nil
	}
	b.mkdirCache[dir] = true

	if buildN || buildX {
		b.showcmd("", "mkdir -p %s", dir)
		if buildN {
			return nil
		}
	}

	if err := os.MkdirAll(dir, 0777); err != nil {
		return err
	}
	return nil
}

// mkAbs returns an absolute path corresponding to
// evaluating f in the directory dir.
// We always pass absolute paths of source files so that
// the error messages will include the full path to a file
// in need of attention.
func mkAbs(dir, f string) string {
	// Leave absolute paths alone.
	// Also, during -n mode we use the pseudo-directory $WORK
	// instead of creating an actual work directory that won't be used.
	// Leave paths beginning with $WORK alone too.
	if filepath.IsAbs(f) || strings.HasPrefix(f, "$WORK") {
		return f
	}
	return filepath.Join(dir, f)
}

type toolchain interface {
	// gc runs the compiler in a specific directory on a set of files
	// and returns the name of the generated output file. 
	gc(b *builder, p *Package, obj string, importArgs []string, gofiles []string) (ofile string, err error)
	// cc runs the toolchain's C compiler in a directory on a C file
	// to produce an output file.
	cc(b *builder, p *Package, objdir, ofile, cfile string) error
	// asm runs the assembler in a specific directory on a specific file
	// to generate the named output file. 
	asm(b *builder, p *Package, obj, ofile, sfile string) error
	// pkgpath creates the appropriate destination path for a package file.
	pkgpath(basedir string, p *Package) string
	// pack runs the archive packer in a specific directory to create
	// an archive from a set of object files.
	// typically it is run in the object directory.
	pack(b *builder, p *Package, objDir, afile string, ofiles []string) error
	// ld runs the linker to create a package starting at mainpkg.
	ld(b *builder, p *Package, out string, allactions []*action, mainpkg string, ofiles []string) error
}

type goToolchain struct{}
type gccgoToolchain struct{}

var buildToolchain toolchain

func init() {
	if os.Getenv("GC") == "gccgo" {
		buildToolchain = gccgoToolchain{}
	} else {
		buildToolchain = goToolchain{}
	}
}

// The Go toolchain.

func (goToolchain) gc(b *builder, p *Package, obj string, importArgs []string, gofiles []string) (ofile string, err error) {
	out := "_go_." + b.arch
	ofile = obj + out
	gcargs := []string{"-p", p.ImportPath}
	if p.Standard && p.ImportPath == "runtime" {
		// runtime compiles with a special 6g flag to emit
		// additional reflect type data.
		gcargs = append(gcargs, "-+")
	}

	args := stringList(tool(b.arch+"g"), "-o", ofile, b.gcflags, gcargs, importArgs)
	for _, f := range gofiles {
		args = append(args, mkAbs(p.Dir, f))
	}
	return ofile, b.run(p.Dir, p.ImportPath, args)
}

func (goToolchain) asm(b *builder, p *Package, obj, ofile, sfile string) error {
	sfile = mkAbs(p.Dir, sfile)
	return b.run(p.Dir, p.ImportPath, tool(b.arch+"a"), "-I", obj, "-o", ofile, "-DGOOS_"+b.goos, "-DGOARCH_"+b.goarch, sfile)
}

func (goToolchain) pkgpath(basedir string, p *Package) string {
	return filepath.Join(basedir, filepath.FromSlash(p.ImportPath+".a"))
}

func (goToolchain) pack(b *builder, p *Package, objDir, afile string, ofiles []string) error {
	var absOfiles []string
	for _, f := range ofiles {
		absOfiles = append(absOfiles, mkAbs(objDir, f))
	}
	return b.run(p.Dir, p.ImportPath, tool("pack"), "grc", mkAbs(objDir, afile), absOfiles)
}

func (goToolchain) ld(b *builder, p *Package, out string, allactions []*action, mainpkg string, ofiles []string) error {
	importArgs := b.includeArgs("-L", allactions)
	return b.run(p.Dir, p.ImportPath, tool(b.arch+"l"), "-o", out, importArgs, mainpkg)
}

func (goToolchain) cc(b *builder, p *Package, objdir, ofile, cfile string) error {
	inc := filepath.Join(goroot, "pkg", fmt.Sprintf("%s_%s", b.goos, b.goarch))
	cfile = mkAbs(p.Dir, cfile)
	return b.run(p.Dir, p.ImportPath, tool(b.arch+"c"), "-FVw",
		"-I", objdir, "-I", inc, "-o", ofile,
		"-DGOOS_"+b.goos, "-DGOARCH_"+b.goarch, cfile)
}

// The Gccgo toolchain.

func (gccgoToolchain) gc(b *builder, p *Package, obj string, importArgs []string, gofiles []string) (ofile string, err error) {
	out := p.Name + ".o"
	ofile = obj + out
	gcargs := []string{"-g"}
	if p.Name != "main" {
		if p.fake {
			gcargs = append(gcargs, "-fgo-prefix=fake_"+p.ImportPath)
		} else {
			gcargs = append(gcargs, "-fgo-prefix=go_"+p.ImportPath)
		}
	}
	args := stringList("gccgo", importArgs, "-c", b.gcflags, gcargs, "-o", ofile)
	for _, f := range gofiles {
		args = append(args, mkAbs(p.Dir, f))
	}
	return ofile, b.run(p.Dir, p.ImportPath, args)
}

func (gccgoToolchain) asm(b *builder, p *Package, obj, ofile, sfile string) error {
	sfile = mkAbs(p.Dir, sfile)
	return b.run(p.Dir, p.ImportPath, "gccgo", "-I", obj, "-o", ofile, "-DGOOS_"+b.goos, "-DGOARCH_"+b.goarch, sfile)
}

func (gccgoToolchain) pkgpath(basedir string, p *Package) string {
	afile := filepath.Join(basedir, filepath.FromSlash(p.ImportPath+".a"))
	// prepend "lib" to the basename
	return filepath.Join(filepath.Dir(afile), "lib"+filepath.Base(afile))
}

func (gccgoToolchain) pack(b *builder, p *Package, objDir, afile string, ofiles []string) error {
	var absOfiles []string
	for _, f := range ofiles {
		absOfiles = append(absOfiles, mkAbs(objDir, f))
	}
	return b.run(p.Dir, p.ImportPath, "ar", "cru", mkAbs(objDir, afile), absOfiles)
}

func (tools gccgoToolchain) ld(b *builder, p *Package, out string, allactions []*action, mainpkg string, ofiles []string) error {
	// gccgo needs explicit linking with all package dependencies,
	// and all LDFLAGS from cgo dependencies
	afiles := []string{}
	ldflags := []string{}
	seen := map[*Package]bool{}
	for _, a := range allactions {
		if a.p != nil && !seen[a.p] {
			seen[a.p] = true
			if !a.p.Standard {
				afiles = append(afiles, a.target)
			}
			ldflags = append(ldflags, a.p.CgoLDFLAGS...)
		}
	}
	return b.run(p.Dir, p.ImportPath, "gccgo", "-o", out, ofiles, "-Wl,-(", afiles, ldflags, "-Wl,-)")
}

func (gccgoToolchain) cc(b *builder, p *Package, objdir, ofile, cfile string) error {
	inc := filepath.Join(goroot, "pkg", fmt.Sprintf("%s_%s", b.goos, b.goarch))
	cfile = mkAbs(p.Dir, cfile)
	return b.run(p.Dir, p.ImportPath, "gcc", "-Wall", "-g",
		"-I", objdir, "-I", inc, "-o", ofile,
		"-DGOOS_"+b.goos, "-DGOARCH_"+b.goarch, "-c", cfile)
}

// gcc runs the gcc C compiler to create an object from a single C file.
func (b *builder) gcc(p *Package, out string, flags []string, cfile string) error {
	cfile = mkAbs(p.Dir, cfile)
	return b.run(p.Dir, p.ImportPath, b.gccCmd(p.Dir), flags, "-o", out, "-c", cfile)
}

// gccld runs the gcc linker to create an executable from a set of object files
func (b *builder) gccld(p *Package, out string, flags []string, obj []string) error {
	return b.run(p.Dir, p.ImportPath, b.gccCmd(p.Dir), "-o", out, obj, flags)
}

// gccCmd returns a gcc command line prefix
func (b *builder) gccCmd(objdir string) []string {
	// TODO: HOST_CC?
	a := []string{"gcc", "-I", objdir, "-g", "-O2"}

	// Definitely want -fPIC but on Windows gcc complains
	// "-fPIC ignored for target (all code is position independent)"
	if b.goos != "windows" {
		a = append(a, "-fPIC")
	}
	switch b.arch {
	case "8":
		a = append(a, "-m32")
	case "6":
		a = append(a, "-m64")
	}
	// gcc-4.5 and beyond require explicit "-pthread" flag
	// for multithreading with pthread library.
	if buildContext.CgoEnabled {
		switch b.goos {
		case "windows":
			a = append(a, "-mthreads")
		default:
			a = append(a, "-pthread")
		}
	}
	return a
}

func envList(key string) []string {
	return strings.Fields(os.Getenv(key))
}

var cgoRe = regexp.MustCompile(`[/\\:]`)

func (b *builder) cgo(p *Package, cgoExe, obj string, gccfiles []string) (outGo, outObj []string, err error) {
	if b.goos != toolGOOS {
		return nil, nil, errors.New("cannot use cgo when compiling for a different operating system")
	}

	cgoCFLAGS := stringList(envList("CGO_CFLAGS"), p.info.CgoCFLAGS)
	cgoLDFLAGS := stringList(envList("CGO_LDFLAGS"), p.info.CgoLDFLAGS)

	if pkgs := p.info.CgoPkgConfig; len(pkgs) > 0 {
		out, err := b.runOut(p.Dir, p.ImportPath, "pkg-config", "--cflags", pkgs)
		if err != nil {
			b.showOutput(p.Dir, "pkg-config --cflags "+strings.Join(pkgs, " "), string(out))
			b.print(err.Error() + "\n")
			return nil, nil, errPrintedOutput
		}
		if len(out) > 0 {
			cgoCFLAGS = append(cgoCFLAGS, strings.Fields(string(out))...)
		}
		out, err = b.runOut(p.Dir, p.ImportPath, "pkg-config", "--libs", pkgs)
		if err != nil {
			b.showOutput(p.Dir, "pkg-config --libs "+strings.Join(pkgs, " "), string(out))
			b.print(err.Error() + "\n")
			return nil, nil, errPrintedOutput
		}
		if len(out) > 0 {
			cgoLDFLAGS = append(cgoLDFLAGS, strings.Fields(string(out))...)
		}
	}

	// Allows including _cgo_export.h from .[ch] files in the package.
	cgoCFLAGS = append(cgoCFLAGS, "-I", obj)

	// cgo
	// TODO: CGOPKGPATH, CGO_FLAGS?
	gofiles := []string{obj + "_cgo_gotypes.go"}
	cfiles := []string{"_cgo_main.c", "_cgo_export.c"}
	for _, fn := range p.CgoFiles {
		f := cgoRe.ReplaceAllString(fn[:len(fn)-2], "_")
		gofiles = append(gofiles, obj+f+"cgo1.go")
		cfiles = append(cfiles, f+"cgo2.c")
	}
	defunC := obj + "_cgo_defun.c"

	cgoflags := []string{}
	// TODO: make cgo not depend on $GOARCH?

	if p.Standard && p.ImportPath == "runtime/cgo" {
		cgoflags = append(cgoflags, "-import_runtime_cgo=false")
	}
	if _, ok := buildToolchain.(gccgoToolchain); ok {
		cgoflags = append(cgoflags, "-gccgo")
	}
	if err := b.run(p.Dir, p.ImportPath, cgoExe, "-objdir", obj, cgoflags, "--", cgoCFLAGS, p.CgoFiles); err != nil {
		return nil, nil, err
	}
	outGo = append(outGo, gofiles...)

	// cc _cgo_defun.c
	defunObj := obj + "_cgo_defun." + b.arch
	if err := buildToolchain.cc(b, p, obj, defunObj, defunC); err != nil {
		return nil, nil, err
	}
	outObj = append(outObj, defunObj)

	// gcc
	var linkobj []string
	for _, cfile := range cfiles {
		ofile := obj + cfile[:len(cfile)-1] + "o"
		if err := b.gcc(p, ofile, cgoCFLAGS, obj+cfile); err != nil {
			return nil, nil, err
		}
		linkobj = append(linkobj, ofile)
		if !strings.HasSuffix(ofile, "_cgo_main.o") {
			outObj = append(outObj, ofile)
		}
	}
	for _, file := range gccfiles {
		ofile := obj + cgoRe.ReplaceAllString(file[:len(file)-1], "_") + "o"
		if err := b.gcc(p, ofile, cgoCFLAGS, file); err != nil {
			return nil, nil, err
		}
		linkobj = append(linkobj, ofile)
		outObj = append(outObj, ofile)
	}
	dynobj := obj + "_cgo_.o"
	if err := b.gccld(p, dynobj, cgoLDFLAGS, linkobj); err != nil {
		return nil, nil, err
	}

	if _, ok := buildToolchain.(gccgoToolchain); ok {
		// we don't use dynimport when using gccgo.
		return outGo, outObj, nil
	}

	// cgo -dynimport
	importC := obj + "_cgo_import.c"
	if err := b.run(p.Dir, p.ImportPath, cgoExe, "-objdir", obj, "-dynimport", dynobj, "-dynout", importC); err != nil {
		return nil, nil, err
	}

	// cc _cgo_import.ARCH
	importObj := obj + "_cgo_import." + b.arch
	if err := buildToolchain.cc(b, p, obj, importObj, importC); err != nil {
		return nil, nil, err
	}

	// NOTE(rsc): The importObj is a 5c/6c/8c object and on Windows
	// must be processed before the gcc-generated objects.
	// Put it first.  http://golang.org/issue/2601
	outObj = append([]string{importObj}, outObj...)

	return outGo, outObj, nil
}

// An actionQueue is a priority queue of actions.
type actionQueue []*action

// Implement heap.Interface
func (q *actionQueue) Len() int           { return len(*q) }
func (q *actionQueue) Swap(i, j int)      { (*q)[i], (*q)[j] = (*q)[j], (*q)[i] }
func (q *actionQueue) Less(i, j int) bool { return (*q)[i].priority < (*q)[j].priority }
func (q *actionQueue) Push(x interface{}) { *q = append(*q, x.(*action)) }
func (q *actionQueue) Pop() interface{} {
	n := len(*q) - 1
	x := (*q)[n]
	*q = (*q)[:n]
	return x
}

func (q *actionQueue) push(a *action) {
	heap.Push(q, a)
}

func (q *actionQueue) pop() *action {
	return heap.Pop(q).(*action)
}

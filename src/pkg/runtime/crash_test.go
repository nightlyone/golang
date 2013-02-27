// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

func executeTest(t *testing.T, templ string, data interface{}) string {
	checkStaleRuntime(t)

	st := template.Must(template.New("crashSource").Parse(templ))

	dir, err := ioutil.TempDir("", "go-build")
	if err != nil {
		t.Fatalf("failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "main.go")
	f, err := os.Create(src)
	if err != nil {
		t.Fatalf("failed to create %v: %v", src, err)
	}
	err = st.Execute(f, data)
	if err != nil {
		f.Close()
		t.Fatalf("failed to execute template: %v", err)
	}
	f.Close()

	// Deadlock tests hang with GOMAXPROCS>1.  Issue 4826.
	cmd := exec.Command("go", "run", src)
	for _, s := range os.Environ() {
		if strings.HasPrefix(s, "GOMAXPROCS") {
			continue
		}
		cmd.Env = append(cmd.Env, s)
	}
	got, _ := cmd.CombinedOutput()
	return string(got)
}

func checkStaleRuntime(t *testing.T) {
	// 'go run' uses the installed copy of runtime.a, which may be out of date.
	out, err := exec.Command("go", "list", "-f", "{{.Stale}}", "runtime").CombinedOutput()
	if err != nil {
		t.Fatalf("failed to execute 'go list': %v\n%v", err, string(out))
	}
	if string(out) != "false\n" {
		t.Fatalf("Stale runtime.a. Run 'go install runtime'.")
	}
}

func testCrashHandler(t *testing.T, cgo bool) {
	type crashTest struct {
		Cgo bool
	}
	got := executeTest(t, crashSource, &crashTest{Cgo: cgo})
	want := "main: recovered done\nnew-thread: recovered done\nsecond-new-thread: recovered done\nmain-again: recovered done\n"
	if got != want {
		t.Fatalf("expected %q, but got %q", want, got)
	}
}

func TestCrashHandler(t *testing.T) {
	testCrashHandler(t, false)
}

func testDeadlock(t *testing.T, source string) {
	got := executeTest(t, source, nil)
	want := "fatal error: all goroutines are asleep - deadlock!\n"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("expected %q, but got %q", want, got)
	}
}

func TestSimpleDeadlock(t *testing.T) {
	testDeadlock(t, simpleDeadlockSource)
}

func TestInitDeadlock(t *testing.T) {
	testDeadlock(t, initDeadlockSource)
}

func TestLockedDeadlock(t *testing.T) {
	testDeadlock(t, lockedDeadlockSource)
}

func TestLockedDeadlock2(t *testing.T) {
	testDeadlock(t, lockedDeadlockSource2)
}

func TestCgoSignalDeadlock(t *testing.T) {
	got := executeTest(t, cgoSignalDeadlockSource, nil)
	want := "OK\n"
	if got != want {
		t.Fatalf("expected %q, but got %q", want, got)
	}
}

const crashSource = `
package main

import (
	"fmt"
	"runtime"
)

{{if .Cgo}}
import "C"
{{end}}

func test(name string) {
	defer func() {
		if x := recover(); x != nil {
			fmt.Printf(" recovered")
		}
		fmt.Printf(" done\n")
	}()
	fmt.Printf("%s:", name)
	var s *string
	_ = *s
	fmt.Print("SHOULD NOT BE HERE")
}

func testInNewThread(name string) {
	c := make(chan bool)
	go func() {
		runtime.LockOSThread()
		test(name)
		c <- true
	}()
	<-c
}

func main() {
	runtime.LockOSThread()
	test("main")
	testInNewThread("new-thread")
	testInNewThread("second-new-thread")
	test("main-again")
}
`

const simpleDeadlockSource = `
package main
func main() {
	select {}
}
`

const initDeadlockSource = `
package main
func init() {
	select {}
}
func main() {
}
`

const lockedDeadlockSource = `
package main
import "runtime"
func main() {
	runtime.LockOSThread()
	select {}
}
`

const lockedDeadlockSource2 = `
package main
import (
	"runtime"
	"time"
)
func main() {
	go func() {
		runtime.LockOSThread()
		select {}
	}()
	time.Sleep(time.Millisecond)
	select {}
}
`

const cgoSignalDeadlockSource = `
package main

import "C"

import (
	"fmt"
	"runtime"
	"time"
)

func main() {
	runtime.GOMAXPROCS(100)
	ping := make(chan bool)
	go func() {
		for i := 0; ; i++ {
			runtime.Gosched()
			select {
			case done := <-ping:
				if done {
					ping <- true
					return
				}
				ping <- true
			default:
			}
			func() {
				defer func() {
					recover()
				}()
				var s *string
				*s = ""
			}()
		}
	}()
	time.Sleep(time.Millisecond)
	for i := 0; i < 64; i++ {
		go func() {
			runtime.LockOSThread()
			select {}
		}()
		go func() {
			runtime.LockOSThread()
			select {}
		}()
		time.Sleep(time.Millisecond)
		ping <- false
		select {
		case <-ping:
		case <-time.After(time.Second):
			fmt.Printf("HANG\n")
			return
		}
	}
	ping <- true
	select {
	case <-ping:
	case <-time.After(time.Second):
		fmt.Printf("HANG\n")
		return
	}
	fmt.Printf("OK\n")
}
`

// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package html

import (
	"fmt"
)

// Error describes a problem encountered during template Escaping.
type Error struct {
	// ErrorCode describes the kind of error.
	ErrorCode ErrorCode
	// Name is the name of the template in which the error was encountered.
	Name string
	// Line is the line number of the error in the template source or 0.
	Line int
	// Description is a human-readable description of the problem.
	Description string
}

// ErrorCode is a code for a kind of error.
type ErrorCode int

// We define codes for each error that manifests while escaping templates, but
// escaped templates may also fail at runtime.
//
// Output: "ZgotmplZ"
// Example:
//   <img src="{{.X}}">
//   where {{.X}} evaluates to `javascript:...`
// Discussion:
//   "ZgotmplZ" is a special value that indicates that unsafe content reached a
//   CSS or URL context at runtime. The output of the example will be
//     <img src="#ZgotmplZ">
//   If the data comes from a trusted source, use content types to exempt it
//   from filtering: URL(`javascript:...`).
const (
	// OK indicates the lack of an error.
	OK ErrorCode = iota

	// ErrorAmbigContext: "... appears in an ambiguous URL context"
	// Example:
	//   <a href="
	//      {{if .C}}
	//        /path/
	//      {{else}}
	//        /search?q=
	//      {{end}}
	//      {{.X}}
	//   ">
	// Discussion:
	//   {{.X}} is in an ambiguous URL context since, depending on {{.C}},
	//  it may be either a URL suffix or a query parameter.
	//   Moving {{.X}} into the condition removes the ambiguity:
	//   <a href="{{if .C}}/path/{{.X}}{{else}}/search?q={{.X}}">
	ErrAmbigContext

	// TODO: document
	ErrBadHTML

	// ErrBranchEnd: "{{if}} branches end in different contexts"
	// Example:
	//   {{if .C}}<a href="{{end}}{{.X}}
	// Discussion:
	//   EscapeSet statically examines each possible path when it encounters
	//   a {{if}}, {{range}}, or {{with}} to escape any following pipelines.
	//   The example is ambiguous since {{.X}} might be an HTML text node,
	//   or a URL prefix in an HTML attribute. EscapeSet needs to understand
	//   the context of {{.X}} to escape it, but that depends on the
	//   run-time value of {{.C}}.
	//
	//   The problem is usually something like missing quotes or angle
	//   brackets, or can be avoided by refactoring to put the two contexts
	//   into different branches of an if, range or with. If the problem
	//   is in a {{range}} over a collection that should never be empty,
	//   adding a dummy {{else}} can help.
	ErrBranchEnd

	// ErrEndContext: "... ends in a non-text context: ..."
	// Examples:
	//   <div
	//   <div title="no close quote>
	//   <script>f()
	// Discussion:
	//   EscapeSet assumes the ouput is a DocumentFragment of HTML.
	//   Templates that end without closing tags will trigger this error.
	//   Templates that produce incomplete Fragments should not be named
	//   in the call to EscapeSet.
	//
	// If you have a helper template in your set that is not meant to
	// produce a document fragment, then do not pass its name to
	// EscapeSet(set, ...names).
	//
	//   {{define "main"}} <script>{{template "helper"}}</script> {{end}}
	//   {{define "helper"}} document.write(' <div title=" ') {{end}}
	// 
	// "helper" does not produce a valid document fragment, though it does
	// produce a valid JavaScript Program.
	ErrEndContext

	// ErrNoNames: "must specify names of top level templates"
	// 
	//   EscapeSet does not assume that all templates in a set produce HTML.
	//   Some may be helpers that produce snippets of other languages.
	//   Passing in no template names is most likely an error,
	//   so EscapeSet(set) will panic.
	//   If you call EscapeSet with a slice of names, guard it with len:
	// 
	//     if len(names) != 0 {
	//       set, err := EscapeSet(set, ...names)
	//     }
	ErrNoNames

	// ErrNoSuchTemplate: "no such template ..."
	// Examples:
	//    {{define "main"}}<div {{template "attrs"}}>{{end}}
	//    {{define "attrs"}}href="{{.URL}}"{{end}}
	// Discussion:
	//   EscapeSet looks through template calls to compute the context.
	//   Here the {{.URL}} in "attrs" must be treated as a URL when called
	//   from "main", but if "attrs" is not in set when
	//   EscapeSet(&set, "main") is called, this error will arise.
	ErrNoSuchTemplate

	// TODO: document
	ErrOutputContext

	// ErrPartialCharset: "unfinished JS regexp charset in ..."
	// Example:
	//     <script>var pattern = /foo[{{.Chars}}]/</script>
	// Discussion:
	//   EscapeSet does not support interpolation into regular expression
	//   literal character sets.
	ErrPartialCharset

	// ErrPartialEscape: "unfinished escape sequence in ..."
	// Example:
	//   <script>alert("\{{.X}}")</script>
	// Discussion:
	//   EscapeSet does not support actions following a backslash.
	//   This is usually an error and there are better solutions; for
	//   our example
	//     <script>alert("{{.X}}")</script>
	//   should work, and if {{.X}} is a partial escape sequence such as
	//   "xA0", mark the whole sequence as safe content: JSStr(`\xA0`)
	ErrPartialEscape

	// ErrRangeLoopReentry: "on range loop re-entry: ..."
	// Example:
	//   {{range .}}<p class={{.}}{{end}}
	// Discussion:
	//   If an iteration through a range would cause it to end in a
	//   different context than an earlier pass, there is no single context.
	//   In the example, the <p> tag is missing a '>'.
	//   EscapeSet cannot tell whether {{.}} is meant to be an HTML class or
	//   the content of a broken <p> element and complains because the
	//   second iteration would produce something like
	// 
	//     <p class=foo<p class=bar
	ErrRangeLoopReentry

	// TODO: document
	ErrSlashAmbig
)

func (e *Error) String() string {
	if e.Line != 0 {
		return fmt.Sprintf("exp/template/html:%s:%d: %s", e.Name, e.Line, e.Description)
	} else if e.Name != "" {
		return fmt.Sprintf("exp/template/html:%s: %s", e.Name, e.Description)
	}
	return "exp/template/html: " + e.Description
}

// errorf creates an error given a format string f and args.
// The template Name still needs to be supplied.
func errorf(k ErrorCode, line int, f string, args ...interface{}) *Error {
	return &Error{k, "", line, fmt.Sprintf(f, args...)}
}

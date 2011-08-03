// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package template

import (
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
)

// state represents the state of an execution. It's not part of the
// template so that multiple executions of the same template
// can execute in parallel.
type state struct {
	tmpl *Template
	wr   io.Writer
	line int        // line number for errors
	vars []variable // push-down stack of variable values.
}

// variable holds the dynamic value of a variable such as $, $x etc.
type variable struct {
	name  string
	value reflect.Value
}

// push pushes a new variable on the stack.
func (s *state) push(name string, value reflect.Value) {
	s.vars = append(s.vars, variable{name, value})
}

// mark returns the length of the variable stack.
func (s *state) mark() int {
	return len(s.vars)
}

// pop pops the variable stack up to the mark.
func (s *state) pop(mark int) {
	s.vars = s.vars[0:mark]
}

// setVar overwrites the top-nth variable on the stack. Used by range iterations.
func (s *state) setVar(n int, value reflect.Value) {
	s.vars[len(s.vars)-n].value = value
}

// varValue returns the value of the named variable.
func (s *state) varValue(name string) reflect.Value {
	for i := s.mark() - 1; i >= 0; i-- {
		if s.vars[i].name == name {
			return s.vars[i].value
		}
	}
	s.errorf("undefined variable: %s", name)
	return zero
}

var zero reflect.Value

// errorf formats the error and terminates processing.
func (s *state) errorf(format string, args ...interface{}) {
	format = fmt.Sprintf("template: %s:%d: %s", s.tmpl.name, s.line, format)
	panic(fmt.Errorf(format, args...))
}

// error terminates processing.
func (s *state) error(err os.Error) {
	s.errorf("%s", err)
}

// Execute applies a parsed template to the specified data object,
// writing the output to wr.
func (t *Template) Execute(wr io.Writer, data interface{}) (err os.Error) {
	defer t.recover(&err)
	value := reflect.ValueOf(data)
	state := &state{
		tmpl: t,
		wr:   wr,
		line: 1,
		vars: []variable{{"$", value}},
	}
	if t.root == nil {
		state.errorf("must be parsed before execution")
	}
	state.walk(value, t.root)
	return
}

// Walk functions step through the major pieces of the template structure,
// generating output as they go.
func (s *state) walk(dot reflect.Value, n node) {
	switch n := n.(type) {
	case *actionNode:
		s.line = n.line
		// Do not pop variables so they persist until next end.
		// Also, if the action declares variables, don't print the result.
		val := s.evalPipeline(dot, n.pipe)
		if len(n.pipe.decl) == 0 {
			s.printValue(n, val)
		}
	case *ifNode:
		s.line = n.line
		s.walkIfOrWith(nodeIf, dot, n.pipe, n.list, n.elseList)
	case *listNode:
		for _, node := range n.nodes {
			s.walk(dot, node)
		}
	case *rangeNode:
		s.line = n.line
		s.walkRange(dot, n)
	case *templateNode:
		s.line = n.line
		s.walkTemplate(dot, n)
	case *textNode:
		if _, err := s.wr.Write(n.text); err != nil {
			s.error(err)
		}
	case *withNode:
		s.line = n.line
		s.walkIfOrWith(nodeWith, dot, n.pipe, n.list, n.elseList)
	default:
		s.errorf("unknown node: %s", n)
	}
}

// walkIfOrWith walks an 'if' or 'with' node. The two control structures
// are identical in behavior except that 'with' sets dot.
func (s *state) walkIfOrWith(typ nodeType, dot reflect.Value, pipe *pipeNode, list, elseList *listNode) {
	defer s.pop(s.mark())
	val := s.evalPipeline(dot, pipe)
	truth, ok := isTrue(val)
	if !ok {
		s.errorf("if/with can't use value of type %T", val.Interface())
	}
	if truth {
		if typ == nodeWith {
			s.walk(val, list)
		} else {
			s.walk(dot, list)
		}
	} else if elseList != nil {
		s.walk(dot, elseList)
	}
}

// isTrue returns whether the value is 'true', in the sense of not the zero of its type,
// and whether the value has a meaningful truth value.
func isTrue(val reflect.Value) (truth, ok bool) {
	switch val.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		truth = val.Len() > 0
	case reflect.Bool:
		truth = val.Bool()
	case reflect.Complex64, reflect.Complex128:
		truth = val.Complex() != 0
	case reflect.Chan, reflect.Func, reflect.Ptr:
		truth = !val.IsNil()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		truth = val.Int() != 0
	case reflect.Float32, reflect.Float64:
		truth = val.Float() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		truth = val.Uint() != 0
	default:
		return
	}
	return truth, true
}

func (s *state) walkRange(dot reflect.Value, r *rangeNode) {
	defer s.pop(s.mark())
	val, _ := indirect(s.evalPipeline(dot, r.pipe))
	// mark top of stack before any variables in the body are pushed.
	mark := s.mark()
	switch val.Kind() {
	case reflect.Array, reflect.Slice:
		if val.Len() == 0 {
			break
		}
		for i := 0; i < val.Len(); i++ {
			elem := val.Index(i)
			// Set top var (lexically the second if there are two) to the element.
			if len(r.pipe.decl) > 0 {
				s.setVar(1, elem)
			}
			// Set next var (lexically the first if there are two) to the index.
			if len(r.pipe.decl) > 1 {
				s.setVar(2, reflect.ValueOf(i))
			}
			s.walk(elem, r.list)
			s.pop(mark)
		}
		return
	case reflect.Map:
		if val.Len() == 0 {
			break
		}
		for _, key := range val.MapKeys() {
			elem := val.MapIndex(key)
			// Set top var (lexically the second if there are two) to the element.
			if len(r.pipe.decl) > 0 {
				s.setVar(1, elem)
			}
			// Set next var (lexically the first if there are two) to the key.
			if len(r.pipe.decl) > 1 {
				s.setVar(2, key)
			}
			s.walk(elem, r.list)
			s.pop(mark)
		}
		return
	default:
		s.errorf("range can't iterate over value of type %T", val.Interface())
	}
	if r.elseList != nil {
		s.walk(dot, r.elseList)
	}
}

func (s *state) walkTemplate(dot reflect.Value, t *templateNode) {
	set := s.tmpl.set
	if set == nil {
		s.errorf("no set defined in which to invoke template named %q", t.name)
	}
	tmpl := set.tmpl[t.name]
	if tmpl == nil {
		s.errorf("template %q not in set", t.name)
	}
	// Variables declared by the pipeline persist.
	dot = s.evalPipeline(dot, t.pipe)
	newState := *s
	newState.tmpl = tmpl
	// No dynamic scoping: template invocations inherit no variables.
	newState.vars = []variable{{"$", dot}}
	newState.walk(dot, tmpl.root)
}

// Eval functions evaluate pipelines, commands, and their elements and extract
// values from the data structure by examining fields, calling methods, and so on.
// The printing of those values happens only through walk functions.

// evalPipeline returns the value acquired by evaluating a pipeline. If the
// pipeline has a variable declaration, the variable will be pushed on the
// stack. Callers should therefore pop the stack after they are finished
// executing commands depending on the pipeline value.
func (s *state) evalPipeline(dot reflect.Value, pipe *pipeNode) (value reflect.Value) {
	if pipe == nil {
		return
	}
	for _, cmd := range pipe.cmds {
		value = s.evalCommand(dot, cmd, value) // previous value is this one's final arg.
		// If the object has type interface{}, dig down one level to the thing inside.
		if value.Kind() == reflect.Interface && value.Type().NumMethod() == 0 {
			value = reflect.ValueOf(value.Interface()) // lovely!
		}
	}
	for _, variable := range pipe.decl {
		s.push(variable.ident[0], value)
	}
	return value
}

func (s *state) notAFunction(args []node, final reflect.Value) {
	if len(args) > 1 || final.IsValid() {
		s.errorf("can't give argument to non-function %s", args[0])
	}
}

func (s *state) evalCommand(dot reflect.Value, cmd *commandNode, final reflect.Value) reflect.Value {
	firstWord := cmd.args[0]
	switch n := firstWord.(type) {
	case *fieldNode:
		return s.evalFieldNode(dot, n, cmd.args, final)
	case *identifierNode:
		// Must be a function.
		return s.evalFunction(dot, n.ident, cmd.args, final)
	case *variableNode:
		return s.evalVariableNode(dot, n, cmd.args, final)
	}
	s.notAFunction(cmd.args, final)
	switch word := firstWord.(type) {
	case *boolNode:
		return reflect.ValueOf(word.true)
	case *dotNode:
		return dot
	case *numberNode:
		return s.idealConstant(word)
	case *stringNode:
		return reflect.ValueOf(word.text)
	}
	s.errorf("can't evaluate command %q", firstWord)
	panic("not reached")
}

// idealConstant is called to return the value of a number in a context where
// we don't know the type. In that case, the syntax of the number tells us
// its type, and we use Go rules to resolve.  Note there is no such thing as
// a uint ideal constant in this situation - the value must be of int type.
func (s *state) idealConstant(constant *numberNode) reflect.Value {
	// These are ideal constants but we don't know the type
	// and we have no context.  (If it was a method argument,
	// we'd know what we need.) The syntax guides us to some extent.
	switch {
	case constant.isComplex:
		return reflect.ValueOf(constant.complex128) // incontrovertible.
	case constant.isFloat && strings.IndexAny(constant.text, ".eE") >= 0:
		return reflect.ValueOf(constant.float64)
	case constant.isInt:
		n := int(constant.int64)
		if int64(n) != constant.int64 {
			s.errorf("%s overflows int", constant.text)
		}
		return reflect.ValueOf(n)
	case constant.isUint:
		s.errorf("%s overflows int", constant.text)
	}
	return zero
}

func (s *state) evalFieldNode(dot reflect.Value, field *fieldNode, args []node, final reflect.Value) reflect.Value {
	return s.evalFieldChain(dot, dot, field.ident, args, final)
}

func (s *state) evalVariableNode(dot reflect.Value, v *variableNode, args []node, final reflect.Value) reflect.Value {
	// $x.Field has $x as the first ident, Field as the second. Eval the var, then the fields.
	value := s.varValue(v.ident[0])
	if len(v.ident) == 1 {
		return value
	}
	return s.evalFieldChain(dot, value, v.ident[1:], args, final)
}

// evalFieldChain evaluates .X.Y.Z possibly followed by arguments.
// dot is the environment in which to evaluate arguments, while
// receiver is the value being walked along the chain.
func (s *state) evalFieldChain(dot, receiver reflect.Value, ident []string, args []node, final reflect.Value) reflect.Value {
	n := len(ident)
	for i := 0; i < n-1; i++ {
		receiver = s.evalField(dot, ident[i], nil, zero, receiver)
	}
	// Now if it's a method, it gets the arguments.
	return s.evalField(dot, ident[n-1], args, final, receiver)
}

func (s *state) evalFunction(dot reflect.Value, name string, args []node, final reflect.Value) reflect.Value {
	function, ok := findFunction(name, s.tmpl, s.tmpl.set)
	if !ok {
		s.errorf("%q is not a defined function", name)
	}
	return s.evalCall(dot, function, name, args, final)
}

// evalField evaluates an expression like (.Field) or (.Field arg1 arg2).
// The 'final' argument represents the return value from the preceding
// value of the pipeline, if any.
func (s *state) evalField(dot reflect.Value, fieldName string, args []node, final, receiver reflect.Value) reflect.Value {
	if !receiver.IsValid() {
		return zero
	}
	typ := receiver.Type()
	receiver, _ = indirect(receiver)
	// Need to get to a value of type *T to guarantee we see all
	// methods of T and *T.
	ptr := receiver
	if ptr.CanAddr() {
		ptr = ptr.Addr()
	}
	if method, ok := methodByName(ptr, fieldName); ok {
		return s.evalCall(dot, method, fieldName, args, final)
	}
	// It's not a method; is it a field of a struct?
	receiver, isNil := indirect(receiver)
	if receiver.Kind() == reflect.Struct {
		tField, ok := receiver.Type().FieldByName(fieldName)
		if ok {
			field := receiver.FieldByIndex(tField.Index)
			if len(args) > 1 || final.IsValid() {
				s.errorf("%s is not a method but has arguments", fieldName)
			}
			if tField.PkgPath == "" { // field is exported
				return field
			}
		}
	}
	if isNil {
		s.errorf("nil pointer evaluating %s.%s", typ, fieldName)
	}
	s.errorf("can't evaluate field %s in type %s", fieldName, typ)
	panic("not reached")
}

// TODO: delete when reflect's own MethodByName is released.
func methodByName(receiver reflect.Value, name string) (reflect.Value, bool) {
	typ := receiver.Type()
	for i := 0; i < typ.NumMethod(); i++ {
		if typ.Method(i).Name == name {
			return receiver.Method(i), true // This value includes the receiver.
		}
	}
	return zero, false
}

var (
	osErrorType = reflect.TypeOf(new(os.Error)).Elem()
)

// evalCall executes a function or method call. If it's a method, fun already has the receiver bound, so
// it looks just like a function call.  The arg list, if non-nil, includes (in the manner of the shell), arg[0]
// as the function itself.
func (s *state) evalCall(dot, fun reflect.Value, name string, args []node, final reflect.Value) reflect.Value {
	if args != nil {
		args = args[1:] // Zeroth arg is function name/node; not passed to function.
	}
	typ := fun.Type()
	numIn := len(args)
	if final.IsValid() {
		numIn++
	}
	numFixed := len(args)
	if typ.IsVariadic() {
		numFixed = typ.NumIn() - 1 // last arg is the variadic one.
		if numIn < numFixed {
			s.errorf("wrong number of args for %s: want at least %d got %d", name, typ.NumIn()-1, len(args))
		}
	} else if numIn < typ.NumIn()-1 || !typ.IsVariadic() && numIn != typ.NumIn() {
		s.errorf("wrong number of args for %s: want %d got %d", name, typ.NumIn(), len(args))
	}
	if !goodFunc(typ) {
		s.errorf("can't handle multiple results from method/function %q", name)
	}
	// Build the arg list.
	argv := make([]reflect.Value, numIn)
	// Args must be evaluated. Fixed args first.
	i := 0
	for ; i < numFixed; i++ {
		argv[i] = s.evalArg(dot, typ.In(i), args[i])
	}
	// Now the ... args.
	if typ.IsVariadic() {
		argType := typ.In(typ.NumIn() - 1).Elem() // Argument is a slice.
		for ; i < len(args); i++ {
			argv[i] = s.evalArg(dot, argType, args[i])
		}
	}
	// Add final value if necessary.
	if final.IsValid() {
		argv[i] = final
	}
	result := fun.Call(argv)
	// If we have an os.Error that is not nil, stop execution and return that error to the caller.
	if len(result) == 2 && !result[1].IsNil() {
		s.errorf("error calling %s: %s", name, result[1].Interface().(os.Error))
	}
	return result[0]
}

// validateType guarantees that the value is valid and assignable to the type.
func (s *state) validateType(value reflect.Value, typ reflect.Type) reflect.Value {
	if !value.IsValid() {
		s.errorf("invalid value; expected %s", typ)
	}
	if !value.Type().AssignableTo(typ) {
		s.errorf("wrong type for value; expected %s; got %s", typ, value.Type())
	}
	return value
}

func (s *state) evalArg(dot reflect.Value, typ reflect.Type, n node) reflect.Value {
	switch arg := n.(type) {
	case *dotNode:
		return s.validateType(dot, typ)
	case *fieldNode:
		return s.validateType(s.evalFieldNode(dot, arg, []node{n}, zero), typ)
	case *variableNode:
		return s.validateType(s.evalVariableNode(dot, arg, nil, zero), typ)
	}
	switch typ.Kind() {
	case reflect.Bool:
		return s.evalBool(typ, n)
	case reflect.Complex64, reflect.Complex128:
		return s.evalComplex(typ, n)
	case reflect.Float32, reflect.Float64:
		return s.evalFloat(typ, n)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return s.evalInteger(typ, n)
	case reflect.Interface:
		if typ.NumMethod() == 0 {
			return s.evalEmptyInterface(dot, n)
		}
	case reflect.String:
		return s.evalString(typ, n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return s.evalUnsignedInteger(typ, n)
	}
	s.errorf("can't handle %s for arg of type %s", n, typ)
	panic("not reached")
}

func (s *state) evalBool(typ reflect.Type, n node) reflect.Value {
	if n, ok := n.(*boolNode); ok {
		value := reflect.New(typ).Elem()
		value.SetBool(n.true)
		return value
	}
	s.errorf("expected bool; found %s", n)
	panic("not reached")
}

func (s *state) evalString(typ reflect.Type, n node) reflect.Value {
	if n, ok := n.(*stringNode); ok {
		value := reflect.New(typ).Elem()
		value.SetString(n.text)
		return value
	}
	s.errorf("expected string; found %s", n)
	panic("not reached")
}

func (s *state) evalInteger(typ reflect.Type, n node) reflect.Value {
	if n, ok := n.(*numberNode); ok && n.isInt {
		value := reflect.New(typ).Elem()
		value.SetInt(n.int64)
		return value
	}
	s.errorf("expected integer; found %s", n)
	panic("not reached")
}

func (s *state) evalUnsignedInteger(typ reflect.Type, n node) reflect.Value {
	if n, ok := n.(*numberNode); ok && n.isUint {
		value := reflect.New(typ).Elem()
		value.SetUint(n.uint64)
		return value
	}
	s.errorf("expected unsigned integer; found %s", n)
	panic("not reached")
}

func (s *state) evalFloat(typ reflect.Type, n node) reflect.Value {
	if n, ok := n.(*numberNode); ok && n.isFloat {
		value := reflect.New(typ).Elem()
		value.SetFloat(n.float64)
		return value
	}
	s.errorf("expected float; found %s", n)
	panic("not reached")
}

func (s *state) evalComplex(typ reflect.Type, n node) reflect.Value {
	if n, ok := n.(*numberNode); ok && n.isComplex {
		value := reflect.New(typ).Elem()
		value.SetComplex(n.complex128)
		return value
	}
	s.errorf("expected complex; found %s", n)
	panic("not reached")
}

func (s *state) evalEmptyInterface(dot reflect.Value, n node) reflect.Value {
	switch n := n.(type) {
	case *boolNode:
		return reflect.ValueOf(n.true)
	case *dotNode:
		return dot
	case *fieldNode:
		return s.evalFieldNode(dot, n, nil, zero)
	case *identifierNode:
		return s.evalFunction(dot, n.ident, nil, zero)
	case *numberNode:
		return s.idealConstant(n)
	case *stringNode:
		return reflect.ValueOf(n.text)
	case *variableNode:
		return s.evalVariableNode(dot, n, nil, zero)
	}
	s.errorf("can't handle assignment of %s to empty interface argument", n)
	panic("not reached")
}

// indirect returns the item at the end of indirection, and a bool to indicate if it's nil.
// We indirect through pointers and empty interfaces (only) because
// non-empty interfaces have methods we might need.
func indirect(v reflect.Value) (rv reflect.Value, isNil bool) {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return v, true
		}
		if v.Kind() == reflect.Ptr || v.NumMethod() == 0 {
			v = v.Elem()
		}
	}
	return v, false
}

// printValue writes the textual representation of the value to the output of
// the template.
func (s *state) printValue(n node, v reflect.Value) {
	if !v.IsValid() {
		fmt.Fprint(s.wr, "<no value>")
		return
	}
	switch v.Kind() {
	case reflect.Ptr:
		var isNil bool
		if v, isNil = indirect(v); isNil {
			fmt.Fprint(s.wr, "<nil>")
			return
		}
	case reflect.Chan, reflect.Func, reflect.Interface:
		s.errorf("can't print %s of type %s", n, v.Type())
	}
	fmt.Fprint(s.wr, v.Interface())
}

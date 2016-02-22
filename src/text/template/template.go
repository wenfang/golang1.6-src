// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package template

import (
	"reflect"
	"sync"
	"text/template/parse"
)

// common holds the information shared by related templates.
type common struct { // 被相关模板共享的结构
	tmpl   map[string]*Template // Map from name to defined templates.
	option option               // 模板选项
	// We use two maps, one for parsing and one for execution.
	// This separation makes the API cleaner since it doesn't
	// expose reflection to the client.
	muFuncs    sync.RWMutex // protects parseFuncs and execFuncs
	parseFuncs FuncMap
	execFuncs  map[string]reflect.Value
}

// Template is the representation of a parsed template. The *parse.Tree
// field is exported only for use by html/template and should be treated
// as unexported by all other clients.
type Template struct { // Template结构
	name        string // 模板的名称
	*parse.Tree        // 解析树
	*common            // 指向相关模板的指针
	leftDelim   string // 左右分割符
	rightDelim  string
}

// New allocates a new, undefined template with the given name.
func New(name string) *Template { // 创建新模板，根据字符串，创建一个名字为name的模板
	t := &Template{
		name: name,
	}
	t.init()
	return t
}

// Name returns the name of the template.
func (t *Template) Name() string { // 返回模板的名称
	return t.name
}

// New allocates a new, undefined template associated with the given one and with the same
// delimiters. The association, which is transitive, allows one template to
// invoke another with a {{template}} action.
func (t *Template) New(name string) *Template { // 根据指定的模板t创建一个新Template对象，共享common
	t.init() // 如果没有设置，则开始设置相关模板结构
	nt := &Template{
		name:       name,
		common:     t.common,    // 共享相关模板和左右分隔符
		leftDelim:  t.leftDelim, // 设置左右分割
		rightDelim: t.rightDelim,
	}
	return nt
}

// init guarantees that t has a valid common structure.
func (t *Template) init() { // 初始化文本模板
	if t.common == nil { // 如果相关模板结构为空，设置相关模板结构
		c := new(common)
		c.tmpl = make(map[string]*Template)
		c.parseFuncs = make(FuncMap)
		c.execFuncs = make(map[string]reflect.Value)
		t.common = c
	}
}

// Clone returns a duplicate of the template, including all associated
// templates. The actual representation is not copied, but the name space of
// associated templates is, so further calls to Parse in the copy will add
// templates to the copy but not to the original. Clone can be used to prepare
// common templates and use them with variant definitions for other templates
// by adding the variants after the clone is made.
func (t *Template) Clone() (*Template, error) { // 返回一个template的拷贝
	nt := t.copy(nil)
	nt.init()
	if t.common == nil {
		return nt, nil
	}
	for k, v := range t.tmpl {
		if k == t.name {
			nt.tmpl[t.name] = nt
			continue
		}
		// The associated templates share nt's common structure.
		tmpl := v.copy(nt.common)
		nt.tmpl[k] = tmpl
	}
	t.muFuncs.RLock()
	defer t.muFuncs.RUnlock()
	for k, v := range t.parseFuncs {
		nt.parseFuncs[k] = v
	}
	for k, v := range t.execFuncs {
		nt.execFuncs[k] = v
	}
	return nt, nil
}

// copy returns a shallow copy of t, with common set to the argument.
func (t *Template) copy(c *common) *Template {
	nt := New(t.name) // 创建一个新Template
	nt.Tree = t.Tree
	nt.common = c
	nt.leftDelim = t.leftDelim
	nt.rightDelim = t.rightDelim
	return nt
}

// AddParseTree adds parse tree for template with given name and associates it with t.
// If the template does not already exist, it will create a new one.
// If the template does exist, it will be replaced.
func (t *Template) AddParseTree(name string, tree *parse.Tree) (*Template, error) {
	t.init()
	// If the name is the name of this template, overwrite this template.
	nt := t
	if name != t.name {
		nt = t.New(name)
	}
	// Even if nt == t, we need to install it in the common.tmpl map.
	if replace, err := t.associate(nt, tree); err != nil {
		return nil, err
	} else if replace {
		nt.Tree = tree
	}
	return nt, nil
}

// Templates returns a slice of defined templates associated with t.
func (t *Template) Templates() []*Template { // 返回t的所有相关模板的slice
	if t.common == nil {
		return nil
	}
	// Return a slice so we don't expose the map.
	m := make([]*Template, 0, len(t.tmpl))
	for _, v := range t.tmpl {
		m = append(m, v)
	}
	return m
}

// Delims sets the action delimiters to the specified strings, to be used in
// subsequent calls to Parse, ParseFiles, or ParseGlob. Nested template
// definitions will inherit the settings. An empty delimiter stands for the
// corresponding default: {{ or }}.
// The return value is the template, so calls can be chained.
func (t *Template) Delims(left, right string) *Template { // 设置模板的分隔符
	t.init()
	t.leftDelim = left
	t.rightDelim = right
	return t
}

// Funcs adds the elements of the argument map to the template's function map.
// It panics if a value in the map is not a function with appropriate return
// type or if the name cannot be used syntactically as a function in a template.
// It is legal to overwrite elements of the map. The return value is the template,
// so calls can be chained.
func (t *Template) Funcs(funcMap FuncMap) *Template {
	t.init()
	t.muFuncs.Lock()
	defer t.muFuncs.Unlock()
	addValueFuncs(t.execFuncs, funcMap)
	addFuncs(t.parseFuncs, funcMap)
	return t
}

// Lookup returns the template with the given name that is associated with t.
// It returns nil if there is no such template or the template has no definition.
func (t *Template) Lookup(name string) *Template { // 在t的相关模板中，查找具有给定name的模板
	if t.common == nil {
		return nil
	}
	return t.tmpl[name]
}

// Parse defines the template by parsing the text. Nested template definitions will be
// associated with the top-level template t. Parse may be called multiple times
// to parse definitions of templates to associate with t.
func (t *Template) Parse(text string) (*Template, error) {
	t.init()
	t.muFuncs.RLock()
	trees, err := parse.Parse(t.name, text, t.leftDelim, t.rightDelim, t.parseFuncs, builtins)
	t.muFuncs.RUnlock()
	if err != nil {
		return nil, err
	}
	// Add the newly parsed trees, including the one for t, into our common structure.
	for name, tree := range trees {
		if _, err := t.AddParseTree(name, tree); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// associate installs the new template into the group of templates associated
// with t. The two are already known to share the common structure.
// The boolean return value reports whether to store this tree as t.Tree.
func (t *Template) associate(new *Template, tree *parse.Tree) (bool, error) { // 设置两个模板相关
	if new.common != t.common {
		panic("internal error: associate not common")
	}
	if t.tmpl[new.name] != nil && parse.IsEmptyTree(tree.Root) {
		// If a template by that name exists,
		// don't replace it with an empty template.
		return false, nil
	}
	t.tmpl[new.name] = new
	return true, nil
}

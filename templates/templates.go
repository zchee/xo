package templates

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"io/fs"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
	"github.com/traefik/yaegi/stdlib/syscall"
	"github.com/traefik/yaegi/stdlib/unsafe"
	xo "github.com/xo/xo/types"
)

// templates are registered template sets.
var templates = map[string]*TemplateSet{}

// Register registers a template set.
func Register(typ string, set *TemplateSet) {
	templates[typ] = set
}

// BaseFuncs returns base template funcs.
func BaseFuncs() template.FuncMap {
	return sprig.TxtFuncMap()
}

// Types returns the registered type names of template sets.
func Types() []string {
	var types []string
	for k := range templates {
		types = append(types, k)
	}
	sort.Strings(types)
	return types
}

// For returns true if the template is available for the command name.
func For(typ, name string) bool {
	if name == "dump" {
		return true
	}
	if set := templates[typ]; set != nil && set.For != nil {
		return contains(set.For, name)
	}
	return true
}

// Flags returns flag options and context for the template sets for the
// specified command name.
//
// These should be added to the invocation context for any call to a template
// set func.
func Flags(name string) []xo.FlagSet {
	var flags []xo.FlagSet
	for _, typ := range Types() {
		set := templates[typ]
		// skip flag if not for the command name
		if set.For != nil && !contains(set.For, name) {
			continue
		}
		for _, flag := range set.Flags {
			flags = append(flags, xo.FlagSet{
				Type: typ,
				Name: string(flag.ContextKey),
				Flag: flag,
			})
		}
	}
	return flags
}

// Process processes emitted templates for a template set.
func Process(ctx context.Context, doAppend bool, single string, v *xo.XO) error {
	typ := TemplateType(ctx)
	set, ok := templates[typ]
	if !ok {
		return fmt.Errorf("unknown template %q", typ)
	}
	// build context
	if set.BuildContext != nil {
		ctx = set.BuildContext(ctx)
	}
	// build funcs
	if err := set.BuildFuncs(ctx); err != nil {
		return fmt.Errorf("unable to build template funcs: %w", err)
	}
	if err := set.Process(ctx, doAppend, set, v); err != nil {
		return err
	}
	sortEmitted(set.emitted)
	order := set.Order
	// add package templates
	if !doAppend && set.PackageTemplates != nil {
		var additional []string
		for _, tpl := range set.PackageTemplates(ctx) {
			if err := set.Emit(ctx, tpl); err != nil {
				return err
			}
			additional = append(additional, tpl.Template)
		}
		order = removeMatching(set.Order, additional)
		order = append(additional, order...)
	}
	set.files = make(map[string]*EmittedTemplate)
	for _, n := range order {
		for _, tpl := range set.emitted {
			if tpl.Template.Template != n {
				continue
			}
			fileExt := set.FileExt
			if s := Suffix(ctx); s != "" {
				fileExt = s
			}
			// determine filename
			if single != "" {
				tpl.File = single
			} else {
				tpl.File = set.FileName(ctx, tpl.Template) + fileExt
			}
			// load
			file, ok := set.files[tpl.File]
			if !ok {
				buf, err := set.LoadFile(ctx, tpl.File, doAppend)
				if err != nil {
					return err
				}
				file = &EmittedTemplate{
					Buf:  buf,
					File: tpl.File,
				}
				set.files[tpl.File] = file
			}
			file.Buf = append(file.Buf, tpl.Buf...)
		}
	}
	return nil
}

// Write performs post processing of emitted templates to a template set,
// writing to disk the processed content.
func Write(ctx context.Context) error {
	typ := TemplateType(ctx)
	set, ok := templates[typ]
	if !ok {
		return fmt.Errorf("unknown template %q", typ)
	}
	var files []string
	for file := range set.files {
		files = append(files, file)
	}
	sort.Strings(files)
	if set.Post == nil {
		return WriteFiles(ctx)
	}
	for _, file := range files {
		buf, err := set.Post(ctx, set.files[file].Buf)
		switch {
		case err != nil:
			set.files[file].Err = append(set.files[file].Err, &ErrPostFailed{file, err})
		case err == nil:
			set.files[file].Buf = buf
		}
	}
	return WriteFiles(ctx)
}

// WriteFiles writes the generated files to disk.
func WriteFiles(ctx context.Context) error {
	typ := TemplateType(ctx)
	set, ok := templates[typ]
	if !ok {
		return fmt.Errorf("unknown template %q", typ)
	}
	out := Out(ctx)
	var files []string
	for file := range set.files {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		if err := ioutil.WriteFile(filepath.Join(out, file), set.files[file].Buf, 0644); err != nil {
			set.files[file].Err = append(set.files[file].Err, err)
		}
	}
	return nil
}

// WriteRaw writes the raw templates for a template set.
func WriteRaw(ctx context.Context) error {
	typ := TemplateType(ctx)
	set, ok := templates[typ]
	if !ok {
		return fmt.Errorf("unknown template %q", typ)
	}
	out := Out(ctx)
	return fs.WalkDir(set.Files, ".", func(n string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return os.MkdirAll(filepath.Join(out, n), 0755)
		}
		buf, err := set.Files.ReadFile(n)
		if err != nil {
			return err
		}
		return ioutil.WriteFile(filepath.Join(out, n), buf, 0644)
	})
}

// Errors returns errors collected during file generation.
func Errors(ctx context.Context) ([]error, error) {
	typ := TemplateType(ctx)
	set, ok := templates[typ]
	if !ok {
		return nil, fmt.Errorf("unknown template %q", typ)
	}
	var files []string
	for file := range set.files {
		files = append(files, file)
	}
	sort.Strings(files)
	var errors []error
	for _, file := range files {
		errors = append(errors, set.files[file].Err...)
	}
	return errors, nil
}

// TemplateSet is a template set.
type TemplateSet struct {
	// Files are the embedded templates.
	Files embed.FS
	// For are the command names the template set is available for.
	For []string
	// FileExt is the file extension added to out files.
	FileExt string
	// AddType will be called when a new type is encountered.
	AddType func(string)
	// Flags are additional template flags.
	Flags []xo.Flag
	// Order in which to process templates.
	Order []string
	// HeaderTemplate is the name of the header template.
	HeaderTemplate func(context.Context) *Template
	// PackageTemplates returns package templates.
	PackageTemplates func(context.Context) []*Template
	// Funcs provides template funcs for use by templates.
	Funcs func(context.Context) template.FuncMap
	// FileName determines the out file name templates.
	FileName func(context.Context, *Template) string
	// BuildContext provides a way for template sets to inject additional,
	// global context values, prior to template processing.
	BuildContext func(context.Context) context.Context
	// Process performs the preprocessing and the order to load files.
	Process func(context.Context, bool, *TemplateSet, *xo.XO) error
	// Post performs post processing of generated content.
	Post func(context.Context, []byte) ([]byte, error)
	// emitted holds emitted templates.
	emitted []*EmittedTemplate
	// funcs are the template funcs.
	funcs template.FuncMap
	// files holds the generated files.
	files map[string]*EmittedTemplate
}

// BuildFuncs builds the template funcs.
func (set *TemplateSet) BuildFuncs(ctx context.Context) error {
	if set.Funcs == nil {
		set.funcs = BaseFuncs()
	} else {
		set.funcs = set.Funcs(ctx)
	}
	return set.AddCustomFuncs(ctx)
}

// AddCustomFuncs adds funcs from the template's funcs.go.tpl to the set of
// template funcs.
//
// Uses the github.com/traefik/yaegi interpretter to evaluate the funcs.go.tpl,
// adding anything returned by the file's defined `func Init(context.Context) (template.FuncMap, error)`
// to the built template funcs.
//
// See existing templates for implementation examples.
func (set *TemplateSet) AddCustomFuncs(ctx context.Context) error {
	// get template src
	src := Src(ctx)
	if src == nil {
		src = set.Files
	}
	// read custom funcs
	f, err := src.Open("funcs.go.tpl")
	switch {
	case err != nil && os.IsNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf("unable to load funcs.go.tpl: %w", err)
	}
	buf, err := ioutil.ReadAll(f)
	if err != nil {
		return fmt.Errorf("unable to read funcs.go.tpl: %w", err)
	}
	// determine gopath
	var opts interp.Options
	cmd := exec.Command("go", "env", "GOPATH")
	if buf, err := cmd.CombinedOutput(); err == nil {
		opts.GoPath = strings.TrimSpace(string(buf))
	}
	// build interpreter for custom funcs
	i := interp.New(opts)
	if err := i.Use(stdlib.Symbols); err != nil {
		return fmt.Errorf("unable to add stdlib to yaegi interpreter: %w", err)
	}
	if err := i.Use(syscall.Symbols); err != nil {
		return fmt.Errorf("unable to add syscall to yaegi interpreter: %w", err)
	}
	if err := i.Use(unsafe.Symbols); err != nil {
		return fmt.Errorf("unable to add unsafe to yaegi interpreter: %w", err)
	}
	if err := i.Use(Symbols(ctx)); err != nil {
		return fmt.Errorf("unable to add xo to yaegi interpreter: %w", err)
	}
	if _, err := i.Eval(string(buf)); err != nil {
		return fmt.Errorf("unable to eval funcs.go.tpl: %w", err)
	}
	// eval custom funcs
	v, err := i.Eval("funcs.Init")
	if err != nil {
		return fmt.Errorf("unable to eval funcs.Init: %w", err)
	}
	z, ok := v.Interface().(func(context.Context) (template.FuncMap, error))
	if !ok {
		return fmt.Errorf("funcs.Init must have signature `func(context.Context) (template.FuncMap, error)`, has: `%T`", v.Interface())
	}
	// init custom funcs
	m, err := z(ctx)
	if err != nil {
		return fmt.Errorf("funcs.Init error: %w", err)
	}
	// add custom funcs to funcs
	for k, v := range m {
		set.funcs[k] = v
	}
	return nil
}

// Emit emits a template to the template set.
func (set *TemplateSet) Emit(ctx context.Context, tpl *Template) error {
	buf, err := set.Exec(ctx, tpl)
	if err != nil {
		return err
	}
	set.emitted = append(set.emitted, &EmittedTemplate{Template: tpl, Buf: buf})
	return nil
}

// Exec loads and executes a template.
func (set *TemplateSet) Exec(ctx context.Context, tpl *Template) ([]byte, error) {
	t, err := set.Load(ctx, tpl)
	if err != nil {
		return nil, err
	}
	buf := new(bytes.Buffer)
	if err := t.Execute(buf, tpl); err != nil {
		return nil, fmt.Errorf("unable to exec template %s: %w", tpl.File(), err)
	}
	return buf.Bytes(), nil
}

// Load loads a template.
func (set *TemplateSet) Load(ctx context.Context, tpl *Template) (*template.Template, error) {
	// template source
	src := Src(ctx)
	if src == nil {
		src = set.Files
	}
	// load template content
	name := tpl.File() + set.FileExt + ".tpl"
	f, err := src.Open(name)
	if err != nil {
		return nil, fmt.Errorf("unable to open template %s: %w", name, err)
	}
	defer f.Close()
	// read template
	buf, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("unable to read template %s: %w", name, err)
	}
	// parse content
	t, err := template.New(name).Funcs(set.funcs).Parse(string(buf))
	if err != nil {
		return nil, fmt.Errorf("unable to parse template %s: %w", name, err)
	}
	return t, nil
}

// LoadFile loads a file.
func (set *TemplateSet) LoadFile(ctx context.Context, file string, doAppend bool) ([]byte, error) {
	name := filepath.Join(Out(ctx), file)
	fi, err := os.Stat(name)
	switch {
	case (err != nil && os.IsNotExist(err)) || !doAppend:
		if set.HeaderTemplate == nil {
			return nil, nil
		}
		return set.Exec(ctx, set.HeaderTemplate(ctx))
	case err != nil:
		return nil, err
	case fi.IsDir():
		return nil, fmt.Errorf("%s is a directory: cannot emit template", name)
	}
	return ioutil.ReadFile(name)
}

// Template wraps other templates.
type Template struct {
	Set      string
	Template string
	Type     string
	Name     string
	Data     interface{}
	Extra    map[string]interface{}
}

// File returns the file name for the template.
func (tpl *Template) File() string {
	if tpl.Set != "" {
		return tpl.Set + "/" + tpl.Template
	}
	return tpl.Template
}

// EmittedTemplate wraps a template with its content and file name.
type EmittedTemplate struct {
	Template *Template
	Buf      []byte
	File     string
	Err      []error
}

// ErrPostFailed is the post failed error.
type ErrPostFailed struct {
	File string
	Err  error
}

// Error satisfies the error interface.
func (err *ErrPostFailed) Error() string {
	return fmt.Sprintf("post failed %s: %v", err.File, err.Err)
}

// Unwrap satisfies the unwrap interface.
func (err *ErrPostFailed) Unwrap() error {
	return err.Err
}

// Context keys.
const (
	SymbolsKey      xo.ContextKey = "symbols"
	GenTypeKey      xo.ContextKey = "gen-type"
	TemplateTypeKey xo.ContextKey = "template-type"
	SuffixKey       xo.ContextKey = "suffix"
	SrcKey          xo.ContextKey = "src"
	OutKey          xo.ContextKey = "out"
)

// Symbols returns the symbols option from the context.
func Symbols(ctx context.Context) map[string]map[string]reflect.Value {
	v, _ := ctx.Value(SymbolsKey).(map[string]map[string]reflect.Value)
	return v
}

// GenType returns the the gen-type option from the context.
func GenType(ctx context.Context) string {
	s, _ := ctx.Value(GenTypeKey).(string)
	return s
}

// TemplateType returns type option from the context.
func TemplateType(ctx context.Context) string {
	s, _ := ctx.Value(TemplateTypeKey).(string)
	return s
}

// Suffix returns suffix option from the context.
func Suffix(ctx context.Context) string {
	s, _ := ctx.Value(SuffixKey).(string)
	return s
}

// Src returns src option from the context.
func Src(ctx context.Context) fs.FS {
	v, _ := ctx.Value(SrcKey).(fs.FS)
	return v
}

// Out returns out option from the context.
func Out(ctx context.Context) string {
	s, _ := ctx.Value(OutKey).(string)
	return s
}

// sortEmitted sorts the emitted templates.
func sortEmitted(tpl []*EmittedTemplate) {
	sort.Slice(tpl, func(i, j int) bool {
		if tpl[i].Template.Template != tpl[j].Template.Template {
			return tpl[i].Template.Template < tpl[j].Template.Template
		}
		if tpl[i].Template.Type != tpl[j].Template.Type {
			return tpl[i].Template.Type < tpl[j].Template.Type
		}
		return tpl[i].Template.Name < tpl[j].Template.Name
	})
}

// removeMatching builds a new slice from v containing the strings not
// contained in s.
func removeMatching(v []string, s []string) []string {
	var res []string
	for _, z := range v {
		if contains(s, z) {
			continue
		}
		res = append(res, z)
	}
	return res
}

// contains returns true when s is in v.
func contains(v []string, s string) bool {
	for _, z := range v {
		if z == s {
			return true
		}
	}
	return false
}

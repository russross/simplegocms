package simplegocms

import (
	"appengine"
	"appengine/datastore"
	"appengine/user"
	"archive/zip"
	"bytes"
	"errors"
	"github.com/russross/blackfriday"
	"html"
	"html/template"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Object interface {
	Key() string
	Validate() bool
	PreSave()
	PostLoad()
}

type Page struct {
	Url           string
	Title         string
	Created       time.Time
	Modified      time.Time
	Template      string
	Templates     []string `datastore:"-"`
	Tags          []string
	Markdown      string `datastore:"-"`
	MarkdownBytes []byte // strings are limited to 500 chars
	Rendered      template.HTML `datastore:"-"`
}

func NewPage(url string) *Page {
	stamp := time.Now()
	title := strings.Replace(url, "_", " ", -1)
	if slash := strings.LastIndex(url, "/"); slash >= 0 && len(title) > slash+1 {
		title = title[slash+1:]
	}
	title = strings.Title(title)
	return &Page{
		Url:           url,
		Title:         title,
		Created:       stamp,
		Modified:      stamp,
		Template:      "default",
		Markdown:      "\n",
		MarkdownBytes: []byte("\n"),
	}
}

func (p *Page) Render() {
	p.Rendered = template.HTML(blackfriday.MarkdownCommon([]byte(p.Markdown)))
}

func (p *Page) Validate() bool {
	if len(p.Url) == 0 || strings.HasPrefix(p.Url, "/") || strings.Contains(p.Url, "//") {
		return false
	}
	p.Markdown = cleanupLineEndings(p.Markdown)
	return true
}

func (p *Page) Key() string {
	return p.Url
}

func (p *Page) PreSave() {
	p.MarkdownBytes = []byte(p.Markdown)
}

func (p *Page) PostLoad() {
	p.Markdown = string(p.MarkdownBytes)
}

type Template struct {
	Url           string
	Contents      string `datastore:"-"`
	ContentsBytes []byte
}

func (t *Template) Validate() bool {
	if len(t.Url) == 0 || strings.HasPrefix(t.Url, "/") || strings.Contains(t.Url, "//") {
		return false
	}
	t.Contents = cleanupLineEndings(t.Contents)
	return true
}

func (t *Template) Key() string {
	return t.Url
}

func (t *Template) PreSave() {
	t.ContentsBytes = []byte(t.Contents)
}

func (t *Template) PostLoad() {
	t.Contents = string(t.ContentsBytes)
}

type Static struct {
	Url      string
	Text     string `datastore:"-"`
	Contents []byte
}

func (s *Static) Validate() bool {
	if len(s.Url) == 0 || strings.HasPrefix(s.Url, "/") || strings.Contains(s.Url, "//") {
		return false
	}
	return true
}

func (s *Static) Key() string {
	return s.Url
}

func (s *Static) PreSave() {
}

func (s *Static) PostLoad() {
}

func cleanupLineEndings(s string) string {
	s = strings.Replace(s, "\r\n", "\n", -1)
	s = strings.Replace(s, "\r", "\n", -1)
	return strings.Trim(s, "\n") + "\n"
}

type EditList struct {
	Pages     []string
	Templates []string
	Statics   []string
}

type Config struct {
	Editors []string
}

func (c *Config) Validate() bool {
	return true
}

func (c *Config) Key() string {
	return "config"
}

func (c *Config) PreSave() {
}

func (c *Config) PostLoad() {
}

// the set of all built-in templates
var builtins *template.Template

// if the first arguments match each other, return the last as a string
func ifEqual(args ...interface{}) string {
	for i := 0; i < len(args)-2; i++ {
		if args[i] != args[i+1] {
			return ""
		}
	}
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1].(string)
}

const (
	viewpage_prefix = "/"

	editpage_prefix = "/_edit/"
	savepage_prefix = "/_savepage"

	edittemplate_prefix = "/_edittemplate/"
	savetemplate_prefix = "/_savetemplate"

	editstatic_prefix = "/_editstatic/"
	savestatic_prefix = "/_savestatic"

	editlist_prefix = "/_edit"
	export_prefix   = "/_exportpages"
	import_prefix   = "/_importpages"

	editconfig_prefix = "/_config"
	saveconfig_prefix = "/_saveconfig"
)

func init() {
	// assign functions to the templates
	builtins = template.New("root")
	builtins.Funcs(template.FuncMap{
		"ifEqual": ifEqual,
	})
	template.Must(builtins.ParseGlob("*.template"))

	http.HandleFunc(viewpage_prefix, viewpage)

	http.HandleFunc(editpage_prefix, edit)
	http.HandleFunc(savepage_prefix, save)

	http.HandleFunc(edittemplate_prefix, edit)
	http.HandleFunc(savetemplate_prefix, save)

	http.HandleFunc(editstatic_prefix, edit)
	http.HandleFunc(savestatic_prefix, save)

	http.HandleFunc(editlist_prefix, editlist)
	http.HandleFunc(export_prefix, exportpages)
	http.HandleFunc(import_prefix, importpages)

	http.HandleFunc(editconfig_prefix, edit)
	http.HandleFunc(saveconfig_prefix, save)
}

func getConfig(c appengine.Context, host string) (*Config, error) {
	hostkey := datastore.NewKey(c, "Host", host+"/", 0, nil)
	key := datastore.NewKey(c, "Config", "config", 0, hostkey)
	value := new(Config)
	err := datastore.Get(c, key, value)
	if err != nil && err == datastore.ErrNoSuchEntity {
		// just use default values
	} else if err != nil {
		return nil, err
	}
	value.PostLoad()
	return value, nil
}

func requireEditor(c appengine.Context, host string) (err error) {
	if user.IsAdmin(c) {
		return
	}

	config, err := getConfig(c, host)
	if err != nil {
		return
	}
	u := user.Current(c)
	if u == nil {
		return errors.New("Must be logged in")
	}
	for _, elt := range config.Editors {
		if elt == u.Email {
			return
		}
	}
	return errors.New("User " + u.Email + " is not an editor for " + host)
}

func render(c appengine.Context, w http.ResponseWriter, host string, page *Page) (err error) {
	success := false

	// get the template
	hostkey := datastore.NewKey(c, "Host", host+"/", 0, nil)
	key := datastore.NewKey(c, "Template", page.Template, 0, hostkey)
	tmpl := new(Template)
	if err = datastore.Get(c, key, tmpl); err == nil {
		tmpl.PostLoad()
		parsed := template.New(tmpl.Url)
		if _, err = parsed.Parse(tmpl.Contents); err == nil {
			if err = parsed.Execute(w, page); err == nil {
				success = true
			}
		}
	}

	if err != nil || !success {
		fallback := builtins.Lookup("fallback.template")
		err = fallback.Execute(w, page)
	}
	return
}

func GetKeysList(c appengine.Context, host string, table string) (lst []string, err error) {
	hostkey := datastore.NewKey(c, "Host", host+"/", 0, nil)
	q := datastore.NewQuery(table).Ancestor(hostkey).Order("Url")
	q.KeysOnly()
	dummy := []datastore.PropertyList{}
	keys, err := q.GetAll(c, &dummy)
	if err != nil {
		return
	}
	lst = make([]string, len(keys))
	for i, elt := range keys {
		lst[i] = elt.StringID()
	}
	return
}

//
// Edit admin page
//

func editlist(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	err := requireEditor(c, r.URL.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	data := new(EditList)
	if data.Pages, err = GetKeysList(c, r.URL.Host, "Page"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if data.Templates, err = GetKeysList(c, r.URL.Host, "Template"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if data.Statics, err = GetKeysList(c, r.URL.Host, "Static"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := builtins.ExecuteTemplate(&buf, "edit-list.template", data); err != nil {
		panic("Executing edit-list template")
	}

	page := NewPage("Edit_content")
	page.Rendered = template.HTML(buf.Bytes())
	if err := render(c, w, r.URL.Host, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

//
// View content, both rendered and static
//

func viewpage(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	url := r.URL.Path[len(viewpage_prefix):]
	if url == "" || strings.HasSuffix(url, "/") {
		url += "index"
	}
	hostkey := datastore.NewKey(c, "Host", r.URL.Host+"/", 0, nil)

	// check for a static first
	static := &Static{Url: url}
	key := datastore.NewKey(c, "Static", static.Key(), 0, hostkey)
	err := datastore.Get(c, key, static)
	if err != nil && err != datastore.ErrNoSuchEntity {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if err == nil {
		static.PostLoad()

		// guess the MIME type based on the extension
		if dot := strings.LastIndex(static.Url, "."); dot >= 0 && len(static.Url) > dot+1 {
			ext := static.Url[dot:]
			if mimetype := mime.TypeByExtension(ext); mimetype != "" {
				w.Header()["Content-Type"] = []string{mimetype}
			} else {
				w.Header()["Content-Type"] = []string{http.DetectContentType(static.Contents)}
			}
		}
		w.Write(static.Contents)
		return
	}

	// try a rendered page
	page := NewPage(url)
	if !page.Validate() {
		http.Error(w, "Invalid page URL", http.StatusBadRequest)
		return
	}

	// get the page
	key = datastore.NewKey(c, "Page", page.Key(), 0, hostkey)
	err = datastore.Get(c, key, page)
	if err != nil && err == datastore.ErrNoSuchEntity {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page.PostLoad()
	page.Render()
	if err = render(c, w, r.URL.Host, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

//
// Edit content, including rendered, static, and templates
//

func save(w http.ResponseWriter, r *http.Request) {
	// check permissions
	c := appengine.NewContext(r)
	err := requireEditor(c, r.URL.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	var value Object
	var table string

	// what are we supposed to save?
	switch r.URL.Path {
	case savepage_prefix:
		table = "Page"
		value = NewPage("")
	case savestatic_prefix:
		table = "Static"
		value = &Static{}
	case savetemplate_prefix:
		table = "Template"
		value = &Template{}
	case saveconfig_prefix:
		table = "Config"
		value = &Config{}
		if !user.IsAdmin(c) {
			http.Error(w, "Must be logged in as an admin",
				http.StatusUnauthorized)
			return
		}
	default:
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	// extract the form data
	if err := UnmarshalForm(r, value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !value.Validate() {
		http.Error(w, "Invalid URL to save", http.StatusBadRequest)
		return
	}

	hostkey := datastore.NewKey(c, "Host", r.URL.Host+"/", 0, nil)
	key := datastore.NewKey(c, table, value.Key(), 0, hostkey)

	// delete request
	if strings.HasPrefix(r.FormValue("Submit"), "Delete") {
		if err = datastore.Delete(c, key); err != nil && err != datastore.ErrNoSuchEntity {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		err = nil
		http.Redirect(w, r, "/_edit", http.StatusFound)
		return
	}

	// save request

	// special case: for static objects we need to decide what to save
	if r.URL.Path == savestatic_prefix {
		static := value.(*Static)

		// if something was uploaded, it wins
		// otherwise use the contents of the text box
		if len(static.Contents) == 0 {
			static.Contents = []byte(cleanupLineEndings(static.Text))
		}
	}
	value.PreSave()
	if _, err = datastore.Put(c, key, value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	target := "/_edit"
	if r.URL.Path == savepage_prefix {
		target = "/" + r.FormValue("Url")
	} else if r.URL.Path == saveconfig_prefix {
		target = "/_config"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func edit(w http.ResponseWriter, r *http.Request) {
	// only configured editors can edit
	c := appengine.NewContext(r)
	err := requireEditor(c, r.URL.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	var url, table, form string
	var value Object

	// what are we supposed to edit?
	switch path := r.URL.Path; {
	case strings.HasPrefix(path, editpage_prefix):
		url = path[len(editpage_prefix):]
		table = "Page"
		form = "edit-page.template"
		page := NewPage(url)
		if page.Templates, err = GetKeysList(c, r.URL.Host, "Template"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		value = page
	case strings.HasPrefix(path, editstatic_prefix):
		url = path[len(editstatic_prefix):]
		table = "Static"
		form = "edit-static.template"
		value = &Static{Url: url}
	case strings.HasPrefix(path, edittemplate_prefix):
		url = path[len(edittemplate_prefix):]
		table = "Template"
		form = "edit-template.template"
		value = &Template{Url: url}
	case path == editconfig_prefix:
		url = "config"
		table = "Config"
		form = "edit-config.template"
		value = &Config{}

		// only admins can edit configuration
		if !user.IsAdmin(c) {
			http.Error(w, "Must be logged in as an admin", http.StatusUnauthorized)
			return
		}
	default:
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	// load the existing data (if any)
	if !value.Validate() {
		http.Error(w, "Invalid URL to edit", http.StatusBadRequest)
		return
	}
	hostkey := datastore.NewKey(c, "Host", r.URL.Host+"/", 0, nil)
	key := datastore.NewKey(c, table, value.Key(), 0, hostkey)
	err = datastore.Get(c, key, value)
	if err != nil && err == datastore.ErrNoSuchEntity {
		// default empty entry is fine
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	value.PostLoad()

	// for statics, try to decide if it is text or not
	if strings.HasPrefix(r.URL.Path, editstatic_prefix) {
		static := value.(*Static)
		if utf8.Valid(static.Contents) {
			static.Text = string(static.Contents)
		}
	}

	// render the form
	var buf bytes.Buffer
	if err := builtins.ExecuteTemplate(&buf, form, value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// now render the page that holds the form
	page := NewPage("")
	page.Title = "Edit " + table + ": " + url
	if r.URL.Path == editconfig_prefix {
		page.Title = "Configuration"
	}
	page.Rendered = template.HTML(buf.Bytes())
	if err := render(c, w, r.URL.Host, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func UnmarshalForm(r *http.Request, val interface{}) (err error) {
	v := reflect.ValueOf(val)
	if v.Kind() != reflect.Ptr {
		return errors.New("non-pointer passed to UnmarshalForm")
	}
	if v.IsNil() {
		return errors.New("nil pointer passed to UnmarshalForm")
	}
	s := v.Elem()
	if s.Kind() != reflect.Struct {
		return errors.New("pointer to non-struct passed to UnmarshalForm")
	}
	t := s.Type()
	for i, n := 0, t.NumField(); i < n; i++ {
		field := t.Field(i)
		value := s.Field(i)

		switch value.Kind() {
		case reflect.String:
			str := strings.TrimSpace(r.FormValue(field.Name))
			if str == "" {
				continue
			}
			value.SetString(str)
		case reflect.Slice:
			// []byte: look for an uploaded file
			if value.Type().Elem().Kind() == reflect.Uint8 {
				file, _, err := r.FormFile(field.Name)
				if err != nil {
					break
				}
				defer file.Close()
				raw, err := ioutil.ReadAll(file)
				if err != nil {
					return err
				}
				value.Set(reflect.ValueOf(raw))
			}

			// []string: look for a comma-delimited list
			if value.Type().Elem().Kind() == reflect.String {
				str := strings.TrimSpace(r.FormValue(field.Name))
				if len(str) == 0 {
					continue
				}
				lst := strings.Split(str, ",")
				for i, elt := range lst {
					lst[i] = strings.TrimSpace(elt)
				}
				value.Set(reflect.ValueOf(lst))
			}
		case reflect.Int64:
			str := strings.TrimSpace(r.FormValue(field.Name))
			if n, err := strconv.ParseInt(str, 10, 64); err == nil {
				value.SetInt(n)
			}
		}
	}
	return nil
}

//
// Bulk import/export
//

func escapeUrl(s string) string {
	parts := strings.Split(s, "/")
	for i, elt := range parts {
		parts[i] = url.QueryEscape(elt)
	}
	return strings.Join(parts, "/")
}

func unescapeUrl(s string) string {
	parts := strings.Split(s, "/")
	for i, elt := range parts {
		if orig, err := url.QueryUnescape(elt); err == nil {
			parts[i] = orig
		}
	}
	return strings.Join(parts, "/")
}

func exportpages(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	err := requireEditor(c, r.URL.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	// headers
	w.Header()["Content-Type"] = []string{"application/zip"}
	filename := r.URL.Host + "-backup_" +
		time.Now().UTC().Format("2006-01-02") + ".zip"
	w.Header()["Content-Disposition"] =
		[]string{`attachment; filename="` + filename + `"`}

	z := zip.NewWriter(w)
	hostkey := datastore.NewKey(c, "Host", r.URL.Host+"/", 0, nil)

	// pages
	for iter := datastore.NewQuery("Page").Ancestor(hostkey).Run(c); ; {
		var page Page
		_, err := iter.Next(&page)
		if err == datastore.Done {
			break
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		page.PostLoad()
		out, err := z.Create(escapeUrl(page.Url + ".md"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		err = page.WriteTo(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// statics
	for iter := datastore.NewQuery("Static").Ancestor(hostkey).Run(c); ; {
		var static Static
		_, err := iter.Next(&static)
		if err == datastore.Done {
			break
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		static.PostLoad()
		out, err := z.Create(escapeUrl(static.Url))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, err = out.Write(static.Contents)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// templates
	for iter := datastore.NewQuery("Template").Ancestor(hostkey).Run(c); ; {
		var tmpl Template
		_, err := iter.Next(&tmpl)
		if err == datastore.Done {
			break
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tmpl.PostLoad()
		out, err := z.Create(escapeUrl(tmpl.Url + ".template"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, err = out.Write([]byte(tmpl.Contents))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	err = z.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// Parse a page from a text file format:
//
// 0:	Title
// 1: 	Created time
// 2:	Modified time
// 3: 	Template
// 4:	Tags (comma-separated list)
// 5:   ---
// 6:   Markdown...
func ParsePage(raw string, url string) (page *Page) {
	page = new(Page)

	// normalize line endings
	raw = strings.Replace(raw, "\r\n", "\n", -1)
	raw = strings.Replace(raw, "\r", "\n", -1)
	lines := strings.SplitN(raw, "\n", 7)

	// pad it out to the right number of fields
	for i := len(lines); i < 7; i++ {
		lines = append(lines, "")
	}

	// Url: as is
	page.Url = url

	// Title: remove leading and trailing whitespace
	page.Title = strings.TrimSpace(lines[0])
	if page.Title == "" {
		page.Title = "(untitled)"
	}

	// Created time: parse as RFC1123, with or without a timezone
	page.Created = parseTime(lines[1])

	// Modified time: same
	page.Modified = parseTime(lines[2])

	// Template: remove leading and trailing whitespace
	page.Template = strings.TrimSpace(lines[3])
	if page.Template == "" {
		page.Template = "default"
	}

	// Tags: split on commas and remove leading and trailing whitespace
	tags := strings.Split(lines[4], ",")
	for i, elt := range tags {
		tags[i] = strings.TrimSpace(elt)
	}
	page.Tags = tags

	// ---: do nothing

	// Contents: Trim leading newlines, leave exactly one at end
	page.Markdown = lines[6]
	page.Validate()

	return
}

// Parse a time string as RFC1123, with or without a timezone
// return as seconds since the epoc
func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)

	// time zone included?
	if stamp, err := time.Parse(time.RFC1123, raw); err == nil {
		return stamp
	}

	// time zone is missing, assume UTC
	if stamp, err := time.Parse("Mon, 02 Jan 2006 15:04:05", raw); err == nil {
		return stamp
	}

	// unrecognized format, fall back to current time
	return time.Now()
}

func (p *Page) WriteTo(w io.Writer) error {
	header := p.Title + "\n"
	header += p.Created.Format(time.RFC1123) + "\n"
	header += p.Modified.Format(time.RFC1123) + "\n"
	header += p.Template + "\n"
	header += strings.Join(p.Tags, ",") + "\n"
	header += "---\n"
	if _, err := w.Write([]byte(header)); err != nil {
		return err
	}
	if _, err := w.Write([]byte(p.Markdown)); err != nil {
		return err
	}

	return nil
}

type bytebuf []byte

func (b bytebuf) ReadAt(p []byte, off int64) (n int, err error) {
	slice := []byte(b)
	if off >= int64(len(slice)) {
		return 0, errors.New("End of file")
	}
	n = copy(p, slice[off:])
	return n, nil
}

func importpages(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	err := requireEditor(c, r.URL.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	file, _, err := r.FormFile("Zipfile")
	if err != nil {
		http.Error(w, "No zipfile submitted", http.StatusBadRequest)
		return
	}
	raw, err := ioutil.ReadAll(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	z, err := zip.NewReader(bytebuf(raw), int64(len(raw)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hostkey := datastore.NewKey(c, "Host", r.URL.Host+"/", 0, nil)
	report := "<h1>Imported files</h1>\n<ul>\n"
	for _, elt := range z.File {
		name := unescapeUrl(elt.Name)
		fp, err := elt.Open()
		if err != nil {
			report += "<li>Error opening " + html.EscapeString(name) + ": " +
				html.EscapeString(err.Error()) + "</li>\n"
			continue
		}
		data, err := ioutil.ReadAll(fp)
		if err != nil {
			report += "<li>Error reading " + html.EscapeString(name) + ": " +
				html.EscapeString(err.Error()) + "</li>\n"
			continue
		}
		fp.Close()

		// handle different file types
		if strings.HasSuffix(name, ".md") && len(name) > len(".md") {
			url := name[:len(name)-len(".md")]
			page := ParsePage(string(data), url)
			if !page.Validate() {
				report += "<li>Invalid name: " + html.EscapeString(name) + "</ul>\n"
				continue
			}
			key := datastore.NewKey(c, "Page", page.Key(), 0, hostkey)
			page.PreSave()
			if _, err = datastore.Put(c, key, page); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			report += "<li>Page: " + html.EscapeString(url) + "</li>\n"
		} else if strings.HasSuffix(name, ".template") && len(name) > len(".template") {
			url := name[:len(name)-len(".template")]
			tmpl := &Template{Url: url, Contents: string(data)}
			if !tmpl.Validate() {
				report += "<li>Invalid name: " + html.EscapeString(name) + "</ul>\n"
				continue
			}
			key := datastore.NewKey(c, "Template", tmpl.Key(), 0, hostkey)
			tmpl.PreSave()
			if _, err = datastore.Put(c, key, tmpl); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			report += "<li>Template: " + html.EscapeString(url) + "</li>\n"
		} else if !strings.HasSuffix(name, "/") || len(data) > 0 {
			url := name
			static := &Static{Url: url, Contents: data}
			if !static.Validate() {
				report += "<li>Invalid name: " + html.EscapeString(name) + "</ul>\n"
				continue
			}
			key := datastore.NewKey(c, "Static", static.Key(), 0, hostkey)
			static.PreSave()
			if _, err = datastore.Put(c, key, static); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			report += "<li>Static: " + html.EscapeString(url) + "</li>\n"
		} else {
			report += "<li>Skipping directory: " + html.EscapeString(name) + "</li>\n"
		}
	}
	report += "</ul>\n"
	page := NewPage("Imported_data_report")
	page.Rendered = template.HTML(report)
	if err = render(c, w, r.URL.Host, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

package cms

import (
	"appengine"
	"appengine/datastore"
	"appengine/user"
	"archive/zip"
	"bytes"
	"github.com/russross/blackfriday"
	"http"
	"io"
	"io/ioutil"
	"os"
	"reflect"
	"strconv"
	"strings"
	"template"
	"time"
)

type Object interface {
	Key() string
	Validate() bool
}

type Page struct {
	Host	string
	Url       string
	Title     string
	Created   datastore.Time
	Modified  datastore.Time
	Template  string
	Templates []string `datastore:"-"`
	Tags      []string
	Markdown  string
	Rendered  string `datastore:"-"`
}

func NewPage(host string, url string) *Page {
	stamp := datastore.SecondsToTime(time.Seconds())
	return &Page{
		Host: host,
		Url:      url,
		Title:    strings.Replace(url, "_", " ", -1),
		Created:  stamp,
		Modified: stamp,
		Template: "default",
		Markdown: "\n",
	}
}

func (p *Page) Render() {
	p.Rendered = string(blackfriday.MarkdownCommon([]byte(p.Markdown)))
}

func (p *Page) Validate() bool {
	p.Markdown = cleanupLineEndings(p.Markdown)
	if len(p.Url) == 0 {
		return false
	}
	return true
}

func (p *Page) Key() string {
	return p.Host + ":" + p.Url
}

type Template struct {
	Host string
	Url      string
	Contents string
}

func (t *Template) Validate() bool {
	t.Contents = cleanupLineEndings(t.Contents)
	if len(t.Url) == 0 {
		return false
	}
	return true
}

func (t *Template) Key() string {
	return t.Host + ":" + t.Url
}

type Stylesheet struct {
	Host string
	Url      string
	Contents string
}

func (s *Stylesheet) Validate() bool {
	s.Contents = cleanupLineEndings(s.Contents)
	if len(s.Url) == 0 {
		return false
	}
	return true
}

func (s *Stylesheet) Key() string {
	return s.Host + ":" + s.Url
}

func cleanupLineEndings(s string) string {
	s = strings.Replace(s, "\r\n", "\n", -1)
	s = strings.Replace(s, "\r", "\n", -1)
	return strings.Trim(s, "\n") + "\n"
}

type EditList struct {
	Pages       []string
	Stylesheets []string
	Templates   []string
}

type Config struct {
	Host string
	Editors []string
}

func (c *Config) Validate() bool {
	return true
}

func (c *Config) Key() string {
	return c.Host + ":" + "config"
}

const (
	viewpage_prefix       = "/"
	editlist_prefix       = "/edit"
	editconfig_prefix     = "/config"
	saveconfig_prefix     = "/saveconfig"
	editpage_prefix       = "/edit/"
	savepage_prefix       = "/savepage"
	editstylesheet_prefix = "/editstylesheet/"
	savestylesheet_prefix = "/savestylesheet"
	edittemplate_prefix   = "/edittemplate/"
	savetemplate_prefix   = "/savetemplate"
	stylesheet_prefix     = "/css/"
	export_prefix         = "/exportpages"
	import_prefix         = "/importpages"
)

// the set of all global, static templates
var staticTemplates *template.Set

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

func init() {
	// assign functions to the templates
	staticTemplates = new(template.Set)
	staticTemplates.Funcs(template.FuncMap{
		"ifEqual": ifEqual,
	})
	template.SetMust(staticTemplates.ParseTemplateGlob("*.template"))

	http.HandleFunc(viewpage_prefix, viewpage)

	http.HandleFunc(editpage_prefix, edit)
	http.HandleFunc(savepage_prefix, save)

	http.HandleFunc(stylesheet_prefix, viewstylesheet)
	http.HandleFunc(editstylesheet_prefix, edit)
	http.HandleFunc(savestylesheet_prefix, save)

	http.HandleFunc(edittemplate_prefix, edit)
	http.HandleFunc(savetemplate_prefix, save)

	http.HandleFunc(editlist_prefix, editlist)
	http.HandleFunc(editconfig_prefix, edit)
	http.HandleFunc(saveconfig_prefix, save)

	http.HandleFunc(export_prefix, exportpages)
	http.HandleFunc(import_prefix, importpages)
}

func requireEditor(c appengine.Context, host string) (err os.Error) {
	if user.IsAdmin(c) {
		return
	}

	config, err := getConfig(c, host)
	if err != nil {
		return
	}
	u := user.Current(c)
	if u == nil {
		err = os.NewError("Must be logged in")
		return
	}
	for _, elt := range config.Editors {
		if elt == u.Email {
			return
		}
	}
	err = os.NewError("User not logged in as a valid editor for " + host)
	return
}

func getConfig(c appengine.Context, host string) (*Config, os.Error) {
	key := datastore.NewKey(c, "Config", host, 0, nil)
	value := &Config{
		Host: host,
		Editors: nil,
	}
	err := datastore.Get(c, key, value)
	if err != nil && err == datastore.ErrNoSuchEntity {
		// just use default values
	} else if err != nil {
		return nil, err
	}
	return value, nil
}

func render(c appengine.Context, w http.ResponseWriter, page *Page) (err os.Error) {
	success := false

	// get the template
	key := datastore.NewKey(c, "Template", page.Host + ":" + page.Template, 0, nil)
	tmpl := new(Template)
	if err = datastore.Get(c, key, tmpl); err == nil {
		parsed := template.New(tmpl.Url)
		if _, err = parsed.Parse(tmpl.Contents); err == nil {
			if err = parsed.Execute(w, page); err == nil {
				success = true
			}
		}
	}

	if err != nil || !success {
		fallback := staticTemplates.Template("fallback.template")
		err = fallback.Execute(w, page)
	}
	return
}

func GetKeysList(c appengine.Context, host string, table string) (lst []string, err os.Error) {
	q := datastore.NewQuery(table).Filter("Host =", host).Order("Url")
	q.KeysOnly()
	keys, err := q.GetAll(c, nil)
	if err != nil {
		return
	}
	lst = make([]string, len(keys))
	for i, elt := range keys {
		lst[i] = elt.StringID()[len(host)+1:]
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
		http.Error(w, err.String(), http.StatusUnauthorized)
		return
	}

	data := new(EditList)
	if data.Pages, err = GetKeysList(c, r.URL.Host, "Page"); err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
		return
	}
	if data.Stylesheets, err = GetKeysList(c, r.URL.Host, "Stylesheet"); err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
		return
	}
	if data.Templates, err = GetKeysList(c, r.URL.Host, "Template"); err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := staticTemplates.Execute(&buf, "edit-list.template", data); err != nil {
		panic("Executing edit-list template")
	}

	page := NewPage(r.URL.Host, "Edit_content")
	page.Rendered = string(buf.Bytes())
	if err := render(c, w, page); err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
		return
	}
}

//
// Pages
//

func viewpage(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	url := r.URL.Path[len(viewpage_prefix):]
	if url == "" {
		url = "Index"
	}
	page := NewPage(r.URL.Host, url)
	if !page.Validate() {
		http.Error(w, "Invalid page URL", http.StatusBadRequest)
		return
	}

	// get the page
	key := datastore.NewKey(c, "Page", page.Key(), 0, nil)
	err := datastore.Get(c, key, page)
	if err != nil && err == datastore.ErrNoSuchEntity {
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
		return
	}
	page.Render()
	if err = render(c, w, page); err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
	}
}

//
// Stylesheets
//

func viewstylesheet(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	url := r.URL.Path[len(stylesheet_prefix):]
	if strings.HasSuffix(url, ".css") {
		url = url[:len(".css")]
	}
	sheet := &Stylesheet{Host: r.URL.Host, Url: url}
	key := datastore.NewKey(c, "Stylesheet", sheet.Key(), 0, nil)
	err := datastore.Get(c, key, sheet)
	if err != nil && err == datastore.ErrNoSuchEntity {
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
		return
	}

	// set the mime type
	w.Header()["Content-Type"] = []string{"text/css; charset=utf-8"}
	w.Write([]byte(sheet.Contents))
}

//
// Generic actions
//

func save(w http.ResponseWriter, r *http.Request) {
	// check permissions
	c := appengine.NewContext(r)
	err := requireEditor(c, r.URL.Host)
	if err != nil {
		http.Error(w, err.String(), http.StatusUnauthorized)
		return
	}

	var value Object
    var table string

	// what are we supposed to save?
	switch r.URL.Path {
	case savepage_prefix:
		table = "Page"
		value = NewPage(r.URL.Host, "")
	case savestylesheet_prefix:
		table = "Stylesheet"
		value = &Stylesheet{Host: r.URL.Host}
	case savetemplate_prefix:
		table = "Template"
		value = &Template{Host: r.URL.Host}
	case saveconfig_prefix:
		table = "Config"
		value = &Config{Host: r.URL.Host}
		if !user.IsAdmin(c) {
			http.Error(w, "Must be logged in as an admin",
					http.StatusUnauthorized)
			return
		}
	default:
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	}

	// extract the form data
	if err := UnmarshalForm(r, value); err != nil {
		http.Error(w, err.String(), http.StatusBadRequest)
		return
	}
	if !value.Validate() {
		http.Error(w, "Invalid URL to save", http.StatusBadRequest)
		return
	}

	key := datastore.NewKey(c, table, value.Key(), 0, nil)

	// delete request
	if strings.HasPrefix(r.FormValue("Submit"), "Delete") {
		if err = datastore.Delete(c, key); err != nil && err != datastore.ErrNoSuchEntity {
			http.Error(w, err.String(), http.StatusInternalServerError)
			return
		}
		err = nil
		http.Redirect(w, r, "/edit", http.StatusFound)
		return
	}

	// save request
	if _, err = datastore.Put(c, key, value); err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
		return
	}
	target := "/edit"
	if r.URL.Path == savepage_prefix {
		target = "/" + r.FormValue("Url")
	} else if r.URL.Path == saveconfig_prefix {
		target = "/config"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func edit(w http.ResponseWriter, r *http.Request) {
	// only configured editors can edit
	c := appengine.NewContext(r)
	err := requireEditor(c, r.URL.Host)
	if err != nil {
		http.Error(w, err.String(), http.StatusUnauthorized)
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
		page := NewPage(r.URL.Host, url)
		if page.Templates, err = GetKeysList(c, r.URL.Host, "Template"); err != nil {
			http.Error(w, err.String(), http.StatusInternalServerError)
			return
		}
		value = page
	case strings.HasPrefix(path, editstylesheet_prefix):
		url = path[len(editstylesheet_prefix):]
		table = "Stylesheet"
		form = "edit-stylesheet.template"
		value = &Stylesheet{Host: r.URL.Host, Url:url}
	case strings.HasPrefix(path, edittemplate_prefix):
		url = path[len(edittemplate_prefix):]
		table = "Template"
		form = "edit-template.template"
		value = &Template{Host: r.URL.Host, Url:url}
	case path == editconfig_prefix:
		url = "config"
		table = "Config"
		form = "edit-config.template"
		value = &Config{Host: r.URL.Host}

		// only admins can edit configuration
		if !user.IsAdmin(c) {
			http.Error(w, "Must be logged in as an admin", http.StatusUnauthorized)
			return
		}
	default:
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	}

	// load the existing data (if any)
	if !value.Validate() {
		http.Error(w, "Invalid URL to edit", http.StatusBadRequest)
		return
	}
	key := datastore.NewKey(c, table, value.Key(), 0, nil)
	err = datastore.Get(c, key, value)
	if err != nil && err == datastore.ErrNoSuchEntity {
		// default empty entry is fine
	} else if err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
		return
	}

	// render the form
	var buf bytes.Buffer
	if err := staticTemplates.Execute(&buf, form, value); err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
		return
	}

	// now render the page that holds the form
	page := NewPage(r.URL.Host, "")
	page.Title = "Edit " + table + ": " + url
	if r.URL.Path == editconfig_prefix {
		page.Title = "Configuration"
	}
	page.Rendered = string(buf.Bytes())
	if err := render(c, w, page); err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
		return
	}
}

func UnmarshalForm(r *http.Request, val interface{}) (err os.Error) {
	v := reflect.ValueOf(val)
	if v.Kind() != reflect.Ptr {
		return os.NewError("non-pointer passed to UnmarshalForm")
	}
	if v.IsNil() {
		return os.NewError("nil pointer passed to UnmarshalForm")
	}
	s := v.Elem()
	if s.Kind() != reflect.Struct {
		return os.NewError("pointer to non-struct passed to UnmarshalForm")
	}
	t := s.Type()
	for i, n := 0, t.NumField(); i < n; i++ {
		field := t.Field(i)
		value := s.Field(i)

		str := r.FormValue(field.Name)
		if str == "" {
			continue
		}
		switch value.Kind() {
		case reflect.String:
			value.SetString(strings.TrimSpace(str))
		case reflect.Slice:
			if value.Type().Elem().Kind() != reflect.String {
				break
			}
			// split the field on commas
			lst := strings.Split(str, ",")
			for i, elt := range lst {
				lst[i] = strings.TrimSpace(elt)
			}
			value.Set(reflect.ValueOf(lst))
		case reflect.Int64:
			if n, err := strconv.Atoi64(str); err == nil {
				value.SetInt(n)
			}
		}
	}
	return nil
}

//
// Bulk import/export
//

func exportpages(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	err := requireEditor(c, r.URL.Host)
	if err != nil {
		http.Error(w, err.String(), http.StatusUnauthorized)
		return
	}

	// headers
	w.Header()["Content-Type"] = []string{"application/zip"}
	filename := r.URL.Host + "-backup_" +
				  time.UTC().Format("2006-01-02") + ".zip"
	w.Header()["Content-Disposition"] =
		[]string{`attachment; filename="` + filename + `"`}

	z := zip.NewWriter(w)

	// pages
	var page Page
	for iter := datastore.NewQuery("Page").Filter("Host =", r.URL.Host).Run(c); ; {
		_, err := iter.Next(&page)
		if err == datastore.Done {
			break
		}
		if err != nil {
			http.Error(w, err.String(), http.StatusInternalServerError)
			return
		}
		out, err := z.Create(page.Url + ".md")
		if err != nil {
			http.Error(w, err.String(), http.StatusInternalServerError)
			return
		}
		err = page.WriteTo(out)
		if err != nil {
			http.Error(w, err.String(), http.StatusInternalServerError)
			return
		}
	}

	// stylesheets
	var sheet Stylesheet
	for iter := datastore.NewQuery("Stylesheet").Filter("Host =", r.URL.Host).Run(c); ; {
		_, err := iter.Next(&sheet)
		if err == datastore.Done {
			break
		}
		if err != nil {
			http.Error(w, err.String(), http.StatusInternalServerError)
			return
		}
		out, err := z.Create(sheet.Url + ".css")
		if err != nil {
			http.Error(w, err.String(), http.StatusInternalServerError)
			return
		}
		_, err = out.Write([]byte(sheet.Contents))
		if err != nil {
			http.Error(w, err.String(), http.StatusInternalServerError)
			return
		}
	}

	// templates
	var tmpl Template
	for iter := datastore.NewQuery("Template").Filter("Host =", r.URL.Host).Run(c); ; {
		_, err := iter.Next(&tmpl)
		if err == datastore.Done {
			break
		}
		if err != nil {
			http.Error(w, err.String(), http.StatusInternalServerError)
			return
		}
		out, err := z.Create(tmpl.Url + ".template")
		if err != nil {
			http.Error(w, err.String(), http.StatusInternalServerError)
			return
		}
		_, err = out.Write([]byte(tmpl.Contents))
		if err != nil {
			http.Error(w, err.String(), http.StatusInternalServerError)
			return
		}
	}

	err = z.Close()
	if err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
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
func ParsePage(host string, raw string, url string) (page *Page) {
	page = new(Page)
		page.Host = host

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
func parseTime(raw string) datastore.Time {
	raw = strings.TrimSpace(raw)

	// time zone included?
	if stamp, err := time.Parse(time.RFC1123, raw); err == nil {
		return datastore.SecondsToTime(stamp.Seconds() + int64(stamp.ZoneOffset))
	}

	// time zone is missing, assume UTC
	if stamp, err := time.Parse("Mon, 02 Jan 2006 15:04:05", raw); err == nil {
		return datastore.SecondsToTime(stamp.Seconds())
	}

	// unrecognized format, fall back to current time
	return datastore.SecondsToTime(time.Seconds())
}

func (p *Page) WriteTo(w io.Writer) os.Error {
	header := p.Title + "\n"
	header += p.Created.Time().Format(time.RFC1123) + "\n"
	header += p.Modified.Time().Format(time.RFC1123) + "\n"
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

func (b bytebuf) ReadAt(p []byte, off int64) (n int, err os.Error) {
	slice := []byte(b)
	if off >= int64(len(slice)) {
		return 0, os.NewError("End of file")
	}
	n = copy(p, slice[off:])
	return n, nil
}

func importpages(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	err := requireEditor(c, r.URL.Host)
	if err != nil {
		http.Error(w, err.String(), http.StatusUnauthorized)
		return
	}

	file, _, err := r.FormFile("Zipfile")
	if err != nil {
		http.Error(w, "No zipfile submitted", http.StatusBadRequest)
		return
	}
	raw, err := ioutil.ReadAll(file)
	if err != nil {
		http.Error(w, err.String(), http.StatusBadRequest)
		return
	}
	z, err := zip.NewReader(bytebuf(raw), int64(len(raw)))
	if err != nil {
		http.Error(w, err.String(), http.StatusBadRequest)
		return
	}
	report := "<h1>Imported files</h1>\n<ul>\n"
	for _, elt := range z.File {
		fp, err := elt.Open()
		if err != nil {
			report += "<li>Error opening " + elt.Name + ": " + err.String() + "</li>\n"
			continue
		}
		data, err := ioutil.ReadAll(fp)
		if err != nil {
			report += "<li>Error reading " + elt.Name + ": " + err.String() + "</li>\n"
			continue
		}
		fp.Close()
		contents := string(data)

		// handle different file types
		if strings.HasSuffix(elt.Name, ".md") && len(elt.Name) > len(".md") {
			url := elt.Name[:len(elt.Name) - len(".md")]
			page := ParsePage(r.URL.Host, contents, url)
			if !page.Validate() {
				report += "<li>Invalid name: " + elt.Name + "</ul>\n"
				continue
			}
			key := datastore.NewKey(c, "Page", page.Key(), 0, nil)
			if _, err = datastore.Put(c, key, page); err != nil {
				http.Error(w, err.String(), http.StatusInternalServerError)
				return
			}
			report += "<li>Page: " + url + "</li>\n"
		} else if strings.HasSuffix(elt.Name, ".css") && len(elt.Name) > len(".css") {
			url := elt.Name[:len(elt.Name) - len(".css")]
			sheet := &Stylesheet{ Host: r.URL.Host, Url: url, Contents: contents }
			if !sheet.Validate() {
				report += "<li>Invalid name: " + elt.Name + "</ul>\n"
				continue
			}
			key := datastore.NewKey(c, "Stylesheet", sheet.Key(), 0, nil)
			if _, err = datastore.Put(c, key, sheet); err != nil {
				http.Error(w, err.String(), http.StatusInternalServerError)
				return
			}
			report += "<li>Stylesheet: " + url + "</li>\n"
		} else if strings.HasSuffix(elt.Name, ".template") && len(elt.Name) > len(".template") {
			url := elt.Name[:len(elt.Name) - len(".template")]
			tmpl := &Template{ Host: r.URL.Host, Url: url, Contents: contents }
			if !tmpl.Validate() {
				report += "<li>Invalid name: " + elt.Name + "</ul>\n"
				continue
			}
			key := datastore.NewKey(c, "Template", tmpl.Key(), 0, nil)
			if _, err = datastore.Put(c, key, tmpl); err != nil {
				http.Error(w, err.String(), http.StatusInternalServerError)
				return
			}
			report += "<li>Template: " + url + "</li>\n"
		} else {
			report += "<li>Skipping unknown file: " + elt.Name + "</li>\n"
		}
	}
	report += "</ul>\n"
	page := NewPage(r.URL.Host, "Imported_data_report")
	page.Rendered = report
	if err = render(c, w, page); err != nil {
		http.Error(w, err.String(), http.StatusInternalServerError)
		return
	}
}

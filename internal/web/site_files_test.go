package web

import (
	"html/template"
	"strings"
	"testing"
)

func TestParseListing(t *testing.T) {
	out := "/var/www/html\n" +
		"regular file|120|1700000000|composer.json\n" +
		"directory|4096|1700000000|vendor\n" +
		"regular empty file|0|1700000000|.env\n" +
		"directory|4096|1700000000|Storage\n" +
		"regular file|5|1700000000|a|b.txt\n" +
		"basura\n"
	cwd, entries, ok := parseListing(out)
	if !ok || cwd != "/var/www/html" || len(entries) != 5 {
		t.Fatalf("%v %q %+v", ok, cwd, entries)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, ","); got != "Storage,vendor,.env,a|b.txt,composer.json" {
		t.Error(got)
	}
	if entries[3].Path != "/var/www/html/a|b.txt" || entries[4].Size != 120 || entries[0].Modified.IsZero() {
		t.Errorf("%+v", entries)
	}
	if _, _, ok := parseListing("!\n"); ok {
		t.Error("carpeta que no se puede abrir")
	}
	if cwd, entries, ok := parseListing("/\n"); !ok || cwd != "/" || len(entries) != 0 {
		t.Error("carpeta vacía")
	}
}

func TestCleanPathAndCrumbs(t *testing.T) {
	cases := map[string]string{"": "", "/": "/", "var/www/": "/var/www", "/a/../../etc": "/etc", "/a/./b//c": "/a/b/c", "/x\x00y": ""}
	for in, want := range cases {
		if got := cleanPath(in); got != want {
			t.Errorf("%q → %q (esperaba %q)", in, got, want)
		}
	}
	c := pathCrumbs("/var/www/html")
	if len(c) != 4 || c[0].Path != "/" || c[3].Name != "html" || c[3].Path != "/var/www/html" {
		t.Errorf("%+v", c)
	}
	if len(pathCrumbs("/")) != 1 {
		t.Error("raíz")
	}
}

func TestSiteFilesPartial(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	_, entries, _ := parseListing("/srv\ndirectory|0|0|sub dir\nregular file|2048|1700000000|it's.txt\n")
	v := filesView{SiteID: 7, Path: "/srv", Parent: "/", Crumbs: pathCrumbs("/srv"), Entries: entries, Hidden: 3}
	var out strings.Builder
	if err := s.partials.ExecuteTemplate(&out, "site_files", v); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`hx-get="/sites/7/files?path=%2Fsrv%2Fsub%20dir"`, `href="/sites/7/files/download?path=%2fsrv%2fit%27s.txt"`,
		"2 KB", `data-cd="/srv"`, `href="/sites/7/files/download?path=%2fsrv"`, "y 3 más", `hx-get="/sites/7/files?path=%2F"`} {
		if !strings.Contains(html, want) {
			t.Errorf("falta %q\n%s", want, html)
		}
	}
}

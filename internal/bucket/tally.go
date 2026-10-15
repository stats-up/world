package bucket

import "strings"

// MaxFolders limita cuántas carpetas se guardan por bucket. Pasado el límite, lo que vive
// más abajo se suma a la carpeta más profunda ya registrada (los totales siguen exactos).
const MaxFolders = 200_000

// Folder es el tamaño acumulado de un prefijo ("" = raíz del bucket; si no, termina en "/").
type Folder struct {
	Prefix    string
	Bytes     int64 // todo lo que hay debajo, incluidas subcarpetas
	Objects   int64
	FileBytes int64 // solo los archivos sueltos directamente en esta carpeta
	Files     int64
}

// Parent devuelve la carpeta que contiene a esta ("" para las de primer nivel).
func Parent(prefix string) string {
	p := strings.TrimSuffix(prefix, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i+1]
	}
	return ""
}

// Tally suma los objetos por carpeta a medida que se listan.
type Tally struct {
	folders   map[string]*Folder
	Truncated bool // se alcanzó MaxFolders
}

func NewTally() *Tally {
	return &Tally{folders: map[string]*Folder{"": {Prefix: ""}}}
}

func (t *Tally) Add(key string, size int64) {
	last := t.folders[""]
	last.Bytes += size
	last.Objects++
	for i := 0; i < len(key); i++ {
		if key[i] != '/' {
			continue
		}
		prefix := key[:i+1]
		f, ok := t.folders[prefix]
		if !ok {
			if len(t.folders) >= MaxFolders {
				t.Truncated = true
				break
			}
			f = &Folder{Prefix: prefix}
			t.folders[prefix] = f
		}
		f.Bytes += size
		f.Objects++
		last = f
	}
	// Un objeto "carpeta/" vacío (lo crean algunas consolas) no es un archivo.
	if !strings.HasSuffix(key, "/") || size > 0 {
		last.FileBytes += size
		last.Files++
	}
}

func (t *Tally) Root() Folder { return *t.folders[""] }

func (t *Tally) Folders() []Folder {
	out := make([]Folder, 0, len(t.folders))
	for _, f := range t.folders {
		out = append(out, *f)
	}
	return out
}

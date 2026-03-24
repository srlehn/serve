package main

import (
	_ "embed"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
)

//go:embed index.html.template
var pageTemplate string

func main() {
	http.HandleFunc("/", page)
	http.HandleFunc("/upload", upload)

	if err := http.ListenAndServe(`:8000`, nil); err != nil {
		log.Fatal(err)
	}
}

func page(w http.ResponseWriter, req *http.Request) {
	if req.Method == `GET` && req.URL.Path != `/` {
		download(w, req)
		return
	}
	fis, err := ioutil.ReadDir(`.`)
	if err != nil {
		log.Fatal(err)
	}
	funcMap := template.FuncMap{`name`: func(n interface{ Name() string }) string { return n.Name() }}
	tp, err := template.New(`index`).Funcs(funcMap).Parse(pageTemplate)
	if err != nil {
		log.Fatal(err)
	}
	if err := tp.Execute(w, fis); err != nil {
		log.Fatal(err)
	}
}

func download(w http.ResponseWriter, req *http.Request) {
	http.ServeFile(w, req, `.`+req.URL.Path)
}

func upload(w http.ResponseWriter, req *http.Request) {
	if req.Method != `POST` {
		return
	}
	if err := req.ParseForm(); err != nil {
		log.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return
	}
	boundary, ok := params["boundary"]
	if !ok {
		return
	}
	mr := multipart.NewReader(req.Body, boundary)
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		name := p.FileName()
		formName := p.FormName()
		if formName == `submitFiles` {
			continue
		}

		// empty field
		if len(name) == 0 {
			continue // don't replace with empty file
		}
		t, err := os.CreateTemp(".", name+`.*`)
		if err != nil {
			log.Fatal(err)
		}
		tempName := t.Name()
		defer func() {
			t.Close()
			os.Remove(tempName)
		}()
		_, err = io.Copy(t, p)
		if err != nil {
			log.Fatal(err)
		}
		t.Close()
		err = os.Rename(tempName, name)
		if err != nil {
			log.Fatal(err)
		}
		err = os.Chmod(name, 0644)
		if err != nil {
			log.Fatal(err)
		}
	}
}

// testserver exercises MyIDM against the HTTP behaviors that matter:
//
//	/ranged/<file>          full Range support (http.ServeContent)
//	/slow/<file>?bps=N      Range support, throttled to N bytes/sec per connection
//	/norange/<file>         ignores Range, sends Content-Length (single-stream path)
//	/chunked/<file>         ignores Range, chunked transfer (unknown-size path)
//
// Usage: go run ./tools/testserver -listen 127.0.0.1:9090 -dir <files>
package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9090", "listen address")
	dir := flag.String("dir", ".", "directory of files to serve")
	flag.Parse()

	open := func(w http.ResponseWriter, r *http.Request, prefix string) (*os.File, os.FileInfo, bool) {
		name := filepath.Base(r.URL.Path[len(prefix):])
		f, err := os.Open(filepath.Join(*dir, name))
		if err != nil {
			http.NotFound(w, r)
			return nil, nil, false
		}
		fi, _ := f.Stat()
		return f, fi, true
	}

	http.HandleFunc("/ranged/", func(w http.ResponseWriter, r *http.Request) {
		f, fi, ok := open(w, r, "/ranged/")
		if !ok {
			return
		}
		defer f.Close()
		http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
	})

	http.HandleFunc("/slow/", func(w http.ResponseWriter, r *http.Request) {
		f, fi, ok := open(w, r, "/slow/")
		if !ok {
			return
		}
		defer f.Close()
		bps, _ := strconv.Atoi(r.URL.Query().Get("bps"))
		if bps <= 0 {
			bps = 256 << 10
		}
		http.ServeContent(w, r, fi.Name(), fi.ModTime(), &throttledReadSeeker{f: f, bps: bps})
	})

	http.HandleFunc("/norange/", func(w http.ResponseWriter, r *http.Request) {
		f, fi, ok := open(w, r, "/norange/")
		if !ok {
			return
		}
		defer f.Close()
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		w.Header().Set("Content-Type", "application/octet-stream")
		io.Copy(w, f)
	})

	http.HandleFunc("/chunked/", func(w http.ResponseWriter, r *http.Request) {
		f, _, ok := open(w, r, "/chunked/")
		if !ok {
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // force chunked: no Content-Length
		io.Copy(w, f)
	})

	log.Printf("testserver on http://%s serving %s", *listen, *dir)
	log.Fatal(http.ListenAndServe(*listen, nil))
}

// throttledReadSeeker caps read throughput so pause/resume can be tested
// against a download that takes a predictable number of seconds.
type throttledReadSeeker struct {
	f   *os.File
	bps int
}

func (t *throttledReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return t.f.Seek(offset, whence)
}

func (t *throttledReadSeeker) Read(p []byte) (int, error) {
	chunk := 32 << 10
	if len(p) > chunk {
		p = p[:chunk]
	}
	n, err := t.f.Read(p)
	if n > 0 {
		time.Sleep(time.Duration(float64(n) / float64(t.bps) * float64(time.Second)))
	}
	return n, err
}

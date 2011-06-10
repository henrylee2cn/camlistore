/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"http"
	"io"
	"json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"camli/blobref"
	"camli/blobserver"
	"camli/httputil"
	"camli/jsonconfig"
	"camli/schema"
)

var _ = log.Printf

var staticFilePattern = regexp.MustCompile(`^([a-zA-Z0-9\-\_]+\.(html|js|css|png|jpg|gif))$`)
var identPattern = regexp.MustCompile(`^[a-zA-Z\_]+$`)

// Download URL suffix:
//   $1: blobref (checked in download handler)
//   $2: optional "/filename" to be sent as recommended download name,
//       if sane looking
var downloadPattern = regexp.MustCompile(`^download/([^/]+)(/.*)?$`)

// UIHandler handles serving the UI and discovery JSON.
type UIHandler struct {
	// URL prefixes (path or full URL) to the primary blob and
	// search root.  Only used by the UI and thus necessary if UI
	// is true.
	BlobRoot     string
	SearchRoot   string
	JSONSignRoot string

	FilesDir string

	Storage blobserver.Storage // of BlobRoot
	Cache   blobserver.Storage // or nil
}

func defaultFilesDir() string {
	dir, _ := filepath.Split(os.Args[0])
	return filepath.Join(dir, "ui")
}

func init() {
	blobserver.RegisterHandlerConstructor("ui", newUiFromConfig)
}

func newUiFromConfig(ld blobserver.Loader, conf jsonconfig.Obj) (h http.Handler, err os.Error) {
	ui := &UIHandler{}
	ui.BlobRoot = conf.OptionalString("blobRoot", "")
	ui.SearchRoot = conf.OptionalString("searchRoot", "")
	ui.JSONSignRoot = conf.OptionalString("jsonSignRoot", "")
	cachePrefix := conf.OptionalString("cache", "")
	ui.FilesDir = conf.OptionalString("staticFiles", defaultFilesDir())
	if err = conf.Validate(); err != nil {
		return
	}

	checkType := func(key string, htype string) {
		v := conf.OptionalString(key, "")
		if v == "" {
			return
		}
		ct := ld.GetHandlerType(v)
		if ct == "" {
			err = fmt.Errorf("UI handler's %q references non-existant %q", key, v)
		} else if ct != htype {
			err = fmt.Errorf("UI handler's %q references %q of type %q; expected type %q", key, v,
				ct, htype)
		}
	}
	checkType("searchRoot", "search")
	checkType("jsonSignRoot", "jsonsign")
	if err != nil {
		return
	}

	if ui.BlobRoot != "" {
		bs, err := ld.GetStorage(ui.BlobRoot)
		if err != nil {
			return nil, fmt.Errorf("UI handler's blobRoot of %q error: %v", ui.BlobRoot, err)
		}
		ui.Storage = bs
	}

	if cachePrefix != "" {
		bs, err := ld.GetStorage(cachePrefix)
		if err != nil {
			return nil, fmt.Errorf("UI handler's cache of %q error: %v", ui.BlobRoot, err)
		}
		ui.Cache = bs
	}

	fi, sterr := os.Stat(ui.FilesDir)
	if sterr != nil || !fi.IsDirectory() {
		err = fmt.Errorf("UI handler's \"staticFiles\" of %q is invalid", ui.FilesDir)
		return
	}
	return ui, nil
}

func camliMode(req *http.Request) string {
	// TODO-GO: this is too hard to get at the GET Query args on a
	// POST request.
	m, err := http.ParseQuery(req.URL.RawQuery)
	if err != nil {
		return ""
	}
	if mode, ok := m["camli.mode"]; ok && len(mode) > 0 {
		return mode[0]
	}
	return ""
}

func wantsDiscovery(req *http.Request) bool {
	return req.Method == "GET" &&
		(req.Header.Get("Accept") == "text/x-camli-configuration" ||
			camliMode(req) == "config")
}

func wantsUploadHelper(req *http.Request) bool {
	return req.Method == "POST" && camliMode(req) == "uploadhelper"
}

func wantsPermanode(req *http.Request) bool {
	return req.Method == "GET" && blobref.Parse(req.FormValue("p")) != nil
}

func wantsBlobInfo(req *http.Request) bool {
	return req.Method == "GET" && blobref.Parse(req.FormValue("b")) != nil
}

func (ui *UIHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	base := req.Header.Get("X-PrefixHandler-PathBase")
	suffix := req.Header.Get("X-PrefixHandler-PathSuffix")

	rw.Header().Set("Vary", "Accept")
	switch {
	case wantsDiscovery(req):
		ui.serveDiscovery(rw, req)
	case wantsUploadHelper(req):
		ui.serveUploadHelper(rw, req)
	case strings.HasPrefix(suffix, "download/"):
		ui.serveDownload(rw, req)
	default:
		file := ""
		if m := staticFilePattern.FindStringSubmatch(suffix); m != nil {
			file = m[1]
		} else {
			switch {
			case wantsPermanode(req):
				file = "permanode.html"
			case wantsBlobInfo(req):
				file = "blobinfo.html"
			case req.URL.Path == base:
				file = "index.html"
			default:
				http.Error(rw, "Illegal URL.", 404)
				return
			}
		}
		http.ServeFile(rw, req, filepath.Join(ui.FilesDir, file))
	}
}

func (ui *UIHandler) serveDiscovery(rw http.ResponseWriter, req *http.Request) {
	rw.Header().Set("Content-Type", "text/javascript")
	inCb := false
	if cb := req.FormValue("cb"); identPattern.MatchString(cb) {
		fmt.Fprintf(rw, "%s(", cb)
		inCb = true
	}
	bytes, _ := json.Marshal(map[string]interface{}{
		"blobRoot":     ui.BlobRoot,
		"searchRoot":   ui.SearchRoot,
		"jsonSignRoot": ui.JSONSignRoot,
		"uploadHelper": "?camli.mode=uploadhelper", // hack; remove with better javascript
	})
	rw.Write(bytes)
	if inCb {
		rw.Write([]byte{')'})
	}
}

func (ui *UIHandler) storageSeekFetcher() (blobref.SeekFetcher, os.Error) {
	return blobref.SeekerFromStreamingFetcher(ui.Storage)
}

func (ui *UIHandler) serveDownload(rw http.ResponseWriter, req *http.Request) {
	if req.Method != "GET" && req.Method != "HEAD" {
		http.Error(rw, "Invalid download method", 400)
		return
	}
	if ui.Storage == nil {
		http.Error(rw, "No BlobRoot configured", 500)
		return
	}

	fetchSeeker, err := ui.storageSeekFetcher()
	if err != nil {
		http.Error(rw, err.String(), 500)
		return
	}

	suffix := req.Header.Get("X-PrefixHandler-PathSuffix")

	m := downloadPattern.FindStringSubmatch(suffix)
	if m == nil {
		httputil.ErrorRouting(rw, req)
		return
	}

	fbr := blobref.Parse(m[1])
	if fbr == nil {
		http.Error(rw, "Invalid blobref", 400)
		return
	}

	filename := m[2]
	if len(filename) > 0 {
		filename = filename[1:] // remove leading slash
	}

	fr, err := schema.NewFileReader(fetchSeeker, fbr)
	if err != nil {
		http.Error(rw, "Can't serve file: "+err.String(), 500)
		return
	}
	defer fr.Close()

	// TODO: fr.FileSchema() and guess a mime type?  For now:
	schema := fr.FileSchema()
	rw.Header().Set("Content-Type", "application/octet-stream")
	rw.Header().Set("Content-Length", fmt.Sprintf("%d", schema.Size))

	if req.Method == "HEAD" {
		vbr := blobref.Parse(req.FormValue("verifycontents"))
		if vbr == nil {
			return
		}
		hash := vbr.Hash()
		if hash == nil {
			return
		}
		io.Copy(hash, fr) // ignore errors, caught later
		if vbr.HashMatches(hash) {
			rw.Header().Set("X-Camli-Contents", vbr.String())
		}
		return
	}

	n, err := io.Copy(rw, fr)
	log.Printf("For %q request of %s: copied %d, %v", req.Method, req.URL.RawPath, n, err)
	if err != nil {
		log.Printf("error serving download of file schema %s: %v", fbr, err)
		return
	}
	if n != int64(schema.Size) {
		log.Printf("error serving download of file schema %s: sent %d, expected size of %d",
			fbr, n, schema.Size)
		return
	}
}

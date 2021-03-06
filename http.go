package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/gomemcached"
	"github.com/dustin/gomemcached/client"
)

const (
	blobPrefix  = "/.cbfs/blob/"
	metaPrefix  = "/.cbfs/meta/"
	proxyPrefix = "/.cbfs/viewproxy/"
	fetchPrefix = "/.cbfs/fetch/"
)

type BlobOwnership struct {
	OID    string               `json:"oid"`
	Length int64                `json:"length"`
	Nodes  map[string]time.Time `json:"nodes"`
	Type   string               `json:"type"`
}

func (b BlobOwnership) ResolveNodes() NodeList {
	keys := make([]string, 0, len(b.Nodes))
	for k := range b.Nodes {
		keys = append(keys, "/"+k)
	}
	resps := couchbase.GetBulk(keys)

	rv := make(NodeList, 0, len(resps))

	for k, v := range resps {
		if v.Status == gomemcached.SUCCESS {
			a := StorageNode{}
			err := json.Unmarshal(v.Body, &a)
			if err == nil {
				a.name = k[1:]
				rv = append(rv, a)
			}
		}
	}

	sort.Sort(rv)

	return rv
}

func (b BlobOwnership) ResolveRemoteNodes() NodeList {
	return b.ResolveNodes().minusLocal()
}

func recordBlobOwnership(h string, l int64) error {
	k := "/" + h
	return couchbase.Do(k, func(mc *memcached.Client, vb uint16) error {
		_, err := mc.CAS(vb, k, func(in []byte) ([]byte, memcached.CasOp) {
			ownership := BlobOwnership{}
			err := json.Unmarshal(in, &ownership)
			if err == nil {
				ownership.Nodes[serverId] = time.Now().UTC()
			} else {
				ownership.Nodes = map[string]time.Time{
					serverId: time.Now().UTC(),
				}
			}
			ownership.OID = h
			ownership.Length = l
			ownership.Type = "blob"
			return mustEncode(&ownership), memcached.CASStore
		}, 0)
		return err
	})
}

func recordBlobAccess(h string) {
	_, err := couchbase.Incr("/"+h+"/r", 1, 1, 0)
	if err != nil {
		log.Printf("Error incrementing counter for %v: %v", h, err)
	}

	_, err = couchbase.Incr("/"+serverId+"/r", 1, 1, 0)
	if err != nil {
		log.Printf("Error incrementing node identifier: %v", h, err)
	}
}

type storInfo struct {
	node string
	hs   string
	err  error
}

// Given a Reader, we produce a new reader that will duplicate the
// stream into the next available node and reproduce that content into
// another node.  Iff that node successfully stores the content, we
// return the hash it computed.
//
// The returned Reader must be consumed until the input EOFs or is
// closed.  The returned channel may yield a storInfo struct before
// it's closed.  If it's closed without yielding a storInfo, there are
// no remote nodes available.
func altStoreFile(r io.Reader) (io.Reader, <-chan storInfo) {
	bgch := make(chan storInfo, 1)

	nodes, err := findRemoteNodes()
	if err == nil && len(nodes) > 0 {
		r1, r2 := newMultiReader(r)
		r = r2

		go func() {
			defer close(bgch)

			rv := storInfo{node: nodes[0].Address()}

			rurl := "http://" +
				nodes[0].Address() + blobPrefix
			log.Printf("Piping secondary storage to %v",
				nodes[0].Address())
			preq, err := http.NewRequest("POST", rurl, r1)
			if err != nil {
				rv.err = err
				bgch <- rv
				return
			}

			presp, err := http.DefaultClient.Do(preq)
			if err == nil {
				if presp.StatusCode != 201 {
					rv.err = errors.New(presp.Status)
					bgch <- rv
				}
				_, err := io.Copy(ioutil.Discard, presp.Body)
				if err == nil {
					rv.hs = presp.Header.Get("X-Hash")
				}
				presp.Body.Close()
			} else {
				log.Printf("Error http'n to %v: %v", rurl, err)
				io.Copy(ioutil.Discard, r1)
			}
			rv.err = err
			bgch <- rv
		}()
	} else {
		close(bgch)
	}

	return r, bgch
}

func doPostRawBlob(w http.ResponseWriter, req *http.Request) {
	f, err := NewHashRecord(*root, "")
	if err != nil {
		log.Printf("Error writing tmp file: %v", err)
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
		return
	}
	defer f.Close()

	sh, length, err := f.Process(req.Body)
	if err != nil {
		log.Printf("Error linking in raw hash: %v", err)
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
		return
	}

	err = recordBlobOwnership(sh, length)
	if err != nil {
		log.Printf("Error recording blob ownership: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error recording blob ownership: %v", err)
		return
	}

	w.Header().Set("X-Hash", sh)

	w.WriteHeader(201)
}

func putUserFile(w http.ResponseWriter, req *http.Request) {
	f, err := NewHashRecord(*root, "")
	if err != nil {
		log.Printf("Error writing tmp file: %v", err)
		w.WriteHeader(500)
		return
	}
	defer f.Close()

	r, bgch := altStoreFile(req.Body)

	h, length, err := f.Process(r)
	if err != nil {
		log.Printf("Error completing blob write: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error completing blob write: %v", err)
		return
	}

	log.Printf("Wrote %v -> %v", req.URL.Path, h)

	fm := fileMeta{
		Headers:  req.Header,
		OID:      h,
		Length:   length,
		Modified: time.Now(),
	}

	err = recordBlobOwnership(h, length)
	if err != nil {
		log.Printf("Error storing blob ownership: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error recording blob ownership: %v", err)
		return
	}

	if si, hasStuff := <-bgch; hasStuff {
		if si.err != nil || si.hs != h {
			log.Printf("Error in secondary store to %v: %v",
				si.node, si.err)
			w.WriteHeader(500)
			fmt.Fprintf(w, "Error creating secondary copy: %v\n%v",
				si.err, si.hs)
			return
		}
	}

	revs := 0
	rheader := req.Header.Get("X-CBFS-KeepRevs")
	if rheader != "" {
		i, err := strconv.Atoi(rheader)
		if err == nil {
			revs = i
		}
	}

	err = storeMeta(resolvePath(req), fm, revs)
	if err != nil {
		log.Printf("Error storing file meta: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error recording blob ownership: %v", err)
		return
	}

	w.WriteHeader(201)
}

func putRawHash(w http.ResponseWriter, req *http.Request) {
	inputhash := minusPrefix(req.URL.Path, blobPrefix)

	if inputhash == "" {
		w.WriteHeader(400)
		w.Write([]byte("No oid specified"))
		return
	}

	f, err := NewHashRecord(*root, inputhash)
	if err != nil {
		log.Printf("Error writing tmp file: %v", err)
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
		return
	}
	defer f.Close()

	sh, length, err := f.Process(req.Body)
	if err != nil {
		log.Printf("Error linking in raw hash: %v", err)
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
		return
	}

	err = recordBlobOwnership(inputhash, length)
	if err != nil {
		log.Printf("Error recording blob ownership: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error recording blob ownership: %v", err)
		return
	}

	w.Header().Set("X-Hash", sh)

	w.WriteHeader(201)
}

func doPut(w http.ResponseWriter, req *http.Request) {
	switch {
	case strings.HasPrefix(req.URL.Path, blobPrefix):
		putRawHash(w, req)
	case strings.HasPrefix(req.URL.Path, metaPrefix):
		putMeta(w, req, minusPrefix(req.URL.Path, metaPrefix))
	case strings.HasPrefix(req.URL.Path, "/.cbfs/"):
		w.WriteHeader(400)
	default:
		putUserFile(w, req)
	}
}

func isResponseHeader(s string) bool {
	switch s {
	case "Content-Type", "Content-Length":
		return true
	}
	return false
}

func resolvePath(req *http.Request) string {
	path := req.URL.Path
	// Ignore /, but remove leading / from /blah
	for len(path) > 1 && path[0] == '/' {
		path = path[1:]
	}

	if len(path) > 0 && path[len(path)-1] == '/' {
		path = path + "index.html"
	}

	return path
}

func doHead(w http.ResponseWriter, req *http.Request) {
	path := resolvePath(req)
	got := fileMeta{}
	err := couchbase.Get(path, &got)
	if err != nil {
		log.Printf("Error getting file %#v: %v", path, err)
		w.WriteHeader(404)
		w.Write([]byte(err.Error()))
		return
	}

	if req.FormValue("rev") != "" {
		w.WriteHeader(400)
		return
	}

	for k, v := range got.Headers {
		if isResponseHeader(k) {
			w.Header()[k] = v
		}
	}

	oldestRev := got.Revno
	if len(got.Previous) > 0 {
		oldestRev = got.Previous[0].Revno
	}

	w.Header().Set("X-CBFS-Revno", strconv.Itoa(got.Revno))
	w.Header().Set("X-CBFS-OldestRev", strconv.Itoa(oldestRev))
	w.Header().Set("Last-Modified",
		got.Modified.UTC().Format(http.TimeFormat))
	w.Header().Set("Etag", `"`+got.OID+`"`)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", got.Length))

	w.WriteHeader(200)
}

func doGetUserDoc(w http.ResponseWriter, req *http.Request) {
	path := resolvePath(req)
	got := fileMeta{}
	err := couchbase.Get(path, &got)
	if err != nil {
		log.Printf("Error getting file %#v: %v", path, err)
		w.WriteHeader(404)
		w.Write([]byte(err.Error()))
		return
	}

	oid := got.OID
	respHeaders := got.Headers
	modified := got.Modified
	revno := got.Revno
	oldestRev := revno

	if len(got.Previous) > 0 {
		oldestRev = got.Previous[0].Revno
	}

	revnoStr := req.FormValue("rev")
	if revnoStr != "" {
		i, err := strconv.Atoi(revnoStr)
		if err != nil {
			w.WriteHeader(400)
			fmt.Fprintf(w, "Invalid revno")
			return
		}
		revno = i

		oid = ""
		for _, rev := range got.Previous {
			if rev.Revno == revno {
				oid = rev.OID
				modified = rev.Modified
				respHeaders = rev.Headers
				break
			}
		}
		if oid == "" {
			w.WriteHeader(410)
			fmt.Fprintf(w, "Don't have this file with rev %v", revno)
			return
		}
	}

	w.Header().Set("X-CBFS-Revno", strconv.Itoa(revno))
	w.Header().Set("X-CBFS-OldestRev", strconv.Itoa(oldestRev))

	inm := req.Header.Get("If-None-Match")
	if len(inm) > 2 {
		inm = inm[1 : len(inm)-1]
		if got.OID == inm {
			w.WriteHeader(304)
			return
		}
	}

	f, err := os.Open(hashFilename(*root, oid))
	if err != nil {
		getBlobFromRemote(w, oid, respHeaders, *cachePercentage)
		return
	}
	defer f.Close()

	for k, v := range respHeaders {
		if isResponseHeader(k) {
			w.Header()[k] = v
		}
	}

	w.Header().Set("Etag", `"`+oid+`"`)

	go recordBlobAccess(oid)
	http.ServeContent(w, req, path, modified, f)
}

func doServeRawBlob(w http.ResponseWriter, req *http.Request, oid string) {
	f, err := os.Open(hashFilename(*root, oid))
	if err != nil {
		w.WriteHeader(404)
		fmt.Fprintf(w, "Error opening blob: %v", err)
		removeBlobOwnershipRecord(oid, serverId)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")

	go recordBlobAccess(oid)
	http.ServeContent(w, req, "", time.Time{}, f)
}

func getBlobFromRemote(w http.ResponseWriter, oid string,
	respHeader http.Header, cachePerc int) {

	// Find the owners of this blob
	ownership := BlobOwnership{}
	oidkey := "/" + oid
	err := couchbase.Get(oidkey, &ownership)
	if err != nil {
		log.Printf("Missing ownership record for OID: %v", oid)
		// Not sure 404 is the right response here
		w.WriteHeader(404)
		return
	}

	nl := ownership.ResolveRemoteNodes()

	// Loop through the nodes that claim to own this blob
	// If we encounter any errors along the way, try the next node
	for _, sid := range nl {
		log.Printf("Trying to get %s from %s", oid, sid.name)

		resp, err := http.Get(sid.BlobURL(oid))
		if err != nil {
			log.Printf("Error reading oid %s from node %s", oid, sid)
			continue
		}

		if resp.StatusCode != 200 {
			log.Printf("Error response code %d from node %s", resp.StatusCode, sid)
			continue
		}

		// Found one, set the headers and send it.  Keep a
		// local copy for good luck.

		for k, v := range respHeader {
			if isResponseHeader(k) {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(200)
		writeTo := io.Writer(w)
		var hw *hashRecord

		if cachePerc > rand.Intn(100) {
			log.Printf("Storing remotely proxied request")
			hw, err = NewHashRecord(*root, oid)
			if err == nil {
				writeTo = io.MultiWriter(hw, w)
			} else {
				hw = nil
			}
		}

		length, err := io.Copy(writeTo, resp.Body)

		if err != nil {
			log.Printf("Failed to write from remote stream %v", err)
		} else {
			// A successful copy with a working hash
			// record means we should link in and record
			// our copy of this file.
			if hw != nil {
				_, err = hw.Finish()
				if err == nil {
					go recordBlobOwnership(oid, length)
				}
			}
		}

		return
	}

	//if we got to this point, no node in the list actually had it
	log.Printf("Don't have hash file: %v and no remote nodes could help",
		oid)
	w.WriteHeader(500)
	fmt.Fprintf(w, "Cannot locate blob %v", oid)
	return
}

func doList(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(200)
	explen := getHash().Size() * 2
	filepath.Walk(*root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && !strings.HasPrefix(info.Name(), "tmp") &&
			len(info.Name()) == explen {
			_, e := w.Write([]byte(info.Name() + "\n"))
			return e
		}
		return nil
	})
}

func putMeta(w http.ResponseWriter, req *http.Request, path string) {
	got := fileMeta{}
	casid := uint64(0)
	err := couchbase.Gets(path, &got, &casid)
	if err != nil {
		log.Printf("Error getting file %#v: %v", path, err)
		w.WriteHeader(404)
		w.Write([]byte(err.Error()))
		return
	}

	r := json.RawMessage{}
	err = json.NewDecoder(req.Body).Decode(&r)
	if err != nil {
		w.WriteHeader(400)
		w.Write([]byte(err.Error()))
		return
	}

	got.Userdata = &r
	b := mustEncode(&got)

	err = couchbase.Do(path, func(mc *memcached.Client, vb uint16) error {
		req := &gomemcached.MCRequest{
			Opcode:  gomemcached.SET,
			VBucket: vb,
			Key:     []byte(path),
			Cas:     casid,
			Opaque:  0,
			Extras:  []byte{0, 0, 0, 0, 0, 0, 0, 0},
			Body:    b}
		resp, err := mc.Send(req)
		if err != nil {
			return err
		}
		if resp.Status != gomemcached.SUCCESS {
			return resp
		}
		return nil
	})

	if err == nil {
		w.WriteHeader(201)
	} else {
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
	}
}

func doGetMeta(w http.ResponseWriter, req *http.Request, path string) {
	got := fileMeta{}
	err := couchbase.Get(path, &got)
	if err != nil {
		log.Printf("Error getting file %#v: %v", path, err)
		w.WriteHeader(404)
		w.Write([]byte(err.Error()))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	if got.Userdata == nil {
		w.Write([]byte("{}"))
	} else {
		w.Write(*got.Userdata)
	}
}

func doListNodes(w http.ResponseWriter, req *http.Request) {

	nl, err := findAllNodes()
	if err != nil {
		log.Printf("Error executing nodes view: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error generating node list: %v", err)
		return
	}

	respob := map[string]interface{}{}
	for _, node := range nl {
		age := time.Since(node.Time)
		respob[node.name] = map[string]interface{}{
			"size":      node.storageSize,
			"addr":      node.Address(),
			"hbtime":    node.Time,
			"hbage_ms":  age.Nanoseconds() / 1e6,
			"hbage_str": age.String(),
			"hash":      node.Hash,
			"addr_raw":  node.Addr,
			"bindaddr":  node.BindAddr,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(mustEncode(respob))
}

func proxyViewRequest(w http.ResponseWriter, req *http.Request,
	path string) {

	node := couchbase.Nodes[rand.Intn(len(couchbase.Nodes))]
	u, err := url.Parse(node.CouchAPIBase)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	u.Path = "/" + path
	u.RawQuery = req.URL.RawQuery

	res, err := http.Get(u.String())
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer res.Body.Close()

	for k, vs := range res.Header {
		w.Header()[k] = vs
	}
	w.WriteHeader(res.StatusCode)

	io.Copy(w, res.Body)
}

type captureResponseWriter struct {
	hdr        http.Header
	statusCode int
}

func (c *captureResponseWriter) Header() http.Header {
	return c.hdr
}

func (c *captureResponseWriter) Write(b []byte) (int, error) {
	return len(b), nil
}

func (c *captureResponseWriter) WriteHeader(code int) {
	c.statusCode = code
}

func doFetchDoc(w http.ResponseWriter, req *http.Request,
	path string) {

	c := captureResponseWriter{}

	getBlobFromRemote(&c, path, http.Header{}, 100)

	if c.statusCode == 200 {
		w.WriteHeader(204)
	} else {
		w.WriteHeader(c.statusCode)
	}
}

func doGet(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == blobPrefix:
		doList(w, req)
	case req.URL.Path == "/.cbfs/nodes/":
		doListNodes(w, req)
	case strings.HasPrefix(req.URL.Path, fetchPrefix):
		doFetchDoc(w, req,
			minusPrefix(req.URL.Path, fetchPrefix))
	case strings.HasPrefix(req.URL.Path, metaPrefix):
		doGetMeta(w, req,
			minusPrefix(req.URL.Path, metaPrefix))
	case strings.HasPrefix(req.URL.Path, blobPrefix):
		doServeRawBlob(w, req, minusPrefix(req.URL.Path, blobPrefix))
	case *enableViewProxy && strings.HasPrefix(req.URL.Path, proxyPrefix):
		proxyViewRequest(w, req, minusPrefix(req.URL.Path, proxyPrefix))
	case strings.HasPrefix(req.URL.Path, "/.cbfs/"):
		w.WriteHeader(400)
	default:
		doGetUserDoc(w, req)
	}
}

func minusPrefix(s, prefix string) string {
	return s[len(prefix):]
}

func doDeleteOID(w http.ResponseWriter, req *http.Request) {
	oid := minusPrefix(req.URL.Path, blobPrefix)
	err := os.Remove(hashFilename(*root, oid))
	if err == nil {
		w.WriteHeader(204)
	} else {
		w.WriteHeader(404)
		w.Write([]byte(err.Error()))
	}
}

func doDeleteUserDoc(w http.ResponseWriter, req *http.Request) {
	err := couchbase.Delete(resolvePath(req))
	if err == nil {
		w.WriteHeader(204)
	} else {
		w.WriteHeader(404)
		w.Write([]byte(err.Error()))
	}
}

func doDelete(w http.ResponseWriter, req *http.Request) {
	switch {
	case strings.HasPrefix(req.URL.Path, blobPrefix):
		doDeleteOID(w, req)
	case strings.HasPrefix(req.URL.Path, "/.cbfs/"):
		w.WriteHeader(400)
	default:
		doDeleteUserDoc(w, req)
	}
}

func doPost(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == blobPrefix {
		doPostRawBlob(w, req)
	} else {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func httpHandler(w http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	switch req.Method {
	case "PUT":
		doPut(w, req)
	case "POST":
		doPost(w, req)
	case "GET":
		doGet(w, req)
	case "HEAD":
		doHead(w, req)
	case "DELETE":
		doDelete(w, req)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

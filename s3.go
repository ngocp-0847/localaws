package main

// S3 (REST/XML): buckets (create / delete / head / list / versioning / location /
// EventBridge notification), objects (put / get / head / delete / copy, ranged GET,
// version ids, delete markers, user metadata), ListObjects v1/v2 with delimiter,
// ListObjectVersions, DeleteObjects, multipart uploads. Path-style and virtual-host
// style addressing. Buckets with EventBridge notifications enabled emit
// "Object Created" / "Object Deleted" events exactly like S3 → EventBridge.

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Bucket struct {
	Name        string            `json:"name"`
	Created     int64             `json:"created"`
	Versioning  string            `json:"versioning"` // "" | Enabled | Suspended
	EventBridge bool              `json:"eventBridge"`
	Config      map[string]string `json:"config,omitempty"` // subresource (policy, tagging, cors, …) → the document as PUT
	ObjectLock  bool              `json:"objectLock,omitempty"`
	MFADelete   string            `json:"mfaDelete,omitempty"`
}

type objMeta struct {
	ContentType        string            `json:"contentType,omitempty"`
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	ContentLanguage    string            `json:"contentLanguage,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	Expires            string            `json:"expires,omitempty"`
	StorageClass       string            `json:"storageClass,omitempty"`
	SSE                string            `json:"sse,omitempty"`
	SSEKMSKeyID        string            `json:"sseKmsKeyId,omitempty"`
	WebsiteRedirect    string            `json:"websiteRedirect,omitempty"`
	User               map[string]string `json:"user,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
	Parts              int               `json:"parts,omitempty"`
}

type s3Object struct {
	Bucket, Key, VersionID, ETag, Blob string
	Seq, Size, Modified                int64
	DeleteMarker                       bool
	Meta                               objMeta
}

type upload struct {
	ID       string  `json:"id"`
	Bucket   string  `json:"bucket"`
	Key      string  `json:"key"`
	Started  int64   `json:"started"`
	Meta     objMeta `json:"meta"`
	Archived bool    `json:"-"`
}

const s3ns = `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`

// ── routing ──────────────────────────────────────────────────────────────
func (a *App) bucketFromHost(host string) string {
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	for _, suffix := range []string{".s3." + a.cfg.Region + ".amazonaws.com", ".s3.amazonaws.com", ".s3.localhost", ".localhost"} {
		if strings.HasSuffix(host, suffix) {
			return strings.TrimSuffix(host, suffix)
		}
	}
	if i := strings.Index(host, "."); i > 0 && a.db.has("bucket", host[:i]) {
		return host[:i]
	}
	return ""
}

func (a *App) s3(w http.ResponseWriter, r *http.Request) {
	bucket := a.bucketFromHost(r.Host)
	key := strings.TrimPrefix(r.URL.Path, "/")
	if bucket == "" {
		if key == "" {
			if r.Method == http.MethodGet {
				a.s3ListBuckets(w)
				return
			}
			s3Err(w, 400, "InvalidRequest", "no bucket in the request", "/")
			return
		}
		bucket, key, _ = strings.Cut(key, "/")
	}
	q := r.URL.Query()
	if key == "" {
		a.s3Bucket(w, r, bucket, q)
		return
	}
	var b Bucket
	if !a.db.get("bucket", bucket, &b) {
		s3Err(w, 404, "NoSuchBucket", "The specified bucket does not exist", "/"+bucket)
		return
	}
	for _, sub := range unsupportedObjectSubresources {
		if q.Has(sub) {
			s3Err(w, 501, "NotImplemented", "localaws s3: object "+sub+" is not implemented", "/"+bucket+"/"+key)
			return
		}
	}
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		a.s3CreateUpload(w, r, b, key)
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		a.s3CompleteUpload(w, r, b, key, q.Get("uploadId"))
	case r.Method == http.MethodPut && q.Get("uploadId") != "":
		a.s3UploadPart(w, r, q.Get("uploadId"), atoi(q.Get("partNumber")))
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		a.s3AbortUpload(w, q.Get("uploadId"))
	case r.Method == http.MethodGet && q.Get("uploadId") != "":
		a.s3ListParts(w, b, key, q.Get("uploadId"))
	case r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "":
		a.s3Copy(w, r, b, key)
	case q.Has("tagging"):
		a.s3ObjectTagging(w, r, b, key, q.Get("versionId"))
	case q.Has("acl"):
		if r.Method == http.MethodGet {
			writeXML(w, 200, a.s3ACL())
			return
		}
		w.WriteHeader(200)
	case r.Method == http.MethodPut:
		a.s3Put(w, r, b, key)
	case r.Method == http.MethodGet, r.Method == http.MethodHead:
		a.s3Get(w, r, b, key, q.Get("versionId"))
	case r.Method == http.MethodDelete:
		a.s3Delete(w, b, key, q.Get("versionId"), true)
	default:
		s3Err(w, 405, "MethodNotAllowed", "The specified method is not allowed against this resource.", "/"+bucket+"/"+key)
	}
}

// bucketConfigs: the bucket subresources kept as the documents they were PUT with, and
// what GET answers while one is not set — the error code SDKs and Terraform recognise as
// "not configured", or the default AWS reports.
var bucketConfigs = map[string]struct {
	code, msg string // 404 answer when unset
	def       string // or this default document
}{
	"policy":            {"NoSuchBucketPolicy", "The bucket policy does not exist", ""},
	"tagging":           {"NoSuchTagSet", "The TagSet does not exist", ""},
	"cors":              {"NoSuchCORSConfiguration", "The CORS configuration does not exist", ""},
	"lifecycle":         {"NoSuchLifecycleConfiguration", "The lifecycle configuration does not exist", ""},
	"website":           {"NoSuchWebsiteConfiguration", "The specified bucket does not have a website configuration", ""},
	"publicAccessBlock": {"NoSuchPublicAccessBlockConfiguration", "The public access block configuration was not found", ""},
	"ownershipControls": {"OwnershipControlsNotFoundError", "The bucket ownership controls were not found", ""},
	"replication":       {"ReplicationConfigurationNotFoundError", "The replication configuration was not found", ""},
	"object-lock":       {"ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket", ""},
	"encryption": {"", "", "<ServerSideEncryptionConfiguration " + s3ns + "><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm>" +
		"</ApplyServerSideEncryptionByDefault><BucketKeyEnabled>false</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>"},
	"accelerate":     {"", "", "<AccelerateConfiguration " + s3ns + "/>"},
	"requestPayment": {"", "", "<RequestPaymentConfiguration " + s3ns + "><Payer>BucketOwner</Payer></RequestPaymentConfiguration>"},
	"logging":        {"", "", "<BucketLoggingStatus " + s3ns + "/>"},
	"acl":            {"", "", ""}, // see s3ACL
}

// listed configurations (analytics, metrics, …): empty lists, and no single one exists.
var bucketConfigLists = map[string]string{
	"analytics": "ListBucketAnalyticsConfigurationResult", "metrics": "ListMetricsConfigurationsResult",
	"inventory": "ListInventoryConfigurationsResult", "intelligent-tiering": "ListBucketIntelligentTieringConfigurationsResult",
}

func (a *App) s3ACL() string {
	owner := "<ID>" + a.cfg.Account + "</ID><DisplayName>localaws</DisplayName>"
	return "<AccessControlPolicy " + s3ns + "><Owner>" + owner + "</Owner><AccessControlList><Grant><Grantee xmlns:xsi=\"http://www.w3.org/2001/XMLSchema-instance\" xsi:type=\"CanonicalUser\">" +
		owner + "</Grantee><Permission>FULL_CONTROL</Permission></Grant></AccessControlList></AccessControlPolicy>"
}

// subresource names the bucket configuration a request addresses ("" for none).
func subresource(q url.Values) string {
	for k := range bucketConfigs {
		if q.Has(k) {
			return k
		}
	}
	for k := range bucketConfigLists {
		if q.Has(k) {
			return k
		}
	}
	for _, k := range []string{"versioning", "notification", "location", "versions", "uploads", "delete", "policyStatus"} {
		if q.Has(k) {
			return k
		}
	}
	for k := range q { // a configuration this emulator does not know: kept generically ("?name")
		if !plainParams[k] && !strings.HasPrefix(strings.ToLower(k), "x-amz-") && !strings.HasPrefix(k, "response-") {
			return "?" + k
		}
	}
	return ""
}

// plainParams: query parameters that are arguments, not subresources.
var plainParams = map[string]bool{"x-id": true, "list-type": true, "prefix": true, "delimiter": true, "marker": true, "max-keys": true,
	"continuation-token": true, "start-after": true, "encoding-type": true, "fetch-owner": true, "key-marker": true, "version-id-marker": true,
	"upload-id-marker": true, "max-uploads": true, "versionId": true, "allow-unordered": true, "id": true}

// unsupportedObjectSubresources answer 501 rather than being taken for a PutObject / GetObject.
var unsupportedObjectSubresources = []string{"retention", "legal-hold", "attributes", "torrent", "restore", "select", "renameObject"}

func (a *App) s3Bucket(w http.ResponseWriter, r *http.Request, name string, q url.Values) {
	var b Bucket
	exists := a.db.get("bucket", name, &b)
	sub := subresource(q)
	if r.Method == http.MethodPut && sub == "" {
		if exists {
			s3Err(w, 409, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.", "/"+name)
			return
		}
		nb := Bucket{Name: name, ObjectLock: strings.EqualFold(r.Header.Get("x-amz-bucket-object-lock-enabled"), "true")}
		if nb.ObjectLock {
			nb.Versioning = "Enabled"
		}
		if body, _ := readBody(r); len(body) > 0 { // CreateBucketConfiguration may carry the bucket's tags
			var cfg struct {
				Tags struct{ Tag []struct{ Key, Value string } }
			}
			if xml.Unmarshal(body, &cfg) == nil && len(cfg.Tags.Tag) > 0 {
				tags := map[string]string{}
				for _, t := range cfg.Tags.Tag {
					tags[t.Key] = t.Value
				}
				defer func() { a.setBucketTags(Bucket{Name: name}, tags) }()
			}
		}
		if err := a.createBucket(nb); err != nil {
			s3Err(w, 400, "InvalidBucketName", err.Error(), "/"+name)
			return
		}
		w.Header().Set("Location", "/"+name)
		w.WriteHeader(200)
		return
	}
	if !exists {
		s3Err(w, 404, "NoSuchBucket", "The specified bucket does not exist", "/"+name)
		return
	}
	if cfg, ok := bucketConfigs[sub]; ok {
		a.s3BucketConfig(w, r, b, sub, cfg.code, cfg.msg, cfg.def)
		return
	}
	if strings.HasPrefix(sub, "?") {
		a.s3BucketConfig(w, r, b, sub[1:], "NoSuchConfiguration", "The specified configuration does not exist.", "")
		return
	}
	if root, ok := bucketConfigLists[sub]; ok {
		if r.Method == http.MethodGet && q.Get("id") == "" {
			writeXML(w, 200, "<"+root+" "+s3ns+"><IsTruncated>false</IsTruncated></"+root+">")
			return
		}
		if r.Method == http.MethodGet {
			s3Err(w, 404, "NoSuchConfiguration", "The specified configuration does not exist.", "/"+name)
			return
		}
		w.WriteHeader(204)
		return
	}
	switch {
	case r.Method == http.MethodHead:
		w.Header().Set("x-amz-bucket-region", a.cfg.Region)
		w.WriteHeader(200)
	case r.Method == http.MethodDelete && sub == "":
		if len(a.s3Objects(name, "", true)) > 0 {
			s3Err(w, 409, "BucketNotEmpty", "The bucket you tried to delete is not empty", "/"+name)
			return
		}
		_ = a.db.del("bucket", name)
		w.WriteHeader(204)
	case r.Method == http.MethodPut && sub == "versioning":
		body, _ := readBody(r)
		var v struct{ Status, MfaDelete string }
		_ = xml.Unmarshal(body, &v)
		if v.Status != "Enabled" && v.Status != "Suspended" {
			s3Err(w, 400, "MalformedXML", "versioning Status must be Enabled or Suspended", "/"+name)
			return
		}
		b.Versioning, b.MFADelete = v.Status, v.MfaDelete
		_ = a.db.put("bucket", name, b)
		w.WriteHeader(200)
	case r.Method == http.MethodGet && sub == "versioning":
		st := ""
		if b.Versioning != "" {
			st = "<Status>" + b.Versioning + "</Status>"
			if b.MFADelete != "" {
				st += "<MfaDelete>" + b.MFADelete + "</MfaDelete>"
			}
		}
		writeXML(w, 200, "<VersioningConfiguration "+s3ns+">"+st+"</VersioningConfiguration>")
	case r.Method == http.MethodPut && sub == "notification":
		body, _ := readBody(r)
		b.EventBridge = bytes.Contains(body, []byte("EventBridgeConfiguration"))
		if b.Config == nil {
			b.Config = map[string]string{}
		}
		b.Config["notification"] = string(body)
		_ = a.db.put("bucket", name, b)
		w.WriteHeader(200)
	case r.Method == http.MethodGet && sub == "notification":
		if doc := b.Config["notification"]; doc != "" {
			writeXML(w, 200, strings.TrimPrefix(strings.TrimSpace(doc), xml.Header))
			return
		}
		eb := ""
		if b.EventBridge {
			eb = "<EventBridgeConfiguration/>"
		}
		writeXML(w, 200, "<NotificationConfiguration "+s3ns+">"+eb+"</NotificationConfiguration>")
	case r.Method == http.MethodGet && sub == "location":
		loc := a.cfg.Region
		if loc == "us-east-1" {
			loc = ""
		}
		writeXML(w, 200, "<LocationConstraint "+s3ns+">"+loc+"</LocationConstraint>")
	case r.Method == http.MethodGet && sub == "policyStatus":
		writeXML(w, 200, "<PolicyStatus "+s3ns+"><IsPublic>false</IsPublic></PolicyStatus>")
	case r.Method == http.MethodGet && sub == "versions":
		a.s3ListVersions(w, b, q)
	case r.Method == http.MethodGet && sub == "uploads":
		a.s3ListUploads(w, b)
	case r.Method == http.MethodPost && sub == "delete":
		a.s3DeleteObjects(w, r, b)
	case r.Method == http.MethodGet && sub == "":
		a.s3List(w, b, q)
	default:
		s3Err(w, 405, "MethodNotAllowed", "The specified method is not allowed against this resource.", "/"+name)
	}
}

// s3BucketConfig: PUT keeps the document, GET gives it back (or the default / the
// "not configured" error), DELETE forgets it.
func (a *App) s3BucketConfig(w http.ResponseWriter, r *http.Request, b Bucket, sub, code, msg, def string) {
	if b.Config == nil {
		b.Config = map[string]string{}
	}
	switch r.Method {
	case http.MethodPut:
		body, err := readBody(r)
		if err != nil {
			s3Err(w, 400, "IncompleteBody", err.Error(), "/"+b.Name)
			return
		}
		doc := strings.TrimPrefix(strings.TrimSpace(string(body)), xml.Header)
		doc = strings.TrimSpace(strings.TrimPrefix(doc, `<?xml version="1.0" encoding="UTF-8"?>`))
		if sub == "policy" {
			var p any
			if json.Unmarshal(body, &p) != nil {
				s3Err(w, 400, "MalformedPolicy", "Policies must be valid JSON and the first byte must be '{'", "/"+b.Name)
				return
			}
		}
		if sub == "acl" && doc == "" {
			doc = a.s3ACL() // canned ACL (x-amz-acl): the owner keeps full control
		}
		if sub == "object-lock" {
			b.ObjectLock = true
		}
		if sub == "lifecycle" {
			size := r.Header.Get("x-amz-transition-default-minimum-object-size")
			if size == "" {
				size = "all_storage_classes_128K"
			}
			b.Config["lifecycle.minsize"] = size
			w.Header().Set("x-amz-transition-default-minimum-object-size", size)
		}
		b.Config[sub] = doc
		_ = a.db.put("bucket", b.Name, b)
		w.WriteHeader(200)
	case http.MethodGet:
		doc, ok := b.Config[sub]
		switch {
		case ok:
		case sub == "acl":
			doc = a.s3ACL()
		case sub == "object-lock" && b.ObjectLock:
			doc = "<ObjectLockConfiguration " + s3ns + "><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>"
		case def != "":
			doc = def
		default:
			s3Err(w, 404, code, msg, "/"+b.Name)
			return
		}
		if sub == "lifecycle" && b.Config["lifecycle.minsize"] != "" {
			w.Header().Set("x-amz-transition-default-minimum-object-size", b.Config["lifecycle.minsize"])
		}
		if sub == "policy" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("x-amz-request-id", strings.ToUpper(randHex(8)))
			w.WriteHeader(200)
			_, _ = io.WriteString(w, doc)
			return
		}
		writeXML(w, 200, doc)
	case http.MethodDelete:
		delete(b.Config, sub)
		delete(b.Config, sub+".minsize")
		_ = a.db.put("bucket", b.Name, b)
		w.WriteHeader(204)
	default:
		s3Err(w, 405, "MethodNotAllowed", "The specified method is not allowed against this resource.", "/"+b.Name)
	}
}

func validBucketName(n string) bool {
	if len(n) < 3 || len(n) > 63 {
		return false
	}
	for i, c := range n {
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.'
		if !ok || (i == 0 || i == len(n)-1) && (c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func (a *App) createBucket(b Bucket) error {
	if !validBucketName(b.Name) {
		return fmt.Errorf("The specified bucket is not valid: %s", b.Name)
	}
	if b.Created == 0 {
		b.Created = nowMs()
	}
	a.logf("s3 bucket %s created (versioning=%q eventbridge=%v)", b.Name, b.Versioning, b.EventBridge)
	return a.db.put("bucket", b.Name, b)
}

func (a *App) s3ListBuckets(w http.ResponseWriter) {
	var sb strings.Builder
	sb.WriteString("<ListAllMyBucketsResult " + s3ns + "><Owner><ID>" + a.cfg.Account + "</ID><DisplayName>localaws</DisplayName></Owner><Buckets>")
	bs := list[Bucket](a.db, "bucket")
	sort.Slice(bs, func(i, j int) bool { return bs[i].Name < bs[j].Name })
	for _, b := range bs {
		sb.WriteString("<Bucket><Name>" + xe(b.Name) + "</Name><CreationDate>" + isoMs(b.Created) + "</CreationDate></Bucket>")
	}
	sb.WriteString("</Buckets></ListAllMyBucketsResult>")
	writeXML(w, 200, sb.String())
}

// ── bodies ───────────────────────────────────────────────────────────────

// readBody returns the payload, de-framing aws-chunked bodies (SigV4 streaming and the
// SDKs' default flexible-checksum trailers over plain HTTP).
func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") &&
		!strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return b, nil
	}
	var out bytes.Buffer
	rest := b
	for {
		nl := bytes.Index(rest, []byte("\r\n"))
		if nl < 0 {
			break
		}
		head, _, _ := strings.Cut(string(rest[:nl]), ";")
		n, err := strconv.ParseInt(strings.TrimSpace(head), 16, 64)
		if err != nil || n == 0 {
			break
		}
		rest = rest[nl+2:]
		if int64(len(rest)) < n {
			return nil, fmt.Errorf("aws-chunked body truncated")
		}
		out.Write(rest[:n])
		rest = bytes.TrimPrefix(rest[n:], []byte("\r\n"))
	}
	return out.Bytes(), nil
}

func metaFrom(h http.Header) objMeta {
	m := objMeta{ContentType: h.Get("Content-Type"), ContentEncoding: strings.TrimSpace(strings.ReplaceAll(h.Get("Content-Encoding"), "aws-chunked", "")),
		ContentDisposition: h.Get("Content-Disposition"), CacheControl: h.Get("Cache-Control"), ContentLanguage: h.Get("Content-Language"),
		Expires: h.Get("Expires"), StorageClass: h.Get("x-amz-storage-class"), SSE: h.Get("x-amz-server-side-encryption"),
		SSEKMSKeyID: h.Get("x-amz-server-side-encryption-aws-kms-key-id"), WebsiteRedirect: h.Get("x-amz-website-redirect-location"),
		User: map[string]string{}}
	if t := h.Get("x-amz-tagging"); t != "" {
		vals, _ := url.ParseQuery(t)
		m.Tags = map[string]string{}
		for k := range vals {
			m.Tags[k] = vals.Get(k)
		}
	}
	m.ContentEncoding = strings.Trim(m.ContentEncoding, ", ")
	if m.ContentType == "" {
		m.ContentType = "binary/octet-stream"
	}
	for k, v := range h {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-meta-") && len(v) > 0 {
			m.User[strings.TrimPrefix(lk, "x-amz-meta-")] = v[0]
		}
	}
	return m
}

func newVersionID() string { return base64.RawURLEncoding.EncodeToString([]byte(randHex(12)))[:32] }

// store writes a new object version (or replaces the "null" version when the bucket is
// not versioned) and returns it.
func (a *App) storeObject(b Bucket, key string, body []byte, meta objMeta, etag string) (s3Object, error) {
	vid := "null"
	if b.Versioning == "Enabled" {
		vid = newVersionID()
	}
	if etag == "" {
		sum := md5.Sum(body)
		etag = hex.EncodeToString(sum[:])
	}
	o := s3Object{Bucket: b.Name, Key: key, VersionID: vid, ETag: etag, Seq: a.db.next("s3seq"), Size: int64(len(body)),
		Modified: nowMs(), Meta: meta, Blob: randHex(16)}
	if err := os.WriteFile(a.db.blobPath(o.Blob), body, 0o644); err != nil {
		return o, err
	}
	if vid == "null" {
		if old, ok := a.s3Version(b.Name, key, "null"); ok && old.Blob != "" {
			_ = os.Remove(a.db.blobPath(old.Blob))
		}
	}
	err := a.db.exec(`INSERT INTO s3_objects(bucket,key,version_id,seq,size,etag,modified_ms,delete_marker,blob,meta) VALUES(?,?,?,?,?,?,?,0,?,?)
		ON CONFLICT(bucket,key,version_id) DO UPDATE SET seq=excluded.seq,size=excluded.size,etag=excluded.etag,
		modified_ms=excluded.modified_ms,delete_marker=0,blob=excluded.blob,meta=excluded.meta`,
		o.Bucket, o.Key, o.VersionID, o.Seq, o.Size, o.ETag, o.Modified, o.Blob, jsonStr(o.Meta))
	return o, err
}

func (a *App) s3Put(w http.ResponseWriter, r *http.Request, b Bucket, key string) {
	body, err := readBody(r)
	if err != nil {
		s3Err(w, 400, "IncompleteBody", err.Error(), "/"+b.Name+"/"+key)
		return
	}
	// conditional writes (the S3 backend's lock file relies on If-None-Match: *)
	if cur, ok := a.s3Latest(b.Name, key); ok && !cur.DeleteMarker {
		if r.Header.Get("If-None-Match") == "*" {
			s3Err(w, 412, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold", "/"+b.Name+"/"+key)
			return
		}
		if im := r.Header.Get("If-Match"); im != "" && strings.Trim(im, `"`) != cur.ETag {
			s3Err(w, 412, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold", "/"+b.Name+"/"+key)
			return
		}
	} else if r.Header.Get("If-Match") != "" {
		s3Err(w, 404, "NoSuchKey", "The specified key does not exist.", "/"+b.Name+"/"+key)
		return
	}
	if want := r.Header.Get("Content-MD5"); want != "" {
		sum := md5.Sum(body)
		if base64.StdEncoding.EncodeToString(sum[:]) != want {
			s3Err(w, 400, "BadDigest", "The Content-MD5 you specified did not match what we received.", "/"+b.Name+"/"+key)
			return
		}
	}
	o, err := a.storeObject(b, key, body, metaFrom(r.Header), "")
	if err != nil {
		s3Err(w, 500, "InternalError", err.Error(), "/"+b.Name+"/"+key)
		return
	}
	w.Header().Set("ETag", `"`+o.ETag+`"`)
	if b.Versioning != "" {
		w.Header().Set("x-amz-version-id", o.VersionID)
	}
	w.WriteHeader(200)
	a.logf("s3 put s3://%s/%s (%d B, version %s)", b.Name, key, o.Size, o.VersionID)
	a.s3Event(b, "Object Created", "PutObject", o)
}

func (a *App) s3Copy(w http.ResponseWriter, r *http.Request, b Bucket, key string) {
	src, _ := url.PathUnescape(r.Header.Get("x-amz-copy-source"))
	src = strings.TrimPrefix(src, "/")
	srcVersion := ""
	if i := strings.Index(src, "?versionId="); i >= 0 {
		src, srcVersion = src[:i], src[i+len("?versionId="):]
	}
	sb, sk, _ := strings.Cut(src, "/")
	var o *s3Object
	if srcVersion != "" {
		if v, ok := a.s3Version(sb, sk, srcVersion); ok {
			o = &v
		}
	} else if v, ok := a.s3Latest(sb, sk); ok && !v.DeleteMarker {
		o = &v
	}
	if o == nil {
		s3Err(w, 404, "NoSuchKey", "The specified key does not exist.", "/"+src)
		return
	}
	body, err := os.ReadFile(a.db.blobPath(o.Blob))
	if err != nil {
		s3Err(w, 500, "InternalError", err.Error(), "/"+src)
		return
	}
	meta := o.Meta
	if r.Header.Get("x-amz-metadata-directive") == "REPLACE" {
		meta = metaFrom(r.Header)
	}
	n, err := a.storeObject(b, key, body, meta, o.ETag)
	if err != nil {
		s3Err(w, 500, "InternalError", err.Error(), "/"+b.Name+"/"+key)
		return
	}
	if b.Versioning != "" {
		w.Header().Set("x-amz-version-id", n.VersionID)
	}
	writeXML(w, 200, `<CopyObjectResult `+s3ns+`><LastModified>`+isoMs(n.Modified)+`</LastModified><ETag>&quot;`+n.ETag+`&quot;</ETag></CopyObjectResult>`)
	a.s3Event(b, "Object Created", "CopyObject", n)
}

// ── reads ────────────────────────────────────────────────────────────────
const objCols = `bucket,key,version_id,seq,size,etag,modified_ms,delete_marker,blob,meta`

type scanner interface{ Scan(...any) error }

func scanObj(r scanner) (s3Object, error) {
	var o s3Object
	var dm int
	var meta string
	err := r.Scan(&o.Bucket, &o.Key, &o.VersionID, &o.Seq, &o.Size, &o.ETag, &o.Modified, &dm, &o.Blob, &meta)
	o.DeleteMarker = dm == 1
	_ = jsonUnmarshal(meta, &o.Meta)
	return o, err
}

func (a *App) s3Latest(bucket, key string) (s3Object, bool) {
	o, err := scanObj(a.db.db.QueryRow(`SELECT `+objCols+` FROM s3_objects WHERE bucket=? AND key=? ORDER BY seq DESC LIMIT 1`, bucket, key))
	return o, err == nil
}

func (a *App) s3Version(bucket, key, vid string) (s3Object, bool) {
	o, err := scanObj(a.db.db.QueryRow(`SELECT `+objCols+` FROM s3_objects WHERE bucket=? AND key=? AND version_id=?`, bucket, key, vid))
	return o, err == nil
}

func (a *App) s3Get(w http.ResponseWriter, r *http.Request, b Bucket, key, vid string) {
	res := "/" + b.Name + "/" + key
	var o s3Object
	var ok bool
	if vid != "" {
		if o, ok = a.s3Version(b.Name, key, vid); !ok {
			s3Err(w, 404, "NoSuchVersion", "The specified version does not exist.", res)
			return
		}
		if o.DeleteMarker {
			w.Header().Set("x-amz-delete-marker", "true")
			s3Err(w, 405, "MethodNotAllowed", "The specified method is not allowed against this resource.", res)
			return
		}
	} else if o, ok = a.s3Latest(b.Name, key); !ok || o.DeleteMarker {
		if ok {
			w.Header().Set("x-amz-delete-marker", "true")
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(404)
			return
		}
		s3Err(w, 404, "NoSuchKey", "The specified key does not exist.", res)
		return
	}
	etag := `"` + o.ETag + `"`
	if inm := r.Header.Get("If-None-Match"); inm != "" && (inm == etag || inm == "*") {
		w.WriteHeader(304)
		return
	}
	if im := r.Header.Get("If-Match"); im != "" && im != etag && im != "*" {
		s3Err(w, 412, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold", res)
		return
	}
	h := w.Header()
	h.Set("ETag", etag)
	if b.Versioning != "" {
		h.Set("x-amz-version-id", o.VersionID)
	}
	h.Set("Content-Type", o.Meta.ContentType)
	sse := o.Meta.SSE
	if sse == "" {
		sse = "AES256" // S3 encrypts every object at rest by default
	}
	if len(o.Meta.Tags) > 0 {
		h.Set("x-amz-tagging-count", strconv.Itoa(len(o.Meta.Tags)))
	}
	if o.Meta.StorageClass != "" && o.Meta.StorageClass != "STANDARD" {
		h.Set("x-amz-storage-class", o.Meta.StorageClass)
	}
	for k, v := range map[string]string{"Content-Encoding": o.Meta.ContentEncoding, "Content-Disposition": o.Meta.ContentDisposition,
		"Cache-Control": o.Meta.CacheControl, "Content-Language": o.Meta.ContentLanguage, "Expires": o.Meta.Expires,
		"x-amz-server-side-encryption": sse, "x-amz-server-side-encryption-aws-kms-key-id": o.Meta.SSEKMSKeyID,
		"x-amz-website-redirect-location": o.Meta.WebsiteRedirect} {
		if v != "" {
			h.Set(k, v)
		}
	}
	for k, v := range o.Meta.User {
		h.Set("x-amz-meta-"+k, v)
	}
	if o.Meta.Parts > 0 {
		h.Set("x-amz-mp-parts-count", strconv.Itoa(o.Meta.Parts))
	}
	h.Set("Last-Modified", time.UnixMilli(o.Modified).UTC().Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")
	start, end, status := int64(0), o.Size-1, 200
	if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") && o.Size > 0 {
		from, to, _ := strings.Cut(strings.TrimPrefix(rg, "bytes="), "-")
		if from == "" {
			if n, _ := strconv.ParseInt(to, 10, 64); n < o.Size {
				start = o.Size - n
			}
		} else {
			start, _ = strconv.ParseInt(from, 10, 64)
			if to != "" {
				end, _ = strconv.ParseInt(to, 10, 64)
			}
		}
		if end >= o.Size {
			end = o.Size - 1
		}
		if start > end {
			h.Set("Content-Range", "bytes */"+strconv.FormatInt(o.Size, 10))
			s3Err(w, 416, "InvalidRange", "The requested range is not satisfiable", res)
			return
		}
		status = 206
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, o.Size))
	}
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	f, err := os.Open(a.db.blobPath(o.Blob))
	if err != nil {
		s3Err(w, 500, "InternalError", err.Error(), res)
		return
	}
	defer f.Close()
	w.WriteHeader(status)
	_, _ = io.Copy(w, io.NewSectionReader(f, start, end-start+1))
}

// ── deletes ──────────────────────────────────────────────────────────────
type delResult struct {
	VersionID    string
	DeleteMarker bool
	MarkerID     string
}

func (a *App) deleteObject(b Bucket, key, vid string) delResult {
	if vid != "" {
		if o, ok := a.s3Version(b.Name, key, vid); ok {
			_ = a.db.exec(`DELETE FROM s3_objects WHERE bucket=? AND key=? AND version_id=?`, b.Name, key, vid)
			if o.Blob != "" {
				_ = os.Remove(a.db.blobPath(o.Blob))
			}
			a.s3Event(b, "Object Deleted", "DeleteObject", o)
			return delResult{VersionID: vid, DeleteMarker: o.DeleteMarker}
		}
		return delResult{VersionID: vid}
	}
	if b.Versioning == "" {
		if o, ok := a.s3Version(b.Name, key, "null"); ok {
			_ = a.db.exec(`DELETE FROM s3_objects WHERE bucket=? AND key=? AND version_id='null'`, b.Name, key)
			if o.Blob != "" {
				_ = os.Remove(a.db.blobPath(o.Blob))
			}
			a.s3Event(b, "Object Deleted", "DeleteObject", o)
		}
		return delResult{}
	}
	mid := "null"
	if b.Versioning == "Enabled" {
		mid = newVersionID()
	}
	m := s3Object{Bucket: b.Name, Key: key, VersionID: mid, Seq: a.db.next("s3seq"), Modified: nowMs(), DeleteMarker: true}
	_ = a.db.exec(`INSERT INTO s3_objects(bucket,key,version_id,seq,size,etag,modified_ms,delete_marker,blob,meta) VALUES(?,?,?,?,0,'',?,1,'','{}')
		ON CONFLICT(bucket,key,version_id) DO UPDATE SET seq=excluded.seq, delete_marker=1, blob='', size=0`, b.Name, key, mid, m.Seq, m.Modified)
	a.s3Event(b, "Object Deleted", "DeleteObject", m)
	return delResult{DeleteMarker: true, MarkerID: mid}
}

func (a *App) s3Delete(w http.ResponseWriter, b Bucket, key, vid string, _ bool) {
	res := a.deleteObject(b, key, vid)
	if res.DeleteMarker {
		w.Header().Set("x-amz-delete-marker", "true")
	}
	if res.MarkerID != "" {
		w.Header().Set("x-amz-version-id", res.MarkerID)
	} else if res.VersionID != "" {
		w.Header().Set("x-amz-version-id", res.VersionID)
	}
	w.WriteHeader(204)
}

func (a *App) s3DeleteObjects(w http.ResponseWriter, r *http.Request, b Bucket) {
	body, _ := readBody(r)
	var req struct {
		Quiet  bool
		Object []struct{ Key, VersionId string }
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		s3Err(w, 400, "MalformedXML", err.Error(), "/"+b.Name)
		return
	}
	var sb strings.Builder
	sb.WriteString("<DeleteResult " + s3ns + ">")
	for _, o := range req.Object {
		res := a.deleteObject(b, o.Key, o.VersionId)
		if req.Quiet {
			continue
		}
		sb.WriteString("<Deleted><Key>" + xe(o.Key) + "</Key>")
		if o.VersionId != "" {
			sb.WriteString("<VersionId>" + xe(o.VersionId) + "</VersionId>")
		}
		if res.DeleteMarker {
			sb.WriteString("<DeleteMarker>true</DeleteMarker>")
			if res.MarkerID != "" {
				sb.WriteString("<DeleteMarkerVersionId>" + res.MarkerID + "</DeleteMarkerVersionId>")
			}
		}
		sb.WriteString("</Deleted>")
	}
	sb.WriteString("</DeleteResult>")
	writeXML(w, 200, sb.String())
}

// ── listings ─────────────────────────────────────────────────────────────
func likePrefix(p string) string {
	p = strings.ReplaceAll(p, `\`, `\\`)
	p = strings.ReplaceAll(p, "%", `\%`)
	p = strings.ReplaceAll(p, "_", `\_`)
	return p + "%"
}

// s3Objects: every version (all=true), or the live latest version of each key.
func (a *App) s3Objects(bucket, prefix string, all bool) []s3Object {
	rows, err := a.db.db.Query(`SELECT `+objCols+` FROM s3_objects WHERE bucket=? AND key LIKE ? ESCAPE '\' ORDER BY key, seq DESC`,
		bucket, likePrefix(prefix))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []s3Object
	last := "\x00"
	for rows.Next() {
		o, err := scanObj(rows)
		if err != nil {
			continue
		}
		if all {
			out = append(out, o)
			continue
		}
		if o.Key == last {
			continue
		}
		last = o.Key
		if !o.DeleteMarker {
			out = append(out, o)
		}
	}
	return out
}

func (a *App) s3List(w http.ResponseWriter, b Bucket, q url.Values) {
	v2 := q.Get("list-type") == "2"
	prefix, delim := q.Get("prefix"), q.Get("delimiter")
	maxKeys := 1000
	if q.Has("max-keys") {
		maxKeys = atoi(q.Get("max-keys"))
	}
	after := q.Get("marker")
	if v2 {
		after = q.Get("start-after")
		if t := q.Get("continuation-token"); t != "" {
			if d, err := base64.RawURLEncoding.DecodeString(t); err == nil {
				after = string(d)
			}
		}
	}
	var keys []s3Object
	var prefixes []string
	seen := map[string]bool{}
	truncated, last := false, ""
	for _, o := range a.s3Objects(b.Name, prefix, false) {
		if after != "" && o.Key <= after {
			continue
		}
		entry := o.Key
		isPrefix := false
		if delim != "" {
			if i := strings.Index(o.Key[len(prefix):], delim); i >= 0 {
				entry, isPrefix = o.Key[:len(prefix)+i+len(delim)], true
				if seen[entry] {
					continue
				}
			}
		}
		if len(keys)+len(prefixes) >= maxKeys {
			truncated = true
			break
		}
		if isPrefix {
			seen[entry] = true
			prefixes = append(prefixes, entry)
		} else {
			keys = append(keys, o)
		}
		last = entry
	}
	var sb strings.Builder
	sb.WriteString("<ListBucketResult " + s3ns + "><Name>" + xe(b.Name) + "</Name><Prefix>" + xe(prefix) + "</Prefix><MaxKeys>" +
		strconv.Itoa(maxKeys) + "</MaxKeys><IsTruncated>" + strconv.FormatBool(truncated) + "</IsTruncated>")
	if delim != "" {
		sb.WriteString("<Delimiter>" + xe(delim) + "</Delimiter>")
	}
	if v2 {
		sb.WriteString("<KeyCount>" + strconv.Itoa(len(keys)+len(prefixes)) + "</KeyCount>")
		if t := q.Get("continuation-token"); t != "" {
			sb.WriteString("<ContinuationToken>" + xe(t) + "</ContinuationToken>")
		}
		if sa := q.Get("start-after"); sa != "" {
			sb.WriteString("<StartAfter>" + xe(sa) + "</StartAfter>")
		}
		if truncated {
			sb.WriteString("<NextContinuationToken>" + base64.RawURLEncoding.EncodeToString([]byte(last)) + "</NextContinuationToken>")
		}
	} else {
		sb.WriteString("<Marker>" + xe(q.Get("marker")) + "</Marker>")
		if truncated && delim != "" {
			sb.WriteString("<NextMarker>" + xe(last) + "</NextMarker>")
		}
	}
	for _, o := range keys {
		owner := ""
		if !v2 || q.Get("fetch-owner") == "true" {
			owner = "<Owner><ID>" + a.cfg.Account + "</ID><DisplayName>localaws</DisplayName></Owner>"
		}
		sb.WriteString("<Contents><Key>" + xe(o.Key) + "</Key><LastModified>" + isoMs(o.Modified) + `</LastModified><ETag>&quot;` + o.ETag +
			`&quot;</ETag><Size>` + strconv.FormatInt(o.Size, 10) + "</Size>" + owner + "<StorageClass>STANDARD</StorageClass></Contents>")
	}
	for _, p := range prefixes {
		sb.WriteString("<CommonPrefixes><Prefix>" + xe(p) + "</Prefix></CommonPrefixes>")
	}
	sb.WriteString("</ListBucketResult>")
	writeXML(w, 200, sb.String())
}

func (a *App) s3ListVersions(w http.ResponseWriter, b Bucket, q url.Values) {
	prefix := q.Get("prefix")
	var sb strings.Builder
	sb.WriteString("<ListVersionsResult " + s3ns + "><Name>" + xe(b.Name) + "</Name><Prefix>" + xe(prefix) +
		"</Prefix><KeyMarker></KeyMarker><VersionIdMarker></VersionIdMarker><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>")
	last := "\x00"
	for _, o := range a.s3Objects(b.Name, prefix, true) {
		latest := o.Key != last
		last = o.Key
		head := "<Key>" + xe(o.Key) + "</Key><VersionId>" + o.VersionID + "</VersionId><IsLatest>" + strconv.FormatBool(latest) +
			"</IsLatest><LastModified>" + isoMs(o.Modified) + "</LastModified>"
		owner := "<Owner><ID>" + a.cfg.Account + "</ID></Owner>"
		if o.DeleteMarker {
			sb.WriteString("<DeleteMarker>" + head + owner + "</DeleteMarker>")
		} else {
			sb.WriteString("<Version>" + head + `<ETag>&quot;` + o.ETag + `&quot;</ETag><Size>` + strconv.FormatInt(o.Size, 10) +
				"</Size>" + owner + "<StorageClass>STANDARD</StorageClass></Version>")
		}
	}
	sb.WriteString("</ListVersionsResult>")
	writeXML(w, 200, sb.String())
}

// ── S3 Control tagging (/v20180820/tags/<arn>) ──────────────────────────
// Terraform reads and writes bucket tags through S3 Control. For buckets they are the
// same tags GetBucketTagging / PutBucketTagging see; other ARNs use the common store.
// S3 Control prefixes the host with the account id (000000000000.localhost), which
// clients reach through localaws as an HTTP proxy (see README, Terraform).
func (a *App) bucketTags(b Bucket) map[string]string {
	out := map[string]string{}
	var t s3Tagging
	if xml.Unmarshal([]byte(b.Config["tagging"]), &t) == nil {
		for _, tg := range t.TagSet.Tag {
			out[tg.Key] = tg.Value
		}
	}
	return out
}

func (a *App) setBucketTags(b Bucket, tags map[string]string) {
	if b.Config == nil {
		b.Config = map[string]string{}
	}
	if len(tags) == 0 {
		delete(b.Config, "tagging")
	} else {
		var sb strings.Builder
		sb.WriteString("<Tagging " + s3ns + "><TagSet>")
		for _, k := range sortedKeys(tags) {
			sb.WriteString("<Tag><Key>" + xe(k) + "</Key><Value>" + xe(tags[k]) + "</Value></Tag>")
		}
		sb.WriteString("</TagSet></Tagging>")
		b.Config["tagging"] = sb.String()
	}
	_ = a.db.put("bucket", b.Name, b)
}

func (a *App) s3Control(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v20180820/")
	if !strings.HasPrefix(rest, "tags/") {
		s3Err(w, 501, "NotImplemented", "localaws s3control: "+r.Method+" /v20180820/"+rest+" is not implemented", "")
		return
	}
	arn := strings.TrimPrefix(rest, "tags/")
	var b Bucket
	isBucket := strings.HasPrefix(arn, "arn:aws:s3:::")
	if isBucket && !a.db.get("bucket", strings.TrimPrefix(arn, "arn:aws:s3:::"), &b) {
		s3Err(w, 404, "NoSuchBucket", "The specified bucket does not exist", arn)
		return
	}
	tags := a.tagsOf(arn)
	if isBucket {
		tags = a.bucketTags(b)
	}
	switch r.Method {
	case http.MethodGet:
		var sb strings.Builder
		for _, k := range sortedKeys(tags) {
			sb.WriteString("<Tag><Key>" + xe(k) + "</Key><Value>" + xe(tags[k]) + "</Value></Tag>")
		}
		writeXML(w, 200, `<ListTagsForResourceResult xmlns="http://awss3control.amazonaws.com/doc/2018-08-20/"><Tags>`+sb.String()+"</Tags></ListTagsForResourceResult>")
		return
	case http.MethodPost:
		body, _ := readBody(r)
		var req struct {
			Tags struct{ Tag []struct{ Key, Value string } }
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			s3Err(w, 400, "MalformedXML", err.Error(), arn)
			return
		}
		for _, t := range req.Tags.Tag {
			tags[t.Key] = t.Value
		}
	case http.MethodDelete:
		for _, k := range r.URL.Query()["tagKeys"] {
			delete(tags, k)
		}
	}
	if isBucket {
		a.setBucketTags(b, tags)
	} else {
		_ = a.db.put("tags", arn, tags)
	}
	w.WriteHeader(map[string]int{http.MethodPost: 200, http.MethodDelete: 204}[r.Method])
}

// ── object tagging ───────────────────────────────────────────────────────
type s3Tagging struct {
	TagSet struct {
		Tag []struct{ Key, Value string }
	}
}

func (a *App) s3ObjectTagging(w http.ResponseWriter, r *http.Request, b Bucket, key, vid string) {
	res := "/" + b.Name + "/" + key
	var o s3Object
	var ok bool
	if vid != "" {
		o, ok = a.s3Version(b.Name, key, vid)
	} else {
		o, ok = a.s3Latest(b.Name, key)
	}
	if !ok || o.DeleteMarker {
		s3Err(w, 404, "NoSuchKey", "The specified key does not exist.", res)
		return
	}
	if vid != "" || b.Versioning != "" {
		w.Header().Set("x-amz-version-id", o.VersionID)
	}
	switch r.Method {
	case http.MethodGet:
		var sb strings.Builder
		sb.WriteString("<Tagging " + s3ns + "><TagSet>")
		for _, k := range sortedKeys(o.Meta.Tags) {
			sb.WriteString("<Tag><Key>" + xe(k) + "</Key><Value>" + xe(o.Meta.Tags[k]) + "</Value></Tag>")
		}
		sb.WriteString("</TagSet></Tagging>")
		writeXML(w, 200, sb.String())
		return
	case http.MethodPut:
		body, _ := readBody(r)
		var t s3Tagging
		if err := xml.Unmarshal(body, &t); err != nil {
			s3Err(w, 400, "MalformedXML", err.Error(), res)
			return
		}
		o.Meta.Tags = map[string]string{}
		for _, tg := range t.TagSet.Tag {
			o.Meta.Tags[tg.Key] = tg.Value
		}
	case http.MethodDelete:
		o.Meta.Tags = nil
	}
	_ = a.db.exec(`UPDATE s3_objects SET meta=? WHERE bucket=? AND key=? AND version_id=?`, jsonStr(o.Meta), b.Name, key, o.VersionID)
	w.WriteHeader(map[string]int{http.MethodPut: 200, http.MethodDelete: 204}[r.Method])
}

// ── multipart ────────────────────────────────────────────────────────────
func (a *App) s3CreateUpload(w http.ResponseWriter, r *http.Request, b Bucket, key string) {
	u := upload{ID: base64.RawURLEncoding.EncodeToString([]byte(randHex(24))), Bucket: b.Name, Key: key, Started: nowMs(), Meta: metaFrom(r.Header)}
	_ = a.db.put("s3upload", u.ID, u)
	writeXML(w, 200, "<InitiateMultipartUploadResult "+s3ns+"><Bucket>"+xe(b.Name)+"</Bucket><Key>"+xe(key)+"</Key><UploadId>"+u.ID+"</UploadId></InitiateMultipartUploadResult>")
}

func (a *App) s3UploadPart(w http.ResponseWriter, r *http.Request, id string, part int) {
	if !a.db.has("s3upload", id) {
		s3Err(w, 404, "NoSuchUpload", "The specified upload does not exist.", id)
		return
	}
	if part < 1 || part > 10000 {
		s3Err(w, 400, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive", id)
		return
	}
	body, err := readBody(r)
	if err != nil {
		s3Err(w, 400, "IncompleteBody", err.Error(), id)
		return
	}
	sum := md5.Sum(body)
	etag := hex.EncodeToString(sum[:])
	blob := randHex(16)
	if err := os.WriteFile(a.db.blobPath(blob), body, 0o644); err != nil {
		s3Err(w, 500, "InternalError", err.Error(), id)
		return
	}
	_ = a.db.exec(`INSERT INTO s3_parts(upload_id, part, size, etag, blob) VALUES(?,?,?,?,?)
		ON CONFLICT(upload_id, part) DO UPDATE SET size=excluded.size, etag=excluded.etag, blob=excluded.blob`, id, part, len(body), etag, blob)
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(200)
}

type part struct {
	N          int
	Size       int64
	ETag, Blob string
}

func (a *App) parts(id string) []part {
	rows, err := a.db.db.Query(`SELECT part, size, etag, blob FROM s3_parts WHERE upload_id=? ORDER BY part`, id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []part
	for rows.Next() {
		var p part
		_ = rows.Scan(&p.N, &p.Size, &p.ETag, &p.Blob)
		out = append(out, p)
	}
	return out
}

func (a *App) dropUpload(id string) {
	for _, p := range a.parts(id) {
		_ = os.Remove(a.db.blobPath(p.Blob))
	}
	_ = a.db.exec(`DELETE FROM s3_parts WHERE upload_id=?`, id)
	_ = a.db.del("s3upload", id)
}

func (a *App) s3CompleteUpload(w http.ResponseWriter, r *http.Request, b Bucket, key, id string) {
	var u upload
	if !a.db.get("s3upload", id, &u) {
		s3Err(w, 404, "NoSuchUpload", "The specified upload does not exist.", id)
		return
	}
	body, _ := readBody(r)
	var req struct {
		Part []struct {
			PartNumber int
			ETag       string
		}
	}
	_ = xml.Unmarshal(body, &req)
	have := map[int]part{}
	for _, p := range a.parts(id) {
		have[p.N] = p
	}
	var data bytes.Buffer
	var md5s []byte
	prev := 0
	for i, p := range req.Part {
		got, ok := have[p.PartNumber]
		if !ok || strings.Trim(p.ETag, `"`) != got.ETag {
			s3Err(w, 400, "InvalidPart", "One or more of the specified parts could not be found.", id)
			return
		}
		if p.PartNumber <= prev {
			s3Err(w, 400, "InvalidPartOrder", "The list of parts was not in ascending order.", id)
			return
		}
		if i < len(req.Part)-1 && got.Size < 5<<20 {
			s3Err(w, 400, "EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size.", id)
			return
		}
		prev = p.PartNumber
		pb, err := os.ReadFile(a.db.blobPath(got.Blob))
		if err != nil {
			s3Err(w, 500, "InternalError", err.Error(), id)
			return
		}
		data.Write(pb)
		raw, _ := hex.DecodeString(got.ETag)
		md5s = append(md5s, raw...)
	}
	sum := md5.Sum(md5s)
	u.Meta.Parts = len(req.Part)
	o, err := a.storeObject(b, key, data.Bytes(), u.Meta, fmt.Sprintf("%s-%d", hex.EncodeToString(sum[:]), len(req.Part)))
	if err != nil {
		s3Err(w, 500, "InternalError", err.Error(), id)
		return
	}
	a.dropUpload(id)
	if b.Versioning != "" {
		w.Header().Set("x-amz-version-id", o.VersionID)
	}
	writeXML(w, 200, "<CompleteMultipartUploadResult "+s3ns+"><Location>/"+xe(b.Name)+"/"+xe(key)+"</Location><Bucket>"+xe(b.Name)+
		"</Bucket><Key>"+xe(key)+`</Key><ETag>&quot;`+o.ETag+`&quot;</ETag></CompleteMultipartUploadResult>`)
	a.logf("s3 multipart put s3://%s/%s (%d parts, %d B)", b.Name, key, len(req.Part), o.Size)
	a.s3Event(b, "Object Created", "CompleteMultipartUpload", o)
}

func (a *App) s3AbortUpload(w http.ResponseWriter, id string) {
	if !a.db.has("s3upload", id) {
		s3Err(w, 404, "NoSuchUpload", "The specified upload does not exist.", id)
		return
	}
	a.dropUpload(id)
	w.WriteHeader(204)
}

func (a *App) s3ListParts(w http.ResponseWriter, b Bucket, key, id string) {
	var sb strings.Builder
	sb.WriteString("<ListPartsResult " + s3ns + "><Bucket>" + xe(b.Name) + "</Bucket><Key>" + xe(key) + "</Key><UploadId>" + id + "</UploadId><IsTruncated>false</IsTruncated>")
	for _, p := range a.parts(id) {
		sb.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>&quot;%s&quot;</ETag><Size>%d</Size></Part>`, p.N, p.ETag, p.Size))
	}
	sb.WriteString("</ListPartsResult>")
	writeXML(w, 200, sb.String())
}

func (a *App) s3ListUploads(w http.ResponseWriter, b Bucket) {
	var sb strings.Builder
	sb.WriteString("<ListMultipartUploadsResult " + s3ns + "><Bucket>" + xe(b.Name) + "</Bucket><IsTruncated>false</IsTruncated>")
	for _, u := range list[upload](a.db, "s3upload") {
		if u.Bucket == b.Name {
			sb.WriteString("<Upload><Key>" + xe(u.Key) + "</Key><UploadId>" + u.ID + "</UploadId><Initiated>" + isoMs(u.Started) + "</Initiated></Upload>")
		}
	}
	sb.WriteString("</ListMultipartUploadsResult>")
	writeXML(w, 200, sb.String())
}

// ── S3 → EventBridge ─────────────────────────────────────────────────────
func (a *App) s3Event(b Bucket, detailType, reason string, o s3Object) {
	if !b.EventBridge {
		return
	}
	obj := M{"key": o.Key, "sequencer": fmt.Sprintf("%016X", o.Seq)}
	if detailType == "Object Created" {
		obj["size"], obj["etag"] = o.Size, o.ETag
	}
	if o.VersionID != "null" {
		obj["version-id"] = o.VersionID
	}
	detail := M{"version": "0", "bucket": M{"name": b.Name}, "object": obj, "request-id": strings.ToUpper(randHex(8)),
		"requester": a.cfg.Account, "source-ip-address": "127.0.0.1", "reason": reason}
	if detailType == "Object Deleted" {
		if o.DeleteMarker {
			detail["deletion-type"] = "Delete Marker Created"
		} else {
			detail["deletion-type"] = "Permanently Deleted"
		}
	}
	a.publish(Event{Source: "aws.s3", DetailType: detailType, Detail: detail, Resources: []string{"arn:aws:s3:::" + b.Name},
		Time: o.Modified})
}

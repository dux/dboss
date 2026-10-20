package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"

	"dboss/internal/httpx"
)

const (
	// Bodies shorter than this gain too little to pay for the encoder; it is only checked when
	// the response carries a Content-Length, a streamed body is always encoded.
	minCompressSize = 1024
	// Brotli 4 still beats gzip on size at a speed fit for bodies built per request.
	brotliLevel = 4
	gzipLevel   = 5
)

// Types worth encoding beyond text/* and the +json/+xml suffixes. Images, archives and woff fonts
// are compressed already.
var compressibleTypes = map[string]bool{
	"application/javascript":        true,
	"application/x-javascript":      true,
	"application/json":              true,
	"application/xml":               true,
	"application/wasm":              true,
	"application/vnd.ms-fontobject": true,
	"application/x-font-ttf":        true,
	"application/x-font-otf":        true,
	"font/ttf":                      true,
	"font/otf":                      true,
	"image/x-icon":                  true,
	"image/vnd.microsoft.icon":      true,
}

type encoder interface {
	io.WriteCloser
	Flush() error
	Reset(io.Writer)
}

var encoders = map[string]*sync.Pool{
	"br": {New: func() any { return brotli.NewWriterLevel(nil, brotliLevel) }},
	"gzip": {New: func() any {
		writer, _ := gzip.NewWriterLevel(nil, gzipLevel)
		return writer
	}},
}

// Compress encodes compressible responses with brotli or gzip, whichever the client prefers.
// A response that already has a Content-Encoding or says Cache-Control: no-transform passes
// through untouched, so an app opts out by encoding its own answers or sending that header. A
// request Cloudflare forwarded is answered plain.
func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoding := ""
		// Cloudflare compresses for the visitor itself, so its hop to the origin stays plain. A local
		// request carries no CF-Ray, so dev sessions still compress.
		if r.Method != http.MethodHead && !(r.Header.Get("CF-Ray") != "" && httpx.FromCloudflare(r.RemoteAddr)) {
			encoding = pickEncoding(r.Header.Get("Accept-Encoding"))
		}
		writer := &compressWriter{ResponseWriter: w, encoding: encoding}
		defer writer.close()
		next.ServeHTTP(writer, r)
	})
}

// compressWriter picks the encoding once, when the status is written, from the response headers
// the handler set by then.
type compressWriter struct {
	http.ResponseWriter
	encoding string
	encoder  encoder
	decided  bool
}

func (c *compressWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *compressWriter) WriteHeader(status int) {
	if !c.decided && status >= http.StatusOK {
		c.decided = true
		c.start(status)
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *compressWriter) Write(data []byte) (int, error) {
	if !c.decided {
		c.WriteHeader(http.StatusOK)
	}
	if c.encoder == nil {
		return c.ResponseWriter.Write(data)
	}
	return c.encoder.Write(data)
}

// Flush pushes what the encoder holds, so a streamed page still arrives as it is written.
func (c *compressWriter) Flush() {
	if !c.decided {
		c.WriteHeader(http.StatusOK)
	}
	if c.encoder != nil {
		_ = c.encoder.Flush()
	}
	if flusher, ok := c.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (c *compressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := c.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("hijacking is not supported")
	}
	return hijacker.Hijack()
}

func (c *compressWriter) start(status int) {
	header := c.Header()
	if header.Get("Content-Encoding") != "" || !compressible(header.Get("Content-Type")) {
		return
	}
	addVary(header)
	if c.encoding == "" || status == http.StatusNoContent || status == http.StatusPartialContent || status == http.StatusNotModified || noTransform(header) {
		return
	}
	if length, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64); err == nil && length < minCompressSize {
		return
	}
	header.Del("Content-Length")
	header.Del("Accept-Ranges")
	header.Set("Content-Encoding", c.encoding)
	if etag := header.Get("Etag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		header.Set("Etag", "W/"+etag)
	}
	c.encoder = encoders[c.encoding].Get().(encoder)
	c.encoder.Reset(c.ResponseWriter)
}

func (c *compressWriter) close() {
	if c.encoder == nil {
		return
	}
	_ = c.encoder.Close()
	c.encoder.Reset(nil)
	encoders[c.encoding].Put(c.encoder)
	c.encoder = nil
}

// pickEncoding returns "br" or "gzip", whichever Accept-Encoding weighs higher (brotli on a tie),
// or "" when the client takes neither. A q=0 refuses an encoding, and * covers unnamed ones.
func pickEncoding(accept string) string {
	weights := map[string]float64{}
	for _, part := range strings.Split(accept, ",") {
		name, params, _ := strings.Cut(part, ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		weight := 1.0
		if value, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(params)), "q="); ok {
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				weight = parsed
			}
		}
		weights[name] = weight
	}
	best, bestWeight := "", 0.0
	for _, name := range []string{"br", "gzip"} {
		weight, ok := weights[name]
		if !ok {
			weight, ok = weights["*"]
		}
		if ok && weight > bestWeight {
			best, bestWeight = name, weight
		}
	}
	return best
}

func compressible(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	switch {
	case mediaType == "text/event-stream":
		return false
	case strings.HasPrefix(mediaType, "text/"), strings.HasSuffix(mediaType, "+json"), strings.HasSuffix(mediaType, "+xml"):
		return true
	}
	return compressibleTypes[mediaType]
}

func noTransform(header http.Header) bool {
	for _, value := range header.Values("Cache-Control") {
		if strings.Contains(strings.ToLower(value), "no-transform") {
			return true
		}
	}
	return false
}

func addVary(header http.Header) {
	for _, value := range header.Values("Vary") {
		for _, field := range strings.Split(value, ",") {
			field = strings.TrimSpace(field)
			if field == "*" || strings.EqualFold(field, "Accept-Encoding") {
				return
			}
		}
	}
	header.Add("Vary", "Accept-Encoding")
}

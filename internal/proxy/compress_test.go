package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
)

var compressBody = strings.Repeat("<p>hello compressed world</p>\n", 200)

func compressServe(t *testing.T, method, accept string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://demo.test/", nil)
	if accept != "" {
		request.Header.Set("Accept-Encoding", accept)
	}
	recorder := httptest.NewRecorder()
	Compress(handler).ServeHTTP(recorder, request)
	return recorder
}

func htmlPage(header http.Header) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(compressBody)))
		w.Header().Set("Accept-Ranges", "bytes")
		for name, values := range header {
			w.Header()[name] = values
		}
		_, _ = io.WriteString(w, compressBody)
	}
}

func decode(t *testing.T, encoding string, body io.Reader) string {
	t.Helper()
	var reader io.Reader
	switch encoding {
	case "br":
		reader = brotli.NewReader(body)
	case "gzip":
		gz, err := gzip.NewReader(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = gz
	default:
		reader = body
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPickEncoding(t *testing.T) {
	cases := map[string]string{
		"":                        "",
		"identity":                "",
		"gzip, deflate, br, zstd": "br",
		"gzip":                    "gzip",
		"br;q=0, gzip":            "gzip",
		"gzip;q=1, br;q=0.5":      "gzip",
		"GZIP; Q=0.8, BR":         "br",
		"*":                       "br",
		"*;q=0.5, br;q=0":         "gzip",
		"gzip;q=0, br;q=0":        "",
	}
	for accept, want := range cases {
		if got := pickEncoding(accept); got != want {
			t.Errorf("pickEncoding(%q) = %q, want %q", accept, got, want)
		}
	}
}

func TestCompressEncodesBody(t *testing.T) {
	for _, encoding := range []string{"br", "gzip"} {
		response := compressServe(t, http.MethodGet, encoding, htmlPage(http.Header{"Etag": {`"v1"`}}))
		header := response.Header()
		if header.Get("Content-Encoding") != encoding || header.Get("Content-Length") != "" || header.Get("Accept-Ranges") != "" {
			t.Fatalf("%s headers = %v", encoding, header)
		}
		if header.Get("Vary") != "Accept-Encoding" || header.Get("Etag") != `W/"v1"` {
			t.Fatalf("%s vary/etag = %v", encoding, header)
		}
		if response.Body.Len() >= len(compressBody) {
			t.Fatalf("%s body not smaller: %d", encoding, response.Body.Len())
		}
		if got := decode(t, encoding, response.Body); got != compressBody {
			t.Fatalf("%s body did not round-trip", encoding)
		}
	}
}

func TestCompressLeavesResponseAlone(t *testing.T) {
	cases := map[string]struct {
		method string
		accept string
		status int
		header http.Header
		vary   bool
	}{
		"no accept":       {http.MethodGet, "", 200, nil, true},
		"head":            {http.MethodHead, "br", 200, nil, true},
		"app encoded":     {http.MethodGet, "br", 200, http.Header{"Content-Encoding": {"identity"}}, false},
		"no-transform":    {http.MethodGet, "br", 200, http.Header{"Cache-Control": {"public, no-transform"}}, true},
		"small":           {http.MethodGet, "br", 200, http.Header{"Content-Length": {"200"}}, true},
		"partial":         {http.MethodGet, "br", 206, nil, true},
		"image":           {http.MethodGet, "br", 200, http.Header{"Content-Type": {"image/png"}}, false},
		"event stream":    {http.MethodGet, "br", 200, http.Header{"Content-Type": {"text/event-stream"}}, false},
		"no content type": {http.MethodGet, "br", 200, http.Header{"Content-Type": nil}, false},
	}
	for name, tc := range cases {
		response := compressServe(t, tc.method, tc.accept, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			for key, values := range tc.header {
				if values == nil {
					w.Header().Del(key)
					continue
				}
				w.Header()[key] = values
			}
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, compressBody)
		})
		if encoding := response.Header().Get("Content-Encoding"); encoding == "br" {
			t.Errorf("%s: encoded", name)
		}
		if got := response.Header().Get("Vary") == "Accept-Encoding"; got != tc.vary {
			t.Errorf("%s: vary = %v, want %v", name, got, tc.vary)
		}
		if response.Body.String() != compressBody && tc.method != http.MethodHead {
			t.Errorf("%s: body changed", name)
		}
	}
}

func TestCompressSkipsCloudflare(t *testing.T) {
	cases := []struct {
		remote string
		ray    string
		want   string
	}{
		{"173.245.48.1:443", "8f1b2c3d4e5f6a7b-AMS", ""},
		{"127.0.0.1:5000", "8f1b2c3d4e5f6a7b-AMS", ""},
		{"203.0.113.9:443", "8f1b2c3d4e5f6a7b-AMS", "br"},
		{"127.0.0.1:5000", "", "br"},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodGet, "http://demo.test/", nil)
		request.RemoteAddr = tc.remote
		request.Header.Set("Accept-Encoding", "br, gzip")
		if tc.ray != "" {
			request.Header.Set("CF-Ray", tc.ray)
		}
		response := httptest.NewRecorder()
		Compress(htmlPage(nil)).ServeHTTP(response, request)
		if got := response.Header().Get("Content-Encoding"); got != tc.want {
			t.Errorf("%s ray=%q: encoding = %q, want %q", tc.remote, tc.ray, got, tc.want)
		}
		if response.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s ray=%q: vary missing", tc.remote, tc.ray)
		}
	}
}

func TestCompressKeepsExistingVary(t *testing.T) {
	response := compressServe(t, http.MethodGet, "gzip", htmlPage(http.Header{"Vary": {"Cookie, accept-encoding"}}))
	if values := response.Header().Values("Vary"); len(values) != 1 {
		t.Fatalf("vary = %v", values)
	}
}

func TestCompressStreamsOnFlush(t *testing.T) {
	server := httptest.NewServer(Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})))
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("headers = %v", response.Header)
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil || line != "first\n" {
		t.Fatalf("streamed line = %q, %v", line, err)
	}
}

func TestCompressPassesHijack(t *testing.T) {
	server := httptest.NewServer(Compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: test\r\nConnection: Upgrade\r\n\r\nraw")
		_ = buffered.Flush()
	})))
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: demo.test\r\nAccept-Encoding: br\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
	data, _ := io.ReadAll(conn)
	if !strings.HasPrefix(string(data), "HTTP/1.1 101") || !strings.HasSuffix(string(data), "raw") {
		t.Fatalf("hijacked answer = %q", data)
	}
}

package relay

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/influxdata/influxdb/models"
)

// HTTP is a relay for HTTP influxdb writes
type HTTP struct {
	addr string
	name string

	closing int64
	l       net.Listener

	backends []*httpBackend
}

const (
	DefaultHTTPTimeout      = 10 * time.Second
	DefaultMaxDelayInterval = 10 * time.Second
	DefaultBatchSizeKB      = 512

	KB = 1024
	MB = 1024 * KB
)

func NewHTTP(cfg HTTPConfig) (Relay, error) {
	h := new(HTTP)

	h.addr = cfg.Addr
	h.name = cfg.Name

	for i := range cfg.Outputs {
		b := &cfg.Outputs[i]

		if b.Name == "" {
			b.Name = b.Location
		}

		timeout := DefaultHTTPTimeout
		if b.Timeout != "" {
			t, err := time.ParseDuration(b.Timeout)
			if err != nil {
				return nil, fmt.Errorf("error parsing HTTP timeout %v", err)
			}
			timeout = t
		}

		client := new(http.Client)
		client.Timeout = timeout

		// If configured, create a retryBuffer per backend.
		// This way we serialize retries against each backend.
		var rb *retryBuffer
		if b.BufferSizeMB > 0 {
			max := DefaultMaxDelayInterval
			if b.MaxDelayInterval != "" {
				m, err := time.ParseDuration(b.MaxDelayInterval)
				if err != nil {
					return nil, fmt.Errorf("error parsing max retry time %v", err)
				}
				max = m
			}

			batch := DefaultBatchSizeKB * KB
			if b.MaxBatchKB > 0 {
				batch = b.MaxBatchKB * KB
			}

			rb = newRetryBuffer(b.BufferSizeMB*MB, batch, max)
		}

		backend := &httpBackend{
			name:        b.Name,
			location:    b.Location,
			retryBuffer: rb,
			client:      client,
		}

		if rb != nil {
			rb.post = backend.rawPost
		}

		h.backends = append(h.backends, backend)

	}

	return h, nil
}

func (h *HTTP) Name() string {
	if h.name == "" {
		return "http://" + h.addr
	}
	return h.name
}

func (h *HTTP) Run() error {
	l, err := net.Listen("tcp", h.addr)
	if err != nil {
		return err
	}
	h.l = l

	log.Printf("Starting HTTP relay %q on %v", h.Name(), h.addr)

	err = http.Serve(l, h)
	if atomic.LoadInt64(&h.closing) != 0 {
		return nil
	}
	return err
}

func (h *HTTP) Stop() error {
	atomic.StoreInt64(&h.closing, 1)
	return h.l.Close()
}

func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.URL.Path != "/write" {
		jsonError(w, http.StatusNotFound, "invalid write endpoint")
		return
	}

	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
		} else {
			jsonError(w, http.StatusMethodNotAllowed, "invalid write method")
		}
		return
	}

	// fail early if we're missing the database
	if r.URL.Query().Get("db") == "" {
		jsonError(w, http.StatusBadRequest, "missing parameter: db")
		return
	}

	var body = r.Body

	if r.Header.Get("Content-Encoding") == "gzip" {
		b, err := gzip.NewReader(r.Body)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "unable to decode gzip body")
		}
		defer b.Close()
		body = b
	}

	bodyBuf := getBuf()
	_, err := bodyBuf.ReadFrom(body)
	if err != nil {
		putBuf(bodyBuf)
		jsonError(w, http.StatusInternalServerError, "problem reading request body")
		return
	}

	precision := r.URL.Query().Get("precision")
	points, err := models.ParsePointsWithPrecision(bodyBuf.Bytes(), start, precision)
	if err != nil {
		putBuf(bodyBuf)
		jsonError(w, http.StatusBadRequest, "unable to parse points")
		return
	}

	outBuf := getBuf()
	for _, p := range points {
		if _, err = outBuf.WriteString(p.PrecisionString(precision)); err != nil {
			break
		}
		if err = outBuf.WriteByte('\n'); err != nil {
			break
		}
	}

	// done with the input points
	putBuf(bodyBuf)

	if err != nil {
		putBuf(outBuf)
		jsonError(w, http.StatusInternalServerError, "problem writing points")
		return
	}

	// normalize query string
	query := r.URL.Query().Encode()

	outBytes := outBuf.Bytes()

	var wg sync.WaitGroup
	wg.Add(len(h.backends))

	var responses = make(chan *responseData, len(h.backends))

	for _, b := range h.backends {
		b := b
		go func() {
			defer wg.Done()
			resp, err := b.post(outBytes, query)
			if err != nil {
				log.Printf("Problem posting to relay %q backend %q: %v", h.Name(), b.name, err)
				responses <- nil
			} else {
				if resp.StatusCode/100 == 5 {
					log.Printf("5xx response for relay %q backend %q: %v", h.Name(), b.name, resp.StatusCode)
				}
				responses <- resp
			}
		}()
	}

	go func() {
		wg.Wait()
		close(responses)
		putBuf(outBuf)
	}()

	var errResponse *responseData

	for resp := range responses {
		if resp == nil {
			continue
		}

		if resp.StatusCode/100 == 2 { // points written successfully
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if errResponse != nil {
			if errResponse.StatusCode/100 == 4 {
				continue
			}
		}

		// hold on to one of the responses to return back to the client
		errResponse = resp
	}

	// no successful writes
	if errResponse == nil {
		// failed to make any valid request... network error?
		jsonError(w, http.StatusServiceUnavailable, "unable to write points")
		return
	}

	errResponse.Write(w)
}

type responseData struct {
	ContentType     string
	ContentEncoding string
	StatusCode      int
	Body            []byte
}

func (rd *responseData) Write(w http.ResponseWriter) {
	if rd.ContentType != "" {
		w.Header().Set("Content-Type", rd.ContentType)
	}

	if rd.ContentEncoding != "" {
		w.Header().Set("Content-Encoding", rd.ContentEncoding)
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(rd.Body)))
	w.WriteHeader(rd.StatusCode)
	w.Write(rd.Body)
}

func jsonError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	data := fmt.Sprintf("{\"error\":%q}\n", message)
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.WriteHeader(code)
	w.Write([]byte(data))
}

type httpBackend struct {
	name        string
	location    string
	retryBuffer *retryBuffer
	client      *http.Client
}

var ErrBufferFull = errors.New("retry buffer full")

func (b *httpBackend) rawPost(buf []byte, query string) (*responseData, error) {
	req, err := http.NewRequest("POST", b.location, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}

	req.URL.RawQuery = query
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Content-Length", strconv.Itoa(len(buf)))

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if err = resp.Body.Close(); err != nil {
		return nil, err
	}

	return &responseData{
		ContentType:     resp.Header.Get("Conent-Type"),
		ContentEncoding: resp.Header.Get("Conent-Encoding"),
		StatusCode:      resp.StatusCode,
		Body:            data,
	}, nil
}

func (b *httpBackend) post(buf []byte, query string) (*responseData, error) {
	if b.retryBuffer == nil {
		return b.rawPost(buf, query)
	}

	return b.retryBuffer.Post(buf, query)
}

var bufPool = sync.Pool{New: func() interface{} { return new(bytes.Buffer) }}

func getBuf() *bytes.Buffer {
	if bb, ok := bufPool.Get().(*bytes.Buffer); ok {
		return bb
	}
	return new(bytes.Buffer)
}

func putBuf(b *bytes.Buffer) {
	b.Reset()
	bufPool.Put(b)
}

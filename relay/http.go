package relay

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/influxdata/influxdb/models"
)

// HTTP is a relay for HTTP influxdb writes
type HTTP struct {
	addr   string
	name   string
	schema string

	cert string
	rp   string

	closing int64
	l       net.Listener

	backends []*httpBackend
}

// httpBackend代表运行着的influxdb实例
type httpBackend struct {
	poster
	name string
}

type poster interface {
	post([]byte, string, string) (*responseData, error)
}

type responseData struct {
	ContentType     string
	ContentEncoding string
	StatusCode      int
	Body            []byte
}

type simplePoster struct {
	client   *http.Client
	location string
}

func (b *simplePoster) post(buf []byte, query string, auth string) (*responseData, error) {
	req, err := http.NewRequest("POST", b.location, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}

	req.URL.RawQuery = query
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Content-Length", strconv.Itoa(len(buf)))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}

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

const (
	DefaultHTTPTimeout      = 10 * time.Second
	DefaultMaxDelayInterval = 10 * time.Second
	DefaultBatchSizeKB      = 512

	KB = 1024
	MB = 1024 * KB
)

// Relay是接口类型,查看relay.go
func NewHTTP(cfg HTTPConfig) (Relay, error) {
	h := new(HTTP)

	h.addr = cfg.Addr
	h.name = cfg.Name

	h.cert = cfg.SSLCombinedPem
	h.rp = cfg.DefaultRetentionPolicy

	// good tasty
	h.schema = "http"
	if h.cert != "" {
		h.schema = "https"
	}

	// Outputs: influxdb实例.
	for i := range cfg.Outputs {
		backend, err := newHTTPBackend(&cfg.Outputs[i])
		if err != nil {
			return nil, err
		}

		h.backends = append(h.backends, backend)
	}

	return h, nil
}

func newHTTPBackend(cfg *HTTPOutputConfig) (*httpBackend, error) {
	if cfg.Name == "" {
		cfg.Name = cfg.Location
	}

	timeout := DefaultHTTPTimeout
	if cfg.Timeout != "" {
		t, err := time.ParseDuration(cfg.Timeout)
		if err != nil {
			return nil, fmt.Errorf("error parsing HTTP timeout '%v'", err)
		}
		timeout = t
	}

	var p poster = newSimplePoster(cfg.Location, timeout, cfg.SkipTLSVerification)

	// If configured, create a retryBuffer per backend.
	// This way we serialize retries against each backend.
	if cfg.BufferSizeMB > 0 {
		max := DefaultMaxDelayInterval
		if cfg.MaxDelayInterval != "" {
			m, err := time.ParseDuration(cfg.MaxDelayInterval)
			if err != nil {
				return nil, fmt.Errorf("error parsing max retry time %v", err)
			}
			max = m
		}

		batch := DefaultBatchSizeKB * KB
		if cfg.MaxBatchKB > 0 {
			batch = cfg.MaxBatchKB * KB
		}

		p = newRetryBuffer(cfg.BufferSizeMB*MB, batch, max, p)
	}
	// 如果配置了缓冲区间,这post带有重试机制
	return &httpBackend{
		poster: p,
		name:   cfg.Name,
	}, nil
}

func (h *HTTP) Name() string {
	if h.name == "" {
		return fmt.Sprintf("%s://%s", h.schema, h.addr)
	}
	return h.name
}

// 1. 启动监听
func (h *HTTP) Run() error {
	l, err := net.Listen("tcp", h.addr)
	if err != nil {
		return err
	}

	// support HTTPS
	if h.cert != "" {
		cert, err := tls.LoadX509KeyPair(h.cert, h.cert)
		if err != nil {
			return err
		}

		l = tls.NewListener(l, &tls.Config{
			Certificates: []tls.Certificate{cert},
		})
	}

	h.l = l

	log.Printf("Starting %s relay %q on %v", strings.ToUpper(h.schema), h.Name(), h.addr)

	// h实现了ServeHTTP接口
	err = http.Serve(l, h)
	// todo: what ?
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

	// 状态检查
	if r.URL.Path == "/ping" && (r.Method == "GET" || r.Method == "HEAD") {
		w.Header().Add("X-InfluxDB-Version", "relay")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.URL.Path != "/write" {
		jsonError(w, http.StatusNotFound, "invalid write endpoint")
		return
	}

	// 写入要求使用POST方法
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
		} else {
			jsonError(w, http.StatusMethodNotAllowed, "invalid write method")
		}
		return
	}

	queryParams := r.URL.Query()

	// fail early if we're missing the database
	// influxdb API要求参数db
	// 详情参考: https://docs.influxdata.com/influxdb/v1.2/guides/writing_data/
	if queryParams.Get("db") == "" {
		jsonError(w, http.StatusBadRequest, "missing parameter: db")
		return
	}

	// rp: retention_policy_name
	if queryParams.Get("rp") == "" && h.rp != "" {
		queryParams.Set("rp", h.rp)
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

	precision := queryParams.Get("precision")
	// points代表要写入influxdb的数据点
	// 写入前经过一轮精确度相关的处理
	points, err := models.ParsePointsWithPrecision(bodyBuf.Bytes(), start, precision)
	if err != nil {
		// 如果在这发生了错误要归还缓冲池
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
	// 归还bodyBuf.注意区分outBuf
	putBuf(bodyBuf)

	// err对应上面for循环中的err
	if err != nil {
		putBuf(outBuf)
		jsonError(w, http.StatusInternalServerError, "problem writing points")
		return
	}

	// normalize query string
	query := queryParams.Encode()

	outBytes := outBuf.Bytes()

	// check for authorization performed via the header
	authHeader := r.Header.Get("Authorization")

	var wg sync.WaitGroup
	wg.Add(len(h.backends))

	var responses = make(chan *responseData, len(h.backends))

	// 重点: 由relay向influxdb写入数据
	for _, b := range h.backends {
		// 使用下面这种写法的原因:
		// 1. Go语言中的for循环会迭代使用b
		// 2. 新开辟变量,将b付给新的那个变量,那个变量也叫做b,这样每次循环中使用到的b就不会指向同一内存
		// 3. 这样做的本质是避免在闭包中共享了外层函数的变量状态(b变量)
		// 4. 更"传统"的写法是为每个goroutine传入一个参数
		b := b

		go func() {
			defer wg.Done()
			// post运行时候有两种可能:
			// 1.带重试机制
			// 2.不带重试机制
			resp, err := b.post(outBytes, query, authHeader)
			if err != nil {
				log.Printf("Problem posting to relay %q backend %q: %v", h.Name(), b.name, err)
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
		switch resp.StatusCode / 100 {
		case 2:
			w.WriteHeader(http.StatusNoContent)
			return

		case 4:
			// user error
			resp.Write(w)
			return

		default:
			// hold on to one of the responses to return back to the client
			errResponse = resp
		}
	}

	// no successful writes
	if errResponse == nil {
		// failed to make any valid request...
		jsonError(w, http.StatusServiceUnavailable, "unable to write points")
		return
	}

	errResponse.Write(w)
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

func newSimplePoster(location string, timeout time.Duration, skipTLSVerification bool) *simplePoster {
	// Configure custom transport for http.Client
	// Used for support skip-tls-verification option
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: skipTLSVerification,
		},
	}

	return &simplePoster{
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
		location: location,
	}
}

var ErrBufferFull = errors.New("retry buffer full")

var bufPool = sync.Pool{New: func() interface{} { return new(bytes.Buffer) }}

// 返回字节缓冲池
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

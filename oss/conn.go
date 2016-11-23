package oss

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Conn oss conn
type Conn struct {
	config *Config
	url    *urlMaker
}

func (conn Conn) GetUrl(method, bucketName, objectName string, expire int) (string, string, string) {
	uri := conn.url.getURL(bucketName, objectName, "")
	resource := conn.url.getResource(bucketName, objectName, "")
	fmt.Fprintf(os.Stderr, "uri=%v\n", uri)
	fmt.Fprintf(os.Stderr, "resource=%v\n", resource)
	req := &http.Request{
		Method:     method,
		URL:        uri,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Host:       uri.Host,
	}
	date := time.Now().Unix()
	date = date + int64(expire)
	req.Header.Set(HTTPHeaderDate, fmt.Sprintf("%d", date))
	conn.signHeader(req, resource)
	signstr := req.Header.Get(HTTPHeaderAuthorization)
	fmt.Fprintf(os.Stderr, "signstr=%s\n", signstr)
	index := strings.Index(signstr, ":")
	return uri.String(), signstr[index+1:], fmt.Sprintf("%d", date)
}

// Do 处理请求，返回响应结果。
func (conn Conn) Do(method, bucketName, objectName, urlParams, subResource string,
	headers map[string]string, data io.Reader, initCRC uint64) (*Response, error) {
	uri := conn.url.getURL(bucketName, objectName, urlParams)
	resource := conn.url.getResource(bucketName, objectName, subResource)
	return conn.doRequest(method, uri, resource, headers, data, initCRC)
}

func (conn Conn) doRequest(method string, uri *url.URL, canonicalizedResource string,
	headers map[string]string, data io.Reader, initCRC uint64) (*Response, error) {
	httpTimeOut := conn.config.HTTPTimeout
	method = strings.ToUpper(method)
	if !conn.config.IsUseProxy {
		uri.Opaque = uri.Path
	}
	req := &http.Request{
		Method:     method,
		URL:        uri,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Host:       uri.Host,
	}

	fd, crc := conn.handleBody(req, data, initCRC)
	if fd != nil {
		defer func() {
			fd.Close()
			os.Remove(fd.Name())
		}()
	}

	date := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set(HTTPHeaderDate, date)
	req.Header.Set(HTTPHeaderHost, conn.config.Endpoint)
	req.Header.Set(HTTPHeaderUserAgent, conn.config.UserAgent)
	if conn.config.SecurityToken != "" {
		req.Header.Set(HTTPHeaderOssSecurityToken, conn.config.SecurityToken)
	}

	if headers != nil {
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}

	conn.signHeader(req, canonicalizedResource)

	var transport *http.Transport
	if conn.config.IsUseProxy {
		// proxy
		proxyURL, err := url.Parse(conn.config.ProxyHost)
		if err != nil {
			return nil, err
		}

		transport = &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			Dial: func(netw, addr string) (net.Conn, error) {
				conn, err := net.DialTimeout(netw, addr, httpTimeOut.ConnectTimeout)
				if err != nil {
					return nil, err
				}
				return newTimeoutConn(conn, httpTimeOut.ReadWriteTimeout, httpTimeOut.LongTimeout), nil
			},
			ResponseHeaderTimeout: httpTimeOut.HeaderTimeout,
			MaxIdleConnsPerHost:   2000,
		}

		if conn.config.IsAuthProxy {
			auth := conn.config.ProxyUser + ":" + conn.config.ProxyPassword
			basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))
			req.Header.Set("Proxy-Authorization", basic)
		}
	} else {
		// no proxy
		transport = &http.Transport{
			Dial: func(netw, addr string) (net.Conn, error) {
				conn, err := net.DialTimeout(netw, addr, httpTimeOut.ConnectTimeout)
				if err != nil {
					return nil, err
				}
				return newTimeoutConn(conn, httpTimeOut.ReadWriteTimeout, httpTimeOut.LongTimeout), nil
			},
			ResponseHeaderTimeout: httpTimeOut.HeaderTimeout,
			MaxIdleConnsPerHost:   2000,
		}
	}

	timeoutClient := &http.Client{Transport: transport}

	resp, err := timeoutClient.Do(req)
	if err != nil {
		return nil, err
	}

	return conn.handleResponse(resp, crc)
}

// handle request body
func (conn Conn) handleBody(req *http.Request, body io.Reader, initCRC uint64) (*os.File, hash.Hash64) {
	var file *os.File
	var crc hash.Hash64
	reader := body

	// length
	switch v := body.(type) {
	case *bytes.Buffer:
		req.ContentLength = int64(v.Len())
	case *bytes.Reader:
		req.ContentLength = int64(v.Len())
	case *strings.Reader:
		req.ContentLength = int64(v.Len())
	case *os.File:
		req.ContentLength = tryGetFileSize(v)
	}
	req.Header.Set(HTTPHeaderContentLength, strconv.FormatInt(req.ContentLength, 10))

	// md5
	if body != nil && conn.config.IsEnableMD5 && req.Header.Get(HTTPHeaderContentMD5) == "" {
		if req.ContentLength == 0 || req.ContentLength > conn.config.MD5Threshold {
			// huge body, use temporary file
			file, _ = ioutil.TempFile(os.TempDir(), TempFilePrefix)
			if file != nil {
				io.Copy(file, body)
				file.Seek(0, os.SEEK_SET)
				md5 := md5.New()
				io.Copy(md5, file)
				sum := md5.Sum(nil)
				b64 := base64.StdEncoding.EncodeToString(sum[:])
				req.Header.Set(HTTPHeaderContentMD5, b64)
				file.Seek(0, os.SEEK_SET)
				reader = file
			}
		} else {
			// small body, use memory
			buf, _ := ioutil.ReadAll(body)
			sum := md5.Sum(buf)
			b64 := base64.StdEncoding.EncodeToString(sum[:])
			req.Header.Set(HTTPHeaderContentMD5, b64)
			reader = bytes.NewReader(buf)
		}
	}

	if reader != nil && conn.config.IsEnableCRC {
		crc = NewCRC(crcTable(), initCRC)
		reader = io.TeeReader(reader, crc)
	}

	rc, ok := reader.(io.ReadCloser)
	if !ok && reader != nil {
		rc = ioutil.NopCloser(reader)
	}
	req.Body = rc

	return file, crc
}

func tryGetFileSize(f *os.File) int64 {
	fInfo, _ := f.Stat()
	return fInfo.Size()
}

// handle response
func (conn Conn) handleResponse(resp *http.Response, crc hash.Hash64) (*Response, error) {
	var cliCRC uint64
	var srvCRC uint64

	statusCode := resp.StatusCode
	if statusCode >= 400 && statusCode <= 505 {
		// 4xx and 5xx indicate that the operation has error occurred
		var respBody []byte
		respBody, err := readResponseBody(resp)
		if err != nil {
			return nil, err
		}

		if len(respBody) == 0 {
			// no error in response body
			err = fmt.Errorf("oss: service returned without a response body (%s)", resp.Status)
		} else {
			// response contains storage service error object, unmarshal
			srvErr, errIn := serviceErrFromXML(respBody, resp.StatusCode,
				resp.Header.Get(HTTPHeaderOssRequestID))
			if err != nil { // error unmarshaling the error response
				err = errIn
			}
			err = srvErr
		}
		return &Response{
			StatusCode: resp.StatusCode,
			Headers:    resp.Header,
			Body:       ioutil.NopCloser(bytes.NewReader(respBody)), // restore the body
		}, err
	} else if statusCode >= 300 && statusCode <= 307 {
		// oss use 3xx, but response has no body
		err := fmt.Errorf("oss: service returned %d,%s", resp.StatusCode, resp.Status)
		return &Response{
			StatusCode: resp.StatusCode,
			Headers:    resp.Header,
			Body:       resp.Body,
		}, err
	}

	if conn.config.IsEnableCRC && crc != nil {
		cliCRC = crc.Sum64()
	}
	srvCRC, _ = strconv.ParseUint(resp.Header.Get(HTTPHeaderOssCRC64), 10, 64)

	// 2xx, successful
	return &Response{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       resp.Body,
		ClientCRC:  cliCRC,
		ServerCRC:  srvCRC,
	}, nil
}

func readResponseBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	out, err := ioutil.ReadAll(resp.Body)
	if err == io.EOF {
		err = nil
	}
	return out, err
}

func serviceErrFromXML(body []byte, statusCode int, requestID string) (ServiceError, error) {
	var storageErr ServiceError
	if err := xml.Unmarshal(body, &storageErr); err != nil {
		return storageErr, err
	}
	storageErr.StatusCode = statusCode
	storageErr.RequestID = requestID
	storageErr.RawMessage = string(body)
	return storageErr, nil
}

func xmlUnmarshal(body io.Reader, v interface{}) error {
	data, err := ioutil.ReadAll(body)
	if err != nil {
		return err
	}
	return xml.Unmarshal(data, v)
}

// Handle http timeout
type timeoutConn struct {
	conn        net.Conn
	timeout     time.Duration
	longTimeout time.Duration
}

func newTimeoutConn(conn net.Conn, timeout time.Duration, longTimeout time.Duration) *timeoutConn {
	conn.SetReadDeadline(time.Now().Add(longTimeout))
	return &timeoutConn{
		conn:        conn,
		timeout:     timeout,
		longTimeout: longTimeout,
	}
}

func (c *timeoutConn) Read(b []byte) (n int, err error) {
	c.SetReadDeadline(time.Now().Add(c.timeout))
	n, err = c.conn.Read(b)
	c.SetReadDeadline(time.Now().Add(c.longTimeout))
	return n, err
}

func (c *timeoutConn) Write(b []byte) (n int, err error) {
	c.SetWriteDeadline(time.Now().Add(c.timeout))
	n, err = c.conn.Write(b)
	c.SetReadDeadline(time.Now().Add(c.longTimeout))
	return n, err
}

func (c *timeoutConn) Close() error {
	return c.conn.Close()
}

func (c *timeoutConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *timeoutConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *timeoutConn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

func (c *timeoutConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *timeoutConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

// UrlMaker - build url and resource
const (
	urlTypeCname  = 1
	urlTypeIP     = 2
	urlTypeAliyun = 3
)

type urlMaker struct {
	Scheme  string // http or https
	NetLoc  string // host or ip
	Type    int    // 1 CNAME 2 IP 3 ALIYUN
	IsProxy bool   // proxy
}

// Parse endpoint
func (um *urlMaker) Init(endpoint string, isCname bool, isProxy bool) {
	if strings.HasPrefix(endpoint, "http://") {
		um.Scheme = "http"
		um.NetLoc = endpoint[len("http://"):]
	} else if strings.HasPrefix(endpoint, "https://") {
		um.Scheme = "https"
		um.NetLoc = endpoint[len("https://"):]
	} else {
		um.Scheme = "http"
		um.NetLoc = endpoint
	}

	host, _, err := net.SplitHostPort(um.NetLoc)
	if err != nil {
		host = um.NetLoc
	}
	ip := net.ParseIP(host)
	if ip != nil {
		um.Type = urlTypeIP
	} else if isCname {
		um.Type = urlTypeCname
	} else {
		um.Type = urlTypeAliyun
	}
	um.IsProxy = isProxy
}

// Build URL
func (um urlMaker) getURL(bucket, object, params string) *url.URL {
	var host = ""
	var path = ""

	if !um.IsProxy {
		object = url.QueryEscape(object)
	}

	if um.Type == urlTypeCname {
		host = um.NetLoc
		path = "/" + object
	} else if um.Type == urlTypeIP {
		if bucket == "" {
			host = um.NetLoc
			path = "/"
		} else {
			host = um.NetLoc
			path = fmt.Sprintf("/%s/%s", bucket, object)
		}
	} else {
		if bucket == "" {
			host = um.NetLoc
			path = "/"
		} else {
			host = bucket + "." + um.NetLoc
			path = "/" + object
		}
	}

	uri := &url.URL{
		Scheme:   um.Scheme,
		Host:     host,
		Path:     path,
		RawQuery: params,
	}

	return uri
}

// Canonicalized Resource
func (um urlMaker) getResource(bucketName, objectName, subResource string) string {
	if subResource != "" {
		subResource = "?" + subResource
	}
	if bucketName == "" {
		return fmt.Sprintf("/%s%s", bucketName, subResource)
	}
	return fmt.Sprintf("/%s/%s%s", bucketName, objectName, subResource)
}

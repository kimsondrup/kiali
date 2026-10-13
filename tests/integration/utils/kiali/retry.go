package kiali

import (
	"bytes"
	"net/http"
	"time"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/util/httputil"
)

// withRetry does a function call with retries and returns the last error
// if it fails.
func withRetry(f func() error, retries int) error {
	var err error
	for i := 0; i < retries; i++ {
		err = f()
		if err == nil {
			return nil
		}
	}
	return err
}

// httpGETWithRetry wraps the httpGET function with retries.
func httpGETWithRetry(url string, auth *config.Auth, timeout time.Duration, customHeaders map[string]string, cookies []*http.Cookie, conf *config.Config) ([]byte, int, []*http.Cookie, error) {
	var body []byte
	var code int
	var err error
	err = withRetry(func() error {
		body, code, cookies, err = httputil.HttpGet(url, auth, timeout, customHeaders, cookies, conf)
		return err
	}, 3)
	return body, code, cookies, err
}

// httpPOSTWithRetry wraps the httpPOST function with retries.
//
// The body arrives as a byte slice and not as an io.Reader because a Reader is drained
// by the first attempt, so a retry would post an empty body.
func httpPOSTWithRetry(url string, auth *config.Auth, requestBody []byte, timeout time.Duration, customHeaders map[string]string, conf *config.Config) ([]byte, int, []*http.Cookie, error) {
	var body []byte
	var code int
	var cookies []*http.Cookie
	var err error
	err = withRetry(func() error {
		body, code, cookies, err = httputil.HttpPost(url, auth, bytes.NewReader(requestBody), timeout, customHeaders, conf)
		return err
	}, 3)
	return body, code, cookies, err
}

package utils

import (
	"bytes"
	"net/http"
	"time"
)

func Max(x, y int64) int64 {
	if x > y {
		return x
	}
	return y
}

func Min(x, y int64) int64 {
	if x < y {
		return x
	}
	return y
}

func HttpPatch(url string, headers map[string]string, content []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewBuffer(content))
	if err != nil {
		return nil, err
	}

	for header, value := range headers {
		req.Header.Set(header, value)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	return client.Do(req)
}

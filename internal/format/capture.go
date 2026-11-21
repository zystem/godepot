package format

import (
	"bytes"
	"net/http"
)

// Capture is a ResponseWriter that takes the answer into memory, up to Max bytes: what a format that reads one of its own files before it answers (the cached list of an upstream) hands to the handler of
// raw. Full is true when the answer was larger than Max (and Buf then holds the first Max bytes).
type Capture struct {
	Header_ http.Header
	Code    int
	Buf     bytes.Buffer
	Full    bool
	Max     int
}

// NewCapture makes a Capture that holds up to max bytes.
func NewCapture(max int) *Capture { return &Capture{Header_: http.Header{}, Max: max} }

// Header is the header of the answer.
func (c *Capture) Header() http.Header { return c.Header_ }

// WriteHeader notes the first status.
func (c *Capture) WriteHeader(code int) {
	if c.Code == 0 {
		c.Code = code
	}
}

// Write takes bytes.
func (c *Capture) Write(b []byte) (int, error) {
	if c.Code == 0 {
		c.Code = http.StatusOK
	}
	if c.Buf.Len()+len(b) > c.Max {
		c.Full = true
		return len(b), nil
	}
	return c.Buf.Write(b)
}

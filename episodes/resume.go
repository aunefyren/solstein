package episodes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aunefyren/solstein/logger"
)

// maxResumes is how many times one download may carry on after its
// connection broke. Through a VPN a connection can die partway through a
// long episode (seen with Proton when a key's session moved servers); a
// 200 MB download shouldn't start over for that.
const maxResumes = 3

// resumingBody reads a download's body, and when the connection breaks
// before the end, asks the URL that answered for the rest (Range from the
// byte it got to, If-Range with the response's validator) and carries on
// from there. It only resumes when the full length is known and the answer
// is exactly the rest of the same file: a 206 whose Content-Range starts
// where the download stopped and has the same total. Anything else returns
// the original error, and the attempt fails as it would have.
type resumingBody struct {
	ctx       context.Context
	client    *http.Client
	url       string
	prepare   func(*http.Request)
	validator string // a strong ETag, or Last-Modified; empty if neither
	etag      string
	total     int64 // -1 when unknown
	body      io.ReadCloser
	read      int64
	resumes   int
	broken    error // a failed read to resume from on the next Read
}

func newResumingBody(ctx context.Context, client *http.Client, response *http.Response, prepare func(*http.Request)) *resumingBody {
	body := &resumingBody{
		ctx:     ctx,
		client:  client,
		url:     response.Request.URL.String(),
		prepare: prepare,
		etag:    response.Header.Get("ETag"),
		total:   -1,
		body:    response.Body,
	}
	if response.StatusCode == http.StatusOK && response.ContentLength > 0 {
		body.total = response.ContentLength
	}
	// If-Range takes a strong ETag or a date (RFC 9110, 13.1.5).
	if body.etag != "" && !strings.HasPrefix(body.etag, "W/") {
		body.validator = body.etag
	} else {
		body.validator = response.Header.Get("Last-Modified")
	}
	return body
}

func (body *resumingBody) Read(buffer []byte) (int, error) {
	if body.broken != nil {
		cause := body.broken
		body.broken = nil
		if err := body.resume(cause); err != nil {
			return 0, err
		}
	}
	n, err := body.body.Read(buffer)
	body.read += int64(n)
	if err == nil || errors.Is(err, io.EOF) || !body.resumable() {
		return n, err
	}
	if n > 0 {
		body.broken = err // hand over what arrived; resume on the next read
		return n, nil
	}
	if err := body.resume(err); err != nil {
		return 0, err
	}
	return body.Read(buffer)
}

func (body *resumingBody) Close() error {
	return body.body.Close()
}

// resumable reports whether a failed read may be carried on: the download
// wasn't cancelled (by its caller, or by the idle timeout), its length is
// known and it hasn't all arrived, and it has resumes left.
func (body *resumingBody) resumable() bool {
	return body.ctx.Err() == nil && body.total > 0 && body.read < body.total && body.resumes < maxResumes
}

// resume replaces the broken body with the rest of the file, or returns
// why it can't, wrapping cause.
func (body *resumingBody) resume(cause error) error {
	if !body.resumable() {
		return cause
	}
	body.body.Close()
	body.resumes++
	// Not on the connection that just broke: an HTTP/2 connection is
	// shared by every request to its host.
	body.client.CloseIdleConnections()
	select {
	case <-time.After(fetchRetryDelay):
	case <-body.ctx.Done():
		return cause
	}

	request, err := http.NewRequestWithContext(body.ctx, http.MethodGet, body.url, nil)
	if err != nil {
		return cause
	}
	body.prepare(request)
	request.Header.Set("Range", "bytes="+strconv.FormatInt(body.read, 10)+"-")
	if body.validator != "" {
		request.Header.Set("If-Range", body.validator)
	}
	response, err := body.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w (resuming after %d bytes failed too: %w)", cause, body.read, err)
	}
	if problem := body.checkRest(response); problem != "" {
		response.Body.Close()
		return fmt.Errorf("%w (can't resume after %d bytes: %s)", cause, body.read, problem)
	}
	logger.Log.Info(fmt.Sprintf("Download from %s broke after %d of %d bytes (%s); resumed from there.", hostOf(body.url), body.read, body.total, withoutURLs(cause)))
	body.body = response.Body
	return nil
}

// checkRest says what is wrong with a response to a resume request, or ""
// when it is the rest of the same file.
func (body *resumingBody) checkRest(response *http.Response) string {
	if response.StatusCode != http.StatusPartialContent {
		return "the source answered " + response.Status + " instead of the rest of the file"
	}
	if etag := response.Header.Get("ETag"); body.etag != "" && etag != "" && etag != body.etag {
		return "the file has changed"
	}
	start, total, ok := parseContentRange(response.Header.Get("Content-Range"))
	if !ok || start != body.read || total != body.total {
		return fmt.Sprintf("Content-Range %q doesn't continue %d of %d bytes", response.Header.Get("Content-Range"), body.read, body.total)
	}
	return ""
}

// parseContentRange reads "bytes start-end/total", total known.
func parseContentRange(value string) (start, total int64, ok bool) {
	spec, found := strings.CutPrefix(value, "bytes ")
	if !found {
		return 0, 0, false
	}
	span, size, found := strings.Cut(spec, "/")
	if !found {
		return 0, 0, false
	}
	first, _, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	total, err = strconv.ParseInt(size, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return start, total, true
}

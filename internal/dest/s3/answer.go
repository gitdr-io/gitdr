package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"gitdr.io/gitdr/internal/dest"
)

// answers is the option for a call whose answer decides what gitdr says about a bucket. A
// success has to be the document named root, and a failure an S3 error document. Anything else,
// a web page, an empty body or another call's document, fails the call with
// dest.ErrNotStorageAPI before the SDK reads it.
//
// The SDK reads what it is handed. Given a well-formed web page with a 200 where the lock
// configuration belongs, it parsed a configuration with nothing in it, and gitdr reported "Object
// Lock not enabled", an earned negative, about an endpoint that was not S3 at all. Given the page
// with a 404, it made the code NotFound up from the status. The same page in place of an object's
// retention read as an object holding none.
func answers(root string) func(*awss3.Options) {
	return func(o *awss3.Options) {
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			return stack.Deserialize.Insert(answerCheck{root: root}, "OperationDeserializer", middleware.After)
		})
	}
}

// answerCheck runs under the call's deserializer, so it sees a response before the SDK reads it.
// It also runs above the SDK's own check for an S3 error sent with a 200, which turns that status
// into a 500, so such an error reaches it as the failure it is.
type answerCheck struct{ root string }

func (answerCheck) ID() string { return "gitdr:AnswerCheck" }

func (c answerCheck) HandleDeserialize(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (
	middleware.DeserializeOutput, middleware.Metadata, error,
) {
	out, md, err := next.HandleDeserialize(ctx, in)
	if err != nil {
		return out, md, err
	}
	resp, ok := out.RawResponse.(*smithyhttp.Response)
	if !ok || resp.Body == nil {
		return out, md, nil
	}
	want := c.root
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		want = "Error"
	}
	got, read, err := rootElement(resp.Body)
	if err != nil || got != want {
		_ = resp.Body.Close()
		if err == nil {
			err = fmt.Errorf("%w: HTTP %d and %s where %s belongs", dest.ErrNotStorageAPI, resp.StatusCode, describeRoot(got), want)
		}
		return out, md, err
	}
	resp.Body = replayBody{io.MultiReader(bytes.NewReader(read), resp.Body), resp.Body}
	return out, md, nil
}

// rootElement reads r as far as the start of its first element, at most 64 KiB in. It returns
// that element's name, "" when there is none, and every byte it read. Its error is only ever a
// failed read: a connection that broke is not a page.
func rootElement(r io.Reader) (string, []byte, error) {
	var read bytes.Buffer
	src := &readFailure{r: io.LimitReader(r, 64<<10)}
	dec := xml.NewDecoder(io.TeeReader(src, &read))
	for {
		tok, err := dec.RawToken()
		if err != nil {
			return "", read.Bytes(), src.err
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local, read.Bytes(), nil
		}
	}
}

// readFailure remembers a read that failed, as opposed to one that reached the end.
type readFailure struct {
	r   io.Reader
	err error
}

func (f *readFailure) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err != nil && err != io.EOF {
		f.err = err
	}
	return n, err
}

func describeRoot(root string) string {
	if root == "" {
		return "no XML document"
	}
	return "a document whose root is " + dest.ShapedCode(root)
}

// replayBody gives back what rootElement read, then the rest, and closes the original body.
type replayBody struct {
	io.Reader
	io.Closer
}

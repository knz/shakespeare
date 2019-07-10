package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
)

type errorCollection struct {
	errs []error
}

var _ error = (*errorCollection)(nil)
var _ fmt.Formatter = (*errorCollection)(nil)

func combineErrors(err1, err2 error) error {
	if err1 == nil {
		return err2
	}
	if err2 == nil {
		return err1
	}
	c1, ok1 := err1.(*errorCollection)
	c2, ok2 := err2.(*errorCollection)
	if ok1 && ok2 {
		return &errorCollection{
			errs: append(append([]error{}, c1.errs...), c2.errs...),
		}
	} else if ok1 {
		return &errorCollection{
			errs: append(append([]error{}, c1.errs...), err2),
		}
	} else if ok2 {
		return &errorCollection{
			errs: append(append([]error{}, err1), c2.errs...),
		}
	}
	return &errorCollection{errs: []error{err1, err2}}
}

func (e *errorCollection) Error() string { return fmt.Sprintf("%v", e) }

func (e *errorCollection) Cause() error  { return e.errs[len(e.errs)-1] }
func (e *errorCollection) Unwrap() error { return e.errs[len(e.errs)-1] }

func (e *errorCollection) Format(s fmt.State, verb rune) { errors.FormatError(e, s, verb) }

func (e *errorCollection) FormatError(p errors.Printer) error {
	p.Printf("%d errors:", len(e.errs))
	var buf bytes.Buffer
	for i, err := range e.errs {
		p.Printf("\n(%d) ", i+1)
		buf.Reset()
		RenderError(&buf, err)
		p.Print(strings.ReplaceAll(buf.String(), "\n", "\n    "))
	}

	if p.Detail() {
		p.Print("\n-- details follow")
		for i, err := range e.errs {
			p.Printf("\n(%d) %+v", i+1, err)
		}
	}
	return nil
}

type delayedS struct {
	fn func() string
}

func (d delayedS) String() string { return d.fn() }

func wrapCtxErr(ctx context.Context) error {
	return errors.WithContextTags(errors.WithStackDepth(ctx.Err(), 1), ctx)
}

func RenderError(w io.Writer, err error) {
	fmt.Fprintf(w, "%v", err)
	if bufs := errors.GetContextTags(err); len(bufs) > 0 {
		var buf *logtags.Buffer
		for _, b := range bufs {
			buf = buf.Merge(b)
		}
		fmt.Fprintf(w, "\n--\n(context: %s)", buf)
	}
	if d := errors.FlattenDetails(err); d != "" {
		fmt.Fprintf(w, "\n--\n%s", d)
	}
	if d := errors.FlattenHints(err); d != "" {
		fmt.Fprintf(w, "\nHINT: %s", d)
	}
}

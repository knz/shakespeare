package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/errors/errbase"
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

func (e *errorCollection) Error() string {
	var buf bytes.Buffer
	comma := ""
	for i, err := range e.errs {
		buf.WriteString(comma)
		comma = "\n\nand also, another error: "
		if i == len(e.errs)-1 {
			// The last error will be detailed by the surrounding function.
			buf.WriteString(err.Error())
		} else {
			RenderError(&buf, err)
		}
	}
	return buf.String()
}

func (e *errorCollection) Cause() error  { return e.errs[len(e.errs)-1] }
func (e *errorCollection) Unwrap() error { return e.errs[len(e.errs)-1] }

func (e *errorCollection) Format(s fmt.State, verb rune) {
	comma := ""
	for _, err := range e.errs {
		fmt.Fprint(s, comma)
		comma = "\n\nand also, another error: "
		switch {
		case verb == 'v' && s.Flag('+'):
			errbase.FormatError(s, verb, err)
		default:
			RenderError(s, err)
		}
	}
}

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
		fmt.Fprintf(w, "\n--\n(context of error: %s)", buf)
	}
	if d := errors.FlattenDetails(err); d != "" {
		fmt.Fprintf(w, "\n--\n%s", d)
	}
	if d := errors.FlattenHints(err); d != "" {
		fmt.Fprintf(w, "\nHINT: %s", d)
	}
}

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/knz/shakespeare/pkg/crdb/log"
)

type reader struct {
	// readers is the stack of readers. This is expanded with includes.
	readers []*subreader
	// paths to search for includes
	includePath []string
}

type subreader struct {
	// rd is where input is read from.
	rd *bufio.Reader
	// f is the currently opened file for this input. nil when reading from stdin.
	f *os.File
	// file is the name of the file being read from. Used for pos reporting.
	file string
	// lineno is the current line number. Used for pos reporting.
	lineno int
	// lastLines are the lines of input read so far. Has a line for all
	// the values of lineno. Used for pos reporting.
	lines []string
	// parent is the parent reader, used for pos reporting during includes.
	parent *subreader
}

func newReader(ctx context.Context, file string, includePath []string) (*reader, error) {
	r, err := newSubReader(ctx, file, includePath)
	if err != nil {
		return nil, err
	}
	return &reader{readers: []*subreader{r}, includePath: includePath}, nil
}

func newReaderFromString(file string, data string) (*reader, error) {
	sr := &subreader{
		lineno: 1,
		file:   file,
		rd:     bufio.NewReader(strings.NewReader(data)),
	}
	return &reader{readers: []*subreader{sr}, includePath: nil}, nil
}

func hasLocalDir(includePath []string) bool {
	for _, i := range includePath {
		if i == "." {
			return true
		}
	}
	return false
}

func newSubReader(ctx context.Context, file string, includePath []string) (*subreader, error) {
	r := &subreader{lineno: 1, file: file}
	if file == "-" {
		r.file = "<stdin>"
		r.rd = bufio.NewReader(os.Stdin)
	} else {
		var f *os.File
		var fName string

		for _, p := range includePath {
			fName = filepath.Join(p, file)
			candidateF, err := os.Open(fName)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			f = candidateF
			r.file = fName
			break
		}
		if f == nil {
			err := errors.Wrap(os.ErrNotExist, file)
			err = errors.WithDetailf(err, "include search path: %+v", includePath)
			return nil, err
		}
		r.rd = bufio.NewReader(f)
	}
	log.Infof(ctx, "including specification from %s", r.file)
	return r, nil
}

func (r *reader) close() {
	for i := range r.readers {
		r.readers[i].close()
	}
}

func (r *subreader) close() {
	if r.f != nil {
		_ = r.f.Close()
	}
}

type pos struct {
	lineno int
	r      *subreader
}

func (p *pos) wrapErr(err error) error {
	err = errors.WrapWithDepthf(1, err, "%s:%d", p.r.file, p.lineno)

	lineIdx := p.lineno - 1
	var buf bytes.Buffer
	buf.WriteString("while parsing:\n")
	contextLineStart := lineIdx
	if contextLineStart > 0 {
		contextLineStart -= 2
		if contextLineStart < 0 {
			contextLineStart = 0
		}
	}
	if contextLineStart < lineIdx {
		for i, line := range p.r.lines[contextLineStart:lineIdx] {
			fmt.Fprintf(&buf, "%s:%-3d   %s\n", p.r.file, contextLineStart+i+1, strings.TrimSuffix(line, "\n"))
		}
	}
	fmt.Fprintf(&buf, "%s:%-3d > %s\n", p.r.file, lineIdx+1, strings.TrimSuffix(p.r.lines[lineIdx], "\n"))

	contextLineEnd := lineIdx
	if contextLineEnd < len(p.r.lines)-1 {
		contextLineEnd += 2
		if contextLineEnd >= len(p.r.lines) {
			contextLineEnd = len(p.r.lines) - 1
		}
	}
	if contextLineEnd > lineIdx {
		for i, line := range p.r.lines[lineIdx+1 : contextLineEnd+1] {
			fmt.Fprintf(&buf, "%s:%-3d  %s\n", p.r.file, lineIdx+1+i+1, strings.TrimSuffix(line, "\n"))
		}
	}
	err = errors.WithDetail(err, strings.TrimSpace(buf.String()))

	if p.r.parent != nil {
		var buf bytes.Buffer
		buf.WriteString("in file included from:\n")
		comma := ""
		for r := p.r.parent; r != nil; r = r.parent {
			fmt.Fprintf(&buf, "%s%s:%d <- here", comma, r.file, r.lineno)
			comma = "\n"
		}
		err = errors.WithDetail(err, buf.String())
	}
	return err
}

func (r *reader) readLine(
	ctx context.Context,
) (line string, p pos, stop bool, skip bool, err error) {
	return r.readers[len(r.readers)-1].readLine(ctx, r)
}

// readLine reads a logical line of specification. It may be split
// across multiple input lines using backslashes.
func (r *subreader) readLine(
	ctx context.Context, pr *reader,
) (line string, p pos, stop bool, skip bool, err error) {
	startPos := pos{lineno: r.lineno, r: r}
	for {
		var oneline string
		oneline, err = r.rd.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", pos{}, true, false, startPos.wrapErr(err)
		}
		r.lines = append(r.lines, oneline)
		r.lineno++
		if strings.HasSuffix(oneline, "\\\n") {
			oneline = oneline[:len(oneline)-2] + "\n"
			line += oneline
			continue
		}
		oneline = strings.TrimSuffix(oneline, "\n")
		if err == io.EOF && line != "" {
			return "", pos{}, true, false, startPos.wrapErr(errors.New("EOF encountered while expecting line continuation"))
		}
		line += oneline
		break
	}

	line = strings.TrimSpace(line)
	if err == io.EOF && ignoreLine(line) {
		if len(pr.readers) > 1 {
			// More data perhaps, let the thing continue.
			pr.readers = pr.readers[:len(pr.readers)-1]
			r.close()
			return "", startPos, false, true, nil
		}
		// No more readers, stop.
		return "", startPos, true, false, nil
	}

	if ignoreLine(line) {
		return "", startPos, false, true, nil
	}

	// Handle file includes.
	if strings.HasPrefix(line, "include ") {
		if len(pr.readers) >= 10 {
			// Avoid infinite include recursion.
			return "", pos{}, true, false, startPos.wrapErr(
				errors.New("include depth limit exceeded"))
		}

		fName := strings.TrimPrefix(line, "include ")

		// The first include search path will be the path of the file
		// we're currently reading.
		curPath := filepath.Dir(r.file)
		includePath := append([]string{curPath}, pr.includePath...)

		sr, err := newSubReader(ctx, fName, includePath)
		if err != nil {
			return "", pos{}, true, false, startPos.wrapErr(err)
		}
		sr.parent = pr.readers[len(pr.readers)-1]
		pr.readers = append(pr.readers, sr)
		return "", startPos, false, true, nil
	}

	return line, startPos, false, false, nil
}

// Empty lines and comment lines are ignored.
func ignoreLine(line string) bool {
	return line == "" || strings.HasPrefix(line, "#")
}

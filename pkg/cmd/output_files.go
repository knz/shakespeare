package cmd

import (
	"bufio"
	"os"
)

// outputFiles manages a collection of open files and associated
// buffered writers.
type outputFiles struct {
	files   map[string]*os.File
	writers map[string]*bufio.Writer
}

func newOutputFiles() *outputFiles {
	of := &outputFiles{}
	of.files = make(map[string]*os.File)
	of.writers = make(map[string]*bufio.Writer)
	return of
}

func (o *outputFiles) CloseAll() {
	for fName, f := range o.files {
		_ = o.writers[fName].Flush()
		_ = f.Close()
	}
}

func (o *outputFiles) Flush() {
	for _, w := range o.writers {
		_ = w.Flush()
	}
}

func (o *outputFiles) getWriter(fName string) (*bufio.Writer, error) {
	w, ok := o.writers[fName]
	if !ok {
		f, err := os.OpenFile(fName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return nil, err
		}
		o.files[fName] = f
		w = bufio.NewWriter(f)
		o.writers[fName] = w
	}
	return w, nil
}

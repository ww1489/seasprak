//go:build !windows

package jsonl

import (
	"os"

	product "github.com/ww1489/seasprak/internal/errors"
)

func boundOpenFile(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
	if name != "journal.jsonl" && name != "writer.lock" || flags&os.O_TRUNC != 0 || flags&os.O_CREATE != 0 && flags&os.O_EXCL == 0 {
		return nil, product.NewError(product.CodeInvalidArgument, "journal or writer lock open is invalid")
	}
	return root.OpenFile(name, flags, mode)
}

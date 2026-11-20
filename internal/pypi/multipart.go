package pypi

import (
	"io"
	"mime/multipart"

	"github.com/zystem/godepot/internal/blob"
)

// upload is a file that has been taken in.
type upload struct{ u *blob.Upload }

func newMultipart(r io.Reader, boundary string) *multipart.Reader {
	return multipart.NewReader(r, boundary)
}

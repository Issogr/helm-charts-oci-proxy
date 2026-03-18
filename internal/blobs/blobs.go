package blobs

import (
	cerrors "errors"
	"fmt"
	"github.com/container-registry/helm-charts-oci-proxy/internal/blobs/handler"
	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/sirupsen/logrus"
	"io"
	"net/http"
	"path"
	"strings"
)

type blobHandler interface {
	handler.BlobHandler
	handler.BlobStatHandler
}

// Blobs service
type Blobs struct {
	handler blobHandler `json:"blobHandler"`
	log     logrus.StdLogger
}

func NewBlobs(blobHandler blobHandler, log logrus.StdLogger) *Blobs {
	return &Blobs{handler: blobHandler, log: log}
}

func (b *Blobs) Handle(resp http.ResponseWriter, req *http.Request) error {
	ctx := req.Context()

	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	if elem[len(elem)-1] == "" {
		elem = elem[:len(elem)-1]
	}
	if len(elem) < 4 {
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "NAME_INVALID",
			Message: "Blobs must be attached to a repo",
		}
	}
	target := elem[len(elem)-1]
	repo := req.URL.Host + path.Join(elem[1:len(elem)-2]...)

	switch req.Method {
	case http.MethodHead:
		h, err := v1.NewHash(target)
		if err != nil {
			return &errors.RegError{
				Status:  http.StatusBadRequest,
				Code:    "NAME_INVALID",
				Message: "invalid digest",
			}
		}

		size, err := b.handler.Stat(ctx, repo, h)
		if cerrors.Is(err, ErrNotFound) {
			return regErrBlobUnknown
		}
		if err != nil {
			return errors.RegErrInternal(err)
		}

		resp.Header().Set("Content-Length", fmt.Sprint(size))
		resp.Header().Set("Docker-Content-Digest", h.String())
		resp.WriteHeader(http.StatusOK)
		return nil

	case http.MethodGet:
		h, err := v1.NewHash(target)
		if err != nil {
			return &errors.RegError{
				Status:  http.StatusBadRequest,
				Code:    "NAME_INVALID",
				Message: "invalid digest",
			}
		}

		size, err := b.handler.Stat(ctx, repo, h)
		if cerrors.Is(err, ErrNotFound) {
			return regErrBlobUnknown
		}
		if err != nil {
			return errors.RegErrInternal(err)
		}

		rc, err := b.handler.Get(ctx, repo, h)
		if cerrors.Is(err, ErrNotFound) {
			return regErrBlobUnknown
		}
		if err != nil {
			return errors.RegErrInternal(err)
		}
		defer rc.Close()

		resp.Header().Set("Content-Length", fmt.Sprint(size))
		resp.Header().Set("Docker-Content-Digest", h.String())
		resp.WriteHeader(http.StatusOK)
		_, err = io.Copy(resp, rc)
		if err != nil {
			return errors.RegErrInternal(err)
		}
		return nil

	default:
		return &errors.RegError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
}

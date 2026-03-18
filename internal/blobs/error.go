package blobs

import (
	cerrors "errors"
	"github.com/container-registry/helm-charts-oci-proxy/internal/errors"
	"net/http"
)

var ErrNotFound = cerrors.New("not found")

var regErrBlobUnknown = &errors.RegError{
	Status:  http.StatusNotFound,
	Code:    "BLOB_UNKNOWN",
	Message: "Unknown Blob",
}

package s3

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"sort"
	"strconv"

	"github.com/gogo/protobuf/types"
	"github.com/gorilla/mux"
	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/sirupsen/logrus"
)

// this is a var instead of a const so that we can make a pointer to it
var defaultMaxParts int = 1000

const maxAllowedParts = 10000

const initMultipartSource = `
<InitiateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<Bucket>{{ .bucket }}</Bucket>
	<Key>{{ .key }}</Key>
	<UploadId>{{ .uploadID }}</UploadId>
</InitiateMultipartUploadResult>
`

const listMultipartSource = `
<ListPartsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<Bucket>{{ .bucket }}</Bucket>
	<Key>{{ .key }}</Key>
	<UploadId>{{ .uploadID }}</UploadId>
	<Initiator>
		<ID>00000000000000000000000000000000</ID>
		<DisplayName>pachyderm</DisplayName>
	</Initiator>
	<Owner>
		<ID>00000000000000000000000000000000</ID>
		<DisplayName>pachyderm</DisplayName>
	</Owner>
	<StorageClass>STANDARD</StorageClass>
	<PartNumberMarker>{{ .partNumberMarker }}</PartNumberMarker>
	<NextPartNumberMarker>{{ .nextPartNumberMarker }}</NextPartNumberMarker>
	<MaxParts>{{ .maxParts }}</MaxParts>
	<IsTruncated>{{ .isTruncated }}</IsTruncated>
	{{ range .parts }}
		<Part>
			<PartNumber>{{ .Name }}</PartNumber>
			<LastModified>{{ .ModTime }}</LastModified>
			<ETag></ETag>
			<Size>{{ .Size }}</Size>
		</Part>
	{{ end }}
</ListPartsResult>
`

// multipartCompleteRoot represents the root XML element of a complete
// multipart request
type CompleteMultipartUpload struct {
	Parts []MultipartCompletePart `xml:"Part"`
}

// multipartCompletePart represents a <Part> XML element of a complete
// multipart request
type MultipartCompletePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type objectHandler struct {
	pc                    *client.APIClient
	multipartManager      *multipartFileManager
	initMultipartTemplate xmlTemplate
	listMultipartTemplate xmlTemplate
}

func newObjectHandler(pc *client.APIClient, multipartDir string) *objectHandler {
	var multipartManager *multipartFileManager
	if multipartDir != "" {
		multipartManager = newMultipartFileManager(multipartDir, maxAllowedParts)
	}

	return &objectHandler{
		pc:                    pc,
		multipartManager:      multipartManager,
		initMultipartTemplate: newXmlTemplate(http.StatusOK, "init-multipart", initMultipartSource),
		listMultipartTemplate: newXmlTemplate(http.StatusOK, "list-multipart", listMultipartSource),
	}
}

func (h *objectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	repo := vars["repo"]
	branch := vars["branch"]
	file := vars["file"]

	branchInfo, err := h.pc.InspectBranch(repo, branch)
	if err != nil {
		writeMaybeNotFound(w, r, err)
		return
	}

	if err := r.ParseForm(); err != nil {
		writeBadRequest(w, err)
		return
	}

	uploadID := r.FormValue("uploadId")

	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if uploadID != "" {
			h.listMultipart(w, r, branchInfo, file, uploadID)
		} else {
			h.get(w, r, branchInfo, file)
		}
	} else if r.Method == http.MethodPost {
		if _, ok := r.Form["uploads"]; ok {
			h.initMultipart(w, r, branchInfo, file)
		} else if uploadID != "" {
			h.completeMultipart(w, r, branchInfo, file, uploadID)
		} else {
			http.NotFound(w, r)
		}
	} else if r.Method == http.MethodPut {
		if uploadID != "" {
			h.uploadMultipart(w, r, branchInfo, file, uploadID)
		} else {
			h.put(w, r, branchInfo, file)
		}
	} else if r.Method == http.MethodDelete {
		if uploadID != "" {
			h.abortMultipart(w, r, branchInfo, file, uploadID)
		} else {
			h.delete(w, r, branchInfo, file)
		}
	} else {
		// method filtering on the mux router should prevent this
		panic("unreachable")
	}
}

func (h *objectHandler) get(w http.ResponseWriter, r *http.Request, branchInfo *pfs.BranchInfo, file string) {
	if branchInfo.Head == nil {
		http.NotFound(w, r)
		return
	}

	fileInfo, err := h.pc.InspectFile(branchInfo.Branch.Repo.Name, branchInfo.Branch.Name, file)
	if err != nil {
		writeMaybeNotFound(w, r, err)
		return
	}

	timestamp, err := types.TimestampFromProto(fileInfo.Committed)
	if err != nil {
		writeServerError(w, err)
		return
	}

	reader, err := h.pc.GetFileReadSeeker(branchInfo.Branch.Repo.Name, branchInfo.Branch.Name, file)
	if err != nil {
		writeServerError(w, err)
		return
	}

	http.ServeContent(w, r, "", timestamp, reader)
}

func (h *objectHandler) put(w http.ResponseWriter, r *http.Request, branchInfo *pfs.BranchInfo, file string) {
	success, err := h.withBodyReader(r, func(reader io.Reader) bool {
		_, err := h.pc.PutFileOverwrite(branchInfo.Branch.Repo.Name, branchInfo.Branch.Name, file, reader, 0)

		if err != nil {
			writeServerError(w, err)
			return false
		}

		return true
	})

	// if there's no error but the operation is not successful, we've already
	// written a response to the client
	if err != nil {
		writeBadRequest(w, err)
	} else if success {
		w.WriteHeader(http.StatusOK)
	}
}

func (h *objectHandler) delete(w http.ResponseWriter, r *http.Request, branchInfo *pfs.BranchInfo, file string) {
	if branchInfo.Head == nil {
		http.NotFound(w, r)
		return
	}

	if err := h.pc.DeleteFile(branchInfo.Branch.Repo.Name, branchInfo.Branch.Name, file); err != nil {
		writeMaybeNotFound(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// withBodyReader calls the provided callback with a reader for the HTTP
// request body. This also verifies the body against the `Content-MD5` header.
//
// The callback should return whether or not it succeeded. If it does not
// succeed, it is assumed that the callback wrote an appropriate failure
// response to the client.
//
// This function will return whether it succeeded and an error. If there is an
// error, it is because of a bad request. If this returns a failure but not an
// error, it implies that the callback returned a failure.
func (h *objectHandler) withBodyReader(r *http.Request, f func(io.Reader) bool) (bool, error) {
	expectedHash := r.Header.Get("Content-MD5")

	if expectedHash != "" {
		expectedHashBytes, err := base64.StdEncoding.DecodeString(expectedHash)
		if err != nil {
			err = fmt.Errorf("could not decode `Content-MD5`, as it is not base64-encoded")
			return false, err
		}

		hasher := md5.New()
		reader := io.TeeReader(r.Body, hasher)

		succeeded := f(reader)
		if !succeeded {
			return false, nil
		}

		actualHash := hasher.Sum(nil)
		if !bytes.Equal(expectedHashBytes, actualHash) {
			err = fmt.Errorf("content checksums differ; expected=%x, actual=%x", expectedHash, actualHash)
			return false, err
		}

		return true, nil
	}

	return f(r.Body), nil
}

func (h *objectHandler) initMultipart(w http.ResponseWriter, r *http.Request, branchInfo *pfs.BranchInfo, file string) {
	if h.multipartManager == nil {
		writeBadRequest(w, fmt.Errorf("multipart uploads disabled"))
		return
	}

	uploadID, err := h.multipartManager.init(file)
	if err != nil {
		writeServerError(w, err)
		return
	}

	h.initMultipartTemplate.render(w, map[string]interface{}{
		"bucket":   branchInfo.Branch.Repo.Name,
		"key":      fmt.Sprintf("%s/%s", branchInfo.Branch.Name, file),
		"uploadID": uploadID,
	})
}

func (h *objectHandler) listMultipart(w http.ResponseWriter, r *http.Request, branchInfo *pfs.BranchInfo, file string, uploadID string) {
	if h.multipartManager == nil {
		writeBadRequest(w, fmt.Errorf("multipart uploads disabled"))
		return
	}
	if err := h.multipartManager.checkExists(uploadID); err != nil {
		writeMaybeNotFound(w, r, err)
		return
	}

	marker, err := intFormValue(r, "part-number-marker", 1, defaultMaxParts, &defaultMaxParts)
	if err != nil {
		writeBadRequest(w, err)
		return
	}

	maxParts, err := intFormValue(r, "max-parts", 1, defaultMaxParts, &defaultMaxParts)
	if err != nil {
		writeBadRequest(w, err)
		return
	}

	fileInfos, err := h.multipartManager.listChunks(uploadID)
	if err != nil {
		writeServerError(w, err)
		return
	}

	isTruncated := false
	parts := []os.FileInfo{}
	for _, fileInfo := range fileInfos {
		// ignore errors converting the name since it's already been verified
		name, _ := strconv.Atoi(fileInfo.Name())

		if name < marker {
			continue
		}
		if len(parts) == maxParts {
			isTruncated = true
			break
		}
		parts = append(parts, fileInfo)
	}

	nextPartNumberMarker := ""
	if len(parts) > 0 {
		nextPartNumberMarker = parts[len(parts)-1].Name()
	}

	h.listMultipartTemplate.render(w, map[string]interface{}{
		"bucket":               branchInfo.Branch.Repo.Name,
		"key":                  file,
		"uploadID":             uploadID,
		"partNumberMarker":     marker,
		"nextPartNumberMarker": nextPartNumberMarker,
		"maxParts":             maxParts,
		"isTruncated":          isTruncated,
		"parts":                parts,
	})
}

func (h *objectHandler) uploadMultipart(w http.ResponseWriter, r *http.Request, branchInfo *pfs.BranchInfo, file string, uploadID string) {
	if h.multipartManager == nil {
		writeBadRequest(w, fmt.Errorf("multipart uploads disabled"))
		return
	}
	if err := h.multipartManager.checkExists(uploadID); err != nil {
		writeMaybeNotFound(w, r, err)
		return
	}

	partNumber, err := intFormValue(r, "partNumber", 1, maxAllowedParts, nil)
	if err != nil {
		writeBadRequest(w, err)
		return
	}

	success, err := h.withBodyReader(r, func(reader io.Reader) bool {
		if err := h.multipartManager.writeChunk(uploadID, partNumber, reader); err != nil {
			writeServerError(w, err)
			return false
		}
		return true
	})

	if err != nil || !success {
		// try to clean up the file if something failed
		removeErr := h.multipartManager.removeChunk(uploadID, partNumber)
		if removeErr != nil {
			logrus.Error("could not remove uploadID=%s, partNumber=%d: %v", uploadID, partNumber, removeErr)
		}

		if err != nil {
			writeBadRequest(w, err)
		}

		// if there's no error but the operation is not successful, we've
		// already written a response to the client
	} else {
		w.WriteHeader(http.StatusOK)
	}
}

func (h *objectHandler) completeMultipart(w http.ResponseWriter, r *http.Request, branchInfo *pfs.BranchInfo, file string, uploadID string) {
	if h.multipartManager == nil {
		writeBadRequest(w, fmt.Errorf("multipart uploads disabled"))
		return
	}
	if err := h.multipartManager.checkExists(uploadID); err != nil {
		writeMaybeNotFound(w, r, err)
		return
	}

	name, err := h.multipartManager.filepath(uploadID)
	if err != nil {
		writeServerError(w, err)
		return
	}

	bodyBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		writeServerError(w, fmt.Errorf("could not read request body: %v", err))
		return
	}

	payload := CompleteMultipartUpload{}
	err = xml.Unmarshal(bodyBytes, &payload)
	if err != nil {
		writeBadRequest(w, fmt.Errorf("body is invalid: %v", err))
		return
	}

	// verify that there's at least part, and all parts are in ascending order
	if len(payload.Parts) == 0 {
		writeBadRequest(w, fmt.Errorf("no parts specified"))
		return
	}
	isSorted := sort.SliceIsSorted(payload.Parts, func(i, j int) bool {
		return payload.Parts[i].PartNumber < payload.Parts[j].PartNumber
	})
	if !isSorted {
		writeBadRequest(w, fmt.Errorf("parts not in ascending order"))
		return
	}

	// ensure all the files exist
	for _, part := range payload.Parts {
		if err = h.multipartManager.checkChunkExists(uploadID, part.PartNumber); err != nil {
			writeBadRequest(w, fmt.Errorf("missing part %d", part.PartNumber))
			return
		}
	}

	// pull out the list of part numbers
	partNumbers := []int{}
	for _, part := range payload.Parts {
		partNumbers = append(partNumbers, part.PartNumber)
	}

	reader := newMultipartReader(h.multipartManager, uploadID, partNumbers)
	defer reader.Close()

	_, err = h.pc.PutFileOverwrite(branchInfo.Branch.Repo.Name, branchInfo.Branch.Name, name, reader, 0)
	if err != nil {
		writeServerError(w, err)
		return
	}

	err = h.multipartManager.remove(uploadID)
	if err != nil {
		writeServerError(w, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (h *objectHandler) abortMultipart(w http.ResponseWriter, r *http.Request, branchInfo *pfs.BranchInfo, file string, uploadID string) {
	if h.multipartManager == nil {
		writeBadRequest(w, fmt.Errorf("multipart uploads disabled"))
		return
	}
	if err := h.multipartManager.checkExists(uploadID); err != nil {
		writeMaybeNotFound(w, r, err)
		return
	}
	if err := h.multipartManager.remove(uploadID); err != nil {
		writeServerError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

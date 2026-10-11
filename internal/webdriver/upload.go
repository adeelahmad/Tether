package webdriver

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/adeelahmad/tether/internal/cdp"
)

// maxUpload bounds an uploaded file once unzipped.
const maxUpload = 256 << 20

// uploadFile implements Selenium's file upload extension: the body is a
// base64 zip holding one file, which is unpacked into a session temp dir
// (removed when the session ends). It returns the file's path, which the
// client then types into an <input type=file>.
func (h *Handler) uploadFile(_ context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	var req struct {
		File string `json:"file"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(req.File)
	if err != nil {
		return nil, wdErr(ErrInvalidArgument, "file must be a base64 zip archive")
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, wdErr(ErrInvalidArgument, "file must be a base64 zip archive: "+err.Error())
	}
	var files []*zip.File
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() {
			files = append(files, f)
		}
	}
	if len(files) != 1 {
		return nil, wdErr(ErrInvalidArgument, "the archive must hold exactly one file")
	}
	f := files[0]
	// Keep only the base name, so an entry can never point outside the
	// temp dir (zip slip).
	name := filepath.Base(filepath.FromSlash(f.Name))
	if name == "." || name == string(filepath.Separator) || strings.Contains(name, "..") {
		return nil, wdErr(ErrInvalidArgument, "the archived file has no usable name")
	}
	dir, err := os.MkdirTemp("", "tether-upload-")
	if err != nil {
		return nil, err
	}
	ws.addUpload(dir)
	path := filepath.Join(dir, name)
	if !strings.HasPrefix(path, filepath.Clean(dir)+string(filepath.Separator)) {
		return nil, wdErr(ErrInvalidArgument, "the archived file has no usable name")
	}
	if err := unzipTo(f, path); err != nil {
		return nil, err
	}
	return path, nil
}

func unzipTo(f *zip.File, path string) error {
	src, err := f.Open()
	if err != nil {
		return wdErr(ErrInvalidArgument, "reading the archive: "+err.Error())
	}
	defer src.Close()
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(dst, io.LimitReader(src, maxUpload+1))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > maxUpload {
		return wdErr(ErrInvalidArgument, "the uploaded file is larger than 256 MiB")
	}
	return nil
}

// setFiles implements Element Send Keys on an <input type=file>: text is
// one path per line. Paths are on the machine Tether and Chrome run on,
// such as those uploadFile returns.
func setFiles(ctx context.Context, ws *wdSession, ref, text string, multiple bool) error {
	var files []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		abs, err := filepath.Abs(line)
		if err != nil {
			return wdErr(ErrInvalidArgument, "bad file path "+line)
		}
		if st, err := os.Stat(abs); err != nil || st.IsDir() {
			return wdErr(ErrInvalidArgument, "file not found: "+line)
		}
		files = append(files, abs)
	}
	if len(files) == 0 {
		return wdErr(ErrInvalidArgument, "no file path given")
	}
	if len(files) > 1 && !multiple {
		return wdErr(ErrInvalidArgument, "the input takes one file")
	}
	id, _ := parseElementRef(ref)
	if err := ws.tab().SetFiles(ctx, id, files); err != nil {
		if errors.Is(err, cdp.ErrNoSuchFrame) {
			return err
		}
		return wdErr(ErrInvalidArgument, "setting the files: "+err.Error())
	}
	return nil
}

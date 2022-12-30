package patches

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"io"
	"io/fs"
)

func LoadPatches(patchesFs fs.FS) (string, map[string]*bytes.Reader, error) {
	versionHash := sha256.New()
	patches := make(map[string]*bytes.Reader)
	err := fs.WalkDir(patchesFs, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := patchesFs.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		var patchBuf bytes.Buffer
		if _, err := io.Copy(&patchBuf, f); err != nil {
			return err
		}

		reader := bytes.NewReader(patchBuf.Bytes())
		if _, err := reader.WriteTo(versionHash); err != nil {
			return err
		}

		if _, err := reader.Seek(0, io.SeekStart); err != nil {
			return err
		}

		files, _, err := gitdiff.Parse(reader)
		if err != nil {
			return err
		}
		for _, file := range files {
			if file.IsDelete || file.IsRename {
				panic("delete and rename patch not supported")
			}

			if _, err := reader.Seek(0, io.SeekStart); err != nil {
				return err
			}
			patches[file.OldName] = reader
		}
		return nil
	})

	if err != nil {
		return "", nil, err
	}
	return base64.RawStdEncoding.EncodeToString(versionHash.Sum(nil)), patches, nil
}

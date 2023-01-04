package patches

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"io"
	"io/fs"
	"os/exec"
	"strings"
)

func LoadPatches(patchesFs fs.FS) (string, map[string]string, error) {
	versionHash := sha256.New()
	patches := make(map[string]string)
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

		patchBytes := patchBuf.Bytes()

		if _, err := versionHash.Write(patchBytes); err != nil {
			return err
		}

		files, _, err := gitdiff.Parse(bytes.NewReader(patchBytes))
		if err != nil {
			return err
		}
		for _, file := range files {
			if file.IsDelete || file.IsRename {
				panic("delete and rename patch not supported")
			}

			patches[file.OldName] = string(patchBytes)
		}
		return nil
	})

	if err != nil {
		return "", nil, err
	}
	return base64.RawStdEncoding.EncodeToString(versionHash.Sum(nil)), patches, nil
}

func ApplyPatch(workingDirectory, patch string) error {
	cmd := exec.Command("git", "-C", workingDirectory, "apply")
	cmd.Stdin = strings.NewReader(patch)
	return cmd.Run()
}
